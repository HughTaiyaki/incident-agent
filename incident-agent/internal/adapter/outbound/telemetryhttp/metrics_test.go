package telemetryhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestMetricsPreservesUndefinedAndRejectsForeignSeries(t *testing.T) {
	t.Parallel()
	q := testQuery("metrics")
	q.Metric = "db_connection_wait_rate"
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		expression := r.URL.Query().Get("query")
		if !strings.Contains(expression, "rate(trade_db_acquire_timeout_total{") || !strings.Contains(expression, `environment="experiment"`) {
			t.Errorf("wrong template: %s", expression)
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[
   {"metric":{"service":"order-service","environment":"experiment","pool":"default","secret":"do-not-export"},"values":[[%d,"NaN"],[%d,"2.5"]]},
   {"metric":{"service":"admin-service","environment":"experiment"},"values":[[%d,"999"]]}
  ]}}`, q.Start.Unix(), q.Start.Unix()+15, q.Start.Unix())
	})
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 || result.Records[0].Value != nil || result.Records[1].Value == nil || *result.Records[1].Value != 2.5 {
		t.Fatalf("wrong values: %+v", result.Records)
	}
	if result.Status != "PARTIAL" || result.Quality.Freshness != "UNKNOWN" {
		t.Fatal("missing quality limitation")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "do-not-export") {
		t.Fatal("unknown label leaked")
	}
}

func TestMetricsEmptyIsNotZero(t *testing.T) {
	t.Parallel()
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	q := testQuery("metrics")
	q.Metric = "db_pool_utilization"
	result, err := client.Query(t.Context(), q, telemetry.NewBudget(16))
	if err != nil || result.Status != "EMPTY" || len(result.Records) != 0 || result.Quality.Coverage != "UNKNOWN" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	expression, _ := telemetry.MetricExpression(q)
	if !strings.Contains(expression, "> 0") {
		t.Fatal("unlimited pool denominator was not guarded")
	}
}
