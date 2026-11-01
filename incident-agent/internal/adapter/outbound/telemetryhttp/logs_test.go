package telemetryhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
