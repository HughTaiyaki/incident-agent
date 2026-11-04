package investigation

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// severeLogLimit bounds how many report records are set aside for distinct warnings and errors.
const severeLogLimit = 12

// Coverage tells the report writer what this run did not see, measured by the program, because
// a model that has not seen a record cannot notice it is missing.
type Coverage struct {
	FailedQueries        int `json:"failed_queries"`
	EmptyOrPartial       int `json:"empty_or_partial_queries"`
	TruncatedQueries     int `json:"truncated_queries"`
	ErrorTracesNotOpened int `json:"error_traces_not_opened"`
	SevereLogsNotShown   int `json:"severe_log_kinds_not_shown"`
	CallsMissingASide    int `json:"calls_missing_a_side"`
}

func severeLog(record telemetry.Record) bool {
	switch record.Fields["severity_text"] {
	case "WARN", "ERROR", "FATAL":
		return record.Type == "log"
	}
	return false
}

// severeLogKinds returns one record per distinct warning or error, most severe first.
func severeLogKinds(evidence []telemetry.Result) []telemetry.Record {
	seen := map[string]bool{}
	kinds := []telemetry.Record{}
	for _, severities := range [][]string{{"FATAL", "ERROR"}, {"WARN"}} {
		for _, entry := range evidence {
			for _, record := range entry.Records {
				severity := record.Fields["severity_text"]
				key := record.Service + "\x00" + severity + "\x00" + record.Fields["message"]
				if !severeLog(record) || seen[key] || (severity != severities[0] && severity != severities[len(severities)-1]) {
					continue
				}
				seen[key] = true
				kinds = append(kinds, record)
			}
		}
	}
	return kinds
}

// measureCoverage counts the gaps that can be seen from the evidence itself. It cannot count
// what no query could have shown: all zeros means no known gap, not that nothing is missing.
func measureCoverage(view View, shown map[string]bool) Coverage {
	coverage := Coverage{}
	opened := map[string]bool{}
	for _, entry := range view.Evidence {
		if entry.Query.Kind == "trace" {
			opened[entry.Query.TraceID] = true
		}
		switch entry.Status {
		case "EMPTY", "PARTIAL":
			coverage.EmptyOrPartial++
		case "", "AVAILABLE":
		default:
			coverage.FailedQueries++
		}
		if entry.Quality.Truncated {
			coverage.TruncatedQueries++
		}
	}
	for _, entry := range view.Evidence {
		for _, record := range entry.Records {
			if record.Type == "trace_summary" && record.Fields["error_spans"] != "" && !opened[record.TraceID] {
				coverage.ErrorTracesNotOpened++
			}
		}
	}
	for _, record := range severeLogKinds(view.Evidence) {
		if !shown[record.ID] {
			coverage.SevereLogsNotShown++
		}
	}
	for _, relation := range CallRelations(view.Evidence) {
		if relation.CallerSpan == "" || relation.CalleeSpan == "" {
			coverage.CallsMissingASide++
		}
	}
	return coverage
}

// metricSignalLimit bounds the series summarized for a report.
const metricSignalLimit = 12

// MetricSignal summarizes one queried series in numbers. It says what changed, not why.
type MetricSignal struct {
	Service string  `json:"service"`
	Metric  string  `json:"metric"`
	Labels  string  `json:"labels,omitempty"`
	First   float64 `json:"first"`
	Last    float64 `json:"last"`
	Max     float64 `json:"max"`
	Points  int     `json:"points"`
	Rising  bool    `json:"rising,omitempty"`
}

// MetricCoverage separates what was seen from what returned nothing and what nobody asked,
// because a metric nobody queried says nothing about whether it was healthy.
type MetricCoverage struct {
	Signals                []MetricSignal `json:"signals"`
	OmittedSignals         int            `json:"omitted_signals,omitempty"`
	WithoutData            []string       `json:"without_data"`
	NotChecked             []string       `json:"not_checked"`
	ServicesWithoutMetrics []string       `json:"services_without_metrics"`
}

func measureMetrics(view View) MetricCoverage {
	coverage := MetricCoverage{Signals: []MetricSignal{}, WithoutData: []string{}, NotChecked: []string{}, ServicesWithoutMetrics: []string{}}
	type series struct {
		signal  MetricSignal
		falling bool
	}
	found, order := map[string]*series{}, []string{}
	asked := map[string]bool{}
	checkedService := map[string]bool{}
	for _, entry := range view.Evidence {
		if entry.Query.Kind != "metrics" {
			continue
		}
		name := entry.Query.Service + "/" + entry.Query.Metric
		asked[name], checkedService[entry.Query.Service] = true, true
		points := 0
		for _, record := range entry.Records {
			if record.Value == nil {
				continue
			}
			points++
			labels := []string{}
			for _, key := range []string{"step", "pool", "peer", "method", "outcome"} {
				if value := record.Fields[key]; value != "" {
					labels = append(labels, key+"="+value)
				}
			}
			label := strings.Join(labels, ",")
			key := name + "\x00" + label
			current := found[key]
			if current == nil {
				current = &series{signal: MetricSignal{Service: entry.Query.Service, Metric: entry.Query.Metric, Labels: label, First: *record.Value, Max: *record.Value}}
				found[key] = current
				order = append(order, key)
			}
			current.falling = current.falling || (current.signal.Points > 0 && *record.Value < current.signal.Last)
			current.signal.Last = *record.Value
			current.signal.Max = max(current.signal.Max, *record.Value)
			current.signal.Points++
		}
		if points == 0 {
			coverage.WithoutData = append(coverage.WithoutData, name)
		}
	}
	for _, key := range order {
		signal := found[key].signal
		signal.Rising = signal.Points >= 3 && !found[key].falling && signal.Last > signal.First
		coverage.Signals = append(coverage.Signals, signal)
	}
	slices.SortStableFunc(coverage.Signals, func(a, b MetricSignal) int {
		if a.Rising != b.Rising {
			if a.Rising {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.Max, a.Max)
	})
	if len(coverage.Signals) > metricSignalLimit {
		coverage.OmittedSignals = len(coverage.Signals) - metricSignalLimit
		coverage.Signals = coverage.Signals[:metricSignalLimit]
	}
	for _, metric := range telemetry.MetricCatalog() {
		if !asked[view.Request.Service+"/"+metric] {
			coverage.NotChecked = append(coverage.NotChecked, view.Request.Service+"/"+metric)
		}
	}
	for _, service := range view.Request.Services {
		if !checkedService[service] {
			coverage.ServicesWithoutMetrics = append(coverage.ServicesWithoutMetrics, service)
		}
	}
	return coverage
}

var (
	claimedDuration = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s?(ms|milliseconds?|毫秒|s|sec|secs|seconds?|秒)\b`)
	claimedPercent  = regexp.MustCompile(`(\d+(?:\.\d+)?)\s?%`)
	claimedAbsence  = regexp.MustCompile(`(?i)\b(no|without|zero)\s+(recorded\s+|technical\s+|rpc\s+|other\s+)*(errors?|faults?|failures?)\b|all\s+(recorded\s+)?(calls|steps)\s+succeeded|error-free|没有(任何)?(错误|故障)`)
	scopedAbsence   = regexp.MustCompile(`(?i)\b(shown|cited|sampled|checked|visible|among)\b|已查|所查|展示`)
)

// numberTolerance allows for rounding such as 1203 ms written as 1.2 s.
const numberTolerance = 0.15

func nearly(claimed, recorded float64) bool {
	return math.Abs(claimed-recorded) <= math.Max(recorded*numberTolerance, 5)
}

// CheckClaims returns a rejection code for what can be checked mechanically: durations and
// percentages that match nothing recorded, and flat claims of no errors over parts of the
// system this run did not see. Whether a cause follows from its evidence stays with people.
func CheckClaims(d Draft, evidence []telemetry.Result, expectations []CallExpectation) string {
	claims := []Claim{}
	if d.Primary != nil {
		claims = append(claims, d.Primary.Claim)
	}
	claims = append(claims, d.Facts...)
	for _, claim := range claims {
		if code := checkClaim(claim, evidence, expectations); code != "" {
			return code
		}
	}
	return ""
}

func checkClaim(claim Claim, evidence []telemetry.Result, expectations []CallExpectation) string {
	durations, percents := recordedNumbers(evidence, expectations)
	for _, match := range claimedDuration.FindAllStringSubmatch(claim.Text, -1) {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		unit := strings.ToLower(match[2])
		if !strings.HasPrefix(unit, "m") && unit != "毫秒" {
			value *= 1000
		}
		if !slices.ContainsFunc(durations, func(recorded float64) bool { return nearly(value, recorded) }) {
			return "NUMBER"
		}
	}
	for _, match := range claimedPercent.FindAllStringSubmatch(claim.Text, -1) {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		if !slices.ContainsFunc(percents, func(recorded float64) bool { return nearly(value, recorded*100) || nearly(value, recorded) }) {
			return "NUMBER"
		}
	}
	if claimedAbsence.MatchString(claim.Text) && !scopedAbsence.MatchString(claim.Text) {
		seen := measureCoverage(View{Evidence: evidence}, nil)
		if seen.FailedQueries > 0 || seen.TruncatedQueries > 0 || seen.ErrorTracesNotOpened > 0 {
			return "ABSENCE"
		}
	}
	return ""
}

var writtenNumber = regexp.MustCompile(`(\d+(?:\.\d+)?)\s?(ms|s)?`)

// recordedNumbers collects every duration in milliseconds and every metric value in the
// evidence. Numbers inside messages count too, since a log line may state a timeout itself.
func recordedNumbers(evidence []telemetry.Result, expectations []CallExpectation) (durations, percents []float64) {
	for _, expectation := range expectations {
		durations = append(durations, float64(expectation.ExpectedMs))
	}
	for _, entry := range evidence {
		for _, record := range entry.Records {
			if record.Value != nil {
				percents = append(percents, *record.Value)
			}
			if duration, ok := spanDuration(record); ok && record.Type == "span" {
				durations = append(durations, float64(duration)/float64(time.Millisecond))
			}
			for _, match := range writtenNumber.FindAllStringSubmatch(record.Fields["message"], -1) {
				value, err := strconv.ParseFloat(match[1], 64)
				if err != nil {
					continue
				}
				if match[2] == "s" {
					value *= 1000
				}
				durations = append(durations, value)
			}
		}
	}
	return durations, percents
}

// dropFailedClaims removes what still fails after the repairs ran out, so a report never keeps
// a claim only because the run ran out of chances to correct it.
func dropFailedClaims(d Draft, checker claimChecker, evidence []telemetry.Result) Draft {
	facts := []Claim{}
	for _, fact := range d.Facts {
		if checker.CheckClaims(Draft{Facts: []Claim{fact}}, evidence) == "" {
			facts = append(facts, fact)
		}
	}
	d.Facts = facts
	if d.Primary != nil && (len(facts) == 0 || checker.CheckClaims(Draft{Primary: d.Primary}, evidence) != "") {
		d.Primary = nil
		d.Result = "INCONCLUSIVE"
	}
	return d
}
