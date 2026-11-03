package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type modelResult struct {
	Decision json.RawMessage         `json:"decision"`
	Code     string                  `json:"code"`
	Call     investigation.ModelCall `json:"call"`
}
type queryResult struct {
	Result telemetry.Result `json:"result"`
	Failed bool             `json:"failed"`
}

// run outlives per-call timeouts. A slow provider or backend is a recorded call
// failure; only the end of run means this process must leave the work for recovery.
type durableDecider struct {
	session  *session
	provider investigation.Provider
	job      Job
	step     int
	run      context.Context
}
type durableQuerier struct {
	session *session
	querier investigation.Querier
	step    func() int
	run     context.Context
}

// Stored results share one 2MiB allowance. Query results may not spend what later
// model decisions and failure records need, or a large response would stop the worker.
const (
	resultAllowance = 2 << 20
	resultPerCall   = 65 << 10
	maxModelCalls   = 12
)

func (s *session) queryResultRoom() int {
	return resultAllowance - s.state.ResultBytes - resultPerCall*(maxModelCalls+1-s.state.ModelCalls)
}

func (s *session) audits() []investigation.ModelCall {
	calls := []investigation.ModelCall{}
	for _, op := range s.state.Operations {
		if op.Kind != "model" {
			continue
		}
		var call investigation.ModelCall
		if json.Unmarshal([]byte(op.Input), &call) != nil {
			continue
		}
		if op.Status == "COMMITTED" {
			var saved modelResult
			if json.Unmarshal([]byte(op.Result), &saved) == nil {
				call = saved.Call
			}
		}
		if op.Status == "COMMITTED" && op.Settled > 0 {
			call.Charged = op.Settled
		} else {
			call.Charged = max(call.Charged, op.Cost)
		}
		if op.Status != "COMMITTED" {
			call.Status = "UNKNOWN"
		}
		calls = append(calls, call)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].Step < calls[j].Step })
	return calls
}
func (d *durableDecider) ModelAudit() []investigation.ModelCall {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	return d.session.audits()
}
func (d *durableDecider) Decide(ctx context.Context, view investigation.View) (json.RawMessage, error) {
	d.step = view.Step
	// A sweep query is not a model call, so it has nothing to journal or charge here; its
	// query is journaled as usual and a resumed run recomputes the same decision.
	if sweep := investigation.SweepDecision(view); sweep != nil {
		return sweep, nil
	}
	id := fmt.Sprintf("model-%d", view.Step)
	s := d.session
	s.mu.Lock()
	op, exists := s.state.Operations[id]
	calls := s.audits()
	s.mu.Unlock()
	if exists {
		if op.Status != "COMMITTED" {
			return nil, investigation.ErrModelOutcomeUnknown
		}
		var saved modelResult
		if json.Unmarshal([]byte(op.Result), &saved) != nil {
			return nil, investigation.ErrPersistence
		}
		return saved.Decision, modelError(saved.Code)
	}
	m, err := investigation.NewModelDecider(d.provider, d.job.Model, d.job.Window)
	if err != nil {
		return nil, err
	}
	reserved := false
	err = m.RestoreAccounting(calls, func(ctx context.Context, call investigation.ModelCall) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		input, err := json.Marshal(call)
		if err != nil {
			return err
		}
		if err := s.command(ctx, runstate.Command{Action: "begin", Operation: id, Kind: "model", Cost: call.Reserved, Input: string(input)}); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = m.BeginRun(); err != nil {
		return nil, err
	}
	raw, err := m.Decide(ctx, view)
	if errors.Is(err, investigation.ErrPersistence) {
		return nil, err
	}
	if !reserved {
		return nil, err
	}
	if d.run.Err() != nil {
		return nil, errors.Join(investigation.ErrPersistence, d.run.Err())
	}
	audit := m.ModelAudit()
	call := audit[len(audit)-1]
	saved := modelResult{Decision: investigation.SafeDecision(raw, view), Call: call}
	switch {
	case errors.Is(err, investigation.ErrModelBudget):
		saved.Code = "BUDGET"
	case err != nil:
		saved.Code = "UNAVAILABLE"
	}
	// Only usage the provider reported within the reservation settles it. Anything
	// less certain, including a commit that never lands, stays charged in full.
	settled := 0
	if call.Usage != nil && call.Charged <= call.Reserved {
		settled = call.Charged
	} else {
		saved.Call.Charged = max(call.Charged, call.Reserved)
	}
	data, encodeErr := json.Marshal(saved)
	if encodeErr != nil {
		return nil, investigation.ErrPersistence
	}
	s.mu.Lock()
	commitErr := s.command(d.run, runstate.Command{Action: "commit", Operation: id, Result: string(data), Settled: settled})
	s.mu.Unlock()
	if commitErr != nil {
		return nil, commitErr
	}
	return saved.Decision, modelError(saved.Code)
}
func modelError(code string) error {
	switch code {
	case "":
		return nil
	case "BUDGET":
		return investigation.ErrModelBudget
	default:
		return investigation.ErrModelProvider
	}
}
func (q *durableQuerier) Query(ctx context.Context, query telemetry.Query, budget *telemetry.Budget) (telemetry.Result, error) {
	s := q.session
	id := fmt.Sprintf("tool-%d", q.step())
	input, _ := json.Marshal(query)
	s.mu.Lock()
	op, exists := s.state.Operations[id]
	if exists {
		s.mu.Unlock()
		if op.Input != string(input) {
			return telemetry.Result{}, investigation.ErrPersistence
		}
		if op.Status != "COMMITTED" {
			return telemetry.Result{}, telemetry.ErrUnavailable
		}
		var saved queryResult
		if json.Unmarshal([]byte(op.Result), &saved) != nil {
			return telemetry.Result{}, investigation.ErrPersistence
		}
		if saved.Failed {
			return saved.Result, telemetry.ErrUnavailable
		}
		return saved.Result, nil
	}
	err := s.command(ctx, runstate.Command{Action: "begin", Operation: id, Kind: "tool", Cost: 0, Input: string(input)})
	s.mu.Unlock()
	if err != nil {
		return telemetry.Result{}, err
	}
	result, queryErr := q.querier.Query(ctx, query, budget)
	if errors.Is(queryErr, investigation.ErrPersistence) {
		return telemetry.Result{}, queryErr
	}
	if q.run.Err() != nil {
		return telemetry.Result{}, errors.Join(investigation.ErrPersistence, q.run.Err())
	}
	// Validate/sanitize before persistence, not only after the adapter returns.
	policy, err := investigation.ValidateRequest(q.request())
	if err != nil {
		return telemetry.Result{}, investigation.ErrPersistence
	}
	result = investigation.SanitizeResult(result, query, policy, queryErr)
	data, err := json.Marshal(queryResult{Result: result, Failed: queryErr != nil})
	if err != nil {
		return telemetry.Result{}, investigation.ErrPersistence
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) > s.queryResultRoom() {
		result, queryErr = investigation.FailedResult(query, "RESULT_TOO_LARGE"), telemetry.ErrTooLarge
		if data, err = json.Marshal(queryResult{Result: result, Failed: true}); err != nil {
			return telemetry.Result{}, investigation.ErrPersistence
		}
	}
	if err = s.command(q.run, runstate.Command{Action: "commit", Operation: id, Result: string(data)}); err != nil {
		return telemetry.Result{}, err
	}
	return result, queryErr
}
func (q *durableQuerier) request() investigation.Request {
	q.session.mu.Lock()
	defer q.session.mu.Unlock()
	var job Job
	_ = json.Unmarshal([]byte(q.session.state.Request), &job)
	return job.Request
}
