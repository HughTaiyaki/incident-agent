package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type decideFunc func(context.Context, View) (json.RawMessage, error)

func (f decideFunc) Decide(ctx context.Context, v View) (json.RawMessage, error) { return f(ctx, v) }

type queryFunc func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error)

func (f queryFunc) Query(ctx context.Context, q telemetry.Query, b *telemetry.Budget) (telemetry.Result, error) {
	return f(ctx, q, b)
}

func request() Request {
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	return Request{Service: "order-service", Services: []string{"order-service"}, Environment: "local", Start: start, End: start.Add(5 * time.Minute), Summary: "核查历史订单遥测"}
}
func query() telemetry.Query {
	r := request()
	return telemetry.Query{Kind: "logs", Service: r.Service, Environment: r.Environment, Start: r.Start, End: r.End}
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func observation(q telemetry.Query, suffix string) telemetry.Result {
	r := telemetry.NewResult(q, "loki", "query:"+suffix)
	r.Quality.QueryOutcome = "SUCCEEDED"
	r.Quality.Reachability = "REACHABLE"
	r.Records = []telemetry.Record{{Type: "log", At: q.Start, Service: q.Service, Environment: q.Environment, Fields: map[string]string{"severity_text": "INFO", "event_name": "order.changed", "order_id": "order-" + suffix}}}
	r.Finish()
	return r
}
func validDraft(id string) Draft {
	d := fallback()
	d.Facts = []Claim{{Text: "观察到一条结构化业务事件。", EvidenceIDs: []string{id}}}
	return d
}
func TestExecute(t *testing.T) {
	t.Run("grounded report and detached evidence", func(t *testing.T) {
		calls := 0
		source := queryFunc(func(_ context.Context, q telemetry.Query, b *telemetry.Budget) (telemetry.Result, error) {
			calls++
			if err := b.Reserve(); err != nil {
				return telemetry.Result{}, err
			}
			return observation(q, "1"), nil
		})
		result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			id := v.Evidence[0].Records[0].ID
			v.Evidence[0].Records[0].Fields["event_name"] = "mutated"
			v.Request.Services[0] = "goods-service"
			return raw(Decision{Kind: "report", Report: ptr(validDraft(id))}), nil
		}), source)
		if err != nil || calls != 1 || result.Report.Result != "INCONCLUSIVE" || result.BackendRequests != 1 {
			t.Fatalf("run=%+v err=%v calls=%d", result, err, calls)
		}
		if result.Evidence[0].Records[0].Fields["event_name"] != "order.changed" || result.Request.Services[0] != "order-service" {
			t.Fatal("decider mutated run")
		}
		if result.Report.RunID != result.ID || len(result.ID) != 32 {
			t.Fatal("invalid run identity")
		}
	})
	t.Run("rejected calls consume decisions without query IO", func(t *testing.T) {
		cases := []json.RawMessage{json.RawMessage(`{"kind":"shell","command":"anything"}`), raw(Decision{Kind: "query", Query: ptr(telemetry.Query{Kind: "logs", Service: "goods-service", Environment: "local", Start: request().Start, End: request().End})}), raw(Decision{Kind: "query", Query: ptr(telemetry.Query{Kind: "logs", Service: "order-service", Environment: "local", Start: request().Start.Add(-time.Minute), End: request().End})})}
		result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step <= 3 {
				return cases[v.Step-1], nil
			}
			if v.Phase != "SYNTHESIZING" {
				t.Fatal(v.Phase)
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			t.Fatal("unauthorized IO")
			return telemetry.Result{}, nil
		}))
		if err != nil || result.Decisions != 4 || result.Report.StopReason != "NO_PROGRESS" || len(result.Evidence) != 0 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("same content with different capture time is not progress", func(t *testing.T) {
		calls := 0
		result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Phase == "SYNTHESIZING" {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			q := query()
			q.Limit = 90 + v.Step
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			calls++
			r := observation(q, "same")
			r.CollectedAt = r.CollectedAt.Add(time.Duration(calls) * time.Second)
			r.Records[0].CollectedAt = r.CollectedAt
			r.Finish()
			return r, nil
		}))
		if err != nil || calls != 4 || result.Decisions != 5 || result.Report.StopReason != "NO_PROGRESS" {
			t.Fatalf("calls=%d run=%+v err=%v", calls, result, err)
		}
	})
	t.Run("repeat limit", func(t *testing.T) {
		calls := 0
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 4 {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			q := query()
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			calls++
			return observation(q, fmt.Sprint(calls)), nil
		}))
		if calls != 1 || result.Steps[1].Outcome != "QUERY_REPEATED" || result.Steps[2].Outcome != "QUERY_REPEATED" {
			t.Fatalf("%+v calls=%d", result, calls)
		}
	})
	t.Run("empty first looks are not lack of progress", func(t *testing.T) {
		services := []string{"order-service", "goods-service", "user-service", "payment-simulator", "gateway-h5"}
		r := request()
		r.Services = services
		result, _ := Execute(t.Context(), r, decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step > len(services) {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			q := query()
			q.Service = services[v.Step-1]
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			empty := telemetry.NewResult(q, "loki", "empty")
			empty.Quality.QueryOutcome = "SUCCEEDED"
			empty.Finish()
			return empty, nil
		}))
		if result.Report.StopReason != "REPORT_ACCEPTED" || len(result.Evidence) != len(services) {
			t.Fatalf("%+v", result.Report)
		}
	})
	t.Run("investigation can end itself and then must report", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			switch {
			case v.Step == 1:
				return json.RawMessage(`{"kind":"synthesize"}`), nil // Refused: nothing has been checked.
			case v.Step == 2:
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			case v.Step == 3:
				return json.RawMessage(`{"kind":"synthesize"}`), nil
			case v.Step == 4:
				if v.Phase != "SYNTHESIZING" {
					t.Fatal(v.Phase)
				}
				return json.RawMessage(`{"kind":"synthesize"}`), nil // Refused: only a report is an answer now.
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			return observation(q, "1"), nil
		}))
		if result.Report.StopReason != "INVESTIGATION_COMPLETE" || result.Steps[0].Outcome != "DECISION_REJECTED" || result.Steps[2].Outcome != "SYNTHESIS_REQUESTED" || result.Steps[3].Outcome != "DECISION_REJECTED" {
			t.Fatalf("%+v %+v", result.Report, result.Steps)
		}
	})
	t.Run("failed query may retry within the limit", func(t *testing.T) {
		calls := 0
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 4 {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			q := query()
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			calls++
			return telemetry.Result{}, errors.New("unavailable")
		}))
		if calls != 2 || result.Steps[2].Outcome != "QUERY_REPEAT_LIMIT" {
			t.Fatalf("%+v calls=%d", result, calls)
		}
	})
	t.Run("last decision is report only", func(t *testing.T) {
		calls := 0
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			q := query()
			q.Limit = v.Step
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			calls++
			return observation(q, fmt.Sprint(calls)), nil
		}))
		if calls != 11 || result.Decisions != 12 || result.Report.StopReason != "DECISION_LIMIT" {
			t.Fatalf("%+v calls=%d", result, calls)
		}
	})
	t.Run("actual request budget includes retries", func(t *testing.T) {
		calls := 0
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Phase == "SYNTHESIZING" {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			q := query()
			q.Limit = v.Step
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, b *telemetry.Budget) (telemetry.Result, error) {
			calls++
			for range 2 {
				if err := b.Reserve(); err != nil {
					t.Fatal(err)
				}
			}
			return observation(q, fmt.Sprint(calls)), nil
		}))
		if calls != 8 || result.BackendRequests != 16 || result.Report.StopReason != "BACKEND_BUDGET" {
			t.Fatalf("%+v calls=%d", result, calls)
		}
	})
	t.Run("one repair for invented citation", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			switch v.Step {
			case 1:
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			case 2:
				return raw(Decision{Kind: "report", Report: ptr(validDraft("foreign-run-id"))}), nil
			}
			if v.Phase != "SYNTHESIZING" || v.Feedback != "REPORT_REJECTED:CITATION" {
				t.Fatal(v)
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			return observation(q, "1"), nil
		}))
		if result.Decisions != 3 || result.Report.StopReason != "REPORT_REPAIRED" {
			t.Fatal(result)
		}
	})
	t.Run("third invalid report falls back", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			return raw(Decision{Kind: "report", Report: ptr(validDraft("invented"))}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			return observation(q, "1"), nil
		}))
		if result.Decisions != 4 || result.Report.StopReason != "REPORT_INVALID" || len(result.Report.Facts) != 0 || result.Steps[1].Detail != "CITATION" {
			t.Fatal(result)
		}
	})
	t.Run("malformed answer in synthesis gets a repair", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			switch v.Step {
			case 1:
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			case 2:
				return raw(Decision{Kind: "report", Report: ptr(validDraft("invented"))}), nil
			case 3:
				return json.RawMessage(`{"kind":"report","report":{"alternatives":["a string"]}}`), nil
			}
			if v.Feedback != "DECISION_REJECTED" {
				t.Fatal(v.Feedback)
			}
			return raw(Decision{Kind: "report", Report: ptr(validDraft(v.Evidence[0].Records[0].ID))}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			return observation(q, "1"), nil
		}))
		if result.Report.StopReason != "REPORT_REPAIRED" || len(result.Report.Facts) != 1 {
			t.Fatal(result)
		}
	})
	t.Run("report before any query is asked again", func(t *testing.T) {
		queries := 0
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}
			if v.Step == 2 {
				if v.Phase != "INVESTIGATING" || v.Feedback != "DECISION_REJECTED" {
					t.Fatal(v)
				}
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			queries++
			return observation(q, "1"), nil
		}))
		if queries != 1 || result.Decisions != 3 || result.Report.StopReason != "REPORT_ACCEPTED" {
			t.Fatal(result)
		}
	})
	t.Run("only reports without a query end through synthesis", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(context.Context, View) (json.RawMessage, error) {
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			t.Fatal("IO")
			return telemetry.Result{}, nil
		}))
		if result.Report.StopReason != "NO_PROGRESS" || result.Report.Result != "INCONCLUSIVE" {
			t.Fatal(result)
		}
	})
	t.Run("failure evidence is retained without raw errors", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			return nil, errors.New("secret=private")
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			return telemetry.Result{}, errors.New("secret=private")
		}))
		if len(result.Evidence) != 1 || result.Evidence[0].Status != "UNAVAILABLE" || result.Report.StopReason != "DECIDER_UNAVAILABLE" || strings.Contains(string(raw(result)), "private") {
			t.Fatal(result)
		}
	})
	t.Run("wrong service response cannot become evidence", func(t *testing.T) {
		result, _ := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			r := observation(q, "1")
			r.Records[0].Service = "goods-service"
			r.Finish()
			return r, nil
		}))
		if len(result.Evidence) != 1 || len(result.Evidence[0].Records) != 0 || result.Evidence[0].ErrorCode != "QUERY_FAILED" {
			t.Fatal(result)
		}
	})
}
func ptr[T any](v T) *T { return &v }

func TestExecuteCancellationAndDeadlines(t *testing.T) {
	t.Run("cancelled before decision", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result, err := Execute(ctx, request(), decideFunc(func(context.Context, View) (json.RawMessage, error) {
			t.Fatal("decision after cancellation")
			return nil, nil
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			t.Fatal("IO")
			return telemetry.Result{}, nil
		}))
		if err != nil || result.Status != "CANCELLED" || result.Decisions != 0 {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("cancel during query", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result, _ := Execute(ctx, request(), decideFunc(func(context.Context, View) (json.RawMessage, error) {
			q := query()
			return raw(Decision{Kind: "query", Query: &q}), nil
		}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
			cancel()
			return telemetry.Result{}, context.Canceled
		}))
		if result.Status != "CANCELLED" || result.Decisions != 1 || len(result.Evidence) != 0 {
			t.Fatal(result)
		}
	})
	t.Run("decision timeout ignores late answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			result, _ := Execute(t.Context(), request(), decideFunc(func(ctx context.Context, _ View) (json.RawMessage, error) {
				<-ctx.Done()
				return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
			}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
				t.Fatal("IO")
				return telemetry.Result{}, nil
			}))
			if result.Decisions != 1 || result.Report.StopReason != "DECIDER_UNAVAILABLE" {
				t.Fatal(result)
			}
		})
	})
	t.Run("parent deadline preserved", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, _ := Execute(ctx, request(), decideFunc(func(ctx context.Context, v View) (json.RawMessage, error) {
				if v.Phase != "SYNTHESIZING" {
					t.Fatal(v.Phase)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}), queryFunc(func(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error) {
				t.Fatal("IO")
				return telemetry.Result{}, nil
			}))
			if result.Report.StopReason != "DEADLINE" || result.DeadlineAt.Sub(result.CreatedAt) > time.Second {
				t.Fatal(result)
			}
		})
	})
}

func TestExecuteEvidenceCapacity(t *testing.T) {
	calls := 0
	result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
		if v.Phase == "SYNTHESIZING" {
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}
		q := query()
		q.Kind = "metrics"
		q.Metric = "workflow_backlog"
		q.Limit = 20
		q.Start = q.Start.Add(time.Duration(v.Step) * time.Second)
		return raw(Decision{Kind: "query", Query: &q}), nil
	}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
		calls++
		r := telemetry.NewResult(q, "prometheus", "large-metrics")
		r.Quality.QueryOutcome = "SUCCEEDED"
		for i := range 2000 {
			r.Records = append(r.Records, telemetry.Record{Type: "metric", At: q.Start, Service: q.Service, Environment: q.Environment,
				Fields: map[string]string{"metric_key": q.Metric, "pool": strings.Repeat("p", 128), "step": fmt.Sprintf("step-%d", i), "value_status": "UNDEFINED"}})
		}
		r.Finish()
		return r, nil
	}))
	if err != nil || result.Report.StopReason != "EVIDENCE_LIMIT" || len(result.Evidence) == 0 || calls > 4 {
		t.Fatalf("calls=%d stop=%s entries=%d err=%v", calls, result.Report.StopReason, len(result.Evidence), err)
	}
	size := 0
	for _, e := range result.Evidence {
		size += len(raw(e))
	}
	if size > MaxEvidenceBytes {
		t.Fatalf("saved %d bytes", size)
	}
	if result.Steps[len(result.Steps)-1].Outcome != "REPORT_ACCEPTED" {
		t.Fatal("no final report chance")
	}
}

func TestExecuteQueryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
			if v.Step == 1 {
				q := query()
				return raw(Decision{Kind: "query", Query: &q}), nil
			}
			return raw(Decision{Kind: "report", Report: ptr(fallback())}), nil
		}), queryFunc(func(ctx context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			<-ctx.Done()
			return observation(q, "too-late"), nil
		}))
		if err != nil || len(result.Evidence) != 1 || len(result.Evidence[0].Records) != 0 || result.Steps[0].Outcome != "QUERY_FAILED" {
			t.Fatalf("%+v %v", result, err)
		}
	})
}
