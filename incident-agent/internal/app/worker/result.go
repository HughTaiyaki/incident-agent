package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	// ResultSchema versions the Result projection, independently of stored state.
	ResultSchema = "run-result-v1"
	// EvidenceSchema versions the Evidence projection.
	EvidenceSchema = "run-evidence-v1"
	// terminalRetention mirrors the expiry the state script sets on terminal runs.
	terminalRetention = 7 * 24 * time.Hour
)

var (
	// ErrNotFound covers never-created runs and runs past terminal retention;
	// no marker survives retention, so the two cannot be told apart.
	ErrNotFound = errors.New("run not found: never created or past 7-day retention")
	// ErrCorrupt means stored state is unreadable or inconsistent. No report is inferred from it.
	ErrCorrupt = errors.New("run state corrupt")
	// ErrUnknownSchema means stored state was written by an unsupported version.
	ErrUnknownSchema = errors.New("run state has unknown schema version")
)

// Budget reports durable charges: settled usage where a provider reported it,
// the full reservation for failed or unknown calls.
type Budget struct {
	ChargedTokens   int `json:"charged_tokens"`
	ModelCalls      int `json:"model_calls"`
	BackendRequests int `json:"backend_requests"`
	ResultBytes     int `json:"result_bytes"`
}

// OperationSummary audits one call without its frozen input or result.
type OperationSummary struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Cost   int    `json:"cost"`
	Status string `json:"status"`
	Epoch  int64  `json:"epoch"`
}

// EvidenceSummary identifies one frozen query result without its records.
type EvidenceSummary struct {
	QueryID      string    `json:"query_id"`
	Kind         string    `json:"kind"`
	Source       string    `json:"source"`
	SourceStatus string    `json:"source_status"`
	QueryOutcome string    `json:"query_outcome"`
	ErrorCode    string    `json:"error_code,omitempty"`
	Records      int       `json:"records"`
	CollectedAt  time.Time `json:"collected_at"`
	Digest       string    `json:"digest"`
}

// Result is the operator-facing view of a run. Status always comes from
// state.status; Report is set only for COMPLETED, so a cancelled or expired run
// can never surface a report left in a checkpoint.
type Result struct {
	Schema          string                `json:"schema"`
	ID              string                `json:"id"`
	Status          string                `json:"status"`
	Terminal        bool                  `json:"terminal"`
	CancelRequested bool                  `json:"cancel_requested"`
	Overdue         bool                  `json:"overdue"`
	CreatedAt       time.Time             `json:"created_at"`
	DeadlineAt      time.Time             `json:"deadline_at"`
	FinishedAt      *time.Time            `json:"finished_at,omitempty"`
	RetainedUntil   *time.Time            `json:"retained_until,omitempty"`
	Phase           string                `json:"phase,omitempty"`
	Budget          Budget                `json:"budget"`
	Operations      []OperationSummary    `json:"operations"`
	Evidence        []EvidenceSummary     `json:"evidence"`
	Report          *investigation.Report `json:"report,omitempty"`
}

// Evidence exposes the frozen query results behind a Result. For a run that is
// not COMPLETED these are from its last confirmed checkpoint, not a final set.
type Evidence struct {
	Schema      string                      `json:"schema"`
	ID          string                      `json:"id"`
	Status      string                      `json:"status"`
	Origin      string                      `json:"origin"`
	Evidence    []telemetry.Result          `json:"evidence"`
	LogPatterns []investigation.LogPatterns `json:"log_patterns"`
}

// ReadResult projects the current durable state of a run.
func ReadResult(ctx context.Context, store runstate.Store, id string) (Result, error) {
	reply, err := store.Execute(ctx, runstate.Command{Action: "read", ID: id})
	if err != nil {
		return Result{}, err
	}
	return Project(reply)
}

// CancelRun requests cancellation and projects the persisted outcome, which is
// the completed result when completion won the race.
func CancelRun(ctx context.Context, store runstate.Store, id string) (Result, error) {
	reply, err := store.Execute(ctx, runstate.Command{Action: "cancel", ID: id})
	if err != nil {
		return Result{}, err
	}
	return Project(reply)
}

// ReadEvidence returns the frozen evidence saved for a run.
func ReadEvidence(ctx context.Context, store runstate.Store, id string) (Evidence, error) {
	reply, err := store.Execute(ctx, runstate.Command{Action: "read", ID: id})
	if err != nil {
		return Evidence{}, err
	}
	state, cp, err := decode(reply)
	if err != nil {
		return Evidence{}, err
	}
	origin := "replay"
	var job Job
	if json.Unmarshal([]byte(state.Request), &job) == nil && job.TelemetryID != "" {
		origin = "live"
	}
	view := Evidence{Schema: EvidenceSchema, ID: state.ID, Status: state.Status, Origin: origin, Evidence: []telemetry.Result{}, LogPatterns: []investigation.LogPatterns{}}
	if cp != nil {
		if cp.Run.Evidence != nil {
			view.Evidence = cp.Run.Evidence
		}
		if cp.Run.LogPatterns != nil {
			view.LogPatterns = cp.Run.LogPatterns
		}
	}
	return view, nil
}

// Project maps a protocol reply to a Result, refusing to guess from bad state.
func Project(reply runstate.Reply) (Result, error) {
	state, cp, err := decode(reply)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Schema: ResultSchema, ID: state.ID, Status: state.Status, CancelRequested: state.Cancelled,
		CreatedAt: time.UnixMilli(state.Created).UTC(), DeadlineAt: time.UnixMilli(state.Deadline).UTC(),
		Budget:     Budget{ChargedTokens: state.Tokens, ModelCalls: state.ModelCalls, BackendRequests: state.Requests, ResultBytes: state.ResultBytes},
		Operations: []OperationSummary{}, Evidence: []EvidenceSummary{},
	}
	for id, op := range state.Operations {
		status := op.Status
		if op.Kind == "query" {
			// One charge per backend request; its outcome is on the owning tool operation.
			status = "CHARGED"
		}
		result.Operations = append(result.Operations, OperationSummary{ID: id, Kind: op.Kind, Cost: op.Cost, Status: status, Epoch: op.Epoch})
	}
	sort.Slice(result.Operations, func(i, j int) bool { return result.Operations[i].ID < result.Operations[j].ID })
	if cp != nil {
		result.Phase = cp.Phase
		for _, item := range cp.Run.Evidence {
			result.Evidence = append(result.Evidence, EvidenceSummary{QueryID: item.QueryID, Kind: item.Query.Kind, Source: item.Source, SourceStatus: item.Status, QueryOutcome: item.Quality.QueryOutcome, ErrorCode: item.ErrorCode, Records: len(item.Records), CollectedAt: item.CollectedAt, Digest: item.Digest})
		}
	}
	switch state.Status {
	case "READY", "RUNNING":
		result.Overdue = reply.Now >= state.Deadline
		return result, nil
	case "COMPLETED":
		if cp == nil || cp.Run.Status != "COMPLETED" {
			return Result{}, fmt.Errorf("%w: run %s: completed without final checkpoint", ErrCorrupt, state.ID)
		}
		var report investigation.Report
		if json.Unmarshal([]byte(state.Report), &report) != nil || report.RunID != state.ID || report.StopReason == "" {
			return Result{}, fmt.Errorf("%w: run %s: completed without valid report", ErrCorrupt, state.ID)
		}
		result.Report = &report
	case "CANCELLED", "DEADLINE":
	default:
		return Result{}, fmt.Errorf("%w: run %s: unknown status", ErrCorrupt, state.ID)
	}
	finished := time.UnixMilli(state.Finished).UTC()
	retained := finished.Add(terminalRetention)
	result.Terminal, result.FinishedAt, result.RetainedUntil = true, &finished, &retained
	return result, nil
}

// decode validates the reply and the checkpoint nested inside it. Terminal
// replies carry state too, so a late cancel still reports the persisted outcome.
func decode(reply runstate.Reply) (*runstate.State, *investigation.Checkpoint, error) {
	switch reply.Code {
	case "OK", "TERMINAL":
	case "NOT_FOUND":
		return nil, nil, ErrNotFound
	case "CORRUPT":
		return nil, nil, ErrCorrupt
	case "UNKNOWN_SCHEMA":
		return nil, nil, ErrUnknownSchema
	default:
		return nil, nil, fmt.Errorf("%w: %s", investigation.ErrPersistence, reply.Code)
	}
	if reply.State == nil {
		return nil, nil, ErrCorrupt
	}
	state := reply.State
	var job struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(state.Request), &job) != nil {
		return nil, nil, fmt.Errorf("%w: run %s: unreadable job", ErrCorrupt, state.ID)
	}
	if job.Version != jobVersion {
		return nil, nil, fmt.Errorf("%w: run %s: job %q", ErrUnknownSchema, state.ID, job.Version)
	}
	if state.Checkpoint == "" {
		return state, nil, nil
	}
	var cp investigation.Checkpoint
	if json.Unmarshal([]byte(state.Checkpoint), &cp) != nil {
		return nil, nil, fmt.Errorf("%w: run %s: unreadable checkpoint", ErrCorrupt, state.ID)
	}
	if cp.Schema != 1 {
		return nil, nil, fmt.Errorf("%w: run %s: checkpoint %d", ErrUnknownSchema, state.ID, cp.Schema)
	}
	if cp.Run.ID != state.ID {
		return nil, nil, fmt.Errorf("%w: run %s: checkpoint identity mismatch", ErrCorrupt, state.ID)
	}
	return state, &cp, nil
}
