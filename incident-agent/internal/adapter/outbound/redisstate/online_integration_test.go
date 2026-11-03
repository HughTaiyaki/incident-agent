//go:build redisintegration

package redisstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	realOrderID  = "realorder123"
	logsDecision = `{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:05:00Z","limit":100}}`
	doneDecision = `{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["missing evidence"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
)

// scriptProvider replays decisions, ending with a report, and keeps every prompt it was sent.
type scriptProvider struct {
	mu        sync.Mutex
	decisions []string
	inputs    []string
}

func (p *scriptProvider) Complete(_ context.Context, req investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inputs = append(p.inputs, string(req.Input))
	decision := doneDecision
	if len(p.decisions) > 0 {
		decision, p.decisions = p.decisions[0], p.decisions[1:]
	}
	return investigation.CompletionResponse{Decision: json.RawMessage(decision), Usage: &investigation.ModelUsage{InputTokens: 100, OutputTokens: 100}}, nil
}

func logsBody() string {
	at := time.Date(2026, 10, 28, 6, 1, 0, 0, time.UTC).UnixNano()
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"order-service","deployment_environment_name":"local"},"values":[["%d","private body",{"event_name":"order.transition","severity_text":"ERROR","order_id":"%s"}]]}]}}`, at, realOrderID)
}

func liveClient(t *testing.T, handler http.HandlerFunc) *telemetryhttp.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := telemetryhttp.New(telemetryhttp.Config{PrometheusURL: server.URL, LokiURL: server.URL, JaegerURL: server.URL, Policy: telemetry.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func liveJob(t *testing.T, c *Client, id string) worker.Job {
	t.Helper()
	job, _, _ := workerInputs(t)
	job.FixtureDigest, job.TelemetryID = "", "backends"
	if err := worker.Enqueue(t.Context(), c, id, job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	return job
}

func evidenceOf(t *testing.T, c *Client, id string) []telemetry.Result {
	t.Helper()
	view, err := worker.ReadEvidence(t.Context(), c, id)
	if err != nil {
		t.Fatal(err)
	}
	return view.Evidence
}

func TestLiveRetryChargesEveryRequest(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "retry")
	liveJob(t, c, "run1")
	var calls atomic.Int32
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, logsBody())
	})
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: []string{logsDecision}}, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if calls.Load() != 2 || state.Requests != 2 || state.Status != "COMPLETED" {
		t.Fatalf("calls=%d requests=%d status=%s", calls.Load(), state.Requests, state.Status)
	}
	if state.Operations["request-1"].Kind != "query" || state.Operations["request-2"].Kind != "query" {
		t.Fatalf("attempt not journaled: %v", state.Operations)
	}
	evidence := evidenceOf(t, c, "run1")
	if len(evidence) != 1 || evidence[0].Source != "loki" || evidence[0].Quality.QueryOutcome != "SUCCEEDED" || len(evidence[0].Records) != 1 {
		t.Fatalf("evidence=%+v", evidence)
	}
	result, err := worker.ReadResult(t.Context(), c, "run1")
	if err != nil || result.Budget.BackendRequests != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, op := range result.Operations {
		if op.Kind == "query" && op.Status != "CHARGED" {
			t.Fatalf("charge shown as call outcome: %+v", op)
		}
	}
}

func TestLiveBackendFailureDoesNotStopWorker(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "down")
	liveJob(t, c, "run1")
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: []string{logsDecision}}, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	evidence := evidenceOf(t, c, "run1")
	if len(evidence) != 1 || evidence[0].Status == "AVAILABLE" || evidence[0].ErrorCode == "" || len(evidence[0].Records) != 0 {
		t.Fatalf("failure not recorded: %+v", evidence)
	}
	if status := execute(t, c, Command{Action: "read", ID: "run1"}, "OK").Status; status != "COMPLETED" {
		t.Fatal(status)
	}
}

// A backend that never answers uses up the per-call timeout, which is an
// ordinary failed query rather than a reason to stop the worker for every run.
func TestLiveBackendTimeoutDoesNotStopWorker(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "hang")
	liveJob(t, c, "run1")
	release := make(chan struct{})
	client := liveClient(t, func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: []string{logsDecision}}, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatalf("timeout stopped worker: %v", err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	evidence := evidenceOf(t, c, "run1")
	if state.Status != "COMPLETED" || state.Requests < 1 || len(evidence) != 1 || evidence[0].Quality.QueryOutcome == "SUCCEEDED" {
		t.Fatalf("status=%s requests=%d evidence=%+v", state.Status, state.Requests, evidence)
	}
}

func TestLiveCommitLostDoesNotRepeatRequest(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "lost")
	liveJob(t, c, "run1")
	var calls atomic.Int32
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, logsBody())
	})
	p := &scriptProvider{decisions: []string{logsDecision}}
	first := worker.Worker{Store: &lostAckStore{Store: c, action: "commit", operation: "tool-1"}, Owner: "old", Build: factory(p, client)}
	if err := first.Process(t.Context(), "run1"); !errors.Is(err, investigation.ErrPersistence) {
		t.Fatalf("err=%v", err)
	}
	expireLease(t, r, "lost")
	second := worker.Worker{Store: c, Owner: "new", Build: factory(p, client)}
	if err := second.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if calls.Load() != 1 || state.Requests != 1 || state.Status != "COMPLETED" {
		t.Fatalf("calls=%d requests=%d status=%s", calls.Load(), state.Requests, state.Status)
	}
	if evidence := evidenceOf(t, c, "run1"); len(evidence) != 1 || len(evidence[0].Records) != 1 {
		t.Fatalf("committed result not reused: %+v", evidence)
	}
}

func TestLiveReservationLostPreventsRequest(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "reserve")
	liveJob(t, c, "run1")
	var calls atomic.Int32
	client := liveClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	p := &scriptProvider{decisions: []string{logsDecision}}
	first := worker.Worker{Store: &lostAckStore{Store: c, action: "begin", operation: "request-1"}, Owner: "old", Build: factory(p, client)}
	if err := first.Process(t.Context(), "run1"); !errors.Is(err, investigation.ErrPersistence) {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("backend called without acknowledged charge")
	}
	expireLease(t, r, "reserve")
	second := worker.Worker{Store: c, Owner: "new", Build: factory(p, client)}
	if err := second.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if calls.Load() != 0 || state.Requests != 1 || state.Status != "COMPLETED" {
		t.Fatalf("unknown request retried or refunded: calls=%d requests=%d", calls.Load(), state.Requests)
	}
}

// Process shutdown during a request leaves an unknown call, which is charged and never resent.
func TestLiveShutdownDuringRequestIsNotRepeated(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "shutdown")
	liveJob(t, c, "run1")
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	client := liveClient(t, func(_ http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-req.Context().Done()
	})
	p := &scriptProvider{decisions: []string{logsDecision}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	first := worker.Worker{Store: c, Owner: "old", Build: factory(p, client)}
	go func() { done <- first.Process(ctx, "run1") }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	expireLease(t, r, "shutdown")
	second := worker.Worker{Store: c, Owner: "new", Build: factory(p, client)}
	if err := second.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if calls.Load() != 1 || state.Requests != 1 || state.Status != "COMPLETED" {
		t.Fatalf("calls=%d requests=%d status=%s", calls.Load(), state.Requests, state.Status)
	}
	evidence := evidenceOf(t, c, "run1")
	if len(evidence) != 1 || len(evidence[0].Records) != 0 {
		t.Fatalf("unknown result invented: %+v", evidence)
	}
}

func TestLiveIdentifiersStayOutOfPrompts(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "mask")
	liveJob(t, c, "run1")
	// An explanatory log keeps its text with the ID removed; routine text is not kept at all.
	at := time.Date(2026, 10, 28, 6, 1, 0, 0, time.UTC).UnixNano()
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"order-service","deployment_environment_name":"local"},"values":[`+
		`["%d","order %s moved on",{"event_name":"order.transition","severity_text":"ERROR","order_id":"%s"}],`+
		`["%d","private body",{"severity_text":"INFO"}]]}]}}`, at, realOrderID, realOrderID, at+1)
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
	p := &scriptProvider{decisions: []string{logsDecision}}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.inputs[len(p.inputs)-1], "order [id] moved on") {
		t.Fatalf("explanatory text lost: %.400s", p.inputs[len(p.inputs)-1])
	}
	if len(p.inputs) != 2 {
		t.Fatalf("prompts=%d", len(p.inputs))
	}
	if !strings.Contains(p.inputs[1], "masked-") || !strings.Contains(p.inputs[1], "order.transition") {
		t.Fatalf("second prompt lost the record: %.300s", p.inputs[1])
	}
	for _, input := range p.inputs {
		if strings.Contains(input, realOrderID) || strings.Contains(input, "private body") || strings.Contains(input, `"run1"`) {
			t.Fatalf("prompt leaked private value: %.300s", input)
		}
	}
	// The operator's frozen evidence keeps the real value for follow-up.
	if got := evidenceOf(t, c, "run1")[0].Records[0].Fields["order_id"]; got != realOrderID {
		t.Fatalf("evidence=%s", got)
	}
}

// Every metric result is near the per-query maximum. Whatever fits is kept; the
// rest must end as recorded failures, never as a capacity error stopping the worker.
func TestLiveLargeResultsStayWithinStateLimits(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "large")
	job, _, _ := workerInputs(t)
	job.FixtureDigest, job.TelemetryID = "", "backends"
	job.Request.End = job.Request.Start.Add(59 * time.Minute)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	start := job.Request.Start.Unix()
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) {
		value := calls.Add(1)
		series := make([]string, 20)
		for s := range series {
			points := make([]string, 236)
			for i := range points {
				points[i] = fmt.Sprintf(`[%d,"%d.5"]`, start+int64(i)*15, value)
			}
			series[s] = fmt.Sprintf(`{"metric":{"service":"order-service","environment":"local","pool":"pool%d"},"values":[%s]}`, s, strings.Join(points, ","))
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(series, ","))
	})
	end := job.Request.End.Format(time.RFC3339)
	decisions := []string{}
	for _, step := range []int{15, 16, 17, 18} {
		decisions = append(decisions, fmt.Sprintf(`{"kind":"query","query":{"kind":"metrics","service":"order-service","environment":"local","start":"%s","end":"%s","metric":"db_pool_utilization","step_seconds":%d}}`, job.Request.Start.Format(time.RFC3339), end, step))
	}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: decisions}, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatalf("large result stopped worker: %v", err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if state.Status != "COMPLETED" || state.ResultBytes > 2<<20 {
		t.Fatalf("status=%s bytes=%d", state.Status, state.ResultBytes)
	}
	tooLarge := 0
	for _, entry := range evidenceOf(t, c, "run1") {
		if entry.ErrorCode == "RESULT_TOO_LARGE" {
			tooLarge++
			if entry.Source != "harness" || len(entry.Records) != 0 {
				t.Fatalf("oversized entry kept data: %+v", entry.Quality)
			}
		}
	}
	if tooLarge == 0 {
		t.Fatalf("expected an oversized result; bytes=%d calls=%d", state.ResultBytes, calls.Load())
	}
	if _, err := worker.ReadResult(t.Context(), c, "run1"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCLILiveEndToEnd(t *testing.T) {
	r := newRedis(t)
	var backendCalls, modelCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls.Add(1)
		_, _ = fmt.Fprint(w, logsBody())
	}))
	defer backend.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(body.Messages[1].Content, realOrderID) {
			t.Error("real identifier reached provider")
		}
		decision := logsDecision
		if modelCalls.Add(1) > 1 {
			decision = doneDecision
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": decision}}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 100}})
	}))
	defer model.Close()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "incident-worker")
	// #nosec G204 -- Builds the repository-owned command to a test-generated path.
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/incident-worker")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %.300s", err, out)
	}
	base := append(os.Environ(), "INCIDENT_SYSTEM_PROFILE_FILE=deploy/profiles/goframeshop.json", "INCIDENT_REDIS_NETWORK=unix", "INCIDENT_REDIS_ADDRESS="+r.socket, "INCIDENT_REDIS_NAMESPACE=livecli", "INCIDENT_REDIS_USERNAME=", "INCIDENT_REDIS_PASSWORD=", "INCIDENT_MODEL_ENDPOINT="+model.URL+"/chat", "INCIDENT_MODEL_NAME=test", "INCIDENT_MODEL_CONTEXT_WINDOW=32000", "INCIDENT_MODEL_API_KEY=", "INCIDENT_MAX_ACTIVE_RUNS=4")
	backends := []string{"INCIDENT_PROMETHEUS_URL=" + backend.URL, "INCIDENT_LOKI_URL=" + backend.URL, "INCIDENT_JAEGER_URL=" + backend.URL}
	attempt := func(env []string, args ...string) ([]byte, int) {
		t.Helper()
		// #nosec G204 -- Fixed built binary and test-owned argument list; no shell.
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Dir = root
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("CLI: %v %.300s", err, out)
		}
		return out, 0
	}
	live := append(append([]string{}, base...), backends...)
	request := "testdata/investigation/observed-request.json"
	if _, code := attempt(live, "-mode", "enqueue", "-id", "run1", "-request", request); code != 2 {
		t.Fatalf("no source accepted: %d", code)
	}
	if _, code := attempt(live, "-mode", "enqueue", "-id", "run1", "-request", request, "-live", "-fixture", "testdata/telemetry/observed-20261028/bundle.json"); code != 2 {
		t.Fatalf("two sources accepted: %d", code)
	}
	if out, code := attempt(append(append([]string{}, base...), "INCIDENT_PROMETHEUS_URL=", "INCIDENT_LOKI_URL=", "INCIDENT_JAEGER_URL="), "-mode", "enqueue", "-id", "run1", "-request", request, "-live"); code != 1 || !strings.Contains(string(out), "INCIDENT_PROMETHEUS_URL") {
		t.Fatalf("default backend used: %d %.200s", code, out)
	}
	if out, code := attempt(live, "-mode", "enqueue", "-id", "run1", "-request", request, "-live"); code != 0 {
		t.Fatalf("enqueue: %.300s", out)
	}
	if backendCalls.Load() != 0 || modelCalls.Load() != 0 {
		t.Fatal("enqueue made external calls")
	}
	// Recovery with different backends must stop instead of silently changing data.
	other := append(append([]string{}, base...), "INCIDENT_PROMETHEUS_URL=http://127.0.0.1:1", "INCIDENT_LOKI_URL="+backend.URL, "INCIDENT_JAEGER_URL="+backend.URL)
	if out, code := attempt(other, "-mode", "work", "-once", "-live"); code != 1 || !strings.Contains(string(out), "does not match") {
		t.Fatalf("changed backends accepted: %d %.300s", code, out)
	}
	if backendCalls.Load() != 0 || modelCalls.Load() != 0 {
		t.Fatal("mismatched worker made external calls")
	}
	c := clientFor(t, r, "livecli")
	expireLease(t, r, "livecli")
	if out, code := attempt(live, "-mode", "work", "-once", "-live"); code != 0 {
		t.Fatalf("work: %.300s", out)
	}
	if backendCalls.Load() != 1 || modelCalls.Load() != 2 {
		t.Fatalf("backend=%d model=%d", backendCalls.Load(), modelCalls.Load())
	}
	out, code := attempt(live, "-mode", "status", "-id", "run1")
	var result worker.Result
	if code != 0 || json.Unmarshal(out, &result) != nil || result.Status != "COMPLETED" || result.Report == nil || len(result.Evidence) != 1 || result.Evidence[0].Source != "loki" {
		t.Fatalf("status: %d %.500s", code, out)
	}
	path := filepath.Join(t.TempDir(), "run1.json")
	if out, code := attempt(live, "-mode", "export", "-id", "run1", "-out", path); code != 0 {
		t.Fatalf("export: %.300s", out)
	}
	if _, code := attempt(live, "-mode", "export", "-id", "run1", "-out", path); code != 1 {
		t.Fatal("export overwrote existing file")
	}
	// #nosec G304 -- Test-generated path.
	exported, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), realOrderID) {
		t.Fatal("export kept real identifier")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("export mode: %v %v", info, err)
	}
	bundle, err := telemetry.ReadBundle(strings.NewReader(string(exported)))
	if err != nil || bundle.Origin != "live" || len(bundle.Entries) != 1 {
		t.Fatalf("export unreadable: %+v %v", bundle.Origin, err)
	}
	if execute(t, c, Command{Action: "read", ID: "run1"}, "OK").Status != "COMPLETED" {
		t.Fatal("state changed by export")
	}
}
