//go:build redisintegration

package redisstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const reservePerRun = 8 << 20

func createCommand(id string, limit int) Command {
	return Command{Action: "create", ID: id, Request: "{}", Deadline: time.Now().Add(170 * time.Second).UnixMilli(), MaxActive: limit}
}

func activeRuns(t *testing.T, r *redisFixture, namespace string) int {
	t.Helper()
	total := 0
	for _, index := range []string{"ready", "leases"} {
		count, err := strconv.Atoi(r.command(t, "ZCARD", namespace+":"+index))
		if err != nil {
			t.Fatal(err)
		}
		total += count
	}
	return total
}

func TestAdmissionUnderConcurrentCreate(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "admit")
	const attempts, limit = 16, 4
	codes := make([]string, attempts)
	var group sync.WaitGroup
	for i := range attempts {
		group.Go(func() {
			reply, err := c.Execute(t.Context(), createCommand(fmt.Sprintf("run%d", i), limit))
			if err != nil {
				codes[i] = err.Error()
				return
			}
			codes[i] = reply.Code
		})
	}
	group.Wait()
	counts := map[string]int{}
	for _, code := range codes {
		counts[code]++
	}
	if counts["OK"] != limit || counts["OVER_CAPACITY"] != attempts-limit {
		t.Fatalf("admission raced past limit: %v", counts)
	}
	if got := activeRuns(t, r, "admit"); got != limit {
		t.Fatalf("active=%d", got)
	}
	if keys := r.command(t, "KEYS", "admit:run:*"); strings.Count(keys, "\n")+1 != limit {
		t.Fatalf("rejected create wrote state: %s", keys)
	}
}

func TestDuplicateIDNeverConsumesCapacity(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "dup")
	first := createCommand("run1", 1)
	var group sync.WaitGroup
	codes := make([]string, 8)
	for i := range codes {
		group.Go(func() {
			reply, err := c.Execute(t.Context(), first)
			if err == nil {
				codes[i] = reply.Code
			}
		})
	}
	group.Wait()
	created := 0
	for _, code := range codes {
		if code == "OK" {
			created++
		} else if code != "EXISTS" {
			t.Fatalf("code=%q", code)
		}
	}
	if created != 1 || activeRuns(t, r, "dup") != 1 {
		t.Fatalf("created=%d", created)
	}
	// A full system still identifies an existing ID instead of reporting overload.
	execute(t, c, first, "EXISTS")
	other := first
	other.Request = `{"changed":true}`
	execute(t, c, other, "CONFLICT")
	execute(t, c, createCommand("run2", 1), "OVER_CAPACITY")
}

func TestTerminalRunsReleaseCapacity(t *testing.T) {
	for _, mode := range []string{"queued-cancel", "running-cancel", "deadline", "completed"} {
		t.Run(mode, func(t *testing.T) {
			r := newRedis(t)
			c := clientFor(t, r, "slots")
			execute(t, c, createCommand("run1", 1), "OK")
			execute(t, c, createCommand("run2", 1), "OVER_CAPACITY")
			if mode == "queued-cancel" {
				execute(t, c, Command{Action: "cancel", ID: "run1"}, "OK")
				execute(t, c, createCommand("run2", 1), "OK")
				return
			}
			state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "worker"}, "OK")
			switch mode {
			case "running-cancel":
				state = execute(t, c, Command{Action: "cancel", ID: "run1"}, "OK")
				// A cancel request is not a release: the worker may still be calling out.
				execute(t, c, createCommand("run2", 1), "OVER_CAPACITY")
			case "deadline":
				r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.deadline=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "slots:run:run1")
				execute(t, c, createCommand("run2", 1), "OVER_CAPACITY")
			}
			complete := owned("complete", state)
			complete.Report = "{}"
			execute(t, c, complete, "OK")
			if activeRuns(t, r, "slots") != 0 {
				t.Fatal("terminal run kept a slot")
			}
			execute(t, c, createCommand("run2", 1), "OK")
		})
	}
}

func TestTakeoverDoesNotConsumeAnotherSlot(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "takeover")
	execute(t, c, createCommand("run1", 2), "OK")
	execute(t, c, Command{Action: "claim", ID: "run1", Owner: "old"}, "OK")
	expireLease(t, r, "takeover")
	execute(t, c, Command{Action: "claim", ID: "run1", Owner: "new"}, "OK")
	if activeRuns(t, r, "takeover") != 1 {
		t.Fatal("takeover changed active count")
	}
	execute(t, c, createCommand("run2", 2), "OK")
	execute(t, c, createCommand("run3", 2), "OVER_CAPACITY")
}

func usedMemory(t *testing.T, r *redisFixture) int {
	t.Helper()
	match := regexp.MustCompile(`used_memory:(\d+)`).FindStringSubmatch(r.command(t, "INFO", "memory"))
	if match == nil {
		t.Fatal("used_memory missing")
	}
	used, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return used
}

// New runs are refused while admitted ones can still reach a terminal state.
func TestMemoryReserveKeepsRoomForAdmittedRuns(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "memory")
	execute(t, c, createCommand("run1", 8), "OK")
	limit := usedMemory(t, r) + 2*reservePerRun - (1 << 20)
	r.command(t, "CONFIG", "SET", "maxmemory", strconv.Itoa(limit))
	execute(t, c, createCommand("run2", 8), "OVER_MEMORY")
	if r.command(t, "EXISTS", "memory:run:run2") != "0" {
		t.Fatal("refused run wrote state")
	}
	state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "worker"}, "OK")
	save := owned("checkpoint", state)
	save.Checkpoint = strings.Repeat("x", 3<<20)
	state = execute(t, c, save, "OK")
	complete := owned("complete", state)
	complete.Report = "{}"
	complete.Checkpoint = strings.Repeat("y", 3<<20)
	execute(t, c, complete, "OK")
	execute(t, c, createCommand("run2", 8), "OK")
	// Without a memory limit there is nothing to reserve against: fail closed.
	r.command(t, "CONFIG", "SET", "maxmemory", "0")
	execute(t, c, createCommand("run3", 8), "OVER_MEMORY")
	if err := c.CheckPersistence(t.Context()); err == nil {
		t.Fatal("unbounded Redis accepted")
	}
}

func staleCheckpoint(t *testing.T) string {
	t.Helper()
	// A checkpoint that claims completion must not outrank state.status.
	run := investigation.Run{ID: "run1", Status: "COMPLETED", Evidence: []telemetry.Result{{QueryID: "q1", Digest: "d1"}},
		Report: investigation.Report{RunID: "run1", StopReason: "REPORT_ACCEPTED", Draft: investigation.Draft{Result: "ROOT_CAUSE"}}}
	data, err := json.Marshal(investigation.NewCheckpoint(run))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLateCompletionNeverPublishesReport(t *testing.T) {
	for _, mode := range []string{"CANCELLED", "DEADLINE"} {
		t.Run(mode, func(t *testing.T) {
			r := newRedis(t)
			c := clientFor(t, r, "late")
			job, _, _ := workerInputs(t)
			if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
				t.Fatal(err)
			}
			state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "worker"}, "OK")
			save := owned("checkpoint", state)
			save.Checkpoint = staleCheckpoint(t)
			state = execute(t, c, save, "OK")
			if mode == "CANCELLED" {
				state = execute(t, c, Command{Action: "cancel", ID: "run1"}, "OK")
			} else {
				r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.deadline=0;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "late:run:run1")
			}
			complete := owned("complete", state)
			complete.Report = `{"run_id":"run1","stop_reason":"REPORT_ACCEPTED"}`
			complete.Checkpoint = staleCheckpoint(t)
			execute(t, c, complete, "OK")
			result, err := worker.ReadResult(t.Context(), c, "run1")
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != mode || !result.Terminal || result.Report != nil {
				t.Fatalf("published late report: %+v", result)
			}
			if len(result.Evidence) != 1 || result.Evidence[0].QueryID != "q1" {
				t.Fatal("evidence audit lost")
			}
			evidence, err := worker.ReadEvidence(t.Context(), c, "run1")
			if err != nil || evidence.Status != mode || len(evidence.Evidence) != 1 {
				t.Fatalf("evidence=%+v err=%v", evidence, err)
			}
			again, err := worker.CancelRun(t.Context(), c, "run1")
			if err != nil || again.Status != mode || again.Report != nil {
				t.Fatalf("cancel after terminal: %+v %v", again, err)
			}
		})
	}
}

func TestResultAfterRealWorkerCompletion(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "result")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	queued, err := worker.ReadResult(t.Context(), c, "run1")
	if err != nil || queued.Terminal || queued.Status != "READY" || queued.Report != nil || queued.Overdue {
		t.Fatalf("queued=%+v err=%v", queued, err)
	}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	result, err := worker.ReadResult(t.Context(), c, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "COMPLETED" || result.Report == nil || result.Report.Result != "INCONCLUSIVE" || len(result.Evidence) != 1 || result.Budget.ModelCalls != 2 || len(result.Operations) == 0 {
		t.Fatalf("result=%+v", result)
	}
	if result.RetainedUntil == nil || result.RetainedUntil.Sub(*result.FinishedAt) != 7*24*time.Hour {
		t.Fatal("retention missing")
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"checkpoint"`) || strings.Contains(string(data), `"input"`) {
		t.Fatal("projection leaked internal fields")
	}
}

func TestOverdueRunIsReportedWithoutTerminalClaim(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "overdue")
	job, _, _ := workerInputs(t)
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	r.command(t, "EVAL", `local s=cjson.decode(redis.call('GET',KEYS[1]));s.deadline=1;redis.call('SET',KEYS[1],cjson.encode(s));return 1`, "1", "overdue:run:run1")
	result, err := worker.ReadResult(t.Context(), c, "run1")
	if err != nil || !result.Overdue || result.Terminal || result.Status != "READY" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestBadStateGivesExplicitErrors(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "bad")
	if _, err := worker.ReadResult(t.Context(), c, "gone"); !errors.Is(err, worker.ErrNotFound) {
		t.Fatal(err)
	}
	job, p, q := workerInputs(t)
	for name, tc := range map[string]struct {
		value string
		code  string
		want  error
	}{
		"garbage": {"not json", "CORRUPT", worker.ErrCorrupt},
		"other":   {`{"schema":1,"id":"different"}`, "CORRUPT", worker.ErrCorrupt},
		"future":  {`{"schema":2,"id":"future"}`, "UNKNOWN_SCHEMA", worker.ErrUnknownSchema},
	} {
		t.Run(name, func(t *testing.T) {
			id := name
			r.command(t, "SET", "bad:run:"+id, tc.value)
			if _, err := worker.ReadResult(t.Context(), c, id); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
			// Create must not overwrite what it cannot read, and neither may cancel.
			execute(t, c, createCommand(id, 8), tc.code)
			execute(t, c, Command{Action: "cancel", ID: id}, tc.code)
			if r.command(t, "GET", "bad:run:"+id) != tc.value {
				t.Fatal("bad state overwritten")
			}
			w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, q)}
			err := w.Process(t.Context(), id)
			if !errors.Is(err, investigation.ErrPersistence) || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("undiagnosable worker error: %v", err)
			}
		})
	}
	// Valid state wrapper around an unreadable checkpoint must not yield a report.
	if err := worker.Enqueue(t.Context(), c, "run1", job, testMaxActive); err != nil {
		t.Fatal(err)
	}
	state := execute(t, c, Command{Action: "claim", ID: "run1", Owner: "worker"}, "OK")
	save := owned("checkpoint", state)
	save.Checkpoint = "{broken"
	state = execute(t, c, save, "OK")
	complete := owned("complete", state)
	complete.Report = `{"run_id":"run1","stop_reason":"REPORT_ACCEPTED"}`
	complete.Checkpoint = "{broken"
	execute(t, c, complete, "OK")
	if _, err := worker.ReadResult(t.Context(), c, "run1"); !errors.Is(err, worker.ErrCorrupt) {
		t.Fatalf("err=%v", err)
	}
}

func TestStaleIndexEntryReleasesCapacity(t *testing.T) {
	r := newRedis(t)
	c := clientFor(t, r, "stale")
	job, p, q := workerInputs(t)
	if err := worker.Enqueue(context.Background(), c, "run1", job, 1); err != nil {
		t.Fatal(err)
	}
	r.command(t, "DEL", "stale:run:run1")
	if err := worker.Enqueue(t.Context(), c, "run2", job, 1); !errors.Is(err, worker.ErrOverloaded) {
		t.Fatalf("err=%v", err)
	}
	w := worker.Worker{Store: c, Owner: "worker", Build: factory(p, q)}
	if err := w.Process(t.Context(), "run1"); err != nil {
		t.Fatal(err)
	}
	if activeRuns(t, r, "stale") != 0 {
		t.Fatal("stale index retained")
	}
	if err := worker.Enqueue(t.Context(), c, "run2", job, 1); err != nil {
		t.Fatal(err)
	}
}
