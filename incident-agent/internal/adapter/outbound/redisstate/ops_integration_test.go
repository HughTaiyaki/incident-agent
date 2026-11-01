//go:build redisintegration

package redisstate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/inbound/httpapi"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
)

func waitForStatus(t *testing.T, c *Client, id, want string) {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	for time.Now().Before(until) {
		if result, err := worker.ReadResult(t.Context(), c, id); err == nil && result.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, want)
}

// One bad run must not stop the others: corrupt state and a configuration mismatch
// are skipped, and the worker keeps serving and keeps reporting them.
func TestWorkerServesOtherRunsPastBlockedOnes(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "ops")
	job, p, q := workerInputs(t)
	mismatched := job
	mismatched.Model = "other"
	r.command(t, "SET", "ops:run:corrupt", "not json")
	r.command(t, "ZADD", "ops:ready", "0", "corrupt")
	if err := worker.Enqueue(t.Context(), c, "mismatch", mismatched, testMaxActive); err != nil {
		t.Fatal(err)
	}
	if err := worker.Enqueue(t.Context(), c, "good", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	build := func(ctx context.Context, j worker.Job) (investigation.Provider, investigation.Querier, error) {
		if j.Model != job.Model {
			return nil, nil, errors.New("worker configuration does not match frozen job")
		}
		return factory(p, q)(ctx, j)
	}
	single := worker.Worker{Store: c, Owner: "once", Build: build}
	if err := single.Process(t.Context(), "corrupt"); !errors.Is(err, worker.ErrRunBlocked) {
		t.Fatalf("one-shot processing must see the failure: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	w := worker.Worker{Store: c, Owner: "serving", Build: build}
	go func() { done <- w.Run(ctx) }()
	waitForStatus(t, c, "good", "COMPLETED")
	select {
	case err := <-done:
		t.Fatalf("worker stopped: %v", err)
	default:
	}
	stats, err := c.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Counters["runs_blocked"] < 2 || stats.Counters["runs:COMPLETED"] != 1 || stats.Counters["model_calls"] == 0 {
		t.Fatalf("%+v", stats)
	}
	// Both blocked runs keep their slots until an operator acts or the deadline closes them.
	if stats.Ready+stats.Leased != 2 {
		t.Fatalf("%+v", stats)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMetricsEndpointFollowsStore(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "metrics")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "queued", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	if err := worker.Enqueue(t.Context(), c, "done", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	w := worker.Worker{Store: c, Owner: "w", Build: factory(p, q)}
	if err := w.Process(t.Context(), "done"); err != nil {
		t.Fatal(err)
	}
	if err := c.Heartbeat(t.Context(), "w"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewRouter(&httpapi.Deps{Token: apiToken, Store: c, MaxActive: 4, Readiness: c.Readiness, Now: time.Now, Stats: c.Stats}))
	defer server.Close()
	status, body := apiCall(t, server, http.MethodGet, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	for _, want := range []string{"incident_redis_up 1", "incident_worker_alive 1", `incident_runs_active{state="ready"} 1`, `incident_runs_active{state="leased"} 0`, `incident_runs_finished_total{status="COMPLETED"} 1`, "incident_redis_memory_max_bytes 268435456"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	r.stop(t)
	status, body = apiCall(t, server, http.MethodGet, "/metrics", "")
	if status != http.StatusOK || !strings.Contains(body, "incident_redis_up 0") || strings.Contains(body, "incident_worker_alive") {
		t.Fatalf("%d %s", status, body)
	}
}

// The runbook's backup: rewrite the AOF, wait for fsync, copy the directory while Redis runs.
func TestBackupRestoresFinishedAndQueuedRuns(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "backup")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "finished", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	w := worker.Worker{Store: c, Owner: "w", Build: factory(p, q)}
	if err := w.Process(t.Context(), "finished"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Enqueue(t.Context(), c, "queued", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	original, err := worker.ReadResult(t.Context(), c, "finished")
	if err != nil {
		t.Fatal(err)
	}
	r.command(t, "BGREWRITEAOF")
	until := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.command(t, "INFO", "persistence"), "aof_rewrite_in_progress:0") {
		if time.Now().After(until) {
			t.Fatal("AOF rewrite did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	dir, err := os.MkdirTemp("/tmp", "incident-restore-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.CopyFS(filepath.Join(dir, "appendonlydir"), os.DirFS(filepath.Join(r.dir, "appendonlydir"))); err != nil {
		t.Fatal(err)
	}
	r.stop(t)
	restored := &redisFixture{dir: dir, socket: filepath.Join(dir, "redis.sock")}
	restored.start(t)
	t.Cleanup(func() { restored.stop(t) })
	rc := clientFor(t, restored, "backup")
	result, err := worker.ReadResult(t.Context(), rc, "finished")
	if err != nil || result.Status != "COMPLETED" || result.Report == nil || result.Budget != original.Budget {
		t.Fatalf("%+v %v", result, err)
	}
	// Active work resumes from the restored queue. The deadline keeps running through an
	// outage, so a queued run may legitimately close as DEADLINE instead of completing.
	worker2 := worker.Worker{Store: rc, Owner: "after-restore", Build: factory(p, q)}
	if err := worker2.Process(t.Context(), "queued"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, rc, "queued", "COMPLETED")
}

// A Redis restart stops the worker with an error for its supervisor; a replacement
// picks up what was queued. Readiness must recover without restarting the API.
func TestRedisRestartStopsWorkerThenRecovers(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "restart")
	job, p, q := workerInputs(t)
	first := worker.Worker{Store: c, Owner: "first", Build: factory(p, q)}
	done := make(chan error, 1)
	go func() { done <- first.Run(t.Context()) }()
	until := time.Now().Add(5 * time.Second)
	for len(c.Readiness(t.Context())) != 0 {
		if time.Now().After(until) {
			t.Fatal("worker never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.stop(t)
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("worker must report the outage: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker kept running without Redis")
	}
	if reasons := c.Readiness(t.Context()); len(reasons) != 1 || reasons[0] != "redis_unavailable" {
		t.Fatal(reasons)
	}
	r.start(t)
	// The first attempts may race the client's redial; callers retry an unconfirmed create.
	until = time.Now().Add(5 * time.Second)
	for {
		err := worker.Enqueue(t.Context(), c, "after", job, testMaxActive)
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	second := worker.Worker{Store: c, Owner: "second", Build: factory(p, q)}
	go func() { done <- second.Run(ctx) }()
	waitForStatus(t, c, "after", "COMPLETED")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
