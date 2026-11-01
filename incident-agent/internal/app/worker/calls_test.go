package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type blockingProvider struct{}

func (blockingProvider) Complete(ctx context.Context, _ investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	<-ctx.Done()
	return investigation.CompletionResponse{}, ctx.Err()
}

type blockingQuerier struct{}

func (blockingQuerier) Query(ctx context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
	<-ctx.Done()
	return telemetry.Result{Query: q}, ctx.Err()
}

func callRequest() investigation.Request {
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	return investigation.Request{Service: "order-service", Services: []string{"order-service"}, Environment: "local", Start: start, End: start.Add(5 * time.Minute), Summary: "s"}
}

func callSession(t *testing.T) (*session, *replyStore) {
	t.Helper()
	job, err := json.Marshal(Job{Version: jobVersion, Request: callRequest()})
	if err != nil {
		t.Fatal(err)
	}
	state := runstate.State{Schema: 1, ID: "run1", Request: string(job), Status: "RUNNING", Operations: map[string]runstate.Operation{}}
	store := &replyStore{reply: runstate.Reply{Code: "OK", State: &state}}
	return &session{store: store, state: state}, store
}

// A per-call timeout is a recorded failure. It must not be confused with the run
// ending, which would stop the worker and every other queued run with it.
func TestModelCallTimeoutIsCommittedNotPersistenceFailure(t *testing.T) {
	s, store := callSession(t)
	d := &durableDecider{session: s, provider: blockingProvider{}, job: Job{Model: "m", Window: 32000}, run: t.Context()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Decide(ctx, investigation.View{Request: callRequest(), Phase: "INVESTIGATING", Step: 1})
	if errors.Is(err, investigation.ErrPersistence) || !errors.Is(err, investigation.ErrModelProvider) {
		t.Fatalf("err=%v", err)
	}
	if store.last.Action != "commit" || store.last.Operation != "model-1" {
		t.Fatalf("failure not recorded: %+v", store.last.Action)
	}
}

func TestModelCallLeftForRecoveryWhenRunEnds(t *testing.T) {
	s, store := callSession(t)
	run, stop := context.WithCancel(t.Context())
	d := &durableDecider{session: s, provider: blockingProvider{}, job: Job{Model: "m", Window: 32000}, run: run}
	ctx, cancel := context.WithTimeout(run, 20*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(5 * time.Millisecond); stop() }()
	_, err := d.Decide(ctx, investigation.View{Request: callRequest(), Phase: "INVESTIGATING", Step: 1})
	if !errors.Is(err, investigation.ErrPersistence) || store.last.Action != "begin" {
		t.Fatalf("err=%v last=%s", err, store.last.Action)
	}
}

func callQuery() telemetry.Query {
	r := callRequest()
	return telemetry.Query{Kind: "logs", Service: r.Service, Environment: r.Environment, Start: r.Start, End: r.End, Limit: 100}
}

func TestQueryTimeoutIsCommittedNotPersistenceFailure(t *testing.T) {
	s, store := callSession(t)
	q := &durableQuerier{session: s, querier: blockingQuerier{}, step: func() int { return 1 }, run: t.Context()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	result, err := q.Query(ctx, callQuery(), telemetry.NewBudget(16))
	if errors.Is(err, investigation.ErrPersistence) || err == nil {
		t.Fatalf("err=%v", err)
	}
	if store.last.Action != "commit" || result.Source != "harness" || len(result.Records) != 0 {
		t.Fatalf("last=%s source=%s", store.last.Action, result.Source)
	}
}

func TestQueryResultRoomKeepsModelAllowance(t *testing.T) {
	s, _ := callSession(t)
	if got, want := s.queryResultRoom(), resultAllowance-13*resultPerCall; got != want {
		t.Fatalf("room=%d want=%d", got, want)
	}
	s.state.ModelCalls, s.state.ResultBytes = 12, resultAllowance-resultPerCall
	if s.queryResultRoom() != 0 {
		t.Fatal(s.queryResultRoom())
	}
}
