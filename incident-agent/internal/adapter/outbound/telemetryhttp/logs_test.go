package telemetryhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestLogsStructuredMetadataAndRedaction(t *testing.T) {
	t.Parallel()
	q := testQuery("logs")
	q.EventName = "order.transition"
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		expression := r.URL.Query().Get("query")
		if strings.Contains(expression, "| json") || !strings.Contains(expression, `event_name="order.transition"`) {
			t.Errorf("body/metadata query confused: %s", expression)
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[
   {"stream":{"service_name":"order-service","deployment_environment_name":"experiment"},"values":[
    ["%d","password=hunter2 failpoint goods.reserve.after-commit user@example.com",{"event_name":"order.transition","order_id":"order-123","from":"RESERVING","to":"PENDING_PAYMENT","password":"hunter2","error_stack":"private","trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
   ]},
   {"stream":{"service_name":"order-service","deployment_environment_name":"local"},"values":[["%d","foreign",{"event_name":"order.transition"}]]}
  ]}}`, q.Start.UnixNano(), q.Start.UnixNano())
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || result.Records[0].Fields["to"] != "PENDING_PAYMENT" {
		t.Fatalf("records=%+v", result.Records)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"hunter2", "failpoint", "user@example.com", "error_stack", "foreign"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if result.Status != "PARTIAL" || result.Records[0].Fields["body_omitted"] != "true" {
		t.Fatal("omission not reported")
	}
}

func TestLogsRejectMalformedAndConflictingMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid timestamp", body: `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["bad","text"]]}]}}`},
		{name: "conflicting service", body: fmt.Sprintf(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"admin-service"},"values":[["%d","text",{"service_name":"order-service"}]]}]}}`, testQuery("logs").Start.UnixNano())},
		{name: "wrong result type", body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, tt.body) })
			result, err := client.Query(t.Context(), testQuery("logs"), telemetry.NewBudget(16))
			if err == nil || result.Status != "UNAVAILABLE" || len(result.Records) != 0 {
				t.Fatalf("malformed response accepted: %+v", result)
			}
		})
	}
}

func TestLogsMarksLimitWithoutInventingPagination(t *testing.T) {
	t.Parallel()
	q := testQuery("logs")
	q.Limit = 1
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "1" {
			t.Fatal("limit was not enforced in request")
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"order-service","deployment_environment_name":"experiment"},"values":[["%d","one"],["%d","two"]]}]}}`, q.Start.UnixNano(), q.Start.UnixNano()+1)
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil || len(result.Records) != 1 || !result.Quality.Truncated || result.Requests != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestLogBodyKeptOnlyWhereItExplainsAndAfterSanitizing(t *testing.T) {
	t.Parallel()
	q := testQuery("logs")
	at := q.Start.Add(time.Minute).UnixNano()
	line := func(metadata, body string) string {
		return fmt.Sprintf(`{"stream":{"service_name":"%s","deployment_environment_name":"%s","trace_id":"","span_id":""%s},"values":[["%d",%q]]}`, q.Service, q.Environment, metadata, at, body)
	}
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[%s,%s,%s,%s,%s]}}`,
			line(`,"severity_text":"WARN"`, "order O2026110212560538669776: reserve outcome unknown"),
			line(`,"severity_text":"INFO","event_name":"message.redelivered"`, "event 1fab9758-aa11-4c2d-9e10-0123456789ab already processed"),
			line(`,"severity_text":"INFO"`, "routine text for user alice"),
			line(`,"severity_text":"ERROR"`, "login failed password=hunter2"),
			line(`,"severity_text":"WARN","order_id":"plain-word-id"`, "order plain-word-id: stuck"))
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil {
		t.Fatal(err)
	}
	messages := map[string]bool{}
	omitted := 0
	for _, record := range result.Records {
		if record.Fields["body_omitted"] == "true" {
			omitted++
		}
		if message := record.Fields["message"]; message != "" {
			messages[message] = true
		}
		if err := telemetry.ValidateRecord(record, result.Query, telemetry.DefaultPolicy()); err != nil {
			t.Fatalf("%+v: %v", record, err)
		}
	}
	if len(result.Records) != 5 || omitted != 2 || !messages["order [id]: stuck"] || !messages["order [id]: reserve outcome unknown"] || !messages["event [id] already processed"] {
		t.Fatalf("omitted=%d %v", omitted, messages)
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"alice", "hunter2", "O2026110212560538669776", "1fab9758", "order plain-word-id"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("leaked %s", private)
		}
	}
}

func TestNotableLogsAskLokiForThemAndDropRoutineRecords(t *testing.T) {
	t.Parallel()
	q := testQuery("logs")
	q.Notable = true
	at := q.Start.Add(time.Minute).UnixNano()
	line := func(metadata string) string {
		return fmt.Sprintf(`{"stream":{"service_name":"%s","deployment_environment_name":"%s"%s},"values":[["%d","text"]]}`, q.Service, q.Environment, metadata, at)
	}
	var asked string
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("query")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[%s,%s,%s]}}`,
			line(`,"severity_text":"INFO"`), line(`,"severity_text":"WARN"`), line(`,"severity_text":"INFO","event_name":"order.transition"`))
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, `| event_name!="" or severity_text=~"WARN|ERROR|FATAL"`) {
		t.Fatal(asked)
	}
	if len(result.Records) != 2 {
		t.Fatalf("%d records", len(result.Records))
	}
}

func TestAdapterLogRecordsPassHarnessValidation(t *testing.T) {
	t.Parallel()
	q := testQuery("logs")
	q.Notable = true
	at := q.Start.Add(time.Minute).UnixNano()
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"%s","deployment_environment_name":"%s","severity_text":"WARN"},"values":[["%d","reserve failed"]]}]}}`, q.Service, q.Environment, at)
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil || len(result.Records) != 1 {
		t.Fatalf("%d records: %v", len(result.Records), err)
	}
	// The harness validates every record it receives, so a field added here must pass there.
	if err := telemetry.ValidateRecord(result.Records[0], result.Query, telemetry.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
}
