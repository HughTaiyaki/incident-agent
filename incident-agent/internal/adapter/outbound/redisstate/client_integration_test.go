//go:build redisintegration

package redisstate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func clientFor(t *testing.T, r *redisFixture, namespace string) *Client {
	t.Helper()
	c, err := New(Config{Network: "unix", Address: r.socket, Namespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}
func execute(t *testing.T, c *Client, cmd Command, want string) *State {
	t.Helper()
	reply, err := c.Execute(t.Context(), cmd)
	if err != nil || reply.Code != want {
		t.Fatalf("%s: code=%s err=%v want=%s", cmd.Action, reply.Code, err, want)
	}
	return reply.State
}
func owned(action string, s *State) Command {
	return Command{Action: action, ID: s.ID, Owner: s.Owner, Epoch: s.Epoch, Version: s.Version}
}
func TestClientLifecycleRecoveryAndIsolation(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "agent")
	execute(t, c, Command{Action: "create", ID: "run1", Request: "{}", Deadline: time.Now().Add(170 * time.Second).UnixMilli(), MaxActive: testMaxActive}, "OK")
	other := clientFor(t, r, "other")
	execute(t, other, Command{Action: "read", ID: "run1"}, "NOT_FOUND")
	state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "first"}, "OK")
	begin := owned("begin", state)
	begin.Operation = "model1"
	begin.Kind = "model"
	begin.Cost = 1000
	begin.Input = "{}"
	state = execute(t, c, begin, "OK")
	execute(t, c, begin, "ALREADY")
	commit := owned("commit", state)
	commit.Operation = "model1"
	commit.Result = `{"decision":"saved"}`
	state = execute(t, c, commit, "OK")
	execute(t, c, commit, "ALREADY")
	unknown := owned("begin", state)
	unknown.Operation = "model2"
	unknown.Kind = "model"
	unknown.Cost = 500
	unknown.Input = "{}"
	old := execute(t, c, unknown, "OK")
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.lease_until=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "agent:run:run1")
	state = execute(t, c, Command{Action: "claim", ID: "run1", Owner: "second"}, "OK")
	execute(t, c, owned("renew", old), "FENCED")
	if state.Tokens != 1500 || state.Deadline != old.Deadline || state.Operations["model1"].Result != commit.Result || state.Operations["model2"].Status != "UNKNOWN" {
		t.Fatal("snapshot was not recovered")
	}
	complete := owned("complete", state)
	complete.Report = `{"result":"INCONCLUSIVE"}`
	execute(t, c, complete, "OK")
	// Reopen both process and client to prove the client consumes persisted data.
	r.stop(t)
	r.start(t)
	restarted := clientFor(t, r, "agent")
	state = execute(t, restarted, Command{Action: "read", ID: "run1"}, "OK")
	if state.Report != complete.Report || state.Status != "COMPLETED" {
		t.Fatal("report lost after restart")
	}
}

func TestClientTimeoutIsUnconfirmedAndReadBackWorks(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "timeout")
	execute(t, c, Command{Action: "create", ID: "run1", Request: "{}", Deadline: time.Now().Add(170 * time.Second).UnixMilli(), MaxActive: testMaxActive}, "OK")
	r.command(t, "CLIENT", "PAUSE", "200", "ALL")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Execute(ctx, Command{Action: "claim", ID: "run1", Owner: "worker"})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not classified: %v", err)
	}
	// A timeout is ambiguous. Reading is safe, regardless of whether Claim won.
	state := execute(t, c, Command{Action: "read", ID: "run1"}, "OK")
	if state.Epoch > 1 {
		t.Fatal("claim repeated")
	}
}

func TestWorkerPersistenceConfigurationFailsClosed(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "config")
	if err := c.CheckPersistence(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.command(t, "CONFIG", "SET", "appendfsync", "no")
	if err := c.CheckPersistence(t.Context()); err == nil {
		t.Fatal("unsafe persistence configuration accepted")
	}
}
