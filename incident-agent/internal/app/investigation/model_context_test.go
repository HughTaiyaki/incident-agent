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
	if estimateInput(data) > 4500 || len(input.Records) == 0 || omitted == 0 || len(input.Records) > 24 {
		t.Fatalf("size=%d records=%d omitted=%d", estimateInput(data), len(input.Records), omitted)
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
