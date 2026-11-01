package investigation

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// AggregationVersion identifies the deterministic log grouping contract.
const AggregationVersion = "log-patterns-v1"

// PatternKey deliberately excludes high-cardinality correlation IDs and free text.
type PatternKey struct {
	Service   string `json:"service"`
	EventName string `json:"event_name"`
	ErrorType string `json:"error_type"`
	Severity  string `json:"severity"`
}

// LogPattern describes only the observed sample, never a whole-window backend count.
type LogPattern struct {
	Key           PatternKey `json:"key"`
	ObservedCount int        `json:"observed_count"`
	FirstAt       time.Time  `json:"observed_first_at"`
	LastAt        time.Time  `json:"observed_last_at"`
	RecordIDs     []string   `json:"record_ids"`
}

// LogPatterns retains query and response identity so repeated queries cannot be confused.
type LogPatterns struct {
	Version        string            `json:"aggregation_version"`
	QueryID        string            `json:"query_id"`
	ResultDigest   string            `json:"result_digest"`
	Query          telemetry.Query   `json:"query"`
	SourceStatus   string            `json:"source_status"`
	SourceQuality  telemetry.Quality `json:"source_quality"`
	CountBasis     string            `json:"count_basis"`
	ScannedRecords int               `json:"scanned_records"`
	OmittedGroups  int               `json:"omitted_groups"`
	Truncated      bool              `json:"truncated"`
	Groups         []LogPattern      `json:"groups"`
}

type patternGroup struct {
	key             PatternKey
	records         []telemetry.Record
	normalCandidate bool
}

// SummarizeLogs validates a frozen query result, then derives deterministic bounded summaries.
// It never queries a backend, infers absent fields or turns missing data into a healthy result.
func SummarizeLogs(entry telemetry.Result) (LogPatterns, error) {
	q, err := telemetry.DefaultPolicy().Normalize(entry.Query)
	if err != nil || q.Kind != "logs" || !validResult(entry, q, telemetry.DefaultPolicy()) {
		return LogPatterns{}, errors.New("invalid log evidence for aggregation")
	}
	quality := entry.Quality
	quality.MissingReasons = append([]string{}, quality.MissingReasons...)
	if quality.LatestEventAt != nil {
		at := *quality.LatestEventAt
		quality.LatestEventAt = &at
	}
	result := LogPatterns{Version: AggregationVersion, QueryID: entry.QueryID, ResultDigest: entry.Digest,
		Query: q, SourceStatus: entry.Status, SourceQuality: quality, CountBasis: "OBSERVED_SAMPLE",
		ScannedRecords: len(entry.Records), Truncated: quality.Truncated, Groups: []LogPattern{}}
	groups := map[PatternKey]*patternGroup{}
	for _, record := range entry.Records {
		key := PatternKey{Service: record.Service, EventName: field(record, "event_name"),
			ErrorType: field(record, "error_type"), Severity: field(record, "severity_text")}
		group := groups[key]
		if group == nil {
			group = &patternGroup{key: key, records: []telemetry.Record{}}
			groups[key] = group
		}
		group.records = append(group.records, record)
		// Reserve visibility for explicit success signals, not an asserted recovery assessment.
		outcome := record.Fields["outcome"]
		if key.ErrorType == "unknown" && (key.Severity == "INFO" || key.Severity == "DEBUG") &&
			(outcome == "success" || outcome == "replayed" || outcome == "recovered") {
			group.normalCandidate = true
		}
	}
	ordered := make([]*patternGroup, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i].records) != len(ordered[j].records) {
			return len(ordered[i].records) > len(ordered[j].records)
		}
		return keyString(ordered[i].key) < keyString(ordered[j].key)
	})
	if len(ordered) > 10 {
		selectedNormal := false
		for _, group := range ordered[:10] {
			selectedNormal = selectedNormal || group.normalCandidate
		}
		if !selectedNormal {
			for _, group := range ordered[10:] {
				if group.normalCandidate {
					ordered[9] = group
					break
				}
			}
		}
		result.OmittedGroups = len(ordered) - 10
		result.Truncated = true
		ordered = ordered[:10]
	}
	for _, group := range ordered {
		sort.Slice(group.records, func(i, j int) bool {
			if !group.records[i].At.Equal(group.records[j].At) {
				return group.records[i].At.Before(group.records[j].At)
			}
			return group.records[i].ID < group.records[j].ID
		})
		records := group.records
		result.Groups = append(result.Groups, LogPattern{Key: group.key, ObservedCount: len(records),
			FirstAt: records[0].At, LastAt: records[len(records)-1].At, RecordIDs: representatives(records)})
	}
	return result, nil
}

func field(record telemetry.Record, name string) string {
	if value := record.Fields[name]; value != "" {
		return value
	}
	return "unknown"
}

func keyString(key PatternKey) string {
	data, _ := json.Marshal(key)
	return string(data)
}

func representatives(records []telemetry.Record) []string {
	selected := map[string]bool{records[0].ID: true, records[len(records)-1].ID: true}
	outcomes := map[string]bool{field(records[0], "outcome"): true, field(records[len(records)-1], "outcome"): true}
	for _, record := range records {
		if len(selected) >= 3 {
			break
		}
		if !outcomes[field(record, "outcome")] {
			selected[record.ID] = true
			outcomes[field(record, "outcome")] = true
		}
	}
	for _, record := range records {
		if len(selected) >= 3 {
			break
		}
		selected[record.ID] = true
	}
	ids := []string{}
	for _, record := range records {
		if selected[record.ID] {
			ids = append(ids, record.ID)
			delete(selected, record.ID)
		}
	}
	return ids
}
