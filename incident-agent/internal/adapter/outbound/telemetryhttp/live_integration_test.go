//go:build integration

package telemetryhttp_test

import (
	"os"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/capture"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestLiveContract(t *testing.T) {
	path := os.Getenv("INCIDENT_INTEGRATION_QUERIES")
	if path == "" {
		t.Fatal("INCIDENT_INTEGRATION_QUERIES must name a prepared query file with a known populated window")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("cannot open integration query file")
	}
	defer file.Close()
	policy := telemetry.DefaultPolicy()
	queries, err := capture.ReadQueries(file, policy)
	if err != nil {
		t.Fatal(err)
	}
	env := func(key, fallback string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return fallback
	}
	client, err := telemetryhttp.New(telemetryhttp.Config{
		PrometheusURL: env("INCIDENT_PROMETHEUS_URL", "http://127.0.0.1:9090"),
		LokiURL:       env("INCIDENT_LOKI_URL", "http://127.0.0.1:3100"),
		JaegerURL:     env("INCIDENT_JAEGER_URL", "http://127.0.0.1:16686"), Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	results, err := capture.Collect(t.Context(), client, queries)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, result := range results {
		if len(result.Records) == 0 {
			t.Fatalf("%s: no records in supplied integration window", result.Query.Kind)
		}
		if result.Quality.QueryOutcome != "SUCCEEDED" {
			t.Fatalf("%s query failed", result.Query.Kind)
		}
		for _, record := range result.Records {
			if err = telemetry.ValidateRecord(record, result.Query, policy); err != nil {
				t.Fatalf("%s: invalid normalized evidence", result.Query.Kind)
			}
		}
		kinds[result.Query.Kind] = true
	}
	for _, kind := range []string{"metrics", "logs", "traces", "trace"} {
		if !kinds[kind] {
			t.Fatalf("query plan must include %s", kind)
		}
	}
}
