package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
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
