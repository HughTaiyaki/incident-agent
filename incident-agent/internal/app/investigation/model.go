package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// ModelTokenLimit caps reserved input and output tokens per run.
const ModelTokenLimit = 36000

// ReportTokenReserve protects the final synthesis allowance.
const ReportTokenReserve = 9000

// minInvestigationTokens is the smallest allowance worth spending on another query choice.
const minInvestigationTokens = 4000

// MaxOutputTokens caps a single completion request.
const MaxOutputTokens = 1500

// ReportOutputTokens caps the report call. A query fits in far less, but a report cut off by
// the limit is not valid JSON, and the provider boundary can only discard it.
const ReportOutputTokens = 3000

// MaxConfigurableOutputTokens bounds what ModelTuning may raise the output limit to.
const MaxConfigurableOutputTokens = 8000

// ErrSynthesisRequired transitions investigation into the reserved report phase.
var ErrSynthesisRequired = errors.New("model investigation budget exhausted")

// ErrModelBudget stops calls when available model budget is exhausted.
var ErrModelBudget = errors.New("model token budget exhausted")

// ErrModelContext rejects inputs that cannot fit the configured context.
var ErrModelContext = errors.New("model context cannot fit budget")

// ErrModelProvider hides untrusted provider failure details.
var ErrModelProvider = errors.New("model provider unavailable")

// ProviderError says what kind of failure a call had. Code is a fixed label, never the
// provider's own text, so it is safe to keep in audits.
type ProviderError struct {
	Code        string
	BusyRetries int
}

func (e *ProviderError) Error() string { return ErrModelProvider.Error() + ": " + e.Code }

// Unwrap lets callers keep treating every provider failure as ErrModelProvider.
func (e *ProviderError) Unwrap() error { return ErrModelProvider }

// ErrModelOutcomeUnknown is a call an earlier worker reserved and never recorded. It may still
// be running at the provider, so it ends the run rather than being sent again.
var ErrModelOutcomeUnknown = fmt.Errorf("%w: outcome unknown", ErrModelProvider)

// CompletionRequest contains already bounded text; a provider cannot select tool privileges.
type CompletionRequest struct {
	Instructions    string
	Input           json.RawMessage
	MaxOutputTokens int
}

// ModelUsage records the provider-reported input and output counts.
type ModelUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CompletionResponse carries an untrusted decision and optional usage.
type CompletionResponse struct {
	Decision    json.RawMessage
	Usage       *ModelUsage
	BusyRetries int
}

// Provider is consumed here, and must respect cancellation and the explicit output cap.
type Provider interface {
	Complete(context.Context, CompletionRequest) (CompletionResponse, error)
}

// TokenCounter counts a prompt the way its provider will, before the request is sent.
type TokenCounter interface {
	CountPrompt(instructions string, input []byte) int
	Name() string
}

// tokenCounterSource is implemented by a provider that knows its model's tokenizer.
type tokenCounterSource interface {
	TokenCounter() TokenCounter
}

// countedInputMargin covers what an exact local count cannot see, such as provider-side overhead.
const countedInputMargin = 16

const conservativeEstimator = "utf8-half-bytes-plus-256-v2"

// ModelCall is metadata only: no prompt, secret, raw response or private reasoning is stored.
type ModelCall struct {
	Estimator      string      `json:"estimator"`
	ContextWindow  int         `json:"context_window"`
	OutputLimit    int         `json:"output_limit"`
	Step           int         `json:"step"`
	Model          string      `json:"model"`
	Phase          string      `json:"phase"`
	ContextDigest  string      `json:"context_digest"`
	Reserved       int         `json:"reserved_tokens"`
	Charged        int         `json:"charged_tokens"`
	Estimated      bool        `json:"estimated"`
	Usage          *ModelUsage `json:"usage,omitempty"`
	Status         string      `json:"status"`
	OmittedRecords int         `json:"omitted_records"`
	OmittedQueries int         `json:"omitted_queries"`
}

// ModelDecider owns one run's in-memory accounting. It cannot be reused by another Execute call.
type ModelDecider struct {
	reserve   func(context.Context, ModelCall) error
	exhausted bool
	mu        sync.Mutex
	provider  Provider
	model     string
	window    int
	started   bool
	charged   int
	calls     []ModelCall
	tuning    ModelTuning
	counter   TokenCounter
	relations bool
	coverage  bool
}

// NewModelDecider creates isolated accounting for one investigation.
func NewModelDecider(provider Provider, model string, contextWindow int) (*ModelDecider, error) {
	if provider == nil || model == "" || len(model) > 128 || contextWindow < 4096 || contextWindow > 2000000 {
		return nil, ErrInvalidInput
	}
	m := &ModelDecider{provider: provider, model: model, window: contextWindow, calls: []ModelCall{}}
	if source, ok := provider.(tokenCounterSource); ok {
		m.counter = source.TokenCounter()
	}
	return m, nil
}

// UseRelations adds computed call relations to the report call. Only evaluation sets it,
// until a comparison shows it helps.
func (m *ModelDecider) UseRelations() { m.relations = true }

// UseCoverage makes the report call show distinct warnings first and say what the run did
// not see. Only evaluation sets it, until a comparison shows it helps.
func (m *ModelDecider) UseCoverage() { m.coverage = true }

func (m *ModelDecider) instructions(phase string) string {
	text := instructionsFor(phase)
	if phase != "SYNTHESIZING" {
		return text
	}
	if m.relations {
		text += relationInstructions
	}
	if m.coverage {
		text += coverageInstructions
	}
	return text
}

// countInput sizes a prompt: exactly when the provider's tokenizer is known, otherwise from above.
func (m *ModelDecider) countInput(phase string, input []byte) int {
	if m.counter == nil {
		return estimateInput(m.instructions(phase), input)
	}
	return m.counter.CountPrompt(m.instructions(phase), input) + countedInputMargin
}

func (m *ModelDecider) estimatorName() string {
	if m.counter == nil {
		return conservativeEstimator
	}
	return m.counter.Name()
}

// ModelTuning relaxes limits for a slow model that thinks before answering. Only evaluation
// sets it: the token budget, and every check on what the model returns, stay the same.
type ModelTuning struct {
	MaxOutput       int
	DecisionTimeout time.Duration
	RunTimeout      time.Duration
}

// Tune applies t. A value outside the allowed range is an error, so a typo cannot go unnoticed.
func (m *ModelDecider) Tune(t ModelTuning) error {
	if t.MaxOutput < 1 || t.MaxOutput > MaxConfigurableOutputTokens || t.DecisionTimeout <= 0 || t.RunTimeout < t.DecisionTimeout {
		return ErrInvalidInput
	}
	m.tuning = t
	return nil
}

func (m *ModelDecider) outputLimit(phase string) int {
	if m.tuning.MaxOutput > 0 {
		return m.tuning.MaxOutput
	}
	if phase == "SYNTHESIZING" {
		return ReportOutputTokens
	}
	return MaxOutputTokens
}

// DecisionTimeout is how long the harness waits for one decision.
func (m *ModelDecider) DecisionTimeout() time.Duration {
	if m.tuning.DecisionTimeout > 0 {
		return m.tuning.DecisionTimeout
	}
	return defaultDecisionTimeout
}

// RunTimeout is the run's overall limit.
func (m *ModelDecider) RunTimeout() time.Duration {
	if m.tuning.RunTimeout > 0 {
		return m.tuning.RunTimeout
	}
	return defaultRunTimeout
}

// BeginRun prevents budgets or previous evidence state from being accidentally shared across runs.
func (m *ModelDecider) BeginRun() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("model decider already used by a run")
	}
	m.started = true
	return nil
}

// Decide builds bounded context and reserves budget before requesting a decision.
func (m *ModelDecider) Decide(ctx context.Context, view View) (json.RawMessage, error) {
	// Serialize calls so reservation, provider completion and settlement have one owner.
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !m.started || m.exhausted || len(m.calls) >= 12 {
		return nil, ErrModelBudget
	}
	if sweep := SweepDecision(view); sweep != nil {
		return sweep, nil
	}
	// The reserve is a floor investigation cannot touch, not a cap on the report: a
	// report that cannot see records has nothing to cite and can only be INCONCLUSIVE.
	available := ModelTokenLimit - m.charged
	if view.Phase != "SYNTHESIZING" {
		available -= ReportTokenReserve
	}
	if available < 1 {
		if view.Phase != "SYNTHESIZING" {
			return nil, ErrSynthesisRequired
		}
		return nil, ErrModelBudget
	}
	output := min(m.outputLimit(view.Phase), available/3)
	if output < 256 {
		if view.Phase != "SYNTHESIZING" {
			return nil, ErrSynthesisRequired
		}
		return nil, ErrModelBudget
	}
	// Too little left to show useful evidence: another query would be chosen nearly blind.
	if view.Phase != "SYNTHESIZING" && available < minInvestigationTokens {
		return nil, ErrSynthesisRequired
	}
	view.TokensLeft = ModelTokenLimit - m.charged
	if m.relations && view.Phase == "SYNTHESIZING" {
		view.Relations = CallRelations(view.Evidence)
	}
	view.Coverage = m.coverage && view.Phase == "SYNTHESIZING"
	inputLimit := min(available-output, m.window*3/5-output)
	prompt, omittedRecords, omittedQueries, err := buildCountedInput(view, inputLimit, m.countInput)
	if err != nil {
		if view.Phase != "SYNTHESIZING" && available < ReportTokenReserve {
			return nil, ErrSynthesisRequired
		}
		return nil, err
	}
	reserved := m.countInput(view.Phase, prompt) + output
	call := ModelCall{Estimator: m.estimatorName(), ContextWindow: m.window, OutputLimit: output, Step: view.Step, Model: m.model, Phase: view.Phase,
		ContextDigest: telemetry.Hash(append([]byte(m.instructions(view.Phase)), prompt...)), Reserved: reserved,
		Charged: reserved, Estimated: true, Status: "UNKNOWN", OmittedRecords: omittedRecords, OmittedQueries: omittedQueries}
	if m.reserve != nil {
		if err := m.reserve(ctx, call); err != nil {
			return nil, errors.Join(ErrPersistence, err)
		}
	}
	m.charged += reserved // Reservation occurs before any external call; unknown usage is never refunded.
	m.calls = append(m.calls, call)
	response, callErr := m.provider.Complete(ctx, CompletionRequest{Instructions: m.instructions(view.Phase), Input: prompt, MaxOutputTokens: output})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	index := len(m.calls) - 1
	if callErr != nil {
		var failure *ProviderError
		if errors.As(callErr, &failure) {
			return nil, failure
		}
		return nil, ErrModelProvider
	}
	if response.Usage != nil {
		u := *response.Usage
		if u.InputTokens <= 0 || u.OutputTokens <= 0 || u.InputTokens > 10000000 || u.OutputTokens > 10000000 {
			m.calls[index].Status = "INVALID_USAGE"
			return nil, ErrModelProvider
		}
		actual := u.InputTokens + u.OutputTokens
		m.calls[index].Usage = &u
		m.calls[index].Estimated = false
		m.calls[index].Charged = actual
		m.charged += actual - reserved
		if actual > reserved || u.OutputTokens > output {
			m.calls[index].Status = "RESERVATION_EXCEEDED"
			// Disable further calls even if total reported usage remains below the run limit.
			m.exhausted = true
			return nil, ErrModelBudget
		}
	}
	m.calls[index].Status = "RECEIVED"
	if len(response.Decision) > MaxDecisionBytes {
		m.calls[index].Status = "OUTPUT_TOO_LARGE"
		return nil, ErrModelProvider
	}
	decision := expandCitations(unwrappedReport(response.Decision), view.Evidence)
	return unmaskQuery(decision, view.Evidence, view.RunID), nil
}

// SweepDecision returns the next query of a run's log sweep without involving a model, or
// nil when the sweep is finished or not requested. Coverage must not depend on a model's
// choices: signals often sit in one downstream service, and left to itself a model can spend
// its whole budget elsewhere. The rule is the same for every run, so it reveals no answer,
// and it is derived only from saved evidence, so a resumed run continues where it stopped.
func SweepDecision(view View) json.RawMessage {
	if !view.Request.LogSweep || view.Phase == "SYNTHESIZING" {
		return nil
	}
	for _, service := range view.Request.Services {
		found := false
		for _, entry := range view.Evidence {
			q := entry.Query
			found = found || (q.Kind == "logs" && q.Service == service && q.Notable)
		}
		if !found {
			decision, err := json.Marshal(Decision{Kind: "query", Query: &telemetry.Query{Kind: "logs", Service: service, Notable: true}})
			if err != nil {
				return nil
			}
			return decision
		}
	}
	return nil
}

// unwrappedReport restores the envelope when a model omits kind, which DeepSeek does
// routinely: a report's fields at the top level, or a report under "type" instead of
// "kind". Only the envelope changes: the content still faces strict decoding and the
// report guard, so nothing invalid gets through.
func unwrappedReport(decision json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(decision, &fields) != nil {
		return decision
	}
	_, hasKind := fields["kind"]
	_, hasResult := fields["result"]
	nested, hasReport := fields["report"]
	if hasKind || (!hasResult && !hasReport) {
		return decision
	}
	delete(fields, "type")
	report := nested
	if !hasReport {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return decision
		}
		report = encoded
	} else if len(fields) != 1 {
		return decision
	}
	wrapped, err := json.Marshal(map[string]any{"kind": "report", "report": report})
	if err != nil {
		return decision
	}
	return wrapped
}

// ModelAudit returns detached usage metadata for the final Run artifact.
func (m *ModelDecider) ModelAudit() []ModelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := append([]ModelCall{}, m.calls...)
	for i := range calls {
		if calls[i].Usage != nil {
			usage := *calls[i].Usage
			calls[i].Usage = &usage
		}
	}
	return calls
}

// RestoreAccounting configures a fresh decider with durable charges: settled usage
// where the provider reported it, the full reservation everywhere else.
// The reservation hook must durably acknowledge before Provider.Complete.
func (m *ModelDecider) RestoreAccounting(calls []ModelCall, reserve func(context.Context, ModelCall) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return ErrInvalidInput
	}
	m.reserve = reserve
	for _, call := range calls {
		if call.Usage == nil {
			call.Charged = max(call.Reserved, call.Charged)
		}
		m.charged += call.Charged
		m.exhausted = m.exhausted || call.Status == "RESERVATION_EXCEEDED"
		m.calls = append(m.calls, call)
	}
	return nil
}
