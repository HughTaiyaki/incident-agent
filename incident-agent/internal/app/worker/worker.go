// Package worker resumes bounded investigations using durable run state.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const jobVersion = "worker-v1"

// Job freezes the request and operator-selected configuration identity.
// No credentials, filesystem paths or endpoint URLs are persisted here.
type Job struct {
	Version       string                `json:"version"`
	Request       investigation.Request `json:"request"`
	Model         string                `json:"model"`
	Window        int                   `json:"window"`
	FixtureDigest string                `json:"fixture_digest"`
	// TelemetryID identifies the live backends; a job uses it or FixtureDigest, never both.
	TelemetryID string `json:"telemetry_id,omitempty"`
	// InputDigest identifies an API submission, so a retry can be told from a different request.
	InputDigest string `json:"input_digest,omitempty"`
	ProviderID  string `json:"provider_id"`
	// Flow says who chooses the queries. SystemFacts is the system's knowledge exactly as
	// the run started with it, so recovery cannot change what the model is told.
	Flow           string `json:"flow,omitempty"`
	SystemFacts    string `json:"system_facts,omitempty"`
	ProfileVersion string `json:"profile_version,omitempty"`
}

// Factory verifies frozen configuration and returns per-run dependencies.
type Factory func(context.Context, Job) (investigation.Provider, investigation.Querier, error)

// Worker owns at most one run at a time. Store fencing also protects against
// overlapping replacement processes; no exactly-once external I/O is promised.
type Worker struct {
	Store  runstate.Store
	Owner  string
	Build  Factory
	Logger *slog.Logger
}

// MaxActiveLimit is the protocol ceiling; deployments choose their own lower limit.
const MaxActiveLimit = 1024

// ErrOverloaded means admission refused a new run and nothing was written.
// Callers should retry later with the same ID rather than treat it as a failure of the run.
var ErrOverloaded = errors.New("run admission overloaded")

// ErrRunBlocked means one run cannot proceed for a reason that does not affect the
// others: corrupt state, an unknown version or a configuration mismatch. The serving
// loop skips it; one-shot callers see it as a failure.
var ErrRunBlocked = errors.New("run blocked")

// blockedLogInterval keeps a stuck run visible without logging it on every scan.
const blockedLogInterval = time.Minute

// errRunExists means the run ID is taken; nothing was written.
var errRunExists = errors.New("run already exists")

// Enqueue creates a run with an immutable 180-second execution deadline, admitted
// atomically against maxActive queued and running runs across the deployment.
func Enqueue(ctx context.Context, store runstate.Store, id string, job Job, maxActive int) error {
	if _, err := investigation.ValidateRequest(job.Request); err != nil {
		return err
	}
	if !investigation.ValidFlow(job.Flow) {
		return investigation.ErrInvalidInput
	}
	if job.Model == "" || job.Window < 4096 || job.Window > 2000000 || job.ProviderID == "" || (job.FixtureDigest == "") == (job.TelemetryID == "") || maxActive < 1 || maxActive > MaxActiveLimit {
		return investigation.ErrInvalidInput
	}
	job.Version = jobVersion
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	reply, err := store.Execute(ctx, runstate.Command{Action: "create", ID: id, Request: string(data), Deadline: time.Now().Add(180 * time.Second).UnixMilli(), MaxActive: maxActive})
	if err != nil {
		return err
	}
	switch reply.Code {
	case "OK":
		return nil
	case "OVER_CAPACITY", "OVER_MEMORY":
		return fmt.Errorf("%w: %s", ErrOverloaded, reply.Code)
	case "EXISTS", "CONFLICT":
		return fmt.Errorf("%w: %s", errRunExists, reply.Code)
	case "CORRUPT":
		return ErrCorrupt
	case "UNKNOWN_SCHEMA":
		return ErrUnknownSchema
	}
	return fmt.Errorf("create rejected: %s", reply.Code)
}

// Run scans on startup and every second. Storage failures stop the worker so
// supervisors can report/restart it; all unresolved runs retain their leases.
func (w *Worker) Run(ctx context.Context) error {
	if w.Store == nil || w.Owner == "" || w.Build == nil {
		return investigation.ErrInvalidInput
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if beat, ok := w.Store.(runstate.Heartbeater); ok {
		go heartbeat(ctx, beat, w.Owner)
	}
	lastLogged := map[string]time.Time{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ids, err := w.Store.Candidates(ctx, 32)
		if err != nil {
			return err
		}
		for _, id := range ids {
			err := w.Process(ctx, id)
			if errors.Is(err, ErrRunBlocked) {
				if time.Since(lastLogged[id]) >= blockedLogInterval {
					lastLogged[id] = time.Now()
					w.logger().Error("run blocked", "run_id", id, "error", err)
					if recorder, ok := w.Store.(runstate.Recorder); ok {
						_ = recorder.RecordBlocked(ctx)
					}
				}
				continue
			}
			if err != nil {
				return err
			}
			delete(lastLogged, id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// heartbeat publishes liveness apart from run processing, which may take minutes.
// A failed beat only makes readiness report the worker absent; storage errors that
// matter stop the worker through its own commands.
func heartbeat(ctx context.Context, beat runstate.Heartbeater, owner string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		_ = beat.Heartbeat(ctx, owner)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Process claims a candidate, restores its checkpoint, renews the lease and
// atomically saves its final report. Parent cancellation leaves work resumable.
func (w *Worker) Process(ctx context.Context, id string) error {
	reply, err := w.Store.Execute(ctx, runstate.Command{Action: "claim", ID: id, Owner: w.Owner})
	if err != nil {
		return err
	}
	if reply.Code == "BUSY" || reply.Code == "TERMINAL" || reply.Code == "NOT_FOUND" {
		return nil
	}
	if reply.Code != "OK" || reply.State == nil {
		return fmt.Errorf("%w: %w: run %s: claim %s", ErrRunBlocked, investigation.ErrPersistence, id, reply.Code)
	}
	s := &session{store: w.Store, state: *reply.State}
	var job Job
	if json.Unmarshal([]byte(s.state.Request), &job) != nil || job.Version != jobVersion {
		return fmt.Errorf("%w: %w: run %s: unreadable job", ErrRunBlocked, investigation.ErrInvalidInput, id)
	}
	run := investigation.Run{ID: id, Request: job.Request, Status: "RUNNING", CreatedAt: time.UnixMilli(s.state.Created).UTC(), DeadlineAt: time.UnixMilli(s.state.Deadline).UTC()}
	cp := investigation.NewCheckpoint(run)
	if s.state.Checkpoint != "" {
		if json.Unmarshal([]byte(s.state.Checkpoint), &cp) != nil || cp.Run.ID != id || cp.Run.DeadlineAt.UnixMilli() != s.state.Deadline {
			return fmt.Errorf("%w: %w: run %s: unreadable checkpoint", ErrRunBlocked, investigation.ErrInvalidInput, id)
		}
		old, _ := json.Marshal(cp.Run.Request)
		current, _ := json.Marshal(job.Request)
		if string(old) != string(current) {
			return fmt.Errorf("%w: %w: run %s: checkpoint request mismatch", ErrRunBlocked, investigation.ErrInvalidInput, id)
		}
	}
	// No provider creation is required to close expired/cancelled runs.
	if s.state.Cancelled || time.Now().UnixMilli() >= s.state.Deadline {
		reason := "DEADLINE"
		if s.state.Cancelled {
			reason = "CANCELLED"
		}
		return w.finish(ctx, s, investigation.Stopped(cp.Run, reason))
	}
	provider, querier, err := w.Build(ctx, job)
	if err != nil {
		return fmt.Errorf("%w: run %s: %w", ErrRunBlocked, id, err)
	}
	workCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := s.renew(workCtx); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	d := &durableDecider{session: s, provider: provider, job: job, run: workCtx}
	q := &durableQuerier{session: s, querier: querier, step: func() int { return d.step }, run: workCtx}
	result, runErr := investigation.Resume(workCtx, cp, d, q, s.save)
	cancel()
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Re-read cancellation/ownership before publishing, including after a lost ack.
	current, err := w.Store.Execute(ctx, runstate.Command{Action: "read", ID: id})
	if err != nil || current.State == nil {
		return investigation.ErrPersistence
	}
	if current.State.Owner != w.Owner || current.State.Epoch != s.state.Epoch {
		return investigation.ErrPersistence
	}
	s.state = *current.State
	if s.state.Cancelled || time.Now().UnixMilli() >= s.state.Deadline {
		if s.state.Checkpoint != "" {
			if json.Unmarshal([]byte(s.state.Checkpoint), &cp) != nil {
				return investigation.ErrPersistence
			}
		}
		reason := "DEADLINE"
		if s.state.Cancelled {
			reason = "CANCELLED"
		}
		result = investigation.Stopped(cp.Run, reason)
	} else if errors.Is(runErr, investigation.ErrInvalidInput) {
		// Such as a checkpoint from another harness version: this run cannot resume, others can.
		return fmt.Errorf("%w: run %s: %w", ErrRunBlocked, id, runErr)
	} else if runErr != nil {
		return runErr
	} else if result.Status == "CANCELLED" {
		return investigation.ErrPersistence
	}
	return w.finish(ctx, s, result)
}

// finish publishes the terminal state, then reports it. Reporting cannot fail the run:
// the terminal state is already durable and is the source of truth.
func (w *Worker) finish(ctx context.Context, s *session, run investigation.Run) error {
	if err := s.complete(ctx, run); err != nil {
		return err
	}
	outcome := runstate.RunOutcome{Status: s.state.Status, ModelCalls: s.state.ModelCalls, ChargedTokens: s.state.Tokens, BackendRequests: s.state.Requests}
	if recorder, ok := w.Store.(runstate.Recorder); ok {
		if err := recorder.RecordOutcome(ctx, outcome); err != nil {
			w.logger().Warn("run counters not updated", "run_id", s.state.ID)
		}
	}
	w.logger().Info("run finished", "run_id", s.state.ID, "status", outcome.Status, "stop_reason", run.Report.StopReason,
		"result", run.Report.Result, "model_calls", outcome.ModelCalls, "charged_tokens", outcome.ChargedTokens, "backend_requests", outcome.BackendRequests)
	return nil
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.New(slog.DiscardHandler)
}

type session struct {
	mu    sync.Mutex
	store runstate.Store
	state runstate.State
}

// command requires mu. It never retries a possibly submitted mutation.
func (s *session) command(ctx context.Context, cmd runstate.Command) error {
	cmd.ID, cmd.Owner, cmd.Epoch, cmd.Version = s.state.ID, s.state.Owner, s.state.Epoch, s.state.Version
	reply, err := s.store.Execute(ctx, cmd)
	if err != nil {
		return errors.Join(investigation.ErrPersistence, err)
	}
	if reply.Code != "OK" {
		return fmt.Errorf("%w: %s", investigation.ErrPersistence, reply.Code)
	}
	s.state = *reply.State
	return nil
}
func (s *session) renew(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.command(ctx, runstate.Command{Action: "renew"}); err != nil {
		return err
	}
	if s.state.Cancelled {
		return investigation.ErrPersistence
	}
	return nil
}
func (s *session) save(ctx context.Context, cp investigation.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if err := s.command(ctx, runstate.Command{Action: "checkpoint", Checkpoint: string(data)}); err != nil {
		return err
	}
	if s.state.Cancelled {
		return investigation.ErrPersistence
	}
	return nil
}
func (s *session) complete(ctx context.Context, run investigation.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Audit comes from durable operations, including unknown calls after recovery.
	run.ModelCalls = s.audits()
	run.BackendRequests = s.state.Requests
	data, err := json.Marshal(investigation.NewCheckpoint(run))
	if err != nil {
		return err
	}
	report, err := json.Marshal(run.Report)
	if err != nil {
		return err
	}
	return s.command(ctx, runstate.Command{Action: "complete", Report: string(report), Checkpoint: string(data)})
}

// Budget journals every actual request, including retries made by adapters.
func (q *durableQuerier) Budget(ctx context.Context) *telemetry.Budget {
	q.session.mu.Lock()
	used := q.session.state.Requests
	q.session.mu.Unlock()
	return telemetry.RestoreBudget(16, used, func() error {
		s := q.session
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.command(ctx, runstate.Command{Action: "begin", Operation: fmt.Sprintf("request-%d", s.state.Requests+1), Kind: "query", Cost: 1, Input: "{}"})
	})
}
