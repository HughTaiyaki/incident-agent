package telemetry

import (
	"math"
	"regexp"
	"slices"
	"strings"
)

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var fieldPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,128}$`)
var spanIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

// ValidateRecord constrains imported evidence to the same structural contract as online adapters.
// Hashes establish integrity only; they do not make fixture content trusted.
func ValidateRecord(record Record, q Query, policy Policy) error {
	if !policy.Allows(record.Service, record.Environment, record.At, q) {
		return ErrResponse
	}
	if q.Kind != "trace" && record.Service != q.Service {
		return ErrResponse
	}
	types := map[string]string{"metrics": "metric", "logs": "log", "traces": "trace_summary", "trace": "span"}
	if types[q.Kind] != record.Type {
		return ErrResponse
	}
	if record.TraceID != "" && !traceID.MatchString(record.TraceID) {
		return ErrResponse
	}
	if record.SpanID != "" && !spanIDPattern.MatchString(record.SpanID) {
		return ErrResponse
	}
	if q.TraceID != "" && record.TraceID != q.TraceID {
		return ErrResponse
	}
	if record.Value != nil && (math.IsNaN(*record.Value) || math.IsInf(*record.Value, 0)) {
		return ErrResponse
	}
	allowed := map[string][]string{
		"metric": {"metric_key", "pool", "step", "peer", "method", "outcome", "value_status"},
		"log": {"body_omitted", "source_record_digest", "event_name", "error_type", "severity_text", "outcome",
			"from", "to", "state", "step", "replayed", "attempt", "consumer", "event_type", "order_id", "operation_id", "event_id"},
		"span": {"duration_us", "operation", "error", "error.type", "otel.status_code", "rpc.system", "rpc.service",
			"rpc.method", "messaging.operation", "messaging.message.id", "order.no", "operation_id", "payment.id"},
		"trace_summary": {"visible_spans"},
	}
	for key, value := range record.Fields {
		if !slices.Contains(allowed[record.Type], key) || !fieldPattern.MatchString(value) {
			return ErrResponse
		}
		for _, marker := range []string{"failpoint", "password", "secret", "bearer", "token", "@"} {
			if strings.Contains(strings.ToLower(value), marker) {
				return ErrResponse
			}
		}
	}
	for _, reference := range record.References {
		if reference.Type != "CHILD_OF" && reference.Type != "FOLLOWS_FROM" {
			return ErrResponse
		}
		if reference.TraceID == "" && reference.SpanID == "" {
			continue
		}
		if !traceID.MatchString(reference.TraceID) || !spanIDPattern.MatchString(reference.SpanID) {
			return ErrResponse
		}
	}
	return nil
}
