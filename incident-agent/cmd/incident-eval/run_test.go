package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/evaluation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func writeSample(t *testing.T, dir string) {
	t.Helper()
	start := time.Date(2026, 10, 27, 7, 25, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	q := telemetry.Query{Kind: "logs", Service: "order-service", Environment: "experiment", Start: start, End: end, Limit: 100}
	logs := telemetry.NewResult(q, "loki", "logs")
	logs.Quality.QueryOutcome = "SUCCEEDED"
	logs.Records = []telemetry.Record{{Type: "log", At: start.Add(time.Minute), Service: "order-service", Environment: "experiment",
		Fields: map[string]string{"severity_text": "ERROR", "event_name": "order.failed"}}}
	logs.Finish()
	bundle, err := telemetry.NewBundle("live", strings.Repeat("a", 40), []telemetry.Result{logs})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = telemetry.WriteBundle(filepath.Join(dir, bundleFile), bundle); err != nil {
		t.Fatal(err)
	}
	policy := telemetry.DefaultPolicy()
	if err = writeNew(filepath.Join(dir, requestFile), evaluation.Request(policy.Services, "experiment", start, end)); err != nil {
		t.Fatal(err)
	}
	if err = writeNew(filepath.Join(dir, manifestFile), evaluation.Manifest{SampleID: filepath.Base(dir), BundleDigest: bundle.Digest}); err != nil {
		t.Fatal(err)
	}
}

func TestInvestigateRecordsEveryAttemptAndNeverOverwrites(t *testing.T) {
	report := `{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["x"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
	query := `{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"experiment","start":"2026-10-27T07:25:00Z","end":"2026-10-27T07:35:00Z"}}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		content := query
		if calls%2 == 0 {
			content = report
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, content)
	}))
	defer server.Close()
	t.Setenv("INCIDENT_MODEL_ENDPOINT", server.URL+"/chat")
	t.Setenv("INCIDENT_MODEL_NAME", "fake")
	t.Setenv("INCIDENT_MODEL_CONTEXT_WINDOW", "32000")
	dir := filepath.Join(t.TempDir(), "s01")
	writeSample(t, dir)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"investigate", "-dir", dir, "-attempts", "2"}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if lines := strings.Count(stdout.String(), "\n"); lines != 2 || !strings.Contains(stdout.String(), "s01 #2 INCONCLUSIVE") {
		t.Fatal(stdout.String())
	}
	before := calls
	stderr.Reset()
	if code := run(context.Background(), []string{"investigate", "-dir", dir, "-attempts", "1"}, &stdout, &stderr); code != 1 || calls != before {
		t.Fatalf("code=%d calls=%d: %s", code, calls, stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"sheet", "-dir", filepath.Dir(dir)}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "## s01 #1") {
		t.Fatal(code, stderr.String(), stdout.String())
	}
}

func TestDeriveMakesASeparateSample(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "s01")
	writeSample(t, source)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"derive", "-from", source, "-dir", filepath.Join(root, "s02"), "-drop", "logs"}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"derived_from":"s01"`) {
		t.Fatal(stdout.String())
	}
	if code := run(context.Background(), []string{"derive", "-from", source, "-dir", filepath.Join(root, "s02"), "-drop", "logs"}, &stdout, &stderr); code != 1 {
		t.Fatal("existing sample overwritten")
	}
}

func TestRejectsBadUsageWithoutSideEffects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{}, {"capture"}, {"nope", "-dir", "x"}, {"investigate", "-dir", "x", "extra"}} {
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Fatal(args, code)
		}
	}
	dir := filepath.Join(t.TempDir(), "s")
	if code := run(context.Background(), []string{"capture", "-dir", dir, "-start", "bad", "-end", "bad"}, &stdout, &stderr); code != 1 {
		t.Fatal(code)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("directory created for invalid input")
	}
}

func TestFixedFlowOnlyCallsTheModelForTheReportAndRecordsCalls(t *testing.T) {
	report := `{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["x"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, "secret=private")
			return
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, report)
	}))
	defer server.Close()
	t.Setenv("INCIDENT_MODEL_ENDPOINT", server.URL+"/chat")
	t.Setenv("INCIDENT_MODEL_NAME", "fake")
	t.Setenv("INCIDENT_MODEL_CONTEXT_WINDOW", "32000")
	dir := filepath.Join(t.TempDir(), "s01")
	writeSample(t, dir)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"fixed", "-dir", dir, "-attempts", "1"}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	// #nosec G304 -- Test-owned path within a temporary directory.
	data, err := os.ReadFile(filepath.Join(dir, "run-01.json"))
	if err != nil {
		t.Fatal(err)
	}
	var outcome evaluation.Outcome
	if err = json.Unmarshal(data, &outcome); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || outcome.Flow != evaluation.FlowFixed || len(outcome.Calls) != 2 || outcome.Calls[0].Failure != "HTTP_5XX" ||
		outcome.Calls[1].Phase != "SYNTHESIZING" || len(outcome.Calls[1].Shown) == 0 || strings.Contains(string(data), "private") {
		t.Fatalf("calls=%d %s", calls, data)
	}
}
