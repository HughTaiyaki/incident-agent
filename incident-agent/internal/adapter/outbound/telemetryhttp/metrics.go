package telemetryhttp

import (
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func (c *Client) metrics(data []byte, result *telemetry.Result) error {
	var response struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return telemetry.ErrResponse
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" {
		return telemetry.ErrResponse
	}
	if len(response.Warnings) > 0 {
		result.Missing("backend_warning")
	}
	q := result.Query
	if len(response.Data.Result) >= q.Limit {
		result.Quality.Truncated = true
		result.Missing("series_limit_reached")
	}
	for i, series := range response.Data.Result {
		if i >= q.Limit {
			break
		}
		if series.Metric["service"] != q.Service || series.Metric["environment"] != q.Environment {
			result.Missing("out_of_scope_series")
			continue
		}
		fields := map[string]string{"metric_key": q.Metric}
		for _, key := range []string{"pool", "step", "peer", "method", "outcome"} {
			if value := series.Metric[key]; safeToken(value) {
				fields[key] = value
			}
		}
		mismatchedFilter := q.Pool != "" && fields["pool"] != q.Pool || q.WorkflowStep != "" && fields["step"] != q.WorkflowStep
		if mismatchedFilter {
			result.Missing("filter_mismatch")
			continue
		}
		for j, pair := range series.Values {
			if j >= 240 {
				result.Quality.Truncated = true
				result.Missing("point_limit_reached")
				break
			}
			if len(pair) != 2 {
				return telemetry.ErrResponse
			}
			var stamp float64
			var raw string
			if json.Unmarshal(pair[0], &stamp) != nil || json.Unmarshal(pair[1], &raw) != nil {
				return telemetry.ErrResponse
			}
			if math.IsInf(stamp, 0) || math.IsNaN(stamp) {
				return telemetry.ErrResponse
			}
			at := time.Unix(int64(stamp), int64((stamp-math.Floor(stamp))*1e9)).UTC()
			if !c.policy.Allows(q.Service, q.Environment, at, q) {
				result.Missing("out_of_scope_point")
				continue
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return telemetry.ErrResponse
			}
			record := telemetry.Record{Type: "metric", At: at, Service: q.Service, Environment: q.Environment, Fields: fields}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				record.Fields = cloneFields(fields)
				record.Fields["value_status"] = "UNDEFINED"
				result.Missing("undefined_metric_value")
			} else {
				record.Value = &value
			}
			result.Records = append(result.Records, record)
		}
	}
	return nil
}
