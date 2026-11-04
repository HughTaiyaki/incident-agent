package investigation

import (
	"encoding/json"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func planView(evidence ...telemetry.Result) View {
	return View{RunID: "run", Phase: "INVESTIGATING", Evidence: evidence,
		Request: Request{Service: "front", Services: []string{"front"}, LogSweep: true}}
}

func planNext(evidence ...telemetry.Result) (*telemetry.Query, string) {
	view := planView(evidence...)
	view.Evidence = append(plannedPrefix(), evidence...)
	return NextPlannedQuery(view)
}

// plannedPrefix is the fixed part of the plan, already done.
func plannedPrefix() []telemetry.Result {
	prefix := []telemetry.Result{{Query: telemetry.Query{Kind: "metrics", Service: "front", Metric: planMetric}}, {Query: telemetry.Query{Kind: "traces", Service: "front"}}}
	for _, name := range planCoreMetrics {
		prefix = append(prefix, telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: "front", Metric: name}})
	}
	return prefix
}

func planLogs(truncated bool, records ...telemetry.Record) telemetry.Result {
	entry := telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "a", Notable: true}, Records: records}
	entry.Quality.Truncated = truncated
	return entry
}

func planLog(trace, severity string, extra ...string) telemetry.Record {
	fields := map[string]string{"severity_text": severity}
	for i := 0; i+1 < len(extra); i += 2 {
		fields[extra[i]] = extra[i+1]
	}
	return telemetry.Record{Type: "log", TraceID: trace, Fields: fields}
}

func TestPlannedQueriesFollowTheEvidence(t *testing.T) {
	first, _ := NextPlannedQuery(planView())
	if first == nil || first.Metric != planMetric || first.Service != "front" {
		t.Fatalf("%+v", first)
	}
	if q, _ := NextPlannedQuery(planView(telemetry.Result{Query: telemetry.Query{Kind: "metrics", Metric: planMetric}})); q == nil || q.Kind != "traces" {
		t.Fatalf("%+v", q)
	}
	errorSummary := telemetry.Result{Query: telemetry.Query{Kind: "traces", Service: "a"}, Records: []telemetry.Record{
		{Type: "trace_summary", TraceID: "ok", Fields: map[string]string{}}, {Type: "trace_summary", TraceID: "failed", Fields: map[string]string{"error_spans": "2"}},
	}}
	t.Run("cut-off sweep asks for errors once", func(t *testing.T) {
		q, reason := planNext(planLogs(true, planLog("x", "INFO")))
		if q == nil || q.Severity != "ERROR" || reason != FillTruncatedLogs {
			t.Fatalf("%+v %s", q, reason)
		}
		q, reason = planNext(planLogs(true, planLog("x", "INFO")), telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "b", Severity: "ERROR"}})
		if q == nil || q.Kind != "trace" || reason == FillTruncatedLogs {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("warning trace comes before a span-only failure", func(t *testing.T) {
		q, reason := planNext(errorSummary, planLogs(false, planLog("info", "INFO"), planLog("warn", "WARN")))
		if q == nil || q.TraceID != "warn" || reason != FillSevereLog {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("failed trace with no warning in the logs is opened", func(t *testing.T) {
		q, reason := planNext(errorSummary, planLogs(false, planLog("info", "INFO")))
		if q == nil || q.TraceID != "failed" || reason != FillErrorTrace {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("retry without a failure on record opens its trace", func(t *testing.T) {
		q, reason := planNext(planLogs(false, planLog("info", "INFO"), planLog("retry", "INFO", "attempt", "1")))
		if q == nil || q.TraceID != "retry" || reason != FillRetryTrace {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("no gap falls back to any trace, then stops", func(t *testing.T) {
		q, reason := planNext(planLogs(false, planLog("info", "INFO")))
		if q == nil || reason != FillDefaultTrace {
			t.Fatalf("%+v %s", q, reason)
		}
		if q, reason = planNext(planLogs(false)); q == nil || reason != FillCalleePool || q.Service != "front" {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("core metrics come before traces", func(t *testing.T) {
		view := planView(telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: "front", Metric: planMetric}}, telemetry.Result{Query: telemetry.Query{Kind: "traces", Service: "front"}},
			planLogs(false, planLog("warn", "WARN")))
		q, _ := NextPlannedQuery(view)
		if q == nil || q.Metric != planCoreMetrics[0] {
			t.Fatalf("%+v", q)
		}
	})
	t.Run("pool metrics follow the failed call", func(t *testing.T) {
		view := planView(append(plannedPrefix(), telemetry.Result{Query: telemetry.Query{Kind: "trace", TraceID: "t"}, Records: []telemetry.Record{
			span("c", "front", "", 0, 3000, "error", "true"), span("s", "back", "c", 1, 100, "error", "true"),
		}})...)
		view.Request.Services = []string{"front", "back"}
		q, reason := NextPlannedQuery(view)
		if q == nil || q.Service != "back" || q.Metric != planPoolMetrics[0] || reason != FillCalleePool {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("stops once traces and pool are done", func(t *testing.T) {
		evidence := plannedPrefix()
		for _, id := range []string{"a", "b", "c"} {
			evidence = append(evidence, telemetry.Result{Query: telemetry.Query{Kind: "trace", TraceID: id}})
		}
		for _, name := range planPoolMetrics {
			evidence = append(evidence, telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: "front", Metric: name}})
		}
		if q, _ := NextPlannedQuery(planView(evidence...)); q != nil {
			t.Fatalf("%+v", q)
		}
	})
}

func TestPlannedDecisionFinishesWhenNothingIsLeft(t *testing.T) {
	evidence := append(plannedPrefix(), telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "front", Notable: true}})
	for _, name := range planPoolMetrics {
		evidence = append(evidence, telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: "front", Metric: name}})
	}
	view := planView(evidence...)
	raw, reason, err := PlannedDecision(view)
	if err != nil || reason != "" {
		t.Fatal(err, reason)
	}
	var decision Decision
	if err = json.Unmarshal(raw, &decision); err != nil || decision.Kind != synthesizeKind {
		t.Fatalf("%s %v", raw, err)
	}
}

// The plan must depend on what happened, not on what the services are called.
func TestPlanAndRelationsIgnoreServiceNames(t *testing.T) {
	build := func(front, back string) []telemetry.Result {
		logs := telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: front, Notable: true}, Records: []telemetry.Record{
			{Type: "log", Service: front, TraceID: "t1", Fields: map[string]string{"severity_text": "WARN", "message": "late"}},
		}}
		spans := telemetry.Result{Query: telemetry.Query{Kind: "trace", Service: front, TraceID: "t2"}, Records: []telemetry.Record{
			span("c", front, "", 0, 800, "error", "true"), span("s", back, "c", 5, 1200),
		}}
		evidence := []telemetry.Result{logs, spans, {Query: telemetry.Query{Kind: "metrics", Service: front, Metric: planMetric}}, {Query: telemetry.Query{Kind: "traces", Service: front}}}
		for _, name := range planCoreMetrics {
			evidence = append(evidence, telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: front, Metric: name}})
		}
		return evidence
	}
	plan := func(front, back string) (string, string) {
		view := View{RunID: "run", Phase: "INVESTIGATING", Evidence: build(front, back), Request: Request{Service: front, Services: []string{front, back}}}
		query, reason := NextPlannedQuery(view)
		if query == nil {
			t.Fatal("no query")
		}
		return query.TraceID + "/" + query.Kind, reason
	}
	mallQuery, mallReason := plan("order-service", "goods-service")
	otherQuery, otherReason := plan("checkout", "ledger")
	if mallQuery != otherQuery || mallReason != otherReason {
		t.Fatalf("%s %s != %s %s", mallQuery, mallReason, otherQuery, otherReason)
	}
	mall := CallRelations(build("order-service", "goods-service"))
	other := CallRelations(build("checkout", "ledger"))
	if len(mall) != 1 || len(other) != 1 || *mall[0].CalleeEndAfterCallerMs != *other[0].CalleeEndAfterCallerMs || other[0].Callee != "ledger" {
		t.Fatalf("%+v %+v", mall, other)
	}
	if measureCoverage(View{Evidence: build("order-service", "goods-service")}, nil) != measureCoverage(View{Evidence: build("checkout", "ledger")}, nil) {
		t.Fatal("coverage depends on names")
	}
}
