package investigation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// Execute runs sequentially with fixed limits. There is no persistence, retry of decisions or resume.
// Decider and Querier must honor context; uncooperative implementations are not forcibly terminated.
func Execute(ctx context.Context, request Request, decider Decider, querier Querier) (Run, error) {
	if _, err := ValidateRequest(request); err != nil || decider == nil || querier == nil {
		return Run{}, ErrInvalidInput
	}
	request.Services = append([]string{}, request.Services...)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Run{}, errors.New("cannot allocate run identity")
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	run := Run{ID: hex.EncodeToString(id), Request: request, Status: "RUNNING", CreatedAt: now, DeadlineAt: deadline,
		Evidence: []telemetry.Result{}, Steps: []Step{}, LogPatterns: []LogPatterns{}}
	cp := Checkpoint{Schema: 1, Policy: PolicyVersion, Run: run, NextStep: 1, Phase: "INVESTIGATING", StopReason: "REPORT_ACCEPTED", Repeated: map[string]int{}, Seen: map[string]bool{}}
	return Resume(ctx, cp, decider, querier, nil)
}

// Resume continues a trusted application checkpoint, preserving scope and limits.
// A save failure stops immediately; it must never turn into a completed report.
func Resume(ctx context.Context, cp Checkpoint, decider Decider, querier Querier, save func(context.Context, Checkpoint) error) (Run, error) {
	policy, err := ValidateRequest(cp.Run.Request)
	if err != nil || decider == nil || querier == nil || cp.Schema != 1 || cp.Policy != PolicyVersion || cp.NextStep < 1 || cp.NextStep > 13 || cp.Run.ID == "" || cp.Run.DeadlineAt.IsZero() || cp.Repeated == nil || cp.Seen == nil {
		return Run{}, ErrInvalidInput
	}
	if model, ok := decider.(interface{ BeginRun() error }); ok {
		if err := model.BeginRun(); err != nil {
			return Run{}, err
		}
	}
	run, request, deadline := cp.Run, cp.Run.Request, cp.Run.DeadlineAt
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	budget := telemetry.RestoreBudget(16, run.BackendRequests, nil)
	if durable, ok := querier.(interface {
		Budget(context.Context) *telemetry.Budget
	}); ok {
		budget = durable.Budget(ctx)
	}
	phase, feedback, stopReason := cp.Phase, cp.Feedback, cp.StopReason
	repeated, seen := cp.Repeated, cp.Seen
	noProgress, reportRejections, evidenceBytes := cp.NoProgress, cp.ReportRejections, cp.EvidenceBytes
	persist := func(step int) error {
		if save == nil {
			return nil
		}
		run.BackendRequests = budget.Used()
		snapshot := Checkpoint{Schema: 1, Policy: PolicyVersion, Run: run, NextStep: step, Phase: phase, Feedback: feedback, StopReason: stopReason, Repeated: repeated, Seen: seen, NoProgress: noProgress, ReportRejections: reportRejections, EvidenceBytes: evidenceBytes}
		if err := save(ctx, snapshot); err != nil {
			return errors.Join(ErrPersistence, err)
		}
		return nil
	}
	finish := func(d Draft, reason string) (Run, error) {
		run.Status = "COMPLETED"
		if model, ok := decider.(interface{ ModelAudit() []ModelCall }); ok {
			run.ModelCalls = model.ModelAudit()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			run.Status, reason, d = "CANCELLED", "CANCELLED", fallback()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason, d = "DEADLINE", fallback()
		}
		run.FinishedAt, run.BackendRequests = time.Now().UTC(), budget.Used()
		run.Report = Report{Draft: d, RunID: run.ID, StopReason: reason, PolicyVersion: PolicyVersion}
		return run, nil
	}
	for step := cp.NextStep; step <= 12; step++ {
		if ctx.Err() != nil {
			return finish(fallback(), "DEADLINE")
		}
		if step == 12 && phase != "SYNTHESIZING" {
			phase, stopReason = "SYNTHESIZING", "DECISION_LIMIT"
		}
		if time.Until(deadline) <= 30*time.Second && phase != "SYNTHESIZING" {
			phase, stopReason = "SYNTHESIZING", "TIME_RESERVE"
		}
		if err := persist(step); err != nil {
			return Run{}, err
		}
		// Deep copy isolates maps, slices, pointers and all frozen evidence from the decider.
		data, err := json.Marshal(View{RunID: run.ID, Request: request, Phase: phase, Step: step, Evidence: run.Evidence, Feedback: feedback, LogPatterns: run.LogPatterns})
		if err != nil {
			return Run{}, errors.New("cannot encode decision view")
		}
		var view View
		if err = json.Unmarshal(data, &view); err != nil {
			return Run{}, errors.New("cannot copy decision view")
		}
		run.Decisions++ // Invalid, timed out and failed decisions consume a slot.
		decisionCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		raw, decideErr := decider.Decide(decisionCtx, view)
		callErr := decisionCtx.Err()
		stop()
		audit := Step{Number: step, Phase: phase}
		if errors.Is(decideErr, ErrPersistence) {
			return Run{}, decideErr
		}
		if decideErr != nil || callErr != nil || ctx.Err() != nil {
			if errors.Is(decideErr, ErrSynthesisRequired) && phase != "SYNTHESIZING" && ctx.Err() == nil {
				audit.Outcome = "MODEL_INVESTIGATION_BUDGET"
				run.Steps = append(run.Steps, audit)
				phase, stopReason = "SYNTHESIZING", "MODEL_BUDGET"
				continue
			}
			if errors.Is(decideErr, ErrModelBudget) {
				stopReason = "MODEL_BUDGET"
			} else if errors.Is(decideErr, ErrModelContext) {
				stopReason = "MODEL_CONTEXT_LIMIT"
			} else {
				stopReason = "DECIDER_UNAVAILABLE"
			}
			audit.Outcome = stopReason
			run.Steps = append(run.Steps, audit)
			return finish(fallback(), stopReason)
		}
		decision, err := decodeDecision(raw)
		if err != nil {
			audit.Outcome, feedback = "DECISION_REJECTED", "DECISION_REJECTED"
			noProgress++
		} else if decision.Kind == "report" {
			if err = ValidateReport(*decision.Report, run.Evidence); err == nil {
				audit.Outcome = "REPORT_ACCEPTED"
				run.Steps = append(run.Steps, audit)
				return finish(*decision.Report, stopReason)
			}
			reportRejections++
			audit.Outcome, feedback = "REPORT_REJECTED", "REPORT_REJECTED"
			run.Steps = append(run.Steps, audit)
			if reportRejections >= 2 || step == 12 {
				return finish(fallback(), "REPORT_INVALID")
			}
			phase, stopReason = "SYNTHESIZING", "REPORT_REPAIRED"
			continue
		} else if phase == "SYNTHESIZING" {
			audit.Outcome = "QUERY_NOT_ALLOWED"
			run.Steps = append(run.Steps, audit)
			return finish(fallback(), stopReason)
		} else {
			q, queryErr := policy.Normalize(*decision.Query)
			within := !q.Start.Before(request.Start) && !q.End.After(request.End)
			canonical, _ := json.Marshal(q)
			key := string(canonical)
			switch {
			case queryErr != nil || !within:
				audit.Outcome = "QUERY_REJECTED"
				noProgress++
			case repeated[key] >= 2:
				audit.Outcome = "QUERY_REPEAT_LIMIT"
				noProgress++
			case budget.Used() >= 16:
				audit.Outcome = "BACKEND_BUDGET"
				phase, stopReason = "SYNTHESIZING", "BACKEND_BUDGET"
			default:
				repeated[key]++
				queryCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				result, queryErr := querier.Query(queryCtx, q, budget)
				if errors.Is(queryErr, ErrPersistence) {
					stop()
					return Run{}, queryErr
				}
				callErr := queryCtx.Err()
				stop()
				if ctx.Err() != nil {
					audit.Outcome = "QUERY_INTERRUPTED"
					run.Steps = append(run.Steps, audit)
					return finish(fallback(), "DEADLINE")
				}
				if callErr != nil {
					queryErr = callErr
				}
				// Invalid or missing results become sanitized failure evidence; raw errors are never saved.
				if !validResult(result, q, policy) || callErr != nil ||
					(queryErr != nil && result.Quality.QueryOutcome == "SUCCEEDED") {
					result = FailedResult(q, "QUERY_FAILED")
					queryErr = telemetry.ErrResponse
				}
				encoded, _ := json.Marshal(result)
				var patterns *LogPatterns
				patternBytes := 0
				if q.Kind == "logs" {
					summary, err := SummarizeLogs(result)
					if err != nil {
						return Run{}, errors.New("cannot summarize saved logs")
					}
					patterns = &summary
					data, _ := json.Marshal(summary)
					patternBytes = len(data)
				}
				if evidenceBytes+len(encoded)+patternBytes > MaxEvidenceBytes {
					audit.Outcome = "EVIDENCE_LIMIT"
					phase, stopReason = "SYNTHESIZING", "EVIDENCE_LIMIT"
					break
				}
				// Detach the adapter's memory before storing or sharing evidence.
				var saved telemetry.Result
				if err = json.Unmarshal(encoded, &saved); err != nil {
					return Run{}, errors.New("cannot save evidence")
				}
				evidenceBytes += len(encoded) + patternBytes
				if patterns != nil {
					run.LogPatterns = append(run.LogPatterns, *patterns)
				}
				run.Evidence = append(run.Evidence, saved)
				audit.QueryID, audit.Outcome = saved.QueryID, "QUERY_SUCCEEDED"
				if queryErr != nil {
					audit.Outcome = "QUERY_FAILED"
				}
				added := 0
				if saved.Quality.QueryOutcome == "SUCCEEDED" {
					for _, record := range saved.Records {
						// Collection time and generated IDs are not evidence of a new observation.
						record.ID, record.CollectedAt = "", time.Time{}
						identity, _ := json.Marshal(record)
						hash := telemetry.Hash(identity)
						if !seen[hash] {
							seen[hash] = true
							added++
						}
					}
				}
				if added == 0 {
					noProgress++
				} else {
					noProgress = 0
				}
			}
			feedback = audit.Outcome
		}
		run.Steps = append(run.Steps, audit)
		if audit.Phase == "SYNTHESIZING" {
			return finish(fallback(), stopReason)
		}
		if noProgress >= 3 {
			phase, stopReason = "SYNTHESIZING", "NO_PROGRESS"
		}
	}
	return finish(fallback(), "DECISION_LIMIT")
}

func validResult(result telemetry.Result, q telemetry.Query, policy telemetry.Policy) bool {
	limit := q.Limit
	if q.Kind == "metrics" {
		limit *= 240
	}
	if len(result.Records) > limit {
		return false
	}
	if result.Query != q || result.PolicyVersion != telemetry.PolicyVersion || result.CollectedAt.IsZero() {
		return false
	}
	if result.Quality.QueryOutcome != "SUCCEEDED" && result.Quality.QueryOutcome != "FAILED" {
		return false
	}
	if result.Quality.QueryOutcome == "FAILED" && len(result.Records) != 0 {
		return false
	}
	for _, record := range result.Records {
		if err := telemetry.ValidateRecord(record, q, policy); err != nil {
			return false
		}
	}
	b, err := telemetry.NewBundle("replay", "unknown", []telemetry.Result{result})
	if err != nil {
		return false
	}
	data, err := json.Marshal(b)
	if err != nil {
		return false
	}
	_, err = telemetry.ReadBundle(bytes.NewReader(data))
	return err == nil
}
