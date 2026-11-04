package investigation

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func logWith(id, severity, message string) telemetry.Record {
	return telemetry.Record{ID: id, Type: "log", Service: "a", Fields: map[string]string{"severity_text": severity, "message": message}}
}

func TestSevereLogKindsAreDistinctAndMostSevereFirst(t *testing.T) {
	got := severeLogKinds([]telemetry.Result{{Records: []telemetry.Record{
		logWith("1", "INFO", "routine"), logWith("2", "WARN", "late"), logWith("3", "WARN", "late"), logWith("4", "ERROR", "boom"),
		{ID: "5", Type: "span", Fields: map[string]string{"severity_text": "ERROR"}},
	}}})
	if len(got) != 2 || got[0].ID != "4" || got[1].ID != "2" {
		t.Fatalf("%+v", got)
	}
}

func TestMeasureCoverageCountsWhatTheRunDidNotSee(t *testing.T) {
	failed := telemetry.Result{Status: "UNAVAILABLE", Query: telemetry.Query{Kind: "logs"}}
	empty := telemetry.Result{Status: "EMPTY", Query: telemetry.Query{Kind: "logs"}}
	cut := telemetry.Result{Query: telemetry.Query{Kind: "logs"}, Records: []telemetry.Record{logWith("w", "WARN", "late")}}
	cut.Quality.Truncated = true
	search := telemetry.Result{Query: telemetry.Query{Kind: "traces"}, Records: []telemetry.Record{
		{Type: "trace_summary", TraceID: "open", Fields: map[string]string{"error_spans": "1"}},
		{Type: "trace_summary", TraceID: "closed", Fields: map[string]string{"error_spans": "1"}},
		{Type: "trace_summary", TraceID: "fine", Fields: map[string]string{}},
	}}
	opened := telemetry.Result{Query: telemetry.Query{Kind: "trace", TraceID: "open"}, Records: []telemetry.Record{
		span("root", "a", "", 0, 900), span("c", "a", "root", 0, 800, "error", "true"),
	}}
	got := measureCoverage(View{Evidence: []telemetry.Result{failed, empty, cut, search, opened}}, map[string]bool{})
	want := Coverage{FailedQueries: 1, EmptyOrPartial: 1, TruncatedQueries: 1, ErrorTracesNotOpened: 1, SevereLogsNotShown: 1, CallsMissingASide: 1}
	if got != want {
		t.Fatalf("%+v", got)
	}
	if got = measureCoverage(View{Evidence: []telemetry.Result{cut}}, map[string]bool{"w": true}); got.SevereLogsNotShown != 0 {
		t.Fatalf("%+v", got)
	}
	if got = measureCoverage(View{}, nil); got != (Coverage{}) {
		t.Fatalf("%+v", got)
	}
}

func TestCoverageShowsWarningsAheadOfBulkSpans(t *testing.T) {
	spans := telemetry.Result{Query: telemetry.Query{Kind: "trace", TraceID: "t"}}
	for i := range 40 {
		spans.Records = append(spans.Records, span(strconv.Itoa(i), "a", "", 0, 5, "error", "true"))
	}
	logs := telemetry.Result{Query: telemetry.Query{Kind: "logs"}}
	for i := range 30 {
		logs.Records = append(logs.Records, logWith("info"+strconv.Itoa(i), "INFO", "routine"))
	}
	logs.Records = append(logs.Records, logWith("late", "WARN", "arrived late"))
	view := View{RunID: "run", Phase: "SYNTHESIZING", Coverage: true, Evidence: []telemetry.Result{spans, logs}}
	input, _, _, err := buildCountedInput(view, 100000, func(_ string, b []byte) int { return len(b) })
	if err != nil {
		t.Fatal(err)
	}
	var decoded modelInput
	if err = json.Unmarshal(input, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coverage == nil || decoded.Coverage.SevereLogsNotShown != 0 || decoded.Records[0].ID != "late" || len(decoded.Records) != synthesisRecordLimit {
		t.Fatalf("%+v", decoded.Coverage)
	}
	view.Coverage = false
	input, _, _, err = buildCountedInput(view, 100000, func(_ string, b []byte) int { return len(b) })
	if err != nil {
		t.Fatal(err)
	}
	decoded = modelInput{}
	if err = json.Unmarshal(input, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coverage != nil || decoded.Records[0].ID == "late" {
		t.Fatalf("coverage applied without being enabled: %+v", decoded.Coverage)
	}
}

func metricEntry(service, metric string, values ...float64) telemetry.Result {
	entry := telemetry.Result{Query: telemetry.Query{Kind: "metrics", Service: service, Metric: metric}}
	for _, value := range values {
		v := value
		entry.Records = append(entry.Records, telemetry.Record{Type: "metric", Service: service, Value: &v, Fields: map[string]string{"metric_key": metric, "step": "outbox"}})
	}
	return entry
}

func TestMeasureMetricsSeparatesSeenEmptyAndUnchecked(t *testing.T) {
	view := View{Request: Request{Service: "front", Services: []string{"front", "back"}}, Evidence: []telemetry.Result{
		metricEntry("front", "workflow_oldest_age", 10, 20, 30, 40), metricEntry("front", "rpc_error_rate", 0, 0.2, 0.1), metricEntry("front", "workflow_backlog"),
	}}
	got := measureMetrics(view)
	if len(got.Signals) != 2 || !got.Signals[0].Rising || got.Signals[0].Metric != "workflow_oldest_age" || got.Signals[0].First != 10 || got.Signals[0].Last != 40 || got.Signals[0].Labels != "step=outbox" {
		t.Fatalf("%+v", got.Signals)
	}
	if got.Signals[1].Rising || got.Signals[1].Max != 0.2 {
		t.Fatalf("fell and came back must not read as rising: %+v", got.Signals[1])
	}
	if !reflect.DeepEqual(got.WithoutData, []string{"front/workflow_backlog"}) || !reflect.DeepEqual(got.ServicesWithoutMetrics, []string{"back"}) {
		t.Fatalf("%+v", got)
	}
	if !slices.Contains(got.NotChecked, "front/db_pool_utilization") || slices.Contains(got.NotChecked, "front/workflow_backlog") {
		t.Fatalf("%+v", got.NotChecked)
	}
	many := View{Request: Request{Service: "front"}}
	for i := range metricSignalLimit + 2 {
		entry := metricEntry("front", "rpc_error_rate", 1)
		entry.Records[0].Fields["peer"] = strconv.Itoa(i)
		many.Evidence = append(many.Evidence, entry)
	}
	if got = measureMetrics(many); len(got.Signals) != metricSignalLimit || got.OmittedSignals != 2 {
		t.Fatalf("%d %d", len(got.Signals), got.OmittedSignals)
	}
}

func TestCheckClaims(t *testing.T) {
	spans := telemetry.Result{Query: telemetry.Query{Kind: "trace"}, Records: []telemetry.Record{span("s", "a", "", 0, 1203)}}
	message := telemetry.Result{Query: telemetry.Query{Kind: "logs"}, Records: []telemetry.Record{logWith("m", "WARN", "gave up after 30s")}}
	rate := 0.6
	metric := telemetry.Result{Query: telemetry.Query{Kind: "metrics"}, Records: []telemetry.Record{{Type: "metric", Value: &rate, Fields: map[string]string{}}}}
	expectations := []CallExpectation{{ExpectedMs: 800}}
	gap := telemetry.Result{Status: "UNAVAILABLE", Query: telemetry.Query{Kind: "trace"}}
	clean := []telemetry.Result{spans, message, metric}
	for _, tt := range []struct {
		name, text string
		evidence   []telemetry.Result
		want       string
	}{
		{"recorded duration rounded", "the call took about 1.2 s", clean, ""},
		{"recorded in another unit", "it ran 1203 ms", clean, ""},
		{"documented expectation", "documentation expects 800 ms", clean, ""},
		{"number from a message", "it gave up after 30 s", clean, ""},
		{"percentage from a metric", "the error rate was 60%", clean, ""},
		{"metric in seconds", "the oldest item was 94 s old", append(clean[:3:3], telemetry.Result{Query: telemetry.Query{Kind: "metrics"}, Records: []telemetry.Record{{Type: "metric", Value: ptr(94.2), Fields: map[string]string{}}}}), ""},
		{"made-up duration", "the call took 45 s", clean, "NUMBER"},
		{"made-up percentage", "the error rate was 90%", clean, "NUMBER"},
		{"plain words are not units", "3 services and 2 steps", clean, ""},
		{"absence with a known gap", "there were no errors in the window", append(clean[:3:3], gap), "ABSENCE"},
		{"absence kept to what was shown", "no errors in the records shown", append(clean[:3:3], gap), ""},
		{"absence with nothing known missing", "no errors in the window", clean, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			draft := Draft{Facts: []Claim{{Text: tt.text}}}
			if got := CheckClaims(draft, tt.evidence, expectations); got != tt.want {
				t.Fatalf("%q", got)
			}
			primary := Draft{Primary: &Hypothesis{Claim: Claim{Text: tt.text}}}
			if got := CheckClaims(primary, tt.evidence, expectations); got != tt.want {
				t.Fatalf("primary %q", got)
			}
		})
	}
	t.Run("metrics alone cannot diagnose", func(t *testing.T) {
		metricsOnly := []telemetry.Result{metric, gap}
		if got := CheckClaims(Draft{Result: "DIAGNOSED"}, metricsOnly, nil); got != "EVENTS" {
			t.Fatalf("%q", got)
		}
		if got := CheckClaims(Draft{Result: "INCONCLUSIVE"}, metricsOnly, nil); got != "" {
			t.Fatalf("%q", got)
		}
		if got := CheckClaims(Draft{Result: "DIAGNOSED"}, clean, nil); got != "" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("what still fails is dropped", func(t *testing.T) {
		m, err := NewModelDecider(providerFunc(nil), "m", 16384)
		if err != nil {
			t.Fatal(err)
		}
		m.UseCoverage()
		kept := dropFailedClaims(Draft{Result: "DIAGNOSED", Facts: []Claim{{Text: "took 45 s"}, {Text: "took 1.2 s"}}, Primary: &Hypothesis{Claim: Claim{Text: "took 45 s"}}}, m, clean)
		if len(kept.Facts) != 1 || kept.Primary != nil || kept.Result != "INCONCLUSIVE" {
			t.Fatalf("%+v", kept)
		}
	})
}
