package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestRunHelpAndUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "help", args: []string{"-help"}, want: 0},
		{name: "missing mode", args: []string{}, want: 2},
		{name: "unknown flag", args: []string{"-url", "http://example.com"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errout bytes.Buffer
			if code := run(t.Context(), tt.args, &out, &errout); code != tt.want {
				t.Fatalf("exit=%d stderr=%s", code, errout.String())
			}
		})
	}
}

func TestRunReplayEndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	q, err := telemetry.DefaultPolicy().Normalize(telemetry.Query{Kind: "logs", Service: "order-service", Environment: "experiment", Start: start, End: start.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	entry := telemetry.NewResult(q, "loki", "synthetic")
	entry.Quality.QueryOutcome = "SUCCEEDED"
	entry.Records = []telemetry.Record{{Type: "log", At: start, Service: q.Service, Environment: q.Environment,
		Fields: map[string]string{"event_name": "inventory.reserved", "outcome": "rejected"}}}
	entry.Finish()
	bundle, err := telemetry.NewBundle("synthetic", "unknown", []telemetry.Result{entry})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "fixture.json")
	if err = telemetry.WriteBundle(source, bundle); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]telemetry.Query{q})
	if err != nil {
		t.Fatal(err)
	}
	queryPath := filepath.Join(dir, "queries.json")
	if err = os.WriteFile(queryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output.json")
	args := []string{"-mode", "replay", "-fixture", source, "-input", queryPath, "-out", output}
	var out, errout bytes.Buffer
	if code := run(t.Context(), args, &out, &errout); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errout.String())
	}
	var summary struct {
		Requests int `json:"backend_requests"`
		Records  int `json:"records"`
	}
	if err = json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 0 || summary.Records != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if code := run(t.Context(), args, &out, &errout); code != 1 {
		t.Fatal("existing capture was overwritten")
	}
}
