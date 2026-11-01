//go:build redisintegration

package redisstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/inbound/httpapi"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
)

const apiToken = "integration-token-long-enough-0123456789"

func apiServer(t *testing.T, c *Client, limit int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(httpapi.NewRouter(&httpapi.Deps{
		Token: apiToken, Store: c, MaxActive: limit, Readiness: c.Readiness, Now: time.Now,
		Template: worker.Template{Model: "test", Window: 32000, ProviderID: "test", TelemetryID: "backends", Services: []string{"gateway-h5", "order-service", "goods-service", "user-service", "payment-simulator"}},
	}))
	t.Cleanup(server.Close)
	return server
}

func apiCall(t *testing.T, server *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func submission(alert, summary string) string {
	return fmt.Sprintf(`{"alert_id":%q,"service":"order-service","environment":"local","started_at":"2026-10-28T06:01:00Z","summary":%q,"initial_window_minutes":1}`, alert, summary)
}

func TestAPIDuplicateSubmissionCreatesOneRun(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "dupapi")
	server := apiServer(t, c, 4)
	var group sync.WaitGroup
	statuses := make([]int, 8)
	bodies := make([]string, 8)
	for i := range statuses {
		group.Go(func() {
			statuses[i], bodies[i] = apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "orders failing"))
		})
	}
	group.Wait()
	created := 0
	var first worker.Submitted
	for i, status := range statuses {
		var got worker.Submitted
		if err := json.Unmarshal([]byte(bodies[i]), &got); err != nil {
			t.Fatalf("%d %s", status, bodies[i])
		}
		switch status {
		case http.StatusAccepted:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("status=%d %s", status, bodies[i])
		}
		if first.RunID != "" && got.RunID != first.RunID {
			t.Fatal("retries resolved to different runs")
		}
		first = got
	}
	if created != 1 || activeRuns(t, r, "dupapi") != 1 {
		t.Fatalf("created=%d active=%d", created, activeRuns(t, r, "dupapi"))
	}
	if status, body := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "something else")); status != http.StatusConflict || !strings.Contains(body, "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("%d %s", status, body)
	}
	if activeRuns(t, r, "dupapi") != 1 {
		t.Fatal("conflicting retry created a run")
	}
}

func TestAPIOverloadWritesNothing(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "overapi")
	server := apiServer(t, c, 1)
	if status, body := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "first")); status != http.StatusAccepted {
		t.Fatalf("%d %s", status, body)
	}
	if status, body := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-2", "second")); status != http.StatusServiceUnavailable || !strings.Contains(body, "OVERLOADED") {
		t.Fatalf("%d %s", status, body)
	}
	// A full system still recognizes a retry of what it already accepted.
	if status, _ := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "first")); status != http.StatusOK {
		t.Fatalf("retry at capacity: %d", status)
	}
	if activeRuns(t, r, "overapi") != 1 {
		t.Fatal("overload wrote state")
	}
}

func TestAPIRunLifecycle(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "lifeapi")
	server := apiServer(t, c, 4)
	status, body := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "orders failing"))
	var submitted worker.Submitted
	if status != http.StatusAccepted || json.Unmarshal([]byte(body), &submitted) != nil {
		t.Fatalf("%d %s", status, body)
	}
	path := "/v1/runs/" + submitted.RunID
	if status, body := apiCall(t, server, http.MethodGet, path+"/report", ""); status != http.StatusConflict || !strings.Contains(body, "NOT_READY") {
		t.Fatalf("%d %s", status, body)
	}
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, logsBody()) })
	decision := `{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:06:00Z","limit":100}}`
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: []string{decision}}, client)}
	if err := w.Process(t.Context(), submitted.RunID); err != nil {
		t.Fatal(err)
	}
	status, body = apiCall(t, server, http.MethodGet, path, "")
	var result worker.Result
	if status != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil || result.Status != "COMPLETED" || len(result.Evidence) != 1 {
		t.Fatalf("%d %.400s", status, body)
	}
	if strings.Contains(body, `"checkpoint"`) || strings.Contains(body, `"owner"`) {
		t.Fatal("internal state exposed")
	}
	if status, body := apiCall(t, server, http.MethodGet, path+"/report", ""); status != http.StatusOK || !strings.Contains(body, "INCONCLUSIVE") {
		t.Fatalf("%d %.300s", status, body)
	}
	status, body = apiCall(t, server, http.MethodGet, path+"/evidence/"+result.Evidence[0].QueryID, "")
	if status != http.StatusOK || !strings.Contains(body, "order.transition") {
		t.Fatalf("%d %.300s", status, body)
	}
	if status, _ := apiCall(t, server, http.MethodGet, path+"/evidence/missing", ""); status != http.StatusNotFound {
		t.Fatal(status)
	}
	// Completion won, so cancel reports the persisted outcome and changes nothing.
	status, body = apiCall(t, server, http.MethodPost, path+"/cancel", "")
	if status != http.StatusOK || !strings.Contains(body, `"status":"COMPLETED"`) || !strings.Contains(body, `"report"`) {
		t.Fatalf("%d %.300s", status, body)
	}
	// A retry after the run finished still finds it instead of starting over.
	status, body = apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "orders failing"))
	if status != http.StatusOK || !strings.Contains(body, submitted.RunID) {
		t.Fatalf("%d %s", status, body)
	}
}

func TestAPICancelledRunHasNoReport(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "cancelapi")
	server := apiServer(t, c, 4)
	_, body := apiCall(t, server, http.MethodPost, "/v1/incidents", submission("alert-1", "orders failing"))
	var submitted worker.Submitted
	if json.Unmarshal([]byte(body), &submitted) != nil {
		t.Fatal(body)
	}
	path := "/v1/runs/" + submitted.RunID
	if status, body := apiCall(t, server, http.MethodPost, path+"/cancel", ""); status != http.StatusOK || !strings.Contains(body, `"status":"CANCELLED"`) {
		t.Fatalf("%d %s", status, body)
	}
	if status, body := apiCall(t, server, http.MethodGet, path+"/report", ""); status != http.StatusConflict || !strings.Contains(body, "NO_REPORT") {
		t.Fatalf("%d %s", status, body)
	}
	if status, _ := apiCall(t, server, http.MethodGet, "/v1/runs/never-created", ""); status != http.StatusNotFound {
		t.Fatal(status)
	}
	r.command(t, "SET", "cancelapi:run:bad", "not json")
	if status, body := apiCall(t, server, http.MethodGet, "/v1/runs/bad", ""); status != http.StatusInternalServerError || !strings.Contains(body, "STATE_CORRUPT") {
		t.Fatalf("%d %s", status, body)
	}
}

func TestReadinessFollowsRedisAndWorker(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "ready")
	server := apiServer(t, c, 4)
	ready := func() (int, string) {
		t.Helper()
		response, err := server.Client().Get(server.URL + "/readyz") //nolint:noctx // Test-owned local server.
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(data)
	}
	if status, body := ready(); status != http.StatusServiceUnavailable || !strings.Contains(body, "worker_absent") {
		t.Fatalf("%d %s", status, body)
	}
	job, p, q := workerInputs(t)
	_ = job
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, q)}
	go func() { done <- w.Run(ctx) }()
	until := time.Now().Add(5 * time.Second)
	for {
		status, _ := ready()
		if status == http.StatusOK {
			break
		}
		if time.Now().After(until) {
			t.Fatal("running worker not reported ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.command(t, "CONFIG", "SET", "appendfsync", "no")
	if status, body := ready(); status != http.StatusServiceUnavailable || !strings.Contains(body, "redis_misconfigured") {
		t.Fatalf("%d %s", status, body)
	}
	r.command(t, "CONFIG", "SET", "appendfsync", "everysec")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A stopped worker stops counting once its heartbeat expires; no cleanup is needed.
	if ttl := r.command(t, "PTTL", "ready:worker:heartbeat"); ttl == "-1" || ttl == "-2" {
		t.Fatalf("heartbeat has no expiry: %s", ttl)
	}
	r.command(t, "DEL", "ready:worker:heartbeat")
	if status, body := ready(); status != http.StatusServiceUnavailable || !strings.Contains(body, "worker_absent") {
		t.Fatalf("%d %s", status, body)
	}
	r.stop(t)
	if status, body := ready(); status != http.StatusServiceUnavailable || !strings.Contains(body, "redis_unavailable") {
		t.Fatalf("%d %s", status, body)
	}
}

func TestReportedUsageSettlesReservation(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "settle")
	liveJob(t, c, "run1")
	client := liveClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, logsBody()) })
	decisions := []string{logsDecision}
	for _, end := range []string{"06:04:00", "06:03:00", "06:02:00", "06:01:30"} {
		decisions = append(decisions, fmt.Sprintf(`{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T%sZ","limit":100}}`, end))
	}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(&scriptProvider{decisions: decisions}, client)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	// Each stand-in call reports 200 tokens, far below what was reserved for it.
	if state.Tokens != state.ModelCalls*200 || state.ModelCalls < 4 {
		t.Fatalf("tokens=%d calls=%d", state.Tokens, state.ModelCalls)
	}
	var cp investigation.Checkpoint
	if json.Unmarshal([]byte(state.Checkpoint), &cp) != nil {
		t.Fatal("checkpoint")
	}
	for _, call := range cp.Run.ModelCalls {
		if call.Charged != 200 || call.Reserved <= 200 {
			t.Fatalf("audit disagrees with settlement: %+v", call)
		}
	}
}

func TestSettlementKeepsFailedCallsCharged(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "settlelua")
	execute(t, c, createCommand("run1", 8), "OK")
	state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "worker"}, "OK")
	begin := owned("begin", state)
	begin.Operation, begin.Kind, begin.Cost, begin.Input = "model1", "model", 1000, "{}"
	state = execute(t, c, begin, "OK")
	commit := owned("commit", state)
	commit.Operation, commit.Result, commit.Settled = "model1", "ok", 1001
	execute(t, c, commit, "INVALID")
	commit.Settled = -1
	execute(t, c, commit, "INVALID")
	commit.Settled = 300
	state = execute(t, c, commit, "OK")
	if state.Tokens != 300 {
		t.Fatalf("tokens=%d", state.Tokens)
	}
	// Replay of an acknowledged commit must not refund twice.
	execute(t, c, commit, "ALREADY")
	if read := execute(t, c, Command{Action: "read", ID: "run1"}, "OK"); read.Tokens != 300 {
		t.Fatalf("double refund: %d", read.Tokens)
	}
	// A call whose result never commits stays charged in full after takeover.
	begin = owned("begin", state)
	begin.Operation, begin.Kind, begin.Cost, begin.Input = "model2", "model", 500, "{}"
	execute(t, c, begin, "OK")
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.lease_until=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "settlelua:run:run1")
	next := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "new"}, "OK")
	if next.Tokens != 800 || next.Operations["model2"].Status != "UNKNOWN" {
		t.Fatalf("unknown refunded: %d", next.Tokens)
	}
	// Non-model operations cannot be settled.
	tool := owned("begin", next)
	tool.Operation, tool.Kind, tool.Cost, tool.Input = "tool1", "tool", 0, "{}"
	next = execute(t, c, tool, "OK")
	bad := owned("commit", next)
	bad.Operation, bad.Result, bad.Settled = "tool1", "x", 1
	execute(t, c, bad, "INVALID")
}
