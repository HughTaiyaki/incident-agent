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
type durableDecider struct {
	session  *session
	provider investigation.Provider
	job      Job
	step     int
}
type durableQuerier struct {
	session *session
	querier investigation.Querier
	step    func() int
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
		call.Charged = max(call.Charged, op.Cost)
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
	id := fmt.Sprintf("model-%d", view.Step)
	s := d.session
	s.mu.Lock()
	op, exists := s.state.Operations[id]
	calls := s.audits()
	s.mu.Unlock()
	if exists {
		if op.Status != "COMMITTED" {
			return nil, investigation.ErrModelProvider
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
	if ctx.Err() != nil {
		return nil, errors.Join(investigation.ErrPersistence, ctx.Err())
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
	// Persistent accounting deliberately never refunds actual usage below reserve.
	saved.Call.Charged = max(call.Charged, call.Reserved)
	data, encodeErr := json.Marshal(saved)
	if encodeErr != nil {
		return nil, investigation.ErrPersistence
	}
	s.mu.Lock()
	commitErr := s.command(ctx, runstate.Command{Action: "commit", Operation: id, Result: string(data)})
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
	if ctx.Err() != nil {
		return telemetry.Result{}, errors.Join(investigation.ErrPersistence, ctx.Err())
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
	err = s.command(ctx, runstate.Command{Action: "commit", Operation: id, Result: string(data)})
	s.mu.Unlock()
	if err != nil {
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
