package investigation

import (
	"encoding/json"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// Flows say who chooses the queries in a run. An empty flow is the original agent flow.
const (
	FlowAgent  = "agent"
	FlowHybrid = "hybrid"
)

// ValidFlow reports whether a configured flow exists.
func ValidFlow(flow string) bool { return flow == "" || flow == FlowAgent || flow == FlowHybrid }

// Fill reasons say why the hybrid flow chose a query beyond its fixed part.
const (
	FillTruncatedLogs = "TRUNCATED_LOGS"
	FillSevereLog     = "SEVERE_LOG_TRACE"
	FillErrorTrace    = "ERROR_TRACE"
	FillRetryTrace    = "RETRY_TRACE"
	FillDefaultTrace  = "DEFAULT_TRACE"
)

const planMetric = "rpc_error_rate"

// PlannedDecision is the next step of the hybrid flow: the same log sweep and error-rate check
// every time plus a trace search, then the remaining query steps spent on gaps the program can
// see in the evidence. No model is asked whether anything is missing, because one that has not
// seen a record cannot tell. It is derived only from saved evidence, so a resumed run continues
// where it stopped.
func PlannedDecision(view View) (json.RawMessage, string, error) {
	decision := Decision{Kind: synthesizeKind}
	reason := ""
	if sweep := SweepDecision(view); sweep != nil {
		return sweep, "", nil
	}
	if query, why := NextPlannedQuery(view); query != nil {
		decision, reason = Decision{Kind: "query", Query: query}, why
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil, "", ErrInvalidDecision
	}
	return encoded, reason, nil
}

func severeRecord(record telemetry.Record) bool {
	severity := record.Fields["severity_text"]
	return severity == "ERROR" || severity == "FATAL" || severity == "WARN"
}

// NextPlannedQuery checks the gaps in order. Each rule reads only evidence fields and names no
// service or case. The retry rule depends on logs carrying an attempt field, so it stays silent
// on a system that does not write one.
func NextPlannedQuery(view View) (*telemetry.Query, string) {
	opened := map[string]bool{}
	asked, metric, searched := false, false, false
	for _, entry := range view.Evidence {
		metric = metric || entry.Query.Metric == planMetric
		searched = searched || entry.Query.Kind == "traces"
		if entry.Query.Kind == "trace" {
			opened[entry.Query.TraceID] = true
		}
		asked = asked || (entry.Query.Kind == "logs" && entry.Query.Severity != "")
	}
	if !metric {
		return &telemetry.Query{Kind: "metrics", Service: view.Request.Service, Metric: planMetric}, ""
	}
	if !searched {
		return &telemetry.Query{Kind: "traces", Service: view.Request.Service}, ""
	}
	// A cut-off sweep may have lost the errors behind routine events. Once per run, so that
	// a noisy system still leaves a step for opening a trace.
	for _, entry := range view.Evidence {
		if entry.Query.Kind == "logs" && entry.Query.Notable && entry.Quality.Truncated && !asked {
			return &telemetry.Query{Kind: "logs", Service: entry.Query.Service, Severity: "ERROR"}, FillTruncatedLogs
		}
	}
	fromLogs := func(pick func(telemetry.Record) bool, reason string) (*telemetry.Query, string) {
		for _, entry := range view.Evidence {
			if entry.Query.Kind != "logs" {
				continue
			}
			for _, record := range entry.Records {
				if record.TraceID != "" && !opened[record.TraceID] && pick(record) {
					return &telemetry.Query{Kind: "trace", Service: entry.Query.Service, TraceID: record.TraceID}, reason
				}
			}
		}
		return nil, ""
	}
	if !openedSevereTrace(view.Evidence, opened) {
		if query, reason := fromLogs(severeRecord, FillSevereLog); query != nil {
			return query, reason
		}
	}
	// A failed span need not leave any warning in the logs, as a handled conflict does not.
	for _, entry := range view.Evidence {
		if entry.Query.Kind != "traces" {
			continue
		}
		for _, record := range entry.Records {
			if record.Fields["error_spans"] != "" && !opened[record.TraceID] {
				return &telemetry.Query{Kind: "trace", Service: entry.Query.Service, TraceID: record.TraceID}, FillErrorTrace
			}
		}
	}
	// A retry with no failure on record: its trace may show what the first attempt did.
	if query, reason := fromLogs(func(record telemetry.Record) bool { return record.Fields["attempt"] != "" }, FillRetryTrace); query != nil {
		return query, reason
	}
	if query, reason := fromLogs(severeRecord, FillSevereLog); query != nil {
		return query, reason
	}
	return fromLogs(func(telemetry.Record) bool { return true }, FillDefaultTrace)
}

// openedSevereTrace reports whether a trace behind a warning or error is already open.
func openedSevereTrace(evidence []telemetry.Result, opened map[string]bool) bool {
	for _, entry := range evidence {
		for _, record := range entry.Records {
			if record.Type == "log" && opened[record.TraceID] && severeRecord(record) {
				return true
			}
		}
	}
	return false
}
