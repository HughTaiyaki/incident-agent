//go:build redisintegration

package redisstate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMaxActive = 8

type redisFixture struct {
	socket, dir string
	process     *exec.Cmd
}
type response struct {
	Code  string         `json:"code"`
	State map[string]any `json:"state"`
}

func newRedis(t *testing.T) *redisFixture {
	t.Helper()
	for _, name := range []string{"redis-server", "redis-cli"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	// macOS Unix socket paths have a small fixed limit; Go's TempDir may exceed it.
	dir, err := os.MkdirTemp("/tmp", "incident-redis-")
	if err != nil {
		t.Fatal(err)
	}
	r := &redisFixture{dir: dir, socket: filepath.Join(dir, "redis.sock")}
	t.Cleanup(func() {
		r.stop(t)
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	r.start(t)
	return r
}
func (r *redisFixture) start(t *testing.T) {
	t.Helper()
	// #nosec G204 -- Fixed test executables, isolated generated socket paths and test-owned arguments; no shell.
	r.process = exec.CommandContext(context.Background(), "redis-server", "--port", "0", "--unixsocket", r.socket, "--unixsocketperm", "700", "--dir", r.dir, "--appendonly", "yes", "--appendfsync", "everysec", "--maxmemory-policy", "noeviction", "--maxmemory", "256mb", "--save", "", "--logfile", filepath.Join(r.dir, "redis.log"))
	r.process.Env = append(os.Environ(), "LC_ALL=C")
	if err := r.process.Start(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		// #nosec G204 -- Fixed test executables, isolated generated socket paths and test-owned arguments; no shell.
		err := exec.CommandContext(ctx, "redis-cli", "-s", r.socket, "PING").Run()
		cancel()
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(r.dir, "redis.log"))
	t.Fatalf("isolated Redis did not start: %s", log)
}
func (r *redisFixture) stop(t *testing.T) {
	t.Helper()
	if r.process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G204 -- Fixed test executables, isolated generated socket paths and test-owned arguments; no shell.
	err := exec.CommandContext(ctx, "redis-cli", "-s", r.socket, "SHUTDOWN").Run()
	if err != nil {
		_ = r.process.Process.Kill()
	}
	_ = r.process.Wait()
	r.process = nil
	if err != nil {
		t.Errorf("Redis shutdown: %v", err)
	}
}
func (r *redisFixture) command(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// #nosec G204 -- Fixed test executables, isolated generated socket paths and test-owned arguments; no shell.
	out, err := exec.CommandContext(ctx, "redis-cli", append([]string{"-s", r.socket, "--raw"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("Redis command: %v %s", err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}
func (r *redisFixture) apply(t *testing.T, cmd map[string]any, want string) response {
	t.Helper()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// #nosec G204 -- Fixed test executables, isolated generated socket paths and test-owned arguments; no shell.
	process := exec.CommandContext(ctx, "redis-cli", "-s", r.socket, "--raw", "-x", "EVAL", Script, "3", "state", "ready", "leases")
	process.Stdin = strings.NewReader(string(data))
	out, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("eval: %v %s", err, out)
	}
	var got response
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("reply: %v: %.200s", err, out)
	}
	if got.Code != want {
		t.Fatalf("%v: code=%s want=%s", cmd["action"], got.Code, want)
	}
	return got
}
func create(t *testing.T, r *redisFixture) response {
	t.Helper()
	return r.apply(t, map[string]any{"action": "create", "id": "run1", "request": "{}", "deadline": time.Now().Add(170 * time.Second).UnixMilli(), "max_active": testMaxActive}, "OK")
}
func action(name string, s map[string]any) map[string]any {
	return map[string]any{"action": name, "id": "run1", "input": "{}", "owner": s["owner"], "epoch": s["epoch"], "version": s["version"]}
}
func TestLifecycleAndLostAcknowledgment(t *testing.T) {
	r := newRedis(t)
	created := create(t, r)
	r.apply(t, map[string]any{"action": "create", "id": "run1", "request": "{}", "deadline": created.State["deadline"]}, "EXISTS")
	if r.command(t, "PTTL", "state") != "-1" {
		t.Fatal("active state has TTL")
	}
	claimed := r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "worker1"}, "OK")
	r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "worker2"}, "BUSY")
	renewed := r.apply(t, action("renew", claimed.State), "OK")
	if renewed.State["version"] != claimed.State["version"] {
		t.Fatal("renew changed version")
	}
	begin := action("begin", claimed.State)
	begin["operation"] = "model1"
	begin["kind"] = "model"
	begin["cost"] = 1000
	started := r.apply(t, begin, "OK")
	r.apply(t, begin, "ALREADY")
	commit := action("commit", started.State)
	commit["operation"] = "model1"
	commit["result"] = `{"decision":"saved"}`
	committed := r.apply(t, commit, "OK")
	again := r.apply(t, commit, "ALREADY")
	if again.State["tokens"] != float64(1000) || again.State["version"] != committed.State["version"] {
		t.Fatal("duplicate committed twice")
	}
	commit["result"] = "different"
	r.apply(t, commit, "CONFLICT")
	complete := action("complete", committed.State)
	complete["report"] = `{"result":"INCONCLUSIVE"}`
	done := r.apply(t, complete, "OK")
	if done.State["status"] != "COMPLETED" {
		t.Fatal(done.State)
	}
	r.apply(t, complete, "ALREADY")
	if r.command(t, "ZCARD", "ready") != "0" || r.command(t, "ZCARD", "leases") != "0" {
		t.Fatal("terminal index leak")
	}
	if r.command(t, "PTTL", "state") == "-1" {
		t.Fatal("terminal retention missing")
	}
	r.stop(t)
	r.start(t)
	restored := r.apply(t, map[string]any{"action": "read", "id": "run1"}, "OK")
	if restored.State["report"] != done.State["report"] {
		t.Fatal("AOF report lost")
	}
}
func TestTakeoverPreservesBudgetAndFencesOldWorker(t *testing.T) {
	r := newRedis(t)
	create(t, r)
	first := r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "old"}, "OK")
	begin := action("begin", first.State)
	begin["operation"] = "model1"
	begin["kind"] = "model"
	begin["cost"] = 36000
	started := r.apply(t, begin, "OK")
	// Force only lease expiry to exercise takeover without a 15-second wall wait.
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.lease_until=0;redis.call('SET',KEYS[1],cjson.encode(s));redis.call('ZADD',KEYS[2],0,s.id);return 1`, "2", "state", "leases")
	next := r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "new"}, "OK")
	if next.State["deadline"] != started.State["deadline"] || next.State["tokens"] != float64(36000) {
		t.Fatal("takeover reset limits")
	}
	operations := next.State["operations"].(map[string]any)
	if operations["model1"].(map[string]any)["status"] != "UNKNOWN" {
		t.Fatal("unknown call lost")
	}
	r.apply(t, action("renew", started.State), "FENCED")
	commit := action("commit", started.State)
	commit["operation"] = "model1"
	commit["result"] = "late"
	r.apply(t, commit, "FENCED")
	more := action("begin", next.State)
	more["operation"] = "model2"
	more["kind"] = "model"
	more["cost"] = 1
	r.apply(t, more, "BUDGET")
}
func TestCancellationDeadlineAndWrongType(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "wrongtype"} {
		t.Run(mode, func(t *testing.T) {
			r := newRedis(t)
			create(t, r)
			claimed := r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "worker"}, "OK")
			begin := action("begin", claimed.State)
			begin["operation"] = "query1"
			begin["kind"] = "query"
			begin["cost"] = 1
			switch mode {
			case "cancel":
				cancelled := r.apply(t, map[string]any{"action": "cancel", "id": "run1"}, "OK")
				begin["version"] = cancelled.State["version"]
				r.apply(t, begin, "CANCELLED")
				end := action("complete", cancelled.State)
				end["report"] = "must not publish"
				done := r.apply(t, end, "OK")
				if done.State["status"] != "CANCELLED" || done.State["report"] != "" {
					t.Fatal(done.State)
				}
			case "deadline":
				r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.deadline=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "state")
				r.apply(t, begin, "DEADLINE")
				end := action("complete", claimed.State)
				end["report"] = "must not publish"
				done := r.apply(t, end, "OK")
				if done.State["status"] != "DEADLINE" || done.State["report"] != "" {
					t.Fatal(done.State)
				}
			case "wrongtype":
				before := r.command(t, "GET", "state")
				r.command(t, "DEL", "leases")
				r.command(t, "SET", "leases", "bad")
				r.apply(t, begin, "WRONGTYPE")
				if r.command(t, "GET", "state") != before {
					t.Fatal("partial mutation")
				}
			}
		})
	}
}

func TestLimitsAndInvalidCommandsDoNotWrite(t *testing.T) {
	r := newRedis(t)
	create(t, r)
	state := r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "worker"}, "OK").State
	for i := 0; i < 16; i++ {
		begin := action("begin", state)
		begin["operation"] = strings.Repeat("q", i+1)
		begin["kind"] = "query"
		begin["cost"] = 1
		state = r.apply(t, begin, "OK").State
	}
	before := r.command(t, "GET", "state")
	begin := action("begin", state)
	begin["operation"] = "extra"
	begin["kind"] = "query"
	begin["cost"] = 1
	r.apply(t, begin, "BUDGET")
	begin["cost"] = -1
	r.apply(t, begin, "INVALID")
	begin["cost"] = 1
	begin["version"] = 0
	r.apply(t, begin, "CONFLICT")
	begin["operation"] = "q"
	begin["cost"] = 2
	r.apply(t, begin, "CONFLICT")
	if r.command(t, "GET", "state") != before {
		t.Fatal("rejection changed state")
	}
	commit := action("commit", state)
	commit["operation"] = "q"
	commit["result"] = strings.Repeat("x", 2097153)
	r.apply(t, commit, "CAPACITY")
	if r.command(t, "GET", "state") != before {
		t.Fatal("capacity rejection changed state")
	}
	commit["result"] = strings.Repeat("x", 2097152)
	state = r.apply(t, commit, "OK").State
	commit = action("commit", state)
	commit["operation"] = "qq"
	commit["result"] = "x"
	r.apply(t, commit, "CAPACITY")
}

func TestQueuedCancellationAndInvalidInput(t *testing.T) {
	r := newRedis(t)
	r.apply(t, map[string]any{"action": "create", "id": "run1", "request": "{}", "deadline": 0}, "INVALID")
	if r.command(t, "EXISTS", "state") != "0" {
		t.Fatal("invalid create wrote state")
	}
	create(t, r)
	cancelled := r.apply(t, map[string]any{"action": "cancel", "id": "run1"}, "OK")
	if cancelled.State["status"] != "CANCELLED" {
		t.Fatal(cancelled.State)
	}
	r.apply(t, map[string]any{"action": "claim", "id": "run1", "owner": "worker"}, "TERMINAL")
	if r.command(t, "ZCARD", "ready") != "0" {
		t.Fatal("cancelled run remained ready")
	}
}
