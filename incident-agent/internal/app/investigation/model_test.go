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

func TestModelDecider(t *testing.T) {
	t.Run("usage settlement and detached audit", func(t *testing.T) {
		model, err := NewModelDecider(providerFunc(func(_ context.Context, r CompletionRequest) (CompletionResponse, error) {
			if r.MaxOutputTokens > 1500 || !json.Valid(r.Input) {
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
		}), "test-model", 8192)
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
	if got.MaxOutputTokens != MaxOutputTokens || len(input.Records) != 8 || input.OmittedRecords != 0 {
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
