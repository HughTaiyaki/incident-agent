package investigation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestBuildModelInput(t *testing.T) {
	entry := logSample(t, 100)
	summary, err := SummarizeLogs(entry)
	if err != nil {
		t.Fatal(err)
	}
	view := View{Request: request(), Phase: "INVESTIGATING", Evidence: []telemetry.Result{entry}, LogPatterns: []LogPatterns{summary}}
	data, omitted, _, err := buildModelInput(view, 4500)
	if err != nil {
		t.Fatal(err)
	}
	var input modelInput
	if err = json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if estimateInput(view.Phase, data) > 4500 || len(input.Records) == 0 || omitted == 0 || len(input.Records) > 24 {
		t.Fatalf("size=%d records=%d omitted=%d", estimateInput(view.Phase, data), len(input.Records), omitted)
	}
	if input.Queries[0].Quality.Coverage != "UNKNOWN" || input.OmittedRecords != 100-len(input.Records) {
		t.Fatal(input)
	}
	if bytes.Contains(data, []byte("canonical_query")) || bytes.Contains(data, []byte("message body")) {
		t.Fatal("raw payload exposed")
	}
	again, _, _, err := buildModelInput(view, 4500)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatal("context not deterministic")
	}
	original := string(raw(view))
	view.Request.Summary = strings.Repeat("长", 2000)
	report, _, _, err := buildModelInput(View{Request: view.Request, Phase: "SYNTHESIZING"}, ReportTokenReserve-512)
	if err != nil {
		t.Fatal("minimum report context does not fit", err)
	}
	if !bytes.Contains(report, []byte(`"summary_truncated":true`)) {
		t.Fatal("missing truncation flag")
	}
	if strings.Contains(original, strings.Repeat("长", 10)) {
		t.Fatal("invalid fixture")
	}
	if _, _, _, err := buildModelInput(view, 100); err == nil {
		t.Fatal("undersized input accepted")
	}
}

func TestModelInputMasksBusinessIdentifiers(t *testing.T) {
	at := time.Date(2026, 10, 28, 6, 1, 0, 0, time.UTC)
	record := telemetry.Record{ID: "r1", Type: "log", At: at, Service: "order-service", Environment: "local", TraceID: strings.Repeat("a", 32),
		Fields: map[string]string{"order_id": "real-order", "operation_id": "real-operation", "event_name": "order.transition"}}
	view := View{RunID: "run-secret", Phase: "INVESTIGATING", Evidence: []telemetry.Result{{QueryID: "q", Records: []telemetry.Record{record}}}}
	first, _, _, err := buildModelInput(view, 16000)
	if err != nil {
		t.Fatal(err)
	}
	text := string(first)
	for _, private := range []string{"real-order", "real-operation", "run-secret"} {
		if strings.Contains(text, private) {
			t.Fatalf("%s reached prompt: %s", private, text)
		}
	}
	if !strings.Contains(text, "order.transition") || !strings.Contains(text, record.TraceID) {
		t.Fatalf("usable context lost: %s", text)
	}
	if view.Evidence[0].Records[0].Fields["order_id"] != "real-order" {
		t.Fatal("masking changed saved evidence")
	}
	again, _, _, _ := buildModelInput(view, 16000)
	if string(again) != text {
		t.Fatal("pseudonym not stable within a run")
	}
	view.RunID = "other-run"
	other, _, _, _ := buildModelInput(view, 16000)
	if string(other) == text {
		t.Fatal("pseudonym shared across runs")
	}
}

func TestFeedbackHintsAreFixedText(t *testing.T) {
	for code := range feedbackHints {
		data, _, _, err := buildModelInput(View{Request: request(), Phase: "INVESTIGATING", Feedback: code}, 6000)
		if err != nil {
			t.Fatal(err)
		}
		var input modelInput
		if err := json.Unmarshal(data, &input); err != nil {
			t.Fatal(err)
		}
		if input.Feedback != feedbackHints[code] || !strings.HasPrefix(input.Feedback, strings.Split(code, ":")[0]) {
			t.Fatalf("%s: %q", code, input.Feedback)
		}
	}
	if got := feedbackText("QUERY_SUCCEEDED"); got != "QUERY_SUCCEEDED" {
		t.Fatal(got)
	}
}

func metricResult(metric string, values ...float64) telemetry.Result {
	q := query()
	q.Kind, q.Metric = "metrics", metric
	result := telemetry.NewResult(q, "prometheus", "query")
	result.Quality.QueryOutcome = "SUCCEEDED"
	for i, value := range values {
		v := value
		result.Records = append(result.Records, telemetry.Record{Type: "metric", At: q.Start.Add(time.Duration(i) * time.Minute), Service: q.Service,
			Environment: q.Environment, Fields: map[string]string{"metric_key": metric}, Value: &v})
	}
	result.Finish()
	return result
}

func decodeInput(t *testing.T, data []byte) modelInput {
	t.Helper()
	var input modelInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestModelInputListsEveryQueryAsked(t *testing.T) {
	evidence := []telemetry.Result{}
	for _, metric := range []string{"order_error_rate", "rpc_error_rate", "rpc_latency_p95", "workflow_backlog", "db_pool_utilization"} {
		evidence = append(evidence, metricResult(metric, 1))
	}
	data, _, _, err := buildModelInput(View{Request: request(), Phase: "INVESTIGATING", Evidence: evidence}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeInput(t, data)
	if len(input.Queries) != 5 || input.OmittedQueries != 0 || input.Queries[0].Metric != "order_error_rate" || input.Queries[0].Service != query().Service {
		t.Fatalf("%+v", input.Queries)
	}
	if input.Queries[0].Quality != nil || input.Queries[4].Quality == nil {
		t.Fatal("only recent queries should carry quality")
	}
}

func TestModelInputShowsMetricPeakBeyondRecordLimit(t *testing.T) {
	values := make([]float64, 100)
	values[97] = 0.8312345
	data, _, _, err := buildModelInput(View{Request: request(), Phase: "INVESTIGATING", Evidence: []telemetry.Result{metricResult("order_error_rate", values...)}}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeInput(t, data)
	found := false
	for _, record := range input.Records {
		found = found || (record.Value != nil && *record.Value == 0.8312)
	}
	if !found || len(input.Records) != investigationRecordLimit {
		t.Fatalf("peak missing from %d records", len(input.Records))
	}
}

func TestModelInputShowsPatternsErrorsFirst(t *testing.T) {
	at := time.Date(2026, 10, 28, 6, 1, 0, 0, time.UTC)
	view := View{Request: request(), Phase: "INVESTIGATING", LogPatterns: []LogPatterns{{Groups: []LogPattern{
		{Key: PatternKey{Service: "order-service", EventName: "order.changed", Severity: "INFO"}, ObservedCount: 90, FirstAt: at, LastAt: at, RecordIDs: []string{strings.Repeat("a", 64)}},
		{Key: PatternKey{Service: "order-service", EventName: "order.failed", Severity: "ERROR"}, ObservedCount: 10, FirstAt: at, LastAt: at, RecordIDs: []string{strings.Repeat("b", 64)}},
	}}}}
	data, _, _, err := buildModelInput(view, 16000)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeInput(t, data)
	if len(input.Patterns) != 2 || input.Patterns[0].Key.Severity != "ERROR" || input.Patterns[0].Count != 10 || input.Patterns[0].RecordIDs[0] != strings.Repeat("b", shortIDLength) {
		t.Fatalf("%+v", input.Patterns)
	}
}

func TestSynthesisDrawsRecordsFromEveryQuery(t *testing.T) {
	evidence := []telemetry.Result{}
	for _, metric := range []string{"order_error_rate", "rpc_error_rate", "rpc_latency_p95", "workflow_backlog", "db_pool_utilization"} {
		evidence = append(evidence, metricResult(metric, 1))
	}
	data, _, _, err := buildModelInput(View{Request: request(), Phase: "SYNTHESIZING", Evidence: evidence}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if input := decodeInput(t, data); len(input.Records) != 5 {
		t.Fatalf("records=%d", len(input.Records))
	}
}

func TestCitationsExpandToStoredIDs(t *testing.T) {
	first, second := "abcdef012345"+strings.Repeat("1", 52), "fedcba987654"+strings.Repeat("2", 52)
	collide := "fedcba987654" + strings.Repeat("3", 52)
	evidence := []telemetry.Result{{Records: []telemetry.Record{{ID: first}, {ID: second}, {ID: collide}}}}
	got := string(expandCitations([]byte(`["abcdef012345","fedcba987654","000000000000","`+first+`"]`), evidence))
	want := `["` + first + `","fedcba987654","000000000000","` + first + `"]`
	if got != want {
		t.Fatalf("%s", got)
	}
}

func TestUnmaskQueryOnlyRewritesQueries(t *testing.T) {
	record := telemetry.Record{ID: "r", Fields: map[string]string{"order_id": "real-order", "event_name": "order.failed"}}
	evidence := []telemetry.Result{{Records: []telemetry.Record{record}}}
	masked := maskIdentifiers(record, "salt").Fields["order_id"]
	query := `{"kind":"query","query":{"kind":"logs","order_id":"` + masked + `"}}`
	if got := string(unmaskQuery([]byte(query), evidence, "salt")); !strings.Contains(got, `"real-order"`) || strings.Contains(got, masked) {
		t.Fatal(got)
	}
	report := `{"kind":"report","report":{"unknowns":["` + masked + `"]}}`
	if got := string(unmaskQuery([]byte(report), evidence, "salt")); got != report {
		t.Fatal("report text must keep pseudonyms:", got)
	}
	unknown := `{"kind":"query","query":{"order_id":"masked-0000000000000000"}}`
	if got := string(unmaskQuery([]byte(unknown), evidence, "salt")); got != unknown {
		t.Fatal(got)
	}
}

func TestModelInputShowsFiltersAndUncheckedServices(t *testing.T) {
	request := request()
	request.Services = []string{"order-service", "goods-service"}
	q := query()
	q.Severity, q.OrderID = "ERROR", "real-order"
	filtered := telemetry.NewResult(q, "loki", "filtered")
	filtered.Quality.QueryOutcome = "SUCCEEDED"
	filtered.Finish()
	data, _, _, err := buildModelInput(View{RunID: "salt", Request: request, Phase: "INVESTIGATING", Evidence: []telemetry.Result{filtered}}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "real-order") {
		t.Fatal("filter value leaked:", string(data))
	}
	input := decodeInput(t, data)
	if input.Queries[0].Severity != "ERROR" || !strings.HasPrefix(input.Queries[0].OrderID, "masked-") {
		t.Fatalf("%+v", input.Queries[0])
	}
	// A filtered query does not count as having looked at the service's logs.
	if len(input.UncheckedLogs) != 2 || len(input.UncheckedTraces) != 2 {
		t.Fatalf("%v %v", input.UncheckedLogs, input.UncheckedTraces)
	}
	plain := query()
	logs := telemetry.NewResult(plain, "loki", "plain")
	logs.Quality.QueryOutcome = "SUCCEEDED"
	logs.Finish()
	data, _, _, err = buildModelInput(View{Request: request, Phase: "INVESTIGATING", Evidence: []telemetry.Result{logs}}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeInput(t, data).UncheckedLogs; len(got) != 1 || got[0] != "goods-service" {
		t.Fatal(got)
	}
}

func TestUndefinedMetricPointsAreCountedNotShown(t *testing.T) {
	result := metricResult("rpc_error_rate", 0.5)
	result.Records = append(result.Records, telemetry.Record{ID: "undefined", Type: "metric", At: result.Query.Start, Service: result.Query.Service,
		Environment: result.Query.Environment, Fields: map[string]string{"metric_key": "rpc_error_rate", "value_status": "UNDEFINED"}})
	data, _, _, err := buildModelInput(View{Request: request(), Phase: "INVESTIGATING", Evidence: []telemetry.Result{result}}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeInput(t, data)
	if len(input.Records) != 1 || input.Queries[0].UndefinedPoints != 1 || input.OmittedRecords != 1 {
		t.Fatalf("records=%d undefined=%d omitted=%d", len(input.Records), input.Queries[0].UndefinedPoints, input.OmittedRecords)
	}
}
