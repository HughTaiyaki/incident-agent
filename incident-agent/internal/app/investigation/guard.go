package investigation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// ErrInvalidInput rejects invalid operator scope or execution configuration.
var ErrInvalidInput = errors.New("invalid investigation input")

// ErrInvalidDecision rejects an untrusted decision without echoing its contents.
var ErrInvalidDecision = errors.New("invalid investigation decision")
var sensitiveText = regexp.MustCompile(`(?i)(bearer\s+|-----BEGIN .*PRIVATE KEY|(?:password|api[_-]?key|secret|token)\s*[:=]|failpoint|goods\.reserve\.after-commit|eventbus\.[a-z.]+after-commit|order\.[a-z.]+after-insert)`)

// Decode rejects oversized, unknown and trailing fields without echoing untrusted input.
func Decode(data []byte, value any) error {
	if len(data) > MaxDecisionBytes || !utf8.Valid(data) {
		return ErrInvalidDecision
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return ErrInvalidDecision
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return ErrInvalidDecision
	}
	return nil
}

// ValidateRequest returns the frozen service allowlist for this run.
func ValidateRequest(r Request) (telemetry.Policy, error) {
	if !safeText(r.Summary, 2000) || len(r.Services) == 0 || len(r.Services) > 5 || !slices.Contains(r.Services, r.Service) {
		return telemetry.Policy{}, ErrInvalidInput
	}
	seen := map[string]bool{}
	for _, service := range r.Services {
		if seen[service] {
			return telemetry.Policy{}, ErrInvalidInput
		}
		seen[service] = true
		// Which names are allowed is the deployment's choice; see SystemProfile.
		if !telemetry.ValidScopeName(service) || !telemetry.ValidScopeName(r.Environment) {
			return telemetry.Policy{}, ErrInvalidInput
		}
		_, err := telemetry.Policy{Services: []string{service}, Environments: []string{r.Environment}}.Normalize(telemetry.Query{Kind: "logs", Service: service, Environment: r.Environment, Start: r.Start, End: r.End})
		if err != nil {
			return telemetry.Policy{}, ErrInvalidInput
		}
	}
	return telemetry.Policy{Services: slices.Clone(r.Services), Environments: []string{r.Environment}}, nil
}

func safeText(text string, limit int) bool {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > limit || sensitiveText.MatchString(text) {
		return false
	}
	for _, char := range text {
		if unicode.IsControl(char) && char != '\n' && char != '\t' {
			return false
		}
	}
	return true
}

func decodeDecision(raw []byte) (Decision, error) {
	var d Decision
	if err := Decode(raw, &d); err != nil {
		return d, err
	}
	if d.Kind == synthesizeKind && d.Query == nil && d.Report == nil {
		return d, nil
	}
	if (d.Kind == "query" && d.Query != nil && d.Report == nil) || (d.Kind == "report" && d.Report != nil && d.Query == nil) {
		return d, nil
	}
	return Decision{}, ErrInvalidDecision
}

// synthesizeKind is how an investigation ends itself and asks to write the report.
const synthesizeKind = "synthesize"

// reportRejected is the outcome code for a decoded report that failed validation.
const reportRejected = "REPORT_REJECTED"

// reportRejection is a rejection with a fixed reason the model can act on.
type reportRejection struct{ reason string }

func (r reportRejection) Error() string { return ErrInvalidDecision.Error() + ": " + r.reason }

// Is keeps every rejection recognizable as an invalid decision.
func (reportRejection) Is(target error) bool { return target == ErrInvalidDecision }

var (
	errReportResult   = reportRejection{"RESULT"}
	errReportCitation = reportRejection{"CITATION"}
	errReportTimeline = reportRejection{"TIMELINE"}
	errReportText     = reportRejection{"TEXT"}
)

// ValidateReport verifies structural grounding, not whether evidence semantically proves a claim.
// Recovery comparison is intentionally unavailable in this offline slice.
func ValidateReport(d Draft, evidence []telemetry.Result) error {
	data, err := json.Marshal(d)
	if err != nil || len(data) > MaxDecisionBytes {
		return errReportResult
	}
	if (d.Result != "DIAGNOSED" && d.Result != "INCONCLUSIVE") || d.Recovery != "UNKNOWN" ||
		len(d.Facts) > 20 || len(d.Timeline) > 20 || len(d.Alternatives) > 3 || len(d.Unknowns) > 20 || len(d.NextActions) > 10 {
		return errReportResult
	}
	if d.Result == "DIAGNOSED" && (d.Primary == nil || len(d.Facts) == 0) {
		return errReportResult
	}
	if d.Result == "INCONCLUSIVE" && len(d.Unknowns) == 0 {
		return errReportResult
	}
	records := map[string]telemetry.Record{}
	for _, entry := range evidence {
		if entry.Quality.QueryOutcome != "SUCCEEDED" {
			continue
		}
		for _, record := range entry.Records {
			records[record.ID] = record
		}
	}
	refsValid := func(ids []string) bool {
		if len(ids) == 0 || len(ids) > 20 {
			return false
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if _, ok := records[id]; !ok || seen[id] {
				return false
			}
			seen[id] = true
		}
		return true
	}
	claimValid := func(c Claim) bool { return safeText(c.Text, 2000) && refsValid(c.EvidenceIDs) }
	hypothesisValid := func(h Hypothesis) bool {
		if !claimValid(h.Claim) || !slices.Contains([]string{"LOW", "MEDIUM", "HIGH"}, h.Strength) {
			return false
		}
		if len(h.ContradictingIDs) > 0 && !refsValid(h.ContradictingIDs) {
			return false
		}
		for _, id := range h.ContradictingIDs {
			if slices.Contains(h.Claim.EvidenceIDs, id) {
				return false
			}
		}
		return true
	}
	for _, fact := range d.Facts {
		if !claimValid(fact) {
			return errReportCitation
		}
	}
	for i, item := range d.Timeline {
		if !claimValid(item.Claim) || item.At.IsZero() || (i > 0 && item.At.Before(d.Timeline[i-1].At)) {
			return errReportTimeline
		}
		matches := false
		for _, id := range item.Claim.EvidenceIDs {
			matches = matches || item.At.Equal(records[id].At)
		}
		if !matches {
			return errReportTimeline
		}
	}
	if d.Primary != nil && !hypothesisValid(*d.Primary) {
		return errReportCitation
	}
	if d.NarrowedTo != nil && !claimValid(*d.NarrowedTo) {
		return errReportCitation
	}
	for _, h := range d.Alternatives {
		if !hypothesisValid(h) {
			return errReportCitation
		}
	}
	for _, values := range [][]string{d.Unknowns, d.NextActions} {
		for _, value := range values {
			if !safeText(value, 2000) {
				return errReportText
			}
		}
	}
	return nil
}

// salvage keeps what a rejected report got right when no repair is left: claims are validated
// one by one, and a report whose hypothesis fails cannot stay DIAGNOSED. One bad citation used
// to discard sound facts along with it.
func salvage(rejected *Draft, evidence []telemetry.Result) Draft {
	if rejected == nil {
		return fallback()
	}
	const removed = "报告中引用无效的内容已被移除，结论需结合冻结证据人工核对。"
	probe := func(d Draft) bool {
		d.Result, d.Recovery, d.Unknowns = "INCONCLUSIVE", "UNKNOWN", []string{removed}
		return ValidateReport(d, evidence) == nil
	}
	out := fallback()
	out.Unknowns = []string{removed}
	for _, fact := range rejected.Facts {
		if probe(Draft{Facts: []Claim{fact}}) && len(out.Facts) < 20 {
			out.Facts = append(out.Facts, fact)
		}
	}
	for _, unknown := range rejected.Unknowns {
		if probe(Draft{Unknowns: []string{unknown}}) && safeText(unknown, 2000) && len(out.Unknowns) < 20 {
			out.Unknowns = append(out.Unknowns, unknown)
		}
	}
	if rejected.Primary != nil && len(out.Facts) > 0 && probe(Draft{Primary: rejected.Primary}) {
		out.Result, out.Primary = "DIAGNOSED", rejected.Primary
	}
	if ValidateReport(out, evidence) != nil {
		return fallback()
	}
	return out
}

func fallback() Draft {
	return Draft{Result: "INCONCLUSIVE", Facts: []Claim{}, Timeline: []TimelineItem{}, Alternatives: []Hypothesis{},
		Unknowns:    []string{"现有证据或有效决策不足以形成可核验结论；来源缺口见冻结查询结果。"},
		NextActions: []string{"人工核对本次证据的范围、来源质量与缺失字段后，再发起独立调查。"}, Recovery: "UNKNOWN"}
}
