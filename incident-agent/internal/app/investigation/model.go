package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// ModelTokenLimit caps reserved input and output tokens per run.
const ModelTokenLimit = 24000

// ReportTokenReserve protects the final synthesis allowance.
const ReportTokenReserve = 9000

// minInvestigationTokens is the smallest allowance worth spending on another query choice.
const minInvestigationTokens = 4000

// MaxOutputTokens caps a single completion request.
const MaxOutputTokens = 1500

// ErrSynthesisRequired transitions investigation into the reserved report phase.
var ErrSynthesisRequired = errors.New("model investigation budget exhausted")

// ErrModelBudget stops calls when available model budget is exhausted.
var ErrModelBudget = errors.New("model token budget exhausted")

// ErrModelContext rejects inputs that cannot fit the configured context.
var ErrModelContext = errors.New("model context cannot fit budget")

// ErrModelProvider hides untrusted provider failure details.
var ErrModelProvider = errors.New("model provider unavailable")

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
	Decision json.RawMessage
	Usage    *ModelUsage
}

// Provider is consumed here, and must respect cancellation and the explicit output cap.
type Provider interface {
	Complete(context.Context, CompletionRequest) (CompletionResponse, error)
}

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
}

// NewModelDecider creates isolated accounting for one investigation.
func NewModelDecider(provider Provider, model string, contextWindow int) (*ModelDecider, error) {
	if provider == nil || model == "" || len(model) > 128 || contextWindow < 4096 || contextWindow > 2000000 {
		return nil, ErrInvalidInput
	}
	return &ModelDecider{provider: provider, model: model, window: contextWindow, calls: []ModelCall{}}, nil
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
	output := min(MaxOutputTokens, available/3)
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
	inputLimit := min(available-output, m.window*3/5-output)
	prompt, omittedRecords, omittedQueries, err := buildModelInput(view, inputLimit)
	if err != nil {
		if view.Phase != "SYNTHESIZING" && available < ReportTokenReserve {
			return nil, ErrSynthesisRequired
		}
		return nil, err
	}
	reserved := estimateInput(prompt) + output
	call := ModelCall{Estimator: "utf8-half-bytes-plus-256-v2", ContextWindow: m.window, OutputLimit: output, Step: view.Step, Model: m.model, Phase: view.Phase,
		ContextDigest: telemetry.Hash(append([]byte(modelInstructions), prompt...)), Reserved: reserved,
		Charged: reserved, Estimated: true, Status: "UNKNOWN", OmittedRecords: omittedRecords, OmittedQueries: omittedQueries}
	if m.reserve != nil {
		if err := m.reserve(ctx, call); err != nil {
			return nil, errors.Join(ErrPersistence, err)
		}
	}
	m.charged += reserved // Reservation occurs before any external call; unknown usage is never refunded.
	m.calls = append(m.calls, call)
	response, callErr := m.provider.Complete(ctx, CompletionRequest{Instructions: modelInstructions, Input: prompt, MaxOutputTokens: output})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	index := len(m.calls) - 1
	if callErr != nil {
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
	return expandCitations(unwrappedReport(response.Decision), view.Evidence), nil
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
