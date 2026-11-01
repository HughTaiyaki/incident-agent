package telemetryhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestTraceScopeAndReferences(t *testing.T) {
	t.Parallel()
	q := testQuery("trace")
	q.TraceID = strings.Repeat("a", 32)
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traces/"+q.TraceID {
			t.Error("wrong trace endpoint")
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"traceID":"%s","spans":[
   {"traceID":"%s","spanID":"1111111111111111","operationName":"order.recover","startTime":%d,"duration":100,"processID":"p1","tags":[{"key":"db.statement","value":"private sql"}],"references":[{"refType":"FOLLOWS_FROM","traceID":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","spanID":"2222222222222222"},{"refType":"CHILD_OF","traceID":"%s","spanID":"3333333333333333"}]},
   {"traceID":"%s","spanID":"3333333333333333","operationName":"admin-secret","startTime":%d,"duration":10,"processID":"p2"}
  ],"processes":{"p1":{"serviceName":"order-service","tags":[{"key":"deployment.environment.name","value":"experiment"}]},"p2":{"serviceName":"admin-service","tags":[{"key":"deployment.environment.name","value":"experiment"}]}}}]}`, q.TraceID, q.TraceID, q.Start.UnixMicro(), q.TraceID, q.TraceID, q.Start.UnixMicro())
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("records=%+v", result.Records)
	}
	refs := result.Records[0].References
	if len(refs) != 2 || refs[0].Type != "FOLLOWS_FROM" || refs[0].TraceID == "" || refs[1].SpanID != "" {
		t.Fatalf("references=%+v", refs)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"private sql", "admin-secret", "3333333333333333"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if result.Status != "PARTIAL" {
		t.Fatal("scope clipping not visible")
	}
}

func TestTraceNotFoundAndMissingEnvironment(t *testing.T) {
	t.Parallel()
	q := testQuery("trace")
	q.TraceID = strings.Repeat("a", 32)
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil || result.Status != "EMPTY" || result.Quality.Coverage != "UNKNOWN" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	client = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":[{"traceID":"%s","spans":[{"traceID":"%s","spanID":"1111111111111111","startTime":%d,"processID":"p1"}],"processes":{"p1":{"serviceName":"order-service","tags":[]}}}]}`, q.TraceID, q.TraceID, q.Start.UnixMicro())
	})
	result, err = client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil || len(result.Records) != 0 || result.Status != "PARTIAL" {
		t.Fatalf("missing environment accepted: %+v %v", result, err)
	}
}
