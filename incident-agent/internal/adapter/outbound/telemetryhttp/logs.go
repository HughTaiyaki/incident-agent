package telemetryhttp

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

var tokenPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,128}$`)
var hexTracePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var hexSpanPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func safeToken(value string) bool {
	lower := strings.ToLower(value)
	for _, blocked := range []string{"failpoint", "password", "secret", "bearer", "token", "@",
		"goods.reserve.after-commit", "order.create.after-insert", "order.payment.before-record",
		"eventbus.relay.after-publish", "eventbus.inbox.after-commit", "order.task.after-claim"} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	return tokenPattern.MatchString(value)
}

func cloneFields(fields map[string]string) map[string]string {
	cloned := make(map[string]string, len(fields))
	for k, v := range fields {
		cloned[k] = v
	}
	return cloned
}

// serviceStartLog begins the line a service writes when it registers at startup. A second one
// inside a window is the only sign of a restart: a crashing process logs nothing on its way out.
const serviceStartLog = "service register"

type logFilter struct {
	key   string
	value string
}

func logFilters(q telemetry.Query) []logFilter {
	return []logFilter{
		{key: "trace_id", value: q.TraceID}, {key: "order_id", value: q.OrderID},
		{key: "operation_id", value: q.OperationID}, {key: "event_id", value: q.EventID},
		{key: "event_name", value: q.EventName}, {key: "severity_text", value: q.Severity},
	}
}

func (c *Client) logs(data []byte, result *telemetry.Result) error {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string   `json:"stream"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil {
		return telemetry.ErrResponse
	}
	if response.Status != "success" || response.Data.ResultType != "streams" {
		return telemetry.ErrResponse
	}
	q := result.Query
	var scanned int
	for _, stream := range response.Data.Result {
		for _, tuple := range stream.Values {
			scanned++
			if scanned > q.Limit {
				result.Quality.Truncated = true
				result.Missing("record_limit_reached")
				break
			}
			if len(tuple) < 2 || len(tuple) > 3 {
				return telemetry.ErrResponse
			}
			var timestamp, body string
			if json.Unmarshal(tuple[0], &timestamp) != nil || json.Unmarshal(tuple[1], &body) != nil {
				return telemetry.ErrResponse
			}
			ns, err := strconv.ParseInt(timestamp, 10, 64)
			if err != nil {
				return telemetry.ErrResponse
			}
			at := time.Unix(0, ns).UTC()
			fields := cloneFields(stream.Stream)
			if len(tuple) == 3 {
				metadata := map[string]string{}
				if json.Unmarshal(tuple[2], &metadata) != nil {
					return telemetry.ErrResponse
				}
				for key, value := range metadata {
					if original, ok := fields[key]; ok && original != value {
						return telemetry.ErrResponse
					}
					fields[key] = value
				}
			}
			delete(fields, "lifecycle")
			if strings.HasPrefix(body, serviceStartLog) {
				fields["lifecycle"] = "service_started"
			}
			service, env := fields["service_name"], fields["deployment_environment_name"]
			if service != q.Service || !c.policy.Allows(service, env, at, q) {
				result.Missing("out_of_scope_record")
				continue
			}
			matched := true
			for _, filter := range logFilters(q) {
				if filter.value != "" && fields[filter.key] != filter.value {
					matched = false
				}
			}
			if q.Notable && !telemetry.NotableLog(fields) {
				matched = false
			}
			if !matched {
				result.Missing("filter_mismatch")
				continue
			}
			// Structural evidence is always retained. Free text may contain secrets or personal data,
			// so a body is kept only where it explains something, and only after sanitizing.
			// Routine INFO and DEBUG text adds exposure without helping a diagnosis.
			clean := map[string]string{"body_omitted": "true", "source_record_digest": telemetry.Hash([]byte(timestamp + body))}
			if fields["lifecycle"] != "" {
				// The line carries addresses and adds nothing to the fact of the start.
				clean["message"] = "service registered"
				delete(clean, "body_omitted")
			} else if telemetry.NotableLog(fields) {
				// Masking works on shape, so first remove the IDs this record names exactly.
				for _, key := range []string{"order_id", "operation_id", "event_id", "trace_id", "span_id"} {
					if value := fields[key]; value != "" {
						body = strings.ReplaceAll(body, value, "[id]")
					}
				}
				if message, ok := telemetry.SanitizeMessage(body); ok {
					clean["message"] = message
					delete(clean, "body_omitted")
				}
			}
			for _, key := range []string{"event_name", "error_type", "severity_text", "outcome", "from", "to", "state", "step",
				"replayed", "attempt", "consumer", "event_type", "order_id", "operation_id", "event_id", "lifecycle"} {
				if value := fields[key]; safeToken(value) {
					clean[key] = value
				}
			}
			record := telemetry.Record{Type: "log", At: at, Service: service, Environment: env, Fields: clean}
			if hexTracePattern.MatchString(fields["trace_id"]) {
				record.TraceID = fields["trace_id"]
			}
			if hexSpanPattern.MatchString(fields["span_id"]) {
				record.SpanID = fields["span_id"]
			}
			result.Records = append(result.Records, record)
		}
	}
	if scanned >= q.Limit {
		result.Quality.Truncated = true
		result.Missing("record_limit_reached")
	}
	if scanned > 0 {
		result.Missing("free_text_omitted")
	}
	return nil
}
