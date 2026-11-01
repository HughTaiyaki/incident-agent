package telemetry_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func query() telemetry.Query {
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	return telemetry.Query{Kind: "logs", Service: "order-service", Environment: "experiment", Start: start, End: start.Add(time.Minute)}
}

func TestPolicyNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*telemetry.Query)
	}{
		{name: "unknown service", change: func(q *telemetry.Query) { q.Service = "inventory-service" }},
		{name: "unknown environment", change: func(q *telemetry.Query) { q.Environment = "production" }},
		{name: "wide window", change: func(q *telemetry.Query) { q.End = q.Start.Add(time.Hour + time.Second) }},
		{name: "reversed window", change: func(q *telemetry.Query) { q.End = q.Start.Add(-time.Second) }},
		{name: "query injection", change: func(q *telemetry.Query) { q.EventName = `x"} | json` }},
		{name: "excessive logs", change: func(q *telemetry.Query) { q.Limit = 101 }},
		{name: "trace path injection", change: func(q *telemetry.Query) { q.TraceID = "../../config" }},
		{name: "irrelevant field", change: func(q *telemetry.Query) { q.Pool = "default" }},
		{name: "unknown metric", change: func(q *telemetry.Query) { q.Kind = "metrics"; q.Metric = "db_acquire_timeout_rate" }},
		{name: "too many points", change: func(q *telemetry.Query) {
			q.Kind = "metrics"
			q.Metric = "rpc_error_rate"
			q.StepSeconds = 1
			q.End = q.Start.Add(time.Hour)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := query()
			tt.change(&q)
			if _, err := telemetry.DefaultPolicy().Normalize(q); !errors.Is(err, telemetry.ErrInvalidQuery) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	q, err := telemetry.DefaultPolicy().Normalize(query())
	if err != nil || q.Limit != 100 {
		t.Fatalf("defaults: %+v, %v", q, err)
	}
}

func TestBudgetConcurrentReservations(t *testing.T) {
	t.Parallel()
	budget := telemetry.NewBudget(16)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() { _ = budget.Reserve() })
	}
	wg.Wait()
	if budget.Used() != 16 {
		t.Fatalf("used %d requests", budget.Used())
	}
	if !errors.Is(budget.Reserve(), telemetry.ErrBudget) {
		t.Fatal("exhaustion was not enforced")
	}
}

func TestRestoredBudgetRequiresDurableReservation(t *testing.T) {
	calls := 0
	budget := telemetry.RestoreBudget(16, 15, func() error { calls++; return telemetry.ErrUnavailable })
	if err := budget.Reserve(); !errors.Is(err, telemetry.ErrUnavailable) || budget.Used() != 15 || calls != 1 {
		t.Fatal("failed durable reservation consumed or bypassed")
	}
	budget = telemetry.RestoreBudget(16, 15, func() error { return nil })
	if err := budget.Reserve(); err != nil || budget.Used() != 16 {
		t.Fatal("restored budget lost used count")
	}
	if err := budget.Reserve(); !errors.Is(err, telemetry.ErrBudget) {
		t.Fatal("restored budget exceeded")
	}
}
