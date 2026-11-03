package investigation

import (
	"encoding/json"
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
	got := measureCoverage(View{Evidence: []telemetry.Result{failed, cut, search, opened}}, map[string]bool{})
	want := Coverage{FailedQueries: 1, TruncatedQueries: 1, ErrorTracesNotOpened: 1, SevereLogsNotShown: 1, CallsMissingASide: 1}
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
