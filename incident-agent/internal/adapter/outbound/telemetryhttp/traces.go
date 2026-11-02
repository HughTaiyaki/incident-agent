package telemetryhttp

import (
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type tag struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type jaegerTrace struct {
	TraceID string `json:"traceID"`
	Spans   []struct {
		TraceID    string `json:"traceID"`
		SpanID     string `json:"spanID"`
		Operation  string `json:"operationName"`
		Start      int64  `json:"startTime"`
		Duration   int64  `json:"duration"`
		ProcessID  string `json:"processID"`
		Tags       []tag  `json:"tags"`
		References []struct {
			Type    string `json:"refType"`
			TraceID string `json:"traceID"`
			SpanID  string `json:"spanID"`
		} `json:"references"`
	} `json:"spans"`
	Processes map[string]struct {
		Service string `json:"serviceName"`
		Tags    []tag  `json:"tags"`
	} `json:"processes"`
}

func tagsMap(tags []tag) map[string]string {
	values := map[string]string{}
	for _, t := range tags {
		var value string
		if json.Unmarshal(t.Value, &value) == nil {
			values[t.Key] = value
			continue
		}
		var number json.Number
		if json.Unmarshal(t.Value, &number) == nil {
			values[t.Key] = number.String()
			continue
		}
		var boolean bool
		if json.Unmarshal(t.Value, &boolean) == nil {
			values[t.Key] = strconv.FormatBool(boolean)
		}
	}
	return values
}

func (c *Client) traces(data []byte, result *telemetry.Result) error {
	var response struct {
		Data   []jaegerTrace     `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope["data"] == nil {
		return telemetry.ErrResponse
	}
	if json.Unmarshal(data, &response) != nil {
		return telemetry.ErrResponse
	}
	if len(response.Errors) > 0 {
		result.Missing("backend_warning")
	}
	q := result.Query
	for index, tr := range response.Data {
		if q.Kind == "traces" && index >= q.Limit {
			result.Quality.Truncated = true
			result.Missing("trace_limit_reached")
			break
		}
		if !hexTracePattern.MatchString(tr.TraceID) {
			return telemetry.ErrResponse
		}
		if q.TraceID != "" && q.TraceID != tr.TraceID {
			result.Missing("trace_id_mismatch")
			continue
		}
		records, err := c.traceRecords(tr, result)
		if err != nil {
			return err
		}
		containsService := false
		for _, record := range records {
			if record.Service == q.Service {
				containsService = true
			}
		}
		if !containsService {
			result.Missing("requested_service_not_visible")
			continue
		}
		if q.Kind == "traces" {
			sort.Slice(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
			fields := map[string]string{"visible_spans": strconv.Itoa(len(records))}
			failed := 0
			for _, record := range records {
				if record.Fields["error"] != "" || record.Fields["otel.status_code"] != "" {
					failed++
				}
			}
			// A search result must say which trace failed, or opening one is a blind choice.
			if failed > 0 {
				fields["error_spans"] = strconv.Itoa(failed)
			}
			result.Records = append(result.Records, telemetry.Record{
				Type: "trace_summary", At: records[0].At, Service: q.Service, Environment: q.Environment, TraceID: tr.TraceID,
				Fields: fields,
			})
			continue
		}
		for _, record := range records {
			if len(result.Records) >= q.Limit {
				result.Quality.Truncated = true
				result.Missing("span_limit_reached")
				break
			}
			result.Records = append(result.Records, record)
		}
	}
	if q.Kind == "traces" && len(response.Data) >= q.Limit {
		result.Quality.Truncated = true
		result.Missing("trace_limit_reached")
	}
	return nil
}

func (c *Client) traceRecords(tr jaegerTrace, result *telemetry.Result) ([]telemetry.Record, error) {
	q := result.Query
	records := []telemetry.Record{}
	visible := map[string]bool{}
	for _, span := range tr.Spans {
		if len(records) >= 200 {
			result.Quality.Truncated = true
			result.Missing("span_limit_reached")
			break
		}
		if !hexSpanPattern.MatchString(span.SpanID) || span.TraceID != tr.TraceID {
			return nil, telemetry.ErrResponse
		}
		process, ok := tr.Processes[span.ProcessID]
		if !ok {
			result.Missing("missing_process")
			continue
		}
		environment := tagsMap(process.Tags)["deployment.environment.name"]
		at := time.UnixMicro(span.Start).UTC()
		if !c.policy.Allows(process.Service, environment, at, q) {
			result.Missing("out_of_scope_span")
			continue
		}
		if span.Duration < 0 || span.Duration > int64(time.Hour/time.Microsecond) {
			return nil, telemetry.ErrResponse
		}
		if at.Add(time.Duration(span.Duration) * time.Microsecond).After(q.End) {
			result.Missing("span_crosses_window")
			continue
		}
		fields := map[string]string{"duration_us": strconv.FormatInt(span.Duration, 10)}
		if safeToken(span.Operation) {
			fields["operation"] = span.Operation
		}
		values := tagsMap(span.Tags)
		for _, key := range []string{"error", "error.type", "otel.status_code", "rpc.grpc.status_code", "rpc.system", "rpc.service", "rpc.method",
			"messaging.operation", "messaging.message.id", "order.no", "operation_id", "payment.id"} {
			if value := values[key]; safeToken(value) {
				fields[key] = value
			}
		}
		if fields["otel.status_code"] != "" || fields["error"] != "" {
			if description, ok := telemetry.SanitizeMessage(values["otel.status_description"]); ok {
				fields["status_description"] = description
			}
		}
		references := []telemetry.Reference{}
		for _, reference := range span.References {
			validType := reference.Type == "CHILD_OF" || reference.Type == "FOLLOWS_FROM"
			if !validType || !hexTracePattern.MatchString(reference.TraceID) || !hexSpanPattern.MatchString(reference.SpanID) {
				result.Missing("invalid_reference")
				continue
			}
			references = append(references, telemetry.Reference{
				Type: reference.Type, TraceID: reference.TraceID, SpanID: reference.SpanID,
			})
		}
		records = append(records, telemetry.Record{Type: "span", At: at, Service: process.Service,
			Environment: environment, TraceID: tr.TraceID, SpanID: span.SpanID, Fields: fields, References: references})
		visible[span.SpanID] = true
	}
	for i := range records {
		for j := range records[i].References {
			reference := &records[i].References[j]
			if reference.TraceID == tr.TraceID && !visible[reference.SpanID] {
				// Preserve the relationship type but do not expose an out-of-scope span identity.
				reference.TraceID = ""
				reference.SpanID = ""
				result.Missing("reference_target_not_visible")
			}
		}
	}
	return records, nil
}
