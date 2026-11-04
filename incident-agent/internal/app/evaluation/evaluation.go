// Package evaluation prepares frozen fault samples and records scored investigations.
// It only reads telemetry; running a fault experiment stays with the operator.
package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// RequestSummary is the same for every case, so the request itself gives nothing away.
const RequestSummary = "订单交易在该窗口可能出现异常，核查发生了什么、影响范围和当前状态。"

// maxTraces bounds per-sample trace detail queries.
const maxTraces = 12

// Request is what the investigator is told about a sample: a window and nothing else.
func Request(services []string, environment string, start, end time.Time) investigation.Request {
	return investigation.Request{Service: "order-service", Services: slices.Clone(services), Environment: environment,
		Start: start, End: end, Summary: RequestSummary, LogSweep: true}
}

// PlanQueries covers what an investigation could ask of a live backend. Severity-filtered
// logs are captured separately because an unfiltered query keeps only the earliest records.
func PlanQueries(services []string, environment string, start, end time.Time) []telemetry.Query {
	base := telemetry.Query{Environment: environment, Start: start, End: end}
	queries := []telemetry.Query{}
	for _, service := range services {
		add := func(q telemetry.Query) {
			q.Environment, q.Start, q.End, q.Service = base.Environment, base.Start, base.End, service
			queries = append(queries, q)
		}
		add(telemetry.Query{Kind: "logs"})
		add(telemetry.Query{Kind: "logs", Notable: true})
		add(telemetry.Query{Kind: "logs", Severity: "ERROR"})
		add(telemetry.Query{Kind: "logs", Severity: "WARN"})
		add(telemetry.Query{Kind: "traces"})
		for _, metric := range telemetry.MetricCatalog() {
			add(telemetry.Query{Kind: "metrics", Metric: metric})
		}
	}
	return queries
}

// TraceQueries asks for detail on the traces most likely to matter: those on error
// and warning logs first, then those the trace search found.
func TraceQueries(results []telemetry.Result) []telemetry.Query {
	queries := []telemetry.Query{}
	seen := map[string]bool{}
	add := func(entry telemetry.Result, record telemetry.Record) {
		if record.TraceID == "" || seen[record.TraceID] || len(queries) >= maxTraces {
			return
		}
		seen[record.TraceID] = true
		queries = append(queries, telemetry.Query{Kind: "trace", Service: entry.Query.Service, Environment: entry.Query.Environment,
			Start: entry.Query.Start, End: entry.Query.End, TraceID: record.TraceID})
	}
	for _, pick := range []func(telemetry.Query) bool{
		func(q telemetry.Query) bool { return q.Severity == "ERROR" },
		func(q telemetry.Query) bool { return q.Severity == "WARN" },
		func(q telemetry.Query) bool { return q.Notable },
		func(q telemetry.Query) bool { return q.Kind == "traces" },
	} {
		for _, entry := range results {
			if pick(entry.Query) {
				for _, record := range entry.Records {
					add(entry, record)
				}
			}
		}
	}
	return queries
}

// fixedTraces and fixedMetric make up the fixed flow's queries beyond the log sweep. Together
// with the sweep over the five services they use the same query steps an investigation has.
const (
	fixedTraces = 3
	fixedMetric = "rpc_error_rate"
)

// FixedFlow is the baseline an investigation is compared with: the program picks the queries
// by a rule that knows nothing about the case, and the same model writes the report under
// the same prompt, limits and checks. Everything but Decide comes from the model decider.
type FixedFlow struct {
	*investigation.ModelDecider
}

// Decide never spends a model call on choosing a query.
func (f FixedFlow) Decide(ctx context.Context, view investigation.View) (json.RawMessage, error) {
	if view.Phase == "SYNTHESIZING" {
		return f.ModelDecider.Decide(ctx, view)
	}
	if sweep := investigation.SweepDecision(view); sweep != nil {
		return sweep, nil
	}
	decision := investigation.Decision{Kind: "synthesize"}
	if query := nextFixedQuery(view); query != nil {
		decision = investigation.Decision{Kind: "query", Query: query}
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil, investigation.ErrInvalidDecision
	}
	return encoded, nil
}

// nextFixedQuery is one error rate, then the traces of the most severe log records.
func nextFixedQuery(view investigation.View) *telemetry.Query {
	seen := map[string]bool{}
	metric := false
	for _, entry := range view.Evidence {
		metric = metric || entry.Query.Metric == fixedMetric
		if entry.Query.Kind == "trace" {
			seen[entry.Query.TraceID] = true
		}
	}
	if !metric {
		return &telemetry.Query{Kind: "metrics", Service: view.Request.Service, Metric: fixedMetric}
	}
	if len(seen) >= fixedTraces {
		return nil
	}
	for _, severity := range []string{"ERROR", "WARN", ""} {
		for _, entry := range view.Evidence {
			if entry.Query.Kind != "logs" || !entry.Query.Notable {
				continue
			}
			for _, record := range entry.Records {
				if record.TraceID == "" || seen[record.TraceID] || (severity != "" && record.Fields["severity_text"] != severity) {
					continue
				}
				return &telemetry.Query{Kind: "trace", Service: entry.Query.Service, TraceID: record.TraceID}
			}
		}
	}
	return nil
}

// HybridFlow is the production hybrid flow with its fill reasons kept for the evaluation record.
type HybridFlow struct {
	*investigation.ModelDecider
	Fills []string
}

// DecisionLimit matches the production hybrid flow.
func (f *HybridFlow) DecisionLimit() int { return investigation.PlannedDecisionLimit }

// Decide never spends a model call on choosing a query.
func (f *HybridFlow) Decide(ctx context.Context, view investigation.View) (json.RawMessage, error) {
	if view.Phase == "SYNTHESIZING" {
		return f.ModelDecider.Decide(ctx, view)
	}
	decision, reason, err := investigation.PlannedDecision(view)
	if reason != "" {
		f.Fills = append(f.Fills, reason)
	}
	return decision, err
}

var leakPattern = regexp.MustCompile(`(?i)failpoint|\bfault|inject|barrier|mutation|testtrade|\bT[0-9]{1,2}\b|\bC[1-3]\b`)

// LeakHit says where an experiment hint appears. It names the field, not the value.
type LeakHit struct {
	Field string `json:"field"`
	Match string `json:"match"`
	Count int    `json:"count"`
}

// ScanLeaks looks through everything a model could be shown. A hit needs human judgment:
// removing it silently could also remove real evidence.
func ScanLeaks(entries []telemetry.Result) []LeakHit {
	counts := map[[2]string]int{}
	scan := func(field, value string) {
		for _, match := range leakPattern.FindAllString(value, -1) {
			counts[[2]string{field, match}]++
		}
	}
	for _, entry := range entries {
		for _, record := range entry.Records {
			scan("service", record.Service)
			for key, value := range record.Fields {
				scan("fields."+key, value)
				scan("field_name", key)
			}
		}
	}
	hits := []LeakHit{}
	for key, count := range counts {
		hits = append(hits, LeakHit{Field: key[0], Match: key[1], Count: count})
	}
	slices.SortFunc(hits, func(a, b LeakHit) int {
		if a.Field+a.Match < b.Field+b.Match {
			return -1
		}
		return 1
	})
	return hits
}

// SourceSummary records how much each backend returned, including failures.
type SourceSummary struct {
	Queries int `json:"queries"`
	Failed  int `json:"failed"`
	Records int `json:"records"`
}

// Manifest describes a sample. It holds no case label or fault details, which live with
// the evaluator's truth file outside what an investigation reads.
type Manifest struct {
	SampleID     string                   `json:"sample_id"`
	UpstreamSHA  string                   `json:"upstream_sha"`
	Environment  string                   `json:"environment"`
	Start        time.Time                `json:"start"`
	End          time.Time                `json:"end"`
	CapturedAt   time.Time                `json:"captured_at"`
	BundleDigest string                   `json:"bundle_digest"`
	Sources      map[string]SourceSummary `json:"sources"`
	LeakHits     []LeakHit                `json:"leak_hits"`
	DerivedFrom  string                   `json:"derived_from,omitempty"`
	Dropped      []string                 `json:"dropped_sources,omitempty"`
}

// Summarize fills in the per-source counts.
func Summarize(entries []telemetry.Result) map[string]SourceSummary {
	sources := map[string]SourceSummary{}
	for _, entry := range entries {
		summary := sources[entry.Query.Kind]
		summary.Queries++
		summary.Records += len(entry.Records)
		if entry.Quality.QueryOutcome != "SUCCEEDED" {
			summary.Failed++
		}
		sources[entry.Query.Kind] = summary
	}
	return sources
}

// Derive builds an evidence-gap sample: the named query kinds are gone from the bundle,
// so a replay reports them as not covered rather than as healthy and empty.
func Derive(bundle telemetry.Bundle, dropKinds []string) (telemetry.Bundle, error) {
	if len(dropKinds) == 0 {
		return telemetry.Bundle{}, errors.New("nothing to drop")
	}
	kept := []telemetry.Result{}
	for _, entry := range bundle.Entries {
		if !slices.Contains(dropKinds, entry.Query.Kind) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(bundle.Entries) {
		return telemetry.Bundle{}, errors.New("no entries matched the kinds to drop")
	}
	return telemetry.NewBundle("replay", bundle.UpstreamSHA, kept)
}

// RenameServices rewrites every service name in a bundle, including where one appears inside
// text, for testing that results do not depend on what services are called. Renaming only
// shows that much: it says nothing about a system with a different shape.
func RenameServices(bundle telemetry.Bundle, names map[string]string) (telemetry.Bundle, error) {
	replacer, err := serviceReplacer(names)
	if err != nil {
		return telemetry.Bundle{}, err
	}
	encoded, err := json.Marshal(bundle.Entries)
	if err != nil {
		return telemetry.Bundle{}, errors.New("cannot encode bundle")
	}
	var entries []telemetry.Result
	if err = json.Unmarshal(encoded, &entries); err != nil {
		return telemetry.Bundle{}, errors.New("cannot copy bundle")
	}
	for i := range entries {
		entry := &entries[i]
		entry.Query.Service = replacer.Replace(entry.Query.Service)
		entry.Query.Peer = replacer.Replace(entry.Query.Peer)
		entry.CanonicalQuery = replacer.Replace(entry.CanonicalQuery)
		for j := range entry.Records {
			record := &entry.Records[j]
			record.Service = replacer.Replace(record.Service)
			for key, value := range record.Fields {
				record.Fields[key] = replacer.Replace(value)
			}
		}
		entry.Finish()
	}
	return telemetry.NewBundle("replay", bundle.UpstreamSHA, entries)
}

// RenameRequest applies the same names to a request.
func RenameRequest(request investigation.Request, names map[string]string) (investigation.Request, error) {
	replacer, err := serviceReplacer(names)
	if err != nil {
		return investigation.Request{}, err
	}
	request.Service = replacer.Replace(request.Service)
	request.Services = slices.Clone(request.Services)
	for i := range request.Services {
		request.Services[i] = replacer.Replace(request.Services[i])
	}
	return request, nil
}

func serviceReplacer(names map[string]string) (*strings.Replacer, error) {
	if len(names) == 0 {
		return nil, errors.New("nothing to rename")
	}
	pairs := []string{}
	for oldName, newName := range names {
		if !telemetry.ValidScopeName(oldName) || !telemetry.ValidScopeName(newName) {
			return nil, errors.New("invalid service name")
		}
		pairs = append(pairs, oldName, newName)
	}
	return strings.NewReplacer(pairs...), nil
}

// Flows name who picked the queries in an outcome.
const (
	FlowAgent  = investigation.FlowAgent
	FlowFixed  = "fixed"
	FlowHybrid = investigation.FlowHybrid
)

// CallRecord says how one model call went and which records it showed the model. It holds
// record handles and fixed codes only, never the prompt or the provider's text.
type CallRecord struct {
	Phase       string   `json:"phase"`
	Seconds     float64  `json:"seconds"`
	Failure     string   `json:"failure,omitempty"`
	BusyRetries int      `json:"busy_retries,omitempty"`
	Shown       []string `json:"shown"`
}

// Outcome is one investigation's result in a form that can be compared and scored.
type Outcome struct {
	Flow          string               `json:"flow"`
	Relations     bool                 `json:"relations,omitempty"`
	Fills         []string             `json:"fills,omitempty"`
	Calls         []CallRecord         `json:"calls"`
	Sample        string               `json:"sample"`
	Attempt       int                  `json:"attempt"`
	BundleDigest  string               `json:"bundle_digest"`
	Model         string               `json:"model"`
	Policy        string               `json:"policy_version"`
	Seconds       float64              `json:"seconds"`
	InputTokens   int                  `json:"input_tokens"`
	OutputTokens  int                  `json:"output_tokens"`
	ChargedTokens int                  `json:"charged_tokens"`
	ModelCalls    int                  `json:"model_calls"`
	Steps         []investigation.Step `json:"steps"`
	Queries       []telemetry.Query    `json:"queries"`
	Report        investigation.Report `json:"report"`
}

// NewOutcome extracts the comparable fields from a finished run.
func NewOutcome(sample string, attempt int, digest, model string, run investigation.Run) Outcome {
	outcome := Outcome{Sample: sample, Attempt: attempt, BundleDigest: digest, Model: model, Policy: investigation.PolicyVersion,
		Seconds: run.FinishedAt.Sub(run.CreatedAt).Seconds(), ModelCalls: len(run.ModelCalls), Steps: run.Steps,
		Queries: []telemetry.Query{}, Report: run.Report}
	for _, call := range run.ModelCalls {
		outcome.ChargedTokens += call.Charged
		if call.Usage != nil {
			outcome.InputTokens += call.Usage.InputTokens
			outcome.OutputTokens += call.Usage.OutputTokens
		}
	}
	for _, entry := range run.Evidence {
		outcome.Queries = append(outcome.Queries, entry.Query)
	}
	return outcome
}

// Encode writes stable, indented JSON for files people read and diff.
func Encode(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, errors.New("cannot encode evaluation record")
	}
	return append(data, '\n'), nil
}
