package evaluation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

var (
	start    = time.Date(2026, 10, 27, 7, 25, 0, 0, time.UTC)
	end      = start.Add(15 * time.Minute)
	services = telemetry.DefaultPolicy().Services
)

func TestPlanNormalizesAndFitsABundle(t *testing.T) {
	queries := PlanQueries(services, "experiment", start, end)
	if len(queries)+maxTraces > telemetry.MaxBundleEntries {
		t.Fatalf("%d queries cannot fit", len(queries))
	}
	policy := telemetry.DefaultPolicy()
	for _, q := range queries {
		if _, err := policy.Normalize(q); err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
	}
}

func logResult(severity, trace string, fields map[string]string) telemetry.Result {
	q := telemetry.Query{Kind: "logs", Service: "order-service", Environment: "experiment", Start: start, End: end, Severity: severity, Limit: 100}
	r := telemetry.NewResult(q, "loki", "q:"+severity+trace)
	r.Quality.QueryOutcome = "SUCCEEDED"
	r.Records = []telemetry.Record{{Type: "log", At: start.Add(time.Minute), Service: "order-service", Environment: "experiment", TraceID: trace, Fields: fields}}
	r.Finish()
	return r
}

func TestTraceQueriesPutErrorsFirst(t *testing.T) {
	info := logResult("", strings.Repeat("a", 32), map[string]string{"severity_text": "INFO"})
	failed := logResult("ERROR", strings.Repeat("b", 32), map[string]string{"severity_text": "ERROR"})
	queries := TraceQueries([]telemetry.Result{info, failed})
	if len(queries) != 1 || queries[0].TraceID != strings.Repeat("b", 32) || queries[0].Kind != "trace" {
		t.Fatalf("%+v", queries)
	}
}

func TestScanLeaksNamesFieldNotValue(t *testing.T) {
	hits := ScanLeaks([]telemetry.Result{
		logResult("", "", map[string]string{"event_name": "failpoint hit by T3", "step": "reserve"}),
	})
	if len(hits) != 2 {
		t.Fatalf("%+v", hits)
	}
	for _, hit := range hits {
		if hit.Field != "fields.event_name" || hit.Count != 1 {
			t.Fatalf("%+v", hit)
		}
	}
	if clean := ScanLeaks([]telemetry.Result{logResult("", "", map[string]string{"event_name": "order.reserve", "pool": "default"})}); len(clean) != 0 {
		t.Fatalf("%+v", clean)
	}
}

func TestDeriveDropsKindsSoReplayReportsTheGap(t *testing.T) {
	q := telemetry.Query{Kind: "traces", Service: "order-service", Environment: "experiment", Start: start, End: end, Limit: 20}
	traces := telemetry.NewResult(q, "jaeger", "t")
	traces.Quality.QueryOutcome = "SUCCEEDED"
	traces.Finish()
	bundle, err := telemetry.NewBundle("live", strings.Repeat("a", 40), []telemetry.Result{logResult("", "", map[string]string{"severity_text": "INFO"}), traces})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := Derive(bundle, []string{"traces"})
	if err != nil || len(derived.Entries) != 1 || derived.UpstreamSHA != strings.Repeat("a", 40) {
		t.Fatalf("%+v %v", derived, err)
	}
	replay, err := fixture.New(derived, telemetry.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	result, err := replay.Query(t.Context(), q, telemetry.NewBudget(1))
	if err == nil || result.ErrorCode != "FIXTURE_QUERY_NOT_COVERED" {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := Derive(bundle, []string{"metrics"}); err == nil {
		t.Fatal("dropping nothing accepted")
	}
}

func TestManifestAndRequestCarryNoCaseLabel(t *testing.T) {
	request := Request(services, "experiment", start, end)
	data, err := json.Marshal(struct {
		M Manifest
		R any
	}{Manifest{SampleID: "s01"}, request})
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range []string{"T3", "T7", "failpoint", "reserve"} {
		if strings.Contains(string(data), hint) {
			t.Fatal("label leaked:", hint)
		}
	}
}

func fixedView(evidence ...telemetry.Result) investigation.View {
	return investigation.View{RunID: "run", Phase: "INVESTIGATING", Evidence: evidence,
		Request: investigation.Request{Service: "order-service", Services: []string{"order-service"}, LogSweep: true}}
}

func TestFixedFlowPicksQueriesByRuleAndNeverCallsTheModel(t *testing.T) {
	decider, err := investigation.NewModelDecider(providerFunc(func(context.Context, investigation.CompletionRequest) (investigation.CompletionResponse, error) {
		t.Fatal("model chose a query")
		return investigation.CompletionResponse{}, nil
	}), "m", 16384)
	if err != nil {
		t.Fatal(err)
	}
	flow := FixedFlow{ModelDecider: decider}
	next := func(view investigation.View) investigation.Decision {
		raw, err := flow.Decide(t.Context(), view)
		if err != nil {
			t.Fatal(err)
		}
		var decision investigation.Decision
		if err = json.Unmarshal(raw, &decision); err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if first := next(fixedView()); first.Query == nil || first.Query.Kind != "logs" || !first.Query.Notable {
		t.Fatalf("%+v", first)
	}
	logs := telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "order-service", Notable: true}, Records: []telemetry.Record{
		{TraceID: "info", Fields: map[string]string{"severity_text": "INFO"}}, {TraceID: "warn", Fields: map[string]string{"severity_text": "WARN"}},
		{TraceID: "error", Fields: map[string]string{"severity_text": "ERROR"}}, {TraceID: "other", Fields: map[string]string{}},
	}}
	if metric := next(fixedView(logs)); metric.Query == nil || metric.Query.Metric != fixedMetric {
		t.Fatalf("%+v", metric)
	}
	evidence := []telemetry.Result{logs, {Query: telemetry.Query{Kind: "metrics", Metric: fixedMetric}}}
	for _, want := range []string{"error", "warn", "info"} {
		decision := next(fixedView(evidence...))
		if decision.Query == nil || decision.Query.TraceID != want || decision.Query.Service != "order-service" {
			t.Fatalf("want %s: %+v", want, decision)
		}
		evidence = append(evidence, telemetry.Result{Query: *decision.Query})
	}
	if last := next(fixedView(evidence...)); last.Kind != "synthesize" {
		t.Fatalf("%+v", last)
	}
}

type providerFunc func(context.Context, investigation.CompletionRequest) (investigation.CompletionResponse, error)

func (f providerFunc) Complete(ctx context.Context, r investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	return f(ctx, r)
}

func hybridNext(t *testing.T, evidence ...telemetry.Result) (*telemetry.Query, string) {
	t.Helper()
	view := fixedView(evidence...)
	view.Evidence = append([]telemetry.Result{{Query: telemetry.Query{Kind: "metrics", Metric: fixedMetric}}, {Query: telemetry.Query{Kind: "traces", Service: "order-service"}}}, evidence...)
	return nextHybridQuery(view)
}

func logEntry(truncated bool, records ...telemetry.Record) telemetry.Result {
	entry := telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "a", Notable: true}, Records: records}
	entry.Quality.Truncated = truncated
	return entry
}

func logRecord(trace, severity string, extra ...string) telemetry.Record {
	fields := map[string]string{"severity_text": severity}
	for i := 0; i+1 < len(extra); i += 2 {
		fields[extra[i]] = extra[i+1]
	}
	return telemetry.Record{Type: "log", TraceID: trace, Fields: fields}
}

func TestHybridGapRules(t *testing.T) {
	first, _ := nextHybridQuery(fixedView())
	if first == nil || first.Metric != fixedMetric {
		t.Fatalf("%+v", first)
	}
	if q, _ := nextHybridQuery(fixedView(telemetry.Result{Query: telemetry.Query{Kind: "metrics", Metric: fixedMetric}})); q == nil || q.Kind != "traces" {
		t.Fatalf("%+v", q)
	}
	errorSummary := telemetry.Result{Query: telemetry.Query{Kind: "traces", Service: "a"}, Records: []telemetry.Record{
		{Type: "trace_summary", TraceID: "ok", Fields: map[string]string{}}, {Type: "trace_summary", TraceID: "failed", Fields: map[string]string{"error_spans": "2"}},
	}}
	t.Run("cut-off sweep asks for errors once", func(t *testing.T) {
		q, reason := hybridNext(t, logEntry(true, logRecord("x", "INFO")))
		if q == nil || q.Severity != "ERROR" || reason != FillTruncatedLogs {
			t.Fatalf("%+v %s", q, reason)
		}
		q, reason = hybridNext(t, logEntry(true, logRecord("x", "INFO")), telemetry.Result{Query: telemetry.Query{Kind: "logs", Service: "b", Severity: "ERROR"}})
		if q == nil || q.Kind != "trace" || reason == FillTruncatedLogs {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("warning trace comes before a span-only failure", func(t *testing.T) {
		q, reason := hybridNext(t, errorSummary, logEntry(false, logRecord("info", "INFO"), logRecord("warn", "WARN")))
		if q == nil || q.TraceID != "warn" || reason != FillSevereLog {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("failed trace with no warning in the logs is opened", func(t *testing.T) {
		q, reason := hybridNext(t, errorSummary, logEntry(false, logRecord("info", "INFO")))
		if q == nil || q.TraceID != "failed" || reason != FillErrorTrace {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("retry without a failure on record opens its trace", func(t *testing.T) {
		q, reason := hybridNext(t, logEntry(false, logRecord("info", "INFO"), logRecord("retry", "INFO", "attempt", "1")))
		if q == nil || q.TraceID != "retry" || reason != FillRetryTrace {
			t.Fatalf("%+v %s", q, reason)
		}
	})
	t.Run("no gap falls back to any trace, then stops", func(t *testing.T) {
		q, reason := hybridNext(t, logEntry(false, logRecord("info", "INFO")))
		if q == nil || reason != FillDefaultTrace {
			t.Fatalf("%+v %s", q, reason)
		}
		if q, _ = hybridNext(t, logEntry(false)); q != nil {
			t.Fatalf("%+v", q)
		}
	})
}
