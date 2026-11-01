package telemetry

import (
	"fmt"
	"slices"
	"strconv"
)

// MetricExpression builds only reviewed templates, with fixed grouping and selectors.
func MetricExpression(q Query) (string, error) {
	for _, value := range []string{q.Service, q.Environment, q.Peer, q.Pool, q.WorkflowStep} {
		if value != "" && !identifier.MatchString(value) {
			return "", ErrInvalidQuery
		}
	}
	labels := "service=" + strconv.Quote(q.Service) + ",environment=" + strconv.Quote(q.Environment)
	group := "service,environment"
	rpc := q.Metric == "rpc_error_rate" || q.Metric == "rpc_latency_p95"
	workflow := q.Metric == "workflow_backlog" || q.Metric == "workflow_oldest_age"
	database := slices.Contains([]string{"db_pool_utilization", "db_connection_wait_rate", "db_connection_wait_seconds_rate"}, q.Metric)
	if q.Peer != "" && !rpc {
		return "", ErrInvalidQuery
	}
	if q.WorkflowStep != "" && !workflow {
		return "", ErrInvalidQuery
	}
	if q.Pool != "" && !database {
		return "", ErrInvalidQuery
	}
	if q.Peer != "" {
		labels += ",peer=" + strconv.Quote(q.Peer)
	}
	if q.WorkflowStep != "" {
		steps := []string{"reserve", "commit", "close", "tech_refund", "after_sale_refund", "outbox"}
		if !slices.Contains(steps, q.WorkflowStep) {
			return "", ErrInvalidQuery
		}
		labels += ",step=" + strconv.Quote(q.WorkflowStep)
	}
	if q.Pool != "" {
		labels += ",pool=" + strconv.Quote(q.Pool)
	}
	sumRate := func(metric, extra string) string {
		return "sum by (" + group + ") (rate(" + metric + "{" + labels + extra + "}[2m]))"
	}
	switch q.Metric {
	case "order_error_rate":
		return sumRate("trade_order_create_total", `,outcome="error"`) + " / " + sumRate("trade_order_create_total", ""), nil
	case "rpc_error_rate":
		return sumRate("trade_rpc_duration_seconds_count", `,outcome="error"`) + " / " + sumRate("trade_rpc_duration_seconds_count", ""), nil
	case "rpc_latency_p95":
		return fmt.Sprintf("histogram_quantile(0.95, sum by (le,service,environment) (rate(trade_rpc_duration_seconds_bucket{%s}[2m])))", labels), nil
	case "payment_confirmation_delay_p95":
		return fmt.Sprintf("histogram_quantile(0.95, sum by (le,service,environment) (rate(trade_payment_callback_delay_seconds_bucket{%s}[2m])))", labels), nil
	case "workflow_backlog":
		return "trade_workflow_backlog{" + labels + "}", nil
	case "workflow_oldest_age":
		return "trade_workflow_oldest_age_seconds{" + labels + "}", nil
	case "db_pool_utilization":
		return "trade_db_pool_in_use{" + labels + "} / (trade_db_pool_limit{" + labels + "} > 0)", nil
	case "db_connection_wait_rate":
		return "rate(trade_db_acquire_timeout_total{" + labels + "}[2m])", nil
	case "db_connection_wait_seconds_rate":
		return "rate(trade_db_acquire_wait_seconds_total{" + labels + "}[2m])", nil
	default:
		return "", ErrInvalidQuery
	}
}
