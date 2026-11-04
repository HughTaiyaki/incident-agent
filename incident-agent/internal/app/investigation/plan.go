package investigation

import (
	"encoding/json"
	"slices"

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
	FillCalleePool    = "CALLEE_POOL"
)

const (
	planMetric = "rpc_error_rate"
	// planTraces and the pool metrics are bounded so that the plan fits the 16 backend requests
	// a run may make: 5 log sweeps, 1 search, 4 metrics, 1 cut-off log query, 3 traces, 2 pool.
	planTraces = 3
	// PlannedDecisionLimit lets every allowed backend request happen and still leaves the
	// report its decisions. Planned queries cost no model call, so model limits do not move.
	PlannedDecisionLimit = telemetry.MaxBackendRequests + 1 + reportDecisions
)

// planCoreMetrics are checked on the service in the request: slow calls and work piling up
// may be the only sign of a fault, with nothing in the logs.
var planCoreMetrics = []string{"rpc_latency_p95", "workflow_backlog", "workflow_oldest_age"}

// planPoolMetrics are checked on the service that a failed call points to.
var planPoolMetrics = []string{"db_pool_utilization", "db_connection_wait_rate"}

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
	for _, name := range planCoreMetrics {
		if !metricAsked(view.Evidence, view.Request.Service, name) {
			return &telemetry.Query{Kind: "metrics", Service: view.Request.Service, Metric: name}, ""
		}
	}
	// A cut-off sweep may have lost the errors behind routine events. Once per run, so that
	// a noisy system still leaves a step for opening a trace.
	for _, entry := range view.Evidence {
		if entry.Query.Kind == "logs" && entry.Query.Notable && entry.Quality.Truncated && !asked {
			return &telemetry.Query{Kind: "logs", Service: entry.Query.Service, Severity: "ERROR"}, FillTruncatedLogs
		}
	}
	if len(opened) >= planTraces {
		return plannedPoolQuery(view)
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
	if query, reason := fromLogs(func(telemetry.Record) bool { return true }, FillDefaultTrace); query != nil {
		return query, reason
	}
	return plannedPoolQuery(view)
}

func metricAsked(evidence []telemetry.Result, service, metric string) bool {
	return slices.ContainsFunc(evidence, func(entry telemetry.Result) bool {
		return entry.Query.Metric == metric && entry.Query.Service == service
	})
}

// plannedPoolQuery looks at the connections of the service a failed call points to, or of the
// service in the request when no call failed.
func plannedPoolQuery(view View) (*telemetry.Query, string) {
	service := view.Request.Service
	for _, relation := range CallRelations(view.Evidence) {
		if relation.failed && relation.Callee != "" && slices.Contains(view.Request.Services, relation.Callee) {
			service = relation.Callee
			break
		}
	}
	for _, name := range planPoolMetrics {
		if !metricAsked(view.Evidence, service, name) {
			return &telemetry.Query{Kind: "metrics", Service: service, Metric: name}, FillCalleePool
		}
	}
	return nil, ""
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
