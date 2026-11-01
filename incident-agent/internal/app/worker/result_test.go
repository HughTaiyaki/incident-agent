package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type replyStore struct {
	reply runstate.Reply
	err   error
	last  runstate.Command
}

func (s *replyStore) Execute(_ context.Context, cmd runstate.Command) (runstate.Reply, error) {
	s.last = cmd
	return s.reply, s.err
}

func (*replyStore) Candidates(context.Context, int) ([]string, error) { return nil, nil }

func checkpointJSON(t *testing.T, run investigation.Run) string {
	t.Helper()
	data, err := json.Marshal(investigation.NewCheckpoint(run))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func state(status string) *runstate.State {
	return &runstate.State{Schema: 1, ID: "run1", Request: `{"version":"worker-v1"}`, Status: status, Created: 1000, Deadline: 5000, Finished: 3000}
}

func TestProjectUsesStateStatusNotCheckpoint(t *testing.T) {
	stale := investigation.Run{ID: "run1", Status: "COMPLETED", Evidence: []telemetry.Result{{QueryID: "q1"}},
		Report: investigation.Report{RunID: "run1", StopReason: "REPORT_ACCEPTED"}}
	for _, status := range []string{"READY", "RUNNING", "CANCELLED", "DEADLINE"} {
		s := state(status)
		s.Checkpoint = checkpointJSON(t, stale)
		s.Report = `{"run_id":"run1","stop_reason":"REPORT_ACCEPTED"}`
		result, err := Project(runstate.Reply{Code: "OK", State: s, Now: 2000})
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if result.Report != nil || result.Status != status || len(result.Evidence) != 1 {
			t.Fatalf("%s: %+v", status, result)
		}
		if result.Terminal != (status == "CANCELLED" || status == "DEADLINE") {
			t.Fatalf("%s terminal=%v", status, result.Terminal)
		}
		if result.Schema != ResultSchema {
			t.Fatal(result.Schema)
		}
	}
}

func TestProjectCompletedRequiresConsistentReport(t *testing.T) {
	final := checkpointJSON(t, investigation.Run{ID: "run1", Status: "COMPLETED"})
	good := state("COMPLETED")
	good.Checkpoint, good.Report = final, `{"run_id":"run1","stop_reason":"REPORT_ACCEPTED","result":"INCONCLUSIVE"}`
	result, err := Project(runstate.Reply{Code: "OK", State: good})
	if err != nil || result.Report == nil || result.Report.Result != "INCONCLUSIVE" || !result.Terminal {
		t.Fatalf("%+v %v", result, err)
	}
	if !result.RetainedUntil.Equal(time.UnixMilli(3000).Add(terminalRetention)) {
		t.Fatal(result.RetainedUntil)
	}
	for name, mutate := range map[string]func(*runstate.State){
		"no checkpoint": func(s *runstate.State) { s.Checkpoint = "" },
		"interim": func(s *runstate.State) {
			s.Checkpoint = checkpointJSON(t, investigation.Run{ID: "run1", Status: "RUNNING"})
		},
		"no report":      func(s *runstate.State) { s.Report = "" },
		"bad report":     func(s *runstate.State) { s.Report = "{" },
		"foreign report": func(s *runstate.State) { s.Report = `{"run_id":"other","stop_reason":"X"}` },
		"no stop reason": func(s *runstate.State) { s.Report = `{"run_id":"run1"}` },
		"foreign snapshot": func(s *runstate.State) {
			s.Checkpoint = checkpointJSON(t, investigation.Run{ID: "other", Status: "COMPLETED"})
		},
		"bad job":        func(s *runstate.State) { s.Request = "{" },
		"bad checkpoint": func(s *runstate.State) { s.Checkpoint = "{" },
	} {
		s := *good
		mutate(&s)
		if _, err := Project(runstate.Reply{Code: "OK", State: &s}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	unknown := state("MYSTERY")
	if _, err := Project(runstate.Reply{Code: "OK", State: unknown}); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestProjectVersionsAndCodes(t *testing.T) {
	job := state("READY")
	job.Request = `{"version":"worker-v2"}`
	if _, err := Project(runstate.Reply{Code: "OK", State: job}); !errors.Is(err, ErrUnknownSchema) {
		t.Fatal(err)
	}
	future := state("READY")
	future.Checkpoint = `{"schema":2}`
	if _, err := Project(runstate.Reply{Code: "OK", State: future}); !errors.Is(err, ErrUnknownSchema) {
		t.Fatal(err)
	}
	for code, want := range map[string]error{"NOT_FOUND": ErrNotFound, "CORRUPT": ErrCorrupt, "UNKNOWN_SCHEMA": ErrUnknownSchema, "WRONGTYPE": investigation.ErrPersistence} {
		if _, err := Project(runstate.Reply{Code: code}); !errors.Is(err, want) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	if _, err := Project(runstate.Reply{Code: "OK"}); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestProjectOverdueAndOperationOrder(t *testing.T) {
	s := state("RUNNING")
	s.Operations = map[string]runstate.Operation{"b": {Kind: "tool", Status: "UNKNOWN", Input: "secret-input", Result: "secret-result"}, "a": {Kind: "model", Cost: 5, Status: "COMMITTED"}}
	result, err := Project(runstate.Reply{Code: "OK", State: s, Now: 5000})
	if err != nil || !result.Overdue || result.Terminal {
		t.Fatalf("%+v %v", result, err)
	}
	if result.Operations[0].ID != "a" || result.Operations[1].Status != "UNKNOWN" {
		t.Fatalf("%+v", result.Operations)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil || fields["operations"].([]any)[0].(map[string]any)["input"] != nil {
		t.Fatal("operation input leaked")
	}
}

func TestEnqueueLimitAndOverload(t *testing.T) {
	job := Job{Request: investigation.Request{Service: "order-service", Services: []string{"order-service"}, Environment: "local", Start: time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 28, 6, 5, 0, 0, time.UTC), Summary: "s"}, Model: "m", Window: 32000, ProviderID: "p", FixtureDigest: "d"}
	store := &replyStore{reply: runstate.Reply{Code: "OK"}}
	for _, limit := range []int{0, -1, MaxActiveLimit + 1} {
		if err := Enqueue(t.Context(), store, "run1", job, limit); !errors.Is(err, investigation.ErrInvalidInput) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if store.last.Action != "" {
		t.Fatal("invalid limit reached store")
	}
	if err := Enqueue(t.Context(), store, "run1", job, 3); err != nil {
		t.Fatal(err)
	}
	if store.last.MaxActive != 3 {
		t.Fatal(store.last.MaxActive)
	}
	for _, code := range []string{"OVER_CAPACITY", "OVER_MEMORY"} {
		store.reply.Code = code
		if err := Enqueue(t.Context(), store, "run1", job, 3); !errors.Is(err, ErrOverloaded) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	store.reply.Code = "CONFLICT"
	if err := Enqueue(t.Context(), store, "run1", job, 3); err == nil || errors.Is(err, ErrOverloaded) {
		t.Fatal(err)
	}
}

func TestReadAndCancelUseFixedActions(t *testing.T) {
	store := &replyStore{reply: runstate.Reply{Code: "TERMINAL", State: state("CANCELLED")}}
	if result, err := CancelRun(t.Context(), store, "run1"); err != nil || result.Status != "CANCELLED" || store.last.Action != "cancel" {
		t.Fatalf("%+v %v", result, err)
	}
	if view, err := ReadEvidence(t.Context(), store, "run1"); err != nil || store.last.Action != "read" || view.Evidence == nil || view.Schema != EvidenceSchema {
		t.Fatalf("%+v %v", view, err)
	}
	store.err = errors.New("unavailable")
	if _, err := ReadResult(t.Context(), store, "run1"); !errors.Is(err, store.err) {
		t.Fatal(err)
	}
}
