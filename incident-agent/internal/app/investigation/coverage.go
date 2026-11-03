package investigation

import (
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// severeLogLimit bounds how many report records are set aside for distinct warnings and errors.
const severeLogLimit = 12

// Coverage tells the report writer what this run did not see, measured by the program, because
// a model that has not seen a record cannot notice it is missing.
type Coverage struct {
	FailedQueries        int `json:"failed_queries"`
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
		if entry.Status != "" && entry.Status != "AVAILABLE" {
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
