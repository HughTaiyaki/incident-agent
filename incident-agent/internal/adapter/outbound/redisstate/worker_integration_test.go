//go:build redisintegration

package redisstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type testProvider struct{ calls int }

func (p *testProvider) Complete(_ context.Context, req investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	p.calls++
	var view struct {
		Records []telemetry.Record `json:"records"`
	}
	if err := json.Unmarshal(req.Input, &view); err != nil {
		return investigation.CompletionResponse{}, err
	}
	decision := `{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["缺少完整业务证据"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
	if len(view.Records) == 0 {
		decision = `{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:05:00Z","limit":100}}`
	}
	return investigation.CompletionResponse{Decision: json.RawMessage(decision), Usage: &investigation.ModelUsage{InputTokens: 100, OutputTokens: 100}}, nil
}

type countQuerier struct {
	inner investigation.Querier
	calls int
}

func (q *countQuerier) Query(ctx context.Context, input telemetry.Query, budget *telemetry.Budget) (telemetry.Result, error) {
	q.calls++
	return q.inner.Query(ctx, input, budget)
}

func workerInputs(t *testing.T) (worker.Job, *testProvider, *countQuerier) {
	t.Helper()
	data, err := os.ReadFile("../../../../testdata/investigation/observed-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var request investigation.Request
	if json.Unmarshal(data, &request) != nil {
		t.Fatal("request")
	}
	file, err := os.Open("../../../../testdata/telemetry/observed-20261028/bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := telemetry.ReadBundle(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := investigation.ValidateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.New(bundle, policy)
	if err != nil {
		t.Fatal(err)
	}
	return worker.Job{Request: request, Model: "test", Window: 32000, FixtureDigest: bundle.Digest, ProviderID: "test"}, &testProvider{}, &countQuerier{inner: replay}
}
func factory(p investigation.Provider, q investigation.Querier) worker.Factory {
	return func(context.Context, worker.Job) (investigation.Provider, investigation.Querier, error) {
		return p, q, nil
	}
}

type lostAckStore struct {
	runstate.Store
	action, operation string
	fired             bool
	crash             bool
}

func (s *lostAckStore) Execute(ctx context.Context, cmd runstate.Command) (runstate.Reply, error) {
	reply, err := s.Store.Execute(ctx, cmd)
	if err == nil && reply.Code == "OK" && !s.fired && cmd.Action == s.action && (s.operation == "" || cmd.Operation == s.operation) {
		s.fired = true
		if s.crash {
			os.Exit(23)
		}
		return runstate.Reply{}, errors.New("simulated lost acknowledgment")
	}
	return reply, err
}
func expireLease(t *testing.T, r *redisFixture, namespace string) {
	t.Helper()
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.lease_until=0;redis.call('SET',KEYS[1],cjson.encode(s));redis.call('ZADD',KEYS[2],0,s.id);return 1`, "2", namespace+":run:run1", namespace+":leases")
}
func TestWorkerRecoversCommittedCallsWithoutRepeatingIO(t *testing.T) {
	for _, operation := range []string{"model-1", "tool-1"} {
		t.Run(operation, func(t *testing.T) {
			r := newRedis(t)
			c := clientFor(t, r, "worker")
			job, p, q := workerInputs(t)
			if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
				t.Fatal(err)
			}
			ids, err := c.Candidates(t.Context(), 10)
			if err != nil || len(ids) != 1 {
				t.Fatalf("ready discovery: %v %v", ids, err)
			}
			fault := &lostAckStore{Store: c, action: "commit", operation: operation}
			first := worker.Worker{Store: fault, Owner: "old", Build: factory(p, q)}
			if err := first.Process(t.Context(), "run1"); !errors.Is(err, investigation.ErrPersistence) {
				t.Fatalf("lost ack should stop: %v", err)
			}
			before := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
			if before.Checkpoint == "" || before.Status != "RUNNING" {
				t.Fatal("checkpoint missing")
			}
			expireLease(t, r, "worker")
			second := worker.Worker{Store: c, Owner: "new", Build: factory(p, q)}
			if err := second.Process(t.Context(), "run1"); err != nil {
				t.Fatal(err)
			}
			after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
			var cp investigation.Checkpoint
			if json.Unmarshal([]byte(after.Checkpoint), &cp) != nil {
				t.Fatal("checkpoint decode")
			}
			if p.calls != 2 || q.calls != 1 || after.Status != "COMPLETED" || after.Deadline != before.Deadline || after.Tokens < before.Tokens || len(cp.Run.Evidence) != 1 || cp.Run.Decisions != 2 {
				t.Fatalf("unexpected recovery: model=%d query=%d state=%s decisions=%d evidence=%d", p.calls, q.calls, after.Status, cp.Run.Decisions, len(cp.Run.Evidence))
			}
			if cp.Run.Report.Result != "INCONCLUSIVE" || cp.Run.BackendRequests != 0 {
				t.Fatal("invalid report/budget")
			}
		})
	}
}
func TestWorkerUnknownReservationAndCancelledRunDoNotCallProvider(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "unknown")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	fault := &lostAckStore{Store: c, action: "begin", operation: "model-1"}
	w := worker.Worker{Store: fault, Owner: "old", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err == nil {
		t.Fatal("expected stop")
	}
	before := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	expireLease(t, r, "unknown")
	w.Store = c
	w.Owner = "new"
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if p.calls != 0 || q.calls != 0 || after.Tokens != before.Tokens || after.Tokens == 0 || after.Status != "COMPLETED" {
		t.Fatal("unknown was retried or refunded")
	}
	// Running cancellation survives absent workers and closes without factory/model use.
	c2 := clientFor(t, r, "cancelled")
	if err := worker.Enqueue(t.Context(), c2, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	execute(t, c2, Command{Action: "claim", ID: "run1", Owner: "dead"}, "OK")
	execute(t, c2, Command{Action: "cancel", ID: "run1"}, "OK")
	expireLease(t, r, "cancelled")
	w = worker.Worker{Store: c2, Owner: "new", Build: func(context.Context, worker.Job) (investigation.Provider, investigation.Querier, error) {
		t.Fatal("factory called for cancelled run")
		return nil, nil, nil
	}}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	if execute(t, c2, Command{Action: "read", ID: "run1"}, "OK").Status != "CANCELLED" {
		t.Fatal("not cancelled")
	}
}

// The subprocess exits immediately after Redis acknowledges model commit,
// leaving no Go cleanup or graceful completion path in the worker process.
func TestWorkerProcessCrashRecovery(t *testing.T) {
	if os.Getenv("INCIDENT_CRASH_HELPER") == "1" {
		c, err := New(Config{Network: "unix", Address: os.Getenv("INCIDENT_TEST_SOCKET"), Namespace: "crash"})
		if err != nil {
			t.Fatal(err)
		}
		job, p, q := workerInputs(t)
		_ = job
		fault := &lostAckStore{Store: c, action: "commit", operation: "model-1", crash: true}
		w := worker.Worker{Store: fault, Owner: "crashed", Build: factory(p, q)}
		if err := w.Process(t.Context(), "run1"); err != nil {
			t.Fatal(err)
		}
		t.Fatal("helper did not crash")
	}
	r := newRedis(t)
	c := clientFor(t, r, "crash")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- Test binary and generated local socket, no user-controlled command or shell.
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestWorkerProcessCrashRecovery$")
	child.Env = append(os.Environ(), "INCIDENT_CRASH_HELPER=1", "INCIDENT_TEST_SOCKET="+r.socket)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("helper exit: %v %.300s", err, output)
	}
	// Exercise real time-based discovery instead of editing Redis lease metadata.
	deadline := time.Now().Add(18 * time.Second)
	for time.Now().Before(deadline) {
		ids, err := c.Candidates(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	w := worker.Worker{Store: c, Owner: "replacement", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if state.Status != "COMPLETED" || p.calls != 1 || q.calls != 1 {
		t.Fatalf("crash recovery repeated model: %d %d %s", p.calls, q.calls, state.Status)
	}
}

func TestWorkerDoesNotPersistRejectedRawDecision(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "sanitize")
	job, _, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	p := badProvider{}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	state := r.command(t, "GET", "sanitize:run:run1")
	if strings.Contains(state, "secret-canary") {
		t.Fatal("rejected raw output persisted")
	}
}

type badProvider struct{}

func (badProvider) Complete(context.Context, investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	return investigation.CompletionResponse{Decision: json.RawMessage(`{"kind":"bogus","secret":"secret-canary"}`)}, nil
}

func TestWorkerCLIEndToEnd(t *testing.T) {
	r := newRedis(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(body.Messages) != 2 {
			t.Error("invalid messages")
			w.WriteHeader(400)
			return
		}
		p := &testProvider{}
		response, err := p.Complete(req.Context(), investigation.CompletionRequest{Input: json.RawMessage(body.Messages[1].Content)})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": string(response.Decision)}}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 100}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
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
	env := append(os.Environ(), "INCIDENT_REDIS_NETWORK=unix", "INCIDENT_REDIS_ADDRESS="+r.socket, "INCIDENT_REDIS_NAMESPACE=cli", "INCIDENT_REDIS_USERNAME=", "INCIDENT_REDIS_PASSWORD=", "INCIDENT_MODEL_ENDPOINT="+server.URL+"/chat", "INCIDENT_SYSTEM_PROFILE_FILE=deploy/profiles/goframeshop.json", "INCIDENT_MODEL_NAME=test", "INCIDENT_MODEL_CONTEXT_WINDOW=32000", "INCIDENT_MODEL_API_KEY=", "INCIDENT_MAX_ACTIVE_RUNS=")
	attempt := func(limit string, args ...string) ([]byte, int) {
		t.Helper()
		// #nosec G204 -- Fixed built binary and test-owned argument list; no shell.
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Dir = root
		cmd.Env = append(env, "INCIDENT_MAX_ACTIVE_RUNS="+limit)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("CLI: %v %.500s", err, out)
		}
		return out, 0
	}
	invoke := func(args ...string) []byte {
		t.Helper()
		out, code := attempt("2", args...)
		if code != 0 {
			t.Fatalf("CLI exit %d: %.500s", code, out)
		}
		return out
	}
	enqueue := func(id string) []string {
		return []string{"-mode", "enqueue", "-id", id, "-request", "testdata/investigation/observed-request.json", "-fixture", "testdata/telemetry/observed-20261028/bundle.json"}
	}
	if out, code := attempt("", enqueue("run1")...); code != 1 || !strings.Contains(string(out), "INCIDENT_MAX_ACTIVE_RUNS") {
		t.Fatalf("missing limit accepted: %d %.300s", code, out)
	}
	invoke(enqueue("run1")...)
	if calls.Load() != 0 {
		t.Fatal("enqueue called provider")
	}
	invoke("-mode", "work", "-once", "-fixture", "testdata/telemetry/observed-20261028/bundle.json")
	out := invoke("-mode", "status", "-id", "run1")
	var result worker.Result
	if json.Unmarshal(out, &result) != nil || result.Schema != worker.ResultSchema {
		t.Fatalf("invalid status: %.300s", out)
	}
	if result.Status != "COMPLETED" || result.Report == nil || calls.Load() != 2 {
		t.Fatalf("CLI completion: %s %d", result.Status, calls.Load())
	}
	if strings.Contains(string(out), `"checkpoint"`) {
		t.Fatal("status exposed internal checkpoint")
	}
	invoke("-mode", "work", "-once", "-fixture", "testdata/telemetry/observed-20261028/bundle.json")
	if calls.Load() != 2 {
		t.Fatal("terminal run called model again")
	}
	var evidence worker.Evidence
	if json.Unmarshal(invoke("-mode", "evidence", "-id", "run1"), &evidence) != nil || len(evidence.Evidence) != 1 || len(evidence.Evidence[0].Records) == 0 {
		t.Fatal("evidence entry unusable")
	}
	invoke(enqueue("run2")...)
	invoke(enqueue("run3")...)
	if out, code := attempt("2", enqueue("run4")...); code != 3 || !strings.Contains(string(out), "OVER_CAPACITY") {
		t.Fatalf("overload exit: %d %.300s", code, out)
	}
	if out, code := attempt("2", "-mode", "status", "-id", "run4"); code != 1 || !strings.Contains(string(out), "not found") {
		t.Fatalf("refused run visible: %d %.300s", code, out)
	}
	var cancelled worker.Result
	if json.Unmarshal(invoke("-mode", "cancel", "-id", "run2"), &cancelled) != nil || cancelled.Status != "CANCELLED" || cancelled.Report != nil {
		t.Fatal("cancel projection")
	}
	invoke(enqueue("run4")...)
}

type reservingQuerier struct {
	inner    investigation.Querier
	requests int
}

func (q *reservingQuerier) Query(ctx context.Context, input telemetry.Query, budget *telemetry.Budget) (telemetry.Result, error) {
	if err := budget.Reserve(); err != nil {
		return telemetry.Result{}, err
	}
	q.requests++
	return q.inner.Query(ctx, input, budget)
}
func TestWorkerLostRequestReservationPreventsBackendCall(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "requestbudget")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	querier := &reservingQuerier{inner: q}
	fault := &lostAckStore{Store: c, action: "begin", operation: "request-1"}
	w := worker.Worker{Store: fault, Owner: "old", Build: factory(p, querier)}
	if err := w.Process(t.Context(), "run1"); !errors.Is(err, investigation.ErrPersistence) {
		t.Fatal(err)
	}
	before := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if before.Requests != 1 || querier.requests != 0 || q.calls != 0 {
		t.Fatal("backend called without acknowledged reservation")
	}
	expireLease(t, r, "requestbudget")
	w = worker.Worker{Store: c, Owner: "new", Build: factory(reportProvider{}, querier)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if after.Requests != 1 || querier.requests != 0 || after.Status != "COMPLETED" {
		t.Fatal("unknown request retried/refunded")
	}
}

type reportProvider struct{}

func (reportProvider) Complete(context.Context, investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	return investigation.CompletionResponse{Decision: json.RawMessage(`{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["missing evidence"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`)}, nil
}
func TestWorkerExpiredRunDoesNotConstructProvider(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "expired")
	job, _, _ := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.deadline=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "expired:run:run1")
	w := worker.Worker{Store: c, Owner: "new", Build: func(context.Context, worker.Job) (investigation.Provider, investigation.Querier, error) {
		t.Fatal("expired factory")
		return nil, nil, nil
	}}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if after.Status != "DEADLINE" || after.ModelCalls != 0 {
		t.Fatal("deadline run called model")
	}
}

type blockingProvider struct{ started chan struct{} }

func (p blockingProvider) Complete(ctx context.Context, _ investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	close(p.started)
	<-ctx.Done()
	return investigation.CompletionResponse{}, ctx.Err()
}
func TestWorkerRenewsLeaseAndShutdownRemainsRecoverable(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "shutdown")
	job, _, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	p := blockingProvider{started: started}
	w := worker.Worker{Store: c, Owner: "old", Build: factory(p, q)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Process(ctx, "run1") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not started")
	}
	before := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	time.Sleep(5500 * time.Millisecond)
	renewed := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if renewed.LeaseUntil <= before.LeaseUntil || renewed.Version != before.Version {
		t.Fatal("renew did not preserve business version")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not stop worker")
	}
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if state.Status != "RUNNING" || state.Cancelled {
		t.Fatal("process shutdown became user cancellation")
	}
	expireLease(t, r, "shutdown")
	replacement := &testProvider{}
	w = worker.Worker{Store: c, Owner: "new", Build: factory(replacement, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if replacement.calls != 0 || after.Tokens != state.Tokens || after.Status != "COMPLETED" {
		t.Fatal("unknown external call was repeated")
	}
}

// captureProvider records what the worker sends and always answers with an INCONCLUSIVE report.
type captureProvider struct {
	calls        int
	instructions string
	input        string
}

func (p *captureProvider) Complete(_ context.Context, req investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	p.calls++
	p.instructions, p.input = req.Instructions, string(req.Input)
	decision := `{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["x"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
	return investigation.CompletionResponse{Decision: json.RawMessage(decision), Usage: &investigation.ModelUsage{InputTokens: 10, OutputTokens: 5}}, nil
}

func TestHybridJobChoosesQueriesWithoutTheModelAndSurvivesTakeover(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "hybrid")
	job, _, q := workerInputs(t)
	job.Request.LogSweep = true
	job.Flow, job.SystemFacts, job.ProfileVersion = investigation.FlowHybrid, "System facts: frozen knowledge.\n", "v1"
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	// Lose the worker after its first planned query: the next one must carry on from the evidence.
	p := &captureProvider{}
	first := worker.Worker{Store: &lostAckStore{Store: c, action: "commit", operation: "tool-2"}, Owner: "old", Build: factory(p, q)}
	if err := first.Process(t.Context(), "run1"); !errors.Is(err, investigation.ErrPersistence) {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatalf("model chose a query: %d", p.calls)
	}
	expireLease(t, r, "hybrid")
	queriesBefore := q.calls
	replacement := worker.Worker{Store: c, Owner: "new", Build: factory(p, q)}
	if err := replacement.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	after := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if after.Status != "COMPLETED" || p.calls != 1 || q.calls <= queriesBefore {
		t.Fatalf("status=%s model=%d queries=%d->%d", after.Status, p.calls, queriesBefore, q.calls)
	}
	if !strings.Contains(p.instructions, "frozen knowledge") || !strings.Contains(p.instructions, "call_relations") || !strings.Contains(p.input, `"coverage":{`) || !strings.Contains(p.input, `"metrics":{`) {
		t.Fatalf("report call: %s\n%s", p.instructions, p.input)
	}
}

func TestAgentJobKeepsItsOriginalBehaviour(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "agentflow")
	job, _, q := workerInputs(t)
	job.SystemFacts = "System facts: frozen knowledge.\n"
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	p := &captureProvider{}
	w := worker.Worker{Store: c, Owner: "w", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.instructions, "frozen knowledge") || strings.Contains(p.instructions, "call_relations") || strings.Contains(p.input, `"coverage":{`) {
		t.Fatalf("%s\n%s", p.instructions, p.input)
	}
}

func TestUnknownFlowIsRefused(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "badflow")
	job, _, _ := workerInputs(t)
	job.Flow = "other"
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); !errors.Is(err, investigation.ErrInvalidInput) {
		t.Fatal(err)
	}
}
