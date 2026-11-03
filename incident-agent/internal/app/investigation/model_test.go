package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type providerFunc func(context.Context, CompletionRequest) (CompletionResponse, error)

func (f providerFunc) Complete(ctx context.Context, r CompletionRequest) (CompletionResponse, error) {
	return f(ctx, r)
}

type countingProvider struct {
	providerFunc
	counter TokenCounter
}

func (p countingProvider) TokenCounter() TokenCounter { return p.counter }

type fixedCounter int

func (c fixedCounter) CountPrompt(string, []byte) int { return int(c) }
func (fixedCounter) Name() string                     { return "fixed-counter" }

func TestModelDeciderUsesProviderTokenCounter(t *testing.T) {
	const counted = 1000
	provider := countingProvider{counter: fixedCounter(counted), providerFunc: func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
		return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())}), Usage: &ModelUsage{InputTokens: counted, OutputTokens: r.MaxOutputTokens}}, nil
	}}
	model, err := NewModelDecider(provider, "test-model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if err = model.BeginRun(); err != nil {
		t.Fatal(err)
	}
	if _, err = model.Decide(t.Context(), View{Request: request(), Phase: "INVESTIGATING"}); err != nil {
		t.Fatal(err)
	}
	call := model.ModelAudit()[0]
	if call.Estimator != "fixed-counter" || call.Reserved != counted+countedInputMargin+call.OutputLimit || call.Status != "RECEIVED" {
		t.Fatalf("%+v", call)
	}
}

func TestModelDeciderStopsWhenCountedInputCannotFit(t *testing.T) {
	provider := countingProvider{counter: fixedCounter(ModelTokenLimit), providerFunc: func(context.Context, CompletionRequest) (CompletionResponse, error) {
		t.Fatal("provider must not be called")
		return CompletionResponse{}, nil
	}}
	model, err := NewModelDecider(provider, "test-model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if err = model.BeginRun(); err != nil {
		t.Fatal(err)
	}
	if _, err = model.Decide(t.Context(), View{Request: request(), Phase: "SYNTHESIZING"}); !errors.Is(err, ErrModelContext) {
		t.Fatal(err)
	}
}

func TestModelDecider(t *testing.T) {
	t.Run("usage settlement and detached audit", func(t *testing.T) {
		model, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
			if r.MaxOutputTokens > ReportOutputTokens || !json.Valid(r.Input) {
				t.Fatal(r)
			}
			return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())}), Usage: &ModelUsage{InputTokens: 100, OutputTokens: 50}}, nil
		}), "test-model", 8192)
		if err != nil {
			t.Fatal(err)
		}
		if err = model.BeginRun(); err != nil {
			t.Fatal(err)
		}
		_, err = model.Decide(t.Context(), View{Request: request(), Phase: "INVESTIGATING"})
		if err != nil {
			t.Fatal(err)
		}
		calls := model.ModelAudit()
		if len(calls) != 1 || calls[0].Charged != 150 || calls[0].Estimated || calls[0].Reserved <= 150 {
			t.Fatal(calls)
		}
		calls[0].Usage.InputTokens = 999
		if model.ModelAudit()[0].Usage.InputTokens != 100 {
			t.Fatal("shared audit")
		}
		if model.BeginRun() == nil {
			t.Fatal("cross-run budget reuse")
		}
	})
	for _, failure := range []string{"unavailable", "missing usage", "negative usage", "over reservation"} {
		t.Run(failure, func(t *testing.T) {
			model, err := NewModelDecider(providerFunc(func(context.Context, CompletionRequest) (CompletionResponse, error) {
				r := CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())})}
				switch failure {
				case "unavailable":
					return r, errors.New("secret=private")
				case "negative usage":
					r.Usage = &ModelUsage{InputTokens: -1, OutputTokens: 1}
				case "over reservation":
					r.Usage = &ModelUsage{InputTokens: 25000, OutputTokens: 1}
				}
				return r, nil
			}), "test-model", 8192)
			if err != nil {
				t.Fatal(err)
			}
			if err = model.BeginRun(); err != nil {
				t.Fatal(err)
			}
			_, err = model.Decide(t.Context(), View{Request: request(), Phase: "INVESTIGATING"})
			calls := model.ModelAudit()
			if len(calls) != 1 {
				t.Fatal(calls)
			}
			if failure == "missing usage" {
				if err != nil || !calls[0].Estimated || calls[0].Charged != calls[0].Reserved {
					t.Fatalf("%+v %v", calls, err)
				}
			} else if err == nil {
				t.Fatal("expected error")
			}
			if failure == "over reservation" {
				if !errors.Is(err, ErrModelBudget) || calls[0].Charged != 25001 {
					t.Fatalf("%+v %v", calls, err)
				}
				if _, err = model.Decide(t.Context(), View{Request: request(), Phase: "SYNTHESIZING"}); !errors.Is(err, ErrModelBudget) {
					t.Fatal(err)
				}
			} else if calls[0].Charged != calls[0].Reserved {
				t.Fatal("unknown usage refunded")
			}
			if strings.Contains(string(raw(calls)), "private") {
				t.Fatal("error body leaked")
			}
		})
	}
	t.Run("investigation cannot spend report reserve", func(t *testing.T) {
		var phases []string
		model, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
			var input modelInput
			if err := json.Unmarshal(r.Input, &input); err != nil {
				t.Fatal(err)
			}
			phases = append(phases, input.Phase)
			if input.Phase == "SYNTHESIZING" {
				return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())})}, nil
			}
			q := query()
			q.Limit = len(phases)
			return CompletionResponse{Decision: raw(Decision{Kind: "query", Query: &q})}, nil
		}), "test-model", 16384)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Execute(t.Context(), request(), model, queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
			return observation(q, strconv.Itoa(q.Limit)), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if result.Report.StopReason != "MODEL_BUDGET" || phases[len(phases)-1] != "SYNTHESIZING" {
			t.Fatalf("reason=%s phases=%v", result.Report.StopReason, phases)
		}
		total, investigationTokens := 0, 0
		for _, c := range result.ModelCalls {
			total += c.Charged
			if c.Phase != "SYNTHESIZING" {
				investigationTokens += c.Charged
			}
		}
		if total > ModelTokenLimit || investigationTokens > ModelTokenLimit-ReportTokenReserve {
			t.Fatal(total, investigationTokens)
		}
		if len(result.ModelCalls) != len(phases) {
			t.Fatal("audit lost calls")
		}
	})
	t.Run("timeout retains reservation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			model, err := NewModelDecider(providerFunc(func(ctx context.Context, _ CompletionRequest) (CompletionResponse, error) {
				<-ctx.Done()
				return CompletionResponse{}, ctx.Err()
			}), "test-model", 8192)
			if err != nil {
				t.Fatal(err)
			}
			if err = model.BeginRun(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = model.Decide(ctx, View{Request: request(), Phase: "INVESTIGATING"})
			audit := model.ModelAudit()
			if !errors.Is(err, context.DeadlineExceeded) || audit[0].Charged != audit[0].Reserved || audit[0].Status != "UNKNOWN" {
				t.Fatal(audit, err)
			}
		})
	})
}

// A report must be able to see records even when investigation stopped early; the
// reserve is a floor for synthesis, not a limit on what it may spend.
func TestSynthesisUsesRemainingBudget(t *testing.T) {
	var got CompletionRequest
	model, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
		got = r
		return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())})}, nil
	}), "test-model", 1048576)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.BeginRun(); err != nil {
		t.Fatal(err)
	}
	q := query()
	records := []telemetry.Record{}
	for i := range 8 {
		records = append(records, observation(q, strconv.Itoa(i)).Records...)
	}
	result := telemetry.Result{Query: q, QueryID: "q", Records: records}
	result.Quality.QueryOutcome = "SUCCEEDED"
	if _, err := model.Decide(t.Context(), View{RunID: "r", Request: request(), Phase: "SYNTHESIZING", Step: 1, Evidence: []telemetry.Result{result}}); err != nil {
		t.Fatal(err)
	}
	var input modelInput
	if err := json.Unmarshal(got.Input, &input); err != nil {
		t.Fatal(err)
	}
	if got.MaxOutputTokens != ReportOutputTokens || len(input.Records) != 8 || input.OmittedRecords != 0 {
		t.Fatalf("output=%d records=%d omitted=%d", got.MaxOutputTokens, len(input.Records), input.OmittedRecords)
	}
}

func TestUnwrappedReportOnlyAddsEnvelope(t *testing.T) {
	flat := `{"type":"json_object","result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["x"],"next_actions":[],"recovery_assessment":"UNKNOWN"}`
	decision, err := decodeDecision(unwrappedReport(json.RawMessage(flat)))
	if err != nil || decision.Kind != "report" || decision.Report.Unknowns[0] != "x" {
		t.Fatalf("%+v %v", decision, err)
	}
	nested := `{"type":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["y"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}`
	if decision, err := decodeDecision(unwrappedReport(json.RawMessage(nested))); err != nil || decision.Report.Unknowns[0] != "y" {
		t.Fatalf("%+v %v", decision, err)
	}
	// A report beside anything unexpected is left alone for strict decoding to refuse.
	if _, err := decodeDecision(unwrappedReport(json.RawMessage(`{"report":{"result":"INCONCLUSIVE"},"shell":"rm"}`))); err == nil {
		t.Fatal("extra field accepted")
	}
	// Anything else in the fields still fails strict decoding.
	if _, err := decodeDecision(unwrappedReport(json.RawMessage(`{"result":"DIAGNOSED","shell":"rm"}`))); err == nil {
		t.Fatal("unknown field accepted")
	}
	for _, unchanged := range []string{`{"kind":"query","query":{}}`, `{"facts":[]}`, `not json`, `[1]`} {
		if got := unwrappedReport(json.RawMessage(unchanged)); string(got) != unchanged {
			t.Fatalf("%s became %s", unchanged, got)
		}
	}
}

func TestLogSweepCoversEveryServiceBeforeTheModel(t *testing.T) {
	r := request()
	r.Services = []string{"order-service", "goods-service", "user-service"}
	r.LogSweep = true
	var firstPrompt modelInput
	calls := 0
	model, err := NewModelDecider(providerFunc(func(_ context.Context, c CompletionRequest) (CompletionResponse, error) {
		calls++
		if calls == 1 {
			if err := json.Unmarshal(c.Input, &firstPrompt); err != nil {
				t.Fatal(err)
			}
		}
		return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())})}, nil
	}), "test-model", 32000)
	if err != nil {
		t.Fatal(err)
	}
	seen := []string{}
	result, err := Execute(t.Context(), r, model, queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
		seen = append(seen, q.Service)
		return observation(q, q.Service), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "order-service,goods-service,user-service" {
		t.Fatal(seen)
	}
	// The sweep is free of model cost, and the model starts with what it collected.
	if calls != 1 || len(result.ModelCalls) != 1 || len(firstPrompt.Records) != 3 || len(firstPrompt.UncheckedLogs) != 0 {
		t.Fatalf("calls=%d records=%d unchecked=%v", calls, len(firstPrompt.Records), firstPrompt.UncheckedLogs)
	}
	if result.Report.StopReason != "REPORT_ACCEPTED" {
		t.Fatal(result.Report.StopReason)
	}
}

func TestLogSweepIsOffUnlessRequestedAndNotInSynthesis(t *testing.T) {
	r := request()
	if SweepDecision(View{Request: r, Phase: "INVESTIGATING"}) != nil {
		t.Fatal("swept without being asked")
	}
	r.LogSweep = true
	if SweepDecision(View{Request: r, Phase: "SYNTHESIZING"}) != nil {
		t.Fatal("swept during synthesis")
	}
	if SweepDecision(View{Request: r, Phase: "INVESTIGATING"}) == nil {
		t.Fatal("did not sweep")
	}
	// A filtered query does not count as having read the service's logs.
	q := query()
	q.Severity = "ERROR"
	if SweepDecision(View{Request: r, Phase: "INVESTIGATING", Evidence: []telemetry.Result{{Query: q}}}) == nil {
		t.Fatal("filtered query counted as coverage")
	}
	if SweepDecision(View{Request: r, Phase: "INVESTIGATING", Evidence: []telemetry.Result{{Query: query()}}}) == nil {
		t.Fatal("routine logs counted as coverage")
	}
	q = query()
	q.Notable = true
	if SweepDecision(View{Request: r, Phase: "INVESTIGATING", Evidence: []telemetry.Result{{Query: q}}}) != nil {
		t.Fatal("swept twice")
	}
}

func TestTuningChangesLimitsButNotBudgetChecks(t *testing.T) {
	var got CompletionRequest
	usage := ModelUsage{InputTokens: 100, OutputTokens: 100}
	model, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
		got = r
		return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())}), Usage: &usage}, nil
	}), "test-model", 1048576)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ModelTuning{{}, {MaxOutput: MaxConfigurableOutputTokens + 1, DecisionTimeout: time.Minute, RunTimeout: time.Hour}, {MaxOutput: 6000, DecisionTimeout: time.Hour, RunTimeout: time.Minute}} {
		if model.Tune(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if model.DecisionTimeout() != defaultDecisionTimeout || model.RunTimeout() != defaultRunTimeout {
		t.Fatal("defaults changed by a refused tuning")
	}
	if err := model.Tune(ModelTuning{MaxOutput: 6000, DecisionTimeout: time.Minute, RunTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := model.BeginRun(); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Decide(t.Context(), View{Request: request(), Phase: "SYNTHESIZING"}); err != nil {
		t.Fatal(err)
	}
	if got.MaxOutputTokens != 6000 {
		t.Fatal(got.MaxOutputTokens)
	}
	// Going over the output actually allowed still stops the run.
	usage = ModelUsage{InputTokens: 100, OutputTokens: 6001}
	if _, err := model.Decide(t.Context(), View{Request: request(), Phase: "SYNTHESIZING"}); !errors.Is(err, ErrModelBudget) {
		t.Fatal(err)
	}
}

func TestReportCallGetsRoomThatQueriesDoNot(t *testing.T) {
	limits := map[string]int{}
	model, err := NewModelDecider(providerFunc(func(_ context.Context, c CompletionRequest) (CompletionResponse, error) {
		var input modelInput
		if err := json.Unmarshal(c.Input, &input); err != nil {
			t.Fatal(err)
		}
		limits[input.Phase] = c.MaxOutputTokens
		if input.Phase == "SYNTHESIZING" {
			return CompletionResponse{Decision: raw(Decision{Kind: "report", Report: ptr(fallback())})}, nil
		}
		if len(input.Records) == 0 {
			return CompletionResponse{Decision: raw(Decision{Kind: "query", Query: &telemetry.Query{Kind: "logs", Service: "order-service"}})}, nil
		}
		return CompletionResponse{Decision: raw(map[string]string{"kind": synthesizeKind})}, nil
	}), "test-model", 32000)
	if err != nil {
		t.Fatal(err)
	}
	r := request()
	r.Services = []string{"order-service"}
	seen := 0
	if _, err := Execute(t.Context(), r, model, queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
		seen++
		return observation(q, strconv.Itoa(seen)), nil
	})); err != nil {
		t.Fatal(err)
	}
	if limits["INVESTIGATING"] != MaxOutputTokens {
		t.Fatalf("%v", limits)
	}
	if limits["SYNTHESIZING"] != ReportOutputTokens {
		t.Fatalf("%v", limits)
	}
}

// A failed call's reservation is not refunded, so a second report has less room. It must
// stay inside the run's limit and say what it left out.
func TestReportRetryStaysInBudgetAndCountsWhatItDrops(t *testing.T) {
	q := telemetry.Query{Kind: "logs", Service: "order-service"}
	entry := telemetry.Result{Query: q, Status: "AVAILABLE"}
	for i := range 24 {
		entry.Records = append(entry.Records, telemetry.Record{ID: strconv.Itoa(i), Type: "log", Service: "order-service",
			Fields: map[string]string{"message": strings.Repeat("evidence ", 120) + strconv.Itoa(i)}})
	}
	calls := 0
	decider, err := NewModelDecider(providerFunc(func(context.Context, CompletionRequest) (CompletionResponse, error) {
		calls++
		if calls == 1 {
			return CompletionResponse{}, &ProviderError{Code: "NETWORK"}
		}
		return CompletionResponse{Decision: json.RawMessage(`{"kind":"report"}`)}, nil
	}), "m", 1048576)
	if err != nil {
		t.Fatal(err)
	}
	if err = decider.BeginRun(); err != nil {
		t.Fatal(err)
	}
	view := View{RunID: "run", Phase: "SYNTHESIZING", Step: 10, Evidence: []telemetry.Result{entry}}
	_, err = decider.Decide(t.Context(), view)
	var failure *ProviderError
	if !errors.As(err, &failure) || failure.Code != "NETWORK" || !errors.Is(err, ErrModelProvider) {
		t.Fatal(err)
	}
	view.Step = 11
	if _, err = decider.Decide(t.Context(), view); err != nil {
		t.Fatal(err)
	}
	audit := decider.ModelAudit()
	if len(audit) != 2 || audit[0].Reserved+audit[1].Reserved > ModelTokenLimit || audit[1].OmittedRecords <= audit[0].OmittedRecords {
		t.Fatalf("%+v", audit)
	}
}

func TestRelationsReachOnlyTheReportCallWhenEnabled(t *testing.T) {
	entry := telemetry.Result{Query: telemetry.Query{Kind: "trace", Service: "a"}, Status: "AVAILABLE", Records: []telemetry.Record{
		span("c", "a", "", 0, 800, "error", "true"), span("s", "b", "c", 5, 1200),
	}}
	run := func(enable bool, phase string) (CompletionRequest, json.RawMessage) {
		var got CompletionRequest
		decider, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
			got = r
			return CompletionResponse{Decision: json.RawMessage(`{"kind":"synthesize","cited":"` + shortID("id-s") + `"}`)}, nil
		}), "m", 1048576)
		if err != nil {
			t.Fatal(err)
		}
		if enable {
			decider.UseRelations()
		}
		if err = decider.BeginRun(); err != nil {
			t.Fatal(err)
		}
		raw, err := decider.Decide(t.Context(), View{RunID: "run", Phase: phase, Step: 1, Evidence: []telemetry.Result{entry}})
		if err != nil {
			t.Fatal(err)
		}
		return got, raw
	}
	on, _ := run(true, "SYNTHESIZING")
	if !strings.Contains(string(on.Input), `"call_relations"`) || !strings.Contains(on.Instructions, "call_relations") {
		t.Fatal(string(on.Input))
	}
	for _, off := range []struct {
		enable bool
		phase  string
	}{{false, "SYNTHESIZING"}, {true, "INVESTIGATING"}} {
		got, _ := run(off.enable, off.phase)
		if strings.Contains(string(got.Input), "call_relations") || strings.Contains(got.Instructions, "call_relations") {
			t.Fatalf("%+v: %s", off, got.Input)
		}
	}
}

func TestRelationsBeyondTheLimitAreCounted(t *testing.T) {
	records := []telemetry.Record{}
	for i := range relationLimit + 3 {
		id := strconv.Itoa(i)
		records = append(records, span("c"+id, "a", "", 0, 800, "error", "true"), span("s"+id, "b", "c"+id, 5, 1200))
	}
	input, _, _, err := buildCountedInput(View{RunID: "run", Phase: "SYNTHESIZING", Relations: CallRelations([]telemetry.Result{{Records: records}})},
		100000, func(_ string, b []byte) int { return len(b) })
	if err != nil {
		t.Fatal(err)
	}
	var decoded modelInput
	if err = json.Unmarshal(input, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Relations) != relationLimit || decoded.OmittedRelations != 3 {
		t.Fatal(len(decoded.Relations), decoded.OmittedRelations)
	}
}
