// Package investigation executes bounded single-run investigations independently of a model provider.
package investigation

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// PolicyVersion identifies the Harness guard and execution policy.
const PolicyVersion = "harness-v13"

// MaxEvidenceBytes bounds saved evidence and derived log summaries.
const MaxEvidenceBytes = 2 << 20

// MaxDecisionBytes bounds a single untrusted decision.
const MaxDecisionBytes = 64 << 10

// Request is operator-supplied scope; decisions cannot expand it.
type Request struct {
	Service     string    `json:"service"`
	Services    []string  `json:"services"`
	Environment string    `json:"environment"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Summary     string    `json:"summary"`
	// LogSweep has the harness read every service's logs before the model chooses anything.
	LogSweep bool `json:"log_sweep,omitempty"`
}

// Decider returns one untrusted JSON decision. Implementations must honor context cancellation.
// Implementations include local scripts and a budgeted model provider.
type Decider interface {
	Decide(context.Context, View) (json.RawMessage, error)
}

// Querier reserves actual backend requests before I/O using the shared budget.
type Querier interface {
	Query(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error)
}

// View is a detached snapshot; mutation by a decider cannot change saved evidence.
type View struct {
	// RunID salts identifier masking and is never sent to a model provider.
	RunID       string             `json:"run_id"`
	LogPatterns []LogPatterns      `json:"log_patterns"`
	Request     Request            `json:"request"`
	Phase       string             `json:"phase"`
	Step        int                `json:"step"`
	Evidence    []telemetry.Result `json:"evidence"`
	Feedback    string             `json:"feedback"`
	// TokensLeft is set by the model decider for its own context; it is not run state.
	TokensLeft int `json:"-"`
}

// Decision permits exactly one query or report, never arbitrary commands or URLs.
type Decision struct {
	Kind   string           `json:"kind"`
	Query  *telemetry.Query `json:"query,omitempty"`
	Report *Draft           `json:"report,omitempty"`
}

// Claim cites exact record IDs visible in the current run.
type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// TimelineItem binds an ordered event time to cited evidence.
type TimelineItem struct {
	At    time.Time `json:"at"`
	Claim Claim     `json:"claim"`
}

// Hypothesis separates supporting and contradicting evidence.
type Hypothesis struct {
	Claim            Claim    `json:"claim"`
	ContradictingIDs []string `json:"contradicting_ids"`
	Strength         string   `json:"strength"`
}

// Draft has no run ID, stop reason, quality override or execution status controlled by the decider.
type Draft struct {
	Result       string         `json:"result"`
	Facts        []Claim        `json:"facts"`
	Timeline     []TimelineItem `json:"timeline"`
	Primary      *Hypothesis    `json:"primary_hypothesis,omitempty"`
	Alternatives []Hypothesis   `json:"alternatives"`
	Unknowns     []string       `json:"unknowns"`
	NextActions  []string       `json:"next_actions"`
	Recovery     string         `json:"recovery_assessment"`
}

// Report adds Harness-controlled identity and stopping metadata to a draft.
type Report struct {
	Draft
	RunID         string `json:"run_id"`
	StopReason    string `json:"stop_reason"`
	PolicyVersion string `json:"policy_version"`
}

// Step records outcomes without retaining rejected raw decisions or provider error text.
type Step struct {
	Number  int    `json:"number"`
	Phase   string `json:"phase"`
	Outcome string `json:"outcome"`
	// Detail is a fixed reason code for a rejection, never model output.
	Detail  string `json:"detail,omitempty"`
	QueryID string `json:"query_id,omitempty"`
}

// Run is the public execution snapshot. Checkpoint adds the resumable loop state.
type Run struct {
	ModelCalls      []ModelCall        `json:"model_calls"`
	LogPatterns     []LogPatterns      `json:"log_patterns"`
	ID              string             `json:"id"`
	Request         Request            `json:"request"`
	Status          string             `json:"status"`
	CreatedAt       time.Time          `json:"created_at"`
	DeadlineAt      time.Time          `json:"deadline_at"`
	FinishedAt      time.Time          `json:"finished_at"`
	Decisions       int                `json:"decisions"`
	BackendRequests int                `json:"backend_requests"`
	Evidence        []telemetry.Result `json:"evidence"`
	Steps           []Step             `json:"steps"`
	Report          Report             `json:"report"`
}
