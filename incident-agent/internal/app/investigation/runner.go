package investigation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	defaultRunTimeout      = 180 * time.Second
	defaultDecisionTimeout = 30 * time.Second
)

// maxModelRetries is how many failed model calls one run may try again.
const maxModelRetries = 2

// maxDecisions bounds one run. The last reportDecisions are kept for the report, so a failed
// report call can be tried again instead of losing the evidence the run already paid for.
const (
	maxDecisions    = 12
	reportDecisions = 1 + maxModelRetries
)

// lastInvestigationStep is the last step that may still run a query.
const lastInvestigationStep = maxDecisions - reportDecisions

// modelRetries is derived from the steps, so a resumed run keeps the count.
func modelRetries(steps []Step) int {
	count := 0
	for _, step := range steps {
		if step.Outcome == "MODEL_RETRY" {
			count++
		}
	}
	return count
}

// failureDetail labels a failed model call with a fixed code.
func failureDetail(decideErr, callErr error) string {
	if callErr != nil {
		return "TIMEOUT"
	}
	var failure *ProviderError
	if errors.As(decideErr, &failure) {
		return failure.Code
	}
	return "PROVIDER"
}

// Execute runs sequentially with fixed limits; a failed model call is retried at most maxModelRetries times.
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
	runTimeout := defaultRunTimeout
	if tuned, ok := decider.(interface{ RunTimeout() time.Duration }); ok {
		runTimeout = tuned.RunTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
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
	rejected := cp.Rejected
	persist := func(step int) error {
		if save == nil {
			return nil
		}
		run.BackendRequests = budget.Used()
		snapshot := Checkpoint{Schema: 1, Policy: PolicyVersion, Run: run, NextStep: step, Phase: phase, Feedback: feedback, StopReason: stopReason, Repeated: repeated, Seen: seen, NoProgress: noProgress, ReportRejections: reportRejections, EvidenceBytes: evidenceBytes, Rejected: rejected}
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
	for step := cp.NextStep; step <= maxDecisions; step++ {
		if ctx.Err() != nil {
			return finish(fallback(), "DEADLINE")
		}
		if step > lastInvestigationStep && phase != "SYNTHESIZING" {
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
		decisionTimeout := defaultDecisionTimeout
		if tuned, ok := decider.(interface{ DecisionTimeout() time.Duration }); ok {
			decisionTimeout = tuned.DecisionTimeout()
		}
		decisionCtx, stop := context.WithTimeout(ctx, decisionTimeout)
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
				audit.Detail = failureDetail(decideErr, callErr)
				// Evidence already paid for should not be lost to one failed call. The retry
				// reserves its own budget and takes a decision slot, so the limits still hold.
				retryable := !errors.Is(decideErr, ErrModelOutcomeUnknown)
				if retryable && ctx.Err() == nil && step < 12 && modelRetries(run.Steps) < maxModelRetries {
					audit.Outcome = "MODEL_RETRY"
					run.Steps = append(run.Steps, audit)
					continue
				}
				stopReason = "DECIDER_UNAVAILABLE"
			}
			audit.Outcome = stopReason
			run.Steps = append(run.Steps, audit)
			return finish(salvage(rejected, run.Evidence), stopReason)
		}
		decision, err := decodeDecision(raw)
		// A report written before any query has seen nothing, so ask again rather than
		// moving to synthesis, where it could only restate that nothing was checked.
		if err == nil && decision.Kind != "query" && len(run.Evidence) == 0 && phase != "SYNTHESIZING" {
			err = ErrInvalidDecision
		}
		if err == nil && decision.Kind == synthesizeKind && phase != "SYNTHESIZING" {
			audit.Outcome, feedback = "SYNTHESIS_REQUESTED", "SYNTHESIS_REQUESTED"
			run.Steps = append(run.Steps, audit)
			phase, stopReason = "SYNTHESIZING", "INVESTIGATION_COMPLETE"
			continue
		}
		if err == nil && decision.Kind == synthesizeKind {
			err = ErrInvalidDecision // Already writing the report; asking again is not an answer.
		}
		if err != nil && phase == "SYNTHESIZING" {
			// One malformed answer must not discard the evidence: repair like an invalid report.
			reportRejections++
			if loose := looseReport(raw); loose != nil {
				rejected = loose
			}
			audit.Outcome, feedback = "DECISION_REJECTED", "DECISION_REJECTED"
			audit.Detail = decisionRejection(raw)
			run.Steps = append(run.Steps, audit)
			if reportRejections >= maxReportRejections || step == 12 {
				return finish(salvage(rejected, run.Evidence), "REPORT_INVALID")
			}
			continue
		} else if err != nil {
			audit.Outcome, feedback = "DECISION_REJECTED", "DECISION_REJECTED"
			noProgress++
		} else if decision.Kind == "report" {
			groundTimeline(decision.Report, run.Evidence)
			if err = ValidateReport(*decision.Report, run.Evidence); err == nil {
				audit.Outcome = "REPORT_ACCEPTED"
				run.Steps = append(run.Steps, audit)
				return finish(*decision.Report, stopReason)
			}
			reportRejections++
			rejected = decision.Report
			audit.Outcome, feedback = reportRejected, reportRejected
			var rejection reportRejection
			if errors.As(err, &rejection) {
				audit.Detail, feedback = rejection.reason, reportRejected+":"+rejection.reason
			}
			run.Steps = append(run.Steps, audit)
			if reportRejections >= maxReportRejections || step == 12 {
				return finish(salvage(rejected, run.Evidence), "REPORT_INVALID")
			}
			phase, stopReason = "SYNTHESIZING", "REPORT_REPAIRED"
			continue
		} else if phase == "SYNTHESIZING" {
			reportRejections++
			audit.Outcome, feedback = "QUERY_NOT_ALLOWED", "QUERY_NOT_ALLOWED"
			run.Steps = append(run.Steps, audit)
			if reportRejections >= maxReportRejections || step == 12 {
				return finish(salvage(rejected, run.Evidence), stopReason)
			}
			continue
		} else {
			q, queryErr := policy.Normalize(withRequestScope(*decision.Query, request))
			within := !q.Start.Before(request.Start) && !q.End.After(request.End)
			canonical, _ := json.Marshal(q)
			key := string(canonical)
			switch {
			case queryErr != nil || !within:
				audit.Outcome, audit.Detail = "QUERY_REJECTED", queryRejection(*decision.Query, request, queryErr == nil && !within)
				noProgress++
			case succeededBefore(run.Evidence, q):
				// The answer is already frozen; asking again only spends budget.
				audit.Outcome = "QUERY_REPEATED"
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
				// A first look at a service and kind is progress even when empty: quiet
				// services are common in short incidents and say nothing about the next one.
				if added == 0 && !firstLook(run.Evidence[:len(run.Evidence)-1], q) {
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

// maxReportRejections leaves two chances to repair a report. Real models get report
// shape wrong often enough that one failure should not cost the whole investigation.
const maxReportRejections = 3

// groundTimeline takes each timeline time from the evidence instead of trusting the model's
// copy of it. A slip of a few milliseconds used to discard a whole sound report. Citations
// are still validated afterwards, so an item citing no real record is refused.
func groundTimeline(draft *Draft, evidence []telemetry.Result) {
	times := map[string]time.Time{}
	for _, entry := range evidence {
		if entry.Quality.QueryOutcome != "SUCCEEDED" {
			continue
		}
		for _, record := range entry.Records {
			times[record.ID] = record.At
		}
	}
	for i := range draft.Timeline {
		item := &draft.Timeline[i]
		matches := false
		var first time.Time
		for _, id := range item.Claim.EvidenceIDs {
			at, ok := times[id]
			if !ok {
				continue
			}
			if first.IsZero() {
				first = at
			}
			matches = matches || at.Equal(item.At)
		}
		if !matches && !first.IsZero() {
			item.At = first
		}
	}
	slices.SortStableFunc(draft.Timeline, func(a, b TimelineItem) int { return a.At.Compare(b.At) })
}

// withRequestScope fills what a run fixes anyway. Scope checks still apply afterwards, and
// a model that names another environment or a wider window is still refused.
func withRequestScope(q telemetry.Query, request Request) telemetry.Query {
	if q.Environment == "" {
		q.Environment = request.Environment
	}
	if q.Start.IsZero() {
		q.Start = request.Start
	}
	if q.End.IsZero() {
		q.End = request.End
	}
	// A trace spans services, so naming one would add nothing but a way to be refused.
	if q.Kind == "trace" && q.Service == "" {
		q.Service = request.Service
	}
	// Models reuse the logs filter on trace searches; it narrows nothing there.
	if q.Kind != "logs" {
		q.Notable = false
	}
	return q
}

// decisionRejection tells text that was not JSON, often a cut-off answer, from wrong fields.
func decisionRejection(raw []byte) string {
	if !json.Valid(raw) {
		return "JSON"
	}
	return "SHAPE"
}

// looseReport reads what it can from a report that strict decoding refused, so salvage can
// check its claims. Anything kept still has to pass validation one claim at a time.
func looseReport(raw []byte) *Draft {
	var parsed struct {
		Report *Draft `json:"report"`
	}
	_ = json.Unmarshal(raw, &parsed) // A wrongly typed field is skipped; the rest is still read.
	return parsed.Report
}

// queryRejection names why a query was refused with a fixed code, never with model text.
func queryRejection(q telemetry.Query, request Request, outside bool) string {
	switch {
	case outside:
		return "WINDOW"
	case !slices.Contains(request.Services, q.Service):
		return "SERVICE"
	default:
		return "SHAPE"
	}
}

func firstLook(evidence []telemetry.Result, q telemetry.Query) bool {
	for _, entry := range evidence {
		if entry.Query.Kind == q.Kind && entry.Query.Service == q.Service && entry.Query.Metric == q.Metric {
			return false
		}
	}
	return true
}

func succeededBefore(evidence []telemetry.Result, q telemetry.Query) bool {
	want, _ := json.Marshal(q)
	for _, entry := range evidence {
		if entry.Quality.QueryOutcome != "SUCCEEDED" {
			continue
		}
		if got, _ := json.Marshal(entry.Query); string(got) == string(want) {
			return true
		}
	}
	return false
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
