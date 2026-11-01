package investigation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type checkpointDecider struct{ views []View }

func (d *checkpointDecider) Decide(_ context.Context, v View) (json.RawMessage, error) {
	d.views = append(d.views, v)
	return json.Marshal(Decision{Kind: "report", Report: &Draft{Result: "INCONCLUSIVE", Unknowns: []string{"missing evidence"}, Recovery: "UNKNOWN"}})
}

type unusedQuerier struct{}

func (unusedQuerier) Query(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
	panic("query must not run")
}
func checkpointRequest() Request {
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	return Request{Service: "order-service", Services: []string{"order-service"}, Environment: "local", Summary: "check evidence", Start: start, End: start.Add(5 * time.Minute)}
}
func TestResumePreservesStepPhaseAndDeadline(t *testing.T) {
	now := time.Now().UTC()
	cp := NewCheckpoint(Run{ID: "resume", Request: checkpointRequest(), CreatedAt: now.Add(-time.Minute), DeadlineAt: now.Add(time.Minute), Decisions: 5, BackendRequests: 7})
	cp.NextStep = 6
	cp.Phase = "SYNTHESIZING"
	cp.Feedback = "NO_PROGRESS"
	cp.StopReason = "NO_PROGRESS"
	cp.NoProgress = 3
	d := &checkpointDecider{}
	saved := false
	run, err := Resume(t.Context(), cp, d, unusedQuerier{}, func(_ context.Context, got Checkpoint) error {
		saved = true
		if got.NextStep != 6 || got.Run.Decisions != 5 || got.NoProgress != 3 || got.Run.BackendRequests != 7 {
			t.Fatal("checkpoint counters reset")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved || len(d.views) != 1 || d.views[0].Step != 6 || d.views[0].Phase != "SYNTHESIZING" || d.views[0].Feedback != "NO_PROGRESS" || run.Decisions != 6 || run.DeadlineAt != cp.Run.DeadlineAt || run.BackendRequests != 7 {
		t.Fatal("resume reset state")
	}
}
func TestCheckpointFailureStopsBeforeDeciding(t *testing.T) {
	cp := NewCheckpoint(Run{ID: "resume", Request: checkpointRequest(), DeadlineAt: time.Now().Add(time.Minute)})
	d := &checkpointDecider{}
	_, err := Resume(t.Context(), cp, d, unusedQuerier{}, func(context.Context, Checkpoint) error { return errors.New("offline") })
	if !errors.Is(err, ErrPersistence) || len(d.views) != 0 {
		t.Fatal("called provider after persistence failure")
	}
}

func TestSafeDecisionPreservesRejectionKindWithoutRawText(t *testing.T) {
	view := View{Request: checkpointRequest()}
	raw := []byte(`{"kind":"report","report":{"result":"INCONCLUSIVE","unknowns":["password=private"],"recovery_assessment":"UNKNOWN"}}`)
	safe := SafeDecision(raw, view)
	decision, err := decodeDecision(safe)
	if err != nil || decision.Kind != "report" || ValidateReport(*decision.Report, nil) == nil {
		t.Fatal("report rejection semantics changed")
	}
	if bytes.Contains(safe, []byte("private")) {
		t.Fatal("raw rejection persisted")
	}
}
