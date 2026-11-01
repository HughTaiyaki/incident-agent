// Package fixture replays bounded, normalized telemetry evidence without network access.
package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// Replay owns an immutable copy of a verified bundle and the caller's query policy.
type Replay struct {
	bundle telemetry.Bundle
	policy telemetry.Policy
}

// New verifies hashes through the same decoder used for imported files and detaches mutable data.
func New(bundle telemetry.Bundle, policy telemetry.Policy) (*Replay, error) {
	data, err := json.Marshal(bundle)
	if err != nil {
		return nil, errors.New("invalid replay bundle")
	}
	snapshot, err := telemetry.ReadBundle(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	return &Replay{bundle: snapshot, policy: telemetry.Policy{
		Services: slices.Clone(policy.Services), Environments: slices.Clone(policy.Environments),
	}}, nil
}

// Query reuses captured coverage and filters the entire matching sample, not canned answers.
// A wider window or unavailable query is a fixture miss, never a healthy empty result.
func (r *Replay) Query(ctx context.Context, input telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
	if err := ctx.Err(); err != nil {
		return telemetry.Result{}, err
	}
	q, err := r.policy.Normalize(input)
	if err != nil {
		return telemetry.Result{}, err
	}
	canonical, _ := json.Marshal(q)
	result := telemetry.NewResult(q, "fixture", string(canonical))
	found := false
	seen := map[string]bool{}
	for _, entry := range r.bundle.Entries {
		if !covers(entry.Query, q) {
			continue
		}
		found = true
		result.Source = "fixture:" + entry.Source
		if entry.Quality.QueryOutcome != "SUCCEEDED" {
			result.Missing("recorded_source_unavailable")
			continue
		}
		result.Quality.QueryOutcome = "SUCCEEDED"
		result.Quality.Reachability = "UNKNOWN"
		for _, reason := range entry.Quality.MissingReasons {
			result.Missing(reason)
		}
		result.Missing("frozen_sample_only")
		result.Quality.Truncated = result.Quality.Truncated || entry.Quality.Truncated
		for _, record := range entry.Records {
			if !r.matches(record, q) || seen[record.ID] {
				continue
			}
			seen[record.ID] = true
			// JSON copy ensures a caller cannot mutate later replays through shared maps or slices.
			data, _ := json.Marshal(record)
			var detached telemetry.Record
			if json.Unmarshal(data, &detached) != nil {
				return telemetry.Result{}, telemetry.ErrResponse
			}
			result.Records = append(result.Records, detached)
		}
	}
	if !found {
		result.ErrorCode = "FIXTURE_QUERY_NOT_COVERED"
		result.Finish()
		return result, telemetry.ErrUnavailable
	}
	limit := q.Limit
	if q.Kind == "metrics" {
		limit = q.Limit * 240
	}
	if len(result.Records) > limit {
		result.Records = result.Records[:limit]
		result.Quality.Truncated = true
		result.Missing("record_limit_reached")
	}
	result.Finish()
	if result.Quality.QueryOutcome != "SUCCEEDED" {
		return result, telemetry.ErrUnavailable
	}
	return result, nil
}

func covers(captured, q telemetry.Query) bool {
	sameScope := captured.Kind == q.Kind && captured.Service == q.Service && captured.Environment == q.Environment
	withinWindow := !q.Start.Before(captured.Start) && !q.End.After(captured.End)
	if !sameScope || !withinWindow {
		return false
	}
	if captured.Metric != q.Metric || captured.StepSeconds != q.StepSeconds {
		return false
	}
	// A filtered sample cannot answer a query that broadens or changes that filter.
	pairs := [][2]string{
		{captured.TraceID, q.TraceID}, {captured.OrderID, q.OrderID}, {captured.OperationID, q.OperationID},
		{captured.EventID, q.EventID}, {captured.EventName, q.EventName}, {captured.Severity, q.Severity},
		{captured.Peer, q.Peer}, {captured.Pool, q.Pool}, {captured.WorkflowStep, q.WorkflowStep},
	}
	for _, pair := range pairs {
		if pair[0] != "" && pair[0] != pair[1] {
			return false
		}
	}
	// Aggregated metric results cannot be re-filtered by dimensions removed by their template.
	if q.Kind == "metrics" && captured.Peer != q.Peer {
		return false
	}
	return true
}

func (r *Replay) matches(record telemetry.Record, q telemetry.Query) bool {
	if !r.policy.Allows(record.Service, record.Environment, record.At, q) {
		return false
	}
	if q.Kind != "trace" && record.Service != q.Service {
		return false
	}
	if q.TraceID != "" && record.TraceID != q.TraceID {
		return false
	}
	for key, value := range map[string]string{"order_id": q.OrderID, "operation_id": q.OperationID,
		"event_id": q.EventID, "event_name": q.EventName, "severity_text": q.Severity, "pool": q.Pool, "step": q.WorkflowStep} {
		if value != "" && record.Fields[key] != value {
			return false
		}
	}
	return true
}
