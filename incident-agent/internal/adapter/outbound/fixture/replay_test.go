package fixture_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func replay(t *testing.T) (*fixture.Replay, telemetry.Query) {
	t.Helper()
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	q, err := telemetry.DefaultPolicy().Normalize(telemetry.Query{Kind: "logs", Service: "order-service", Environment: "experiment", Start: start, End: start.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	result := telemetry.NewResult(q, "loki", "test-query")
	result.Quality.QueryOutcome = "SUCCEEDED"
	for i, event := range []string{"order.transition", "workflow.retry", "order.transition"} {
		result.Records = append(result.Records, telemetry.Record{Type: "log", At: start.Add(time.Duration(i) * time.Second),
			Service: q.Service, Environment: q.Environment, Fields: map[string]string{"event_name": event, "order_id": "synthetic-order"}})
	}
	result.Finish()
	b, err := telemetry.NewBundle("synthetic", "unknown", []telemetry.Result{result})
	if err != nil {
		t.Fatal(err)
	}
	r, err := fixture.New(b, telemetry.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return r, q
}

func TestReplayFiltersAllCapturedRecordsAndDetachesResults(t *testing.T) {
	t.Parallel()
	r, q := replay(t)
	q.EventName = "order.transition"
	budget := telemetry.NewBudget(16)
	result, err := r.Query(t.Context(), q, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 || budget.Used() != 0 || result.Requests != 0 {
		t.Fatalf("wrong replay: %+v", result)
	}
	result.Records[0].Fields["order_id"] = "mutated"
	again, err := r.Query(t.Context(), q, budget)
	if err != nil || again.Records[0].Fields["order_id"] != "synthetic-order" {
		t.Fatal("caller mutated frozen dataset")
	}
	q.EventName = "inventory.reserved"
	empty, err := r.Query(t.Context(), q, budget)
	if err != nil || len(empty.Records) != 0 || empty.Quality.Coverage != "PARTIAL" {
		t.Fatal("filter miss was not conservative")
	}
}

func TestReplayRejectsBroaderWindowAndCancellation(t *testing.T) {
	t.Parallel()
	r, q := replay(t)
	q.Start = q.Start.Add(-time.Second)
	result, err := r.Query(t.Context(), q, telemetry.NewBudget(16))
	if err == nil || result.ErrorCode != "FIXTURE_QUERY_NOT_COVERED" {
		t.Fatal("unrecorded window treated as empty")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = r.Query(ctx, q, telemetry.NewBudget(16)); err == nil {
		t.Fatal("cancelled replay continued")
	}
}

func TestObservedFixtureReplaysWithoutNetwork(t *testing.T) {
	t.Parallel()
	file, err := os.Open("../../../../testdata/telemetry/observed-20261028/bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	b, err := telemetry.ReadBundle(file)
	if err != nil {
		t.Fatal(err)
	}
	r, err := fixture.New(b, telemetry.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	budget := telemetry.NewBudget(16)
	var total int
	for _, entry := range b.Entries {
		result, err := r.Query(t.Context(), entry.Query, budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Records) != len(entry.Records) {
			t.Fatal("record loss during replay")
		}
		total += len(result.Records)
	}
	if total != 169 || budget.Used() != 0 {
		t.Fatalf("records=%d backend requests=%d", total, budget.Used())
	}
}

func TestReplayAnswersNotableFromAWiderCapture(t *testing.T) {
	t.Parallel()
	r, q := replay(t)
	notable := q
	notable.Notable = true
	result, err := r.Query(context.Background(), notable, telemetry.NewBudget(16))
	if err != nil || len(result.Records) != 3 {
		t.Fatalf("%d records: %v", len(result.Records), err)
	}
}
