package investigation

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// ErrPersistence requires stopping external work and leaving the run recoverable.
var ErrPersistence = errors.New("investigation persistence interrupted")

// Checkpoint contains loop state in addition to the public Run snapshot.
// Only trusted storage may supply it; models cannot write or select checkpoints.
type Checkpoint struct {
	Schema           int             `json:"schema"`
	Policy           string          `json:"policy"`
	Run              Run             `json:"run"`
	NextStep         int             `json:"next_step"`
	Phase            string          `json:"phase"`
	Feedback         string          `json:"feedback"`
	StopReason       string          `json:"stop_reason"`
	Repeated         map[string]int  `json:"repeated"`
	Seen             map[string]bool `json:"seen"`
	NoProgress       int             `json:"no_progress"`
	ReportRejections int             `json:"report_rejections"`
	EvidenceBytes    int             `json:"evidence_bytes"`
}

// NewCheckpoint freezes run identity and time; the caller supplies persisted times.
func NewCheckpoint(run Run) Checkpoint {
	return Checkpoint{Schema: 1, Policy: PolicyVersion, Run: run, NextStep: 1, Phase: "INVESTIGATING", StopReason: "REPORT_ACCEPTED", Repeated: map[string]int{}, Seen: map[string]bool{}}
}

// SafeDecision discards malformed/rejected raw model text before journaling.
func SafeDecision(raw []byte, view View) []byte {
	decision, err := decodeDecision(raw)
	if err == nil && decision.Kind == "report" {
		if ValidateReport(*decision.Report, view.Evidence) != nil {
			return []byte(`{"kind":"report","report":{}}`)
		}
	}
	if err == nil && decision.Kind == "query" {
		policy, e := ValidateRequest(view.Request)
		if e != nil {
			err = e
		} else {
			q, e := policy.Normalize(*decision.Query)
			err = e
			if q.Start.Before(view.Request.Start) || q.End.After(view.Request.End) {
				err = ErrInvalidInput
			}
		}
	}
	if err != nil {
		if decision.Kind == "query" {
			return []byte(`{"kind":"query","query":{}}`)
		}
		return []byte(`{}`)
	}
	return raw
}

// Stopped creates a deterministic report without calling a provider.
func Stopped(run Run, reason string) Run {
	run.Status = "COMPLETED"
	if reason == "CANCELLED" {
		run.Status = "CANCELLED"
	}
	run.Report = Report{Draft: fallback(), RunID: run.ID, StopReason: reason, PolicyVersion: PolicyVersion}
	run.FinishedAt = time.Now().UTC()
	return run
}

// SanitizeResult prevents unvalidated adapter data from entering durable storage.
func SanitizeResult(result telemetry.Result, q telemetry.Query, policy telemetry.Policy, queryErr error) telemetry.Result {
	if validResult(result, q, policy) && !(queryErr != nil && result.Quality.QueryOutcome == "SUCCEEDED") {
		return result
	}
	return FailedResult(q, "QUERY_FAILED")
}

// FailedResult records that no usable source result exists. The "harness" source
// keeps it from being mistaken for data returned by a telemetry backend.
func FailedResult(q telemetry.Query, code string) telemetry.Result {
	canonical, _ := json.Marshal(q)
	result := telemetry.NewResult(q, "harness", string(canonical))
	result.ErrorCode = code
	result.Finish()
	return result
}
