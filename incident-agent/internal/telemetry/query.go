// Package telemetry defines bounded, backend-independent telemetry queries and evidence.
package telemetry

import (
	"errors"
	"regexp"
	"slices"
	"sync"
	"time"
)

// Telemetry policy version and upper bounds apply to every query adapter.
const (
	PolicyVersion    = "telemetry-v2.1/adapter-v1"
	MaxResponseBytes = 2 << 20
	MaxRecords       = 200
)

var (
	// ErrInvalidQuery rejects queries outside the fixed policy.
	ErrInvalidQuery = errors.New("invalid telemetry query")
	// ErrBudget indicates exhausted backend request allowance.
	ErrBudget = errors.New("backend request budget exhausted")
	// ErrUnavailable indicates missing or unreachable telemetry.
	ErrUnavailable = errors.New("telemetry source unavailable")
	// ErrResponse indicates an invalid backend response.
	ErrResponse = errors.New("invalid telemetry response")
	// ErrTooLarge indicates the response exceeded a fixed limit.
	ErrTooLarge = errors.New("telemetry response exceeds limit")
	identifier  = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)
	traceID     = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// Query contains values only; backend expressions and addresses are never caller-controlled.
type Query struct {
	Kind         string    `json:"kind"`
	Service      string    `json:"service"`
	Environment  string    `json:"environment"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Limit        int       `json:"limit,omitempty"`
	Metric       string    `json:"metric,omitempty"`
	StepSeconds  int       `json:"step_seconds,omitempty"`
	Peer         string    `json:"peer,omitempty"`
	WorkflowStep string    `json:"workflow_step,omitempty"`
	Pool         string    `json:"pool,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`
	OrderID      string    `json:"order_id,omitempty"`
	OperationID  string    `json:"operation_id,omitempty"`
	EventID      string    `json:"event_id,omitempty"`
	EventName    string    `json:"event_name,omitempty"`
	Severity     string    `json:"severity,omitempty"`
	// Notable keeps only warnings, errors and named events. Under load routine access logs
	// otherwise fill the earliest records, which are all a logs query returns.
	Notable bool `json:"notable,omitempty"`
}

// NotableLog reports whether a log record explains something rather than being routine.
func NotableLog(fields map[string]string) bool {
	switch fields["severity_text"] {
	case "WARN", "ERROR", "FATAL":
		return true
	}
	return fields["event_name"] != ""
}

// Policy is configured by the operator, never by telemetry text.
type Policy struct {
	Services     []string
	Environments []string
}

// DefaultPolicy is the initial single-deployment allowlist.
func DefaultPolicy() Policy {
	return Policy{
		Services:     []string{"gateway-h5", "order-service", "goods-service", "user-service", "payment-simulator"},
		Environments: []string{"local", "experiment"},
	}
}

// Normalize rejects invalid input before any network request and freezes defaults.
func (p Policy) Normalize(q Query) (Query, error) {
	if !slices.Contains(p.Services, q.Service) || !slices.Contains(p.Environments, q.Environment) {
		return Query{}, ErrInvalidQuery
	}
	validWindow := !q.Start.IsZero() && q.End.After(q.Start) && q.End.Sub(q.Start) <= time.Hour
	if !validWindow || q.Start.Year() < 2000 || q.End.Year() > 2100 {
		return Query{}, ErrInvalidQuery
	}
	q.Start, q.End = q.Start.UTC(), q.End.UTC()
	if q.Peer != "" && !slices.Contains(p.Services, q.Peer) {
		return Query{}, ErrInvalidQuery
	}
	if q.Limit < 0 || q.StepSeconds < 0 {
		return Query{}, ErrInvalidQuery
	}
	hasLogFields := q.OrderID != "" || q.OperationID != "" || q.EventID != "" || q.EventName != "" || q.Severity != "" || q.Notable
	hasMetricFields := q.Metric != "" || q.StepSeconds != 0 || q.Peer != "" || q.WorkflowStep != "" || q.Pool != ""
	switch q.Kind {
	case "metrics":
		if hasLogFields || q.TraceID != "" {
			return Query{}, ErrInvalidQuery
		}
		if _, err := MetricExpression(q); err != nil {
			return Query{}, err
		}
		if q.StepSeconds == 0 {
			q.StepSeconds = 15
		}
		points := int(q.End.Sub(q.Start)/time.Second)/q.StepSeconds + 1
		if q.StepSeconds > 3600 || points > 240 {
			return Query{}, ErrInvalidQuery
		}
		if q.Limit == 0 {
			q.Limit = 20
		}
		if q.Limit > 20 {
			return Query{}, ErrInvalidQuery
		}
	case "logs":
		if hasMetricFields {
			return Query{}, ErrInvalidQuery
		}
		if q.Limit == 0 {
			q.Limit = 100
		}
		if q.Limit > 100 {
			return Query{}, ErrInvalidQuery
		}
		for _, value := range []string{q.OrderID, q.OperationID, q.EventID, q.EventName, q.Severity} {
			if value != "" && !identifier.MatchString(value) {
				return Query{}, ErrInvalidQuery
			}
		}
		if q.Severity != "" && !slices.Contains([]string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}, q.Severity) {
			return Query{}, ErrInvalidQuery
		}
	case "traces", "trace":
		if hasMetricFields || hasLogFields {
			return Query{}, ErrInvalidQuery
		}
		maxLimit := 20
		if q.Kind == "trace" {
			maxLimit = 200
		}
		if q.Limit == 0 {
			q.Limit = maxLimit
		}
		if q.Limit > maxLimit {
			return Query{}, ErrInvalidQuery
		}
		if q.Kind == "trace" && q.TraceID == "" {
			return Query{}, ErrInvalidQuery
		}
		if q.Kind == "traces" && q.TraceID != "" {
			return Query{}, ErrInvalidQuery
		}
	default:
		return Query{}, ErrInvalidQuery
	}
	if q.TraceID != "" && !traceID.MatchString(q.TraceID) {
		return Query{}, ErrInvalidQuery
	}
	return q, nil
}

// Allows verifies each returned record, not just the request selector.
func (p Policy) Allows(service, environment string, at time.Time, q Query) bool {
	return slices.Contains(p.Services, service) && environment == q.Environment &&
		!at.Before(q.Start) && !at.After(q.End)
}

// Budget accounts for actual requests, including every retry; it is safe to share.
type Budget struct {
	mu        sync.Mutex
	remaining int
	used      int
	reserve   func() error
}

// NewBudget creates a bounded request budget; nonpositive limits permit no requests.
func NewBudget(limit int) *Budget { return &Budget{remaining: max(0, limit)} }

// RestoreBudget preserves consumed requests and optionally journals every new reservation.
// The hook must finish successfully before any backend request is allowed.
func RestoreBudget(limit, used int, reserve func() error) *Budget {
	used = max(0, used)
	return &Budget{remaining: max(0, limit-used), used: used, reserve: reserve}
}

// Reserve charges before an external request. Failed requests are not refunded.
func (b *Budget) Reserve() error {
	if b == nil {
		return ErrBudget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining == 0 {
		return ErrBudget
	}
	if b.reserve != nil {
		if err := b.reserve(); err != nil {
			return err
		}
	}
	b.remaining--
	b.used++
	return nil
}

// Used returns the number of reserved backend requests.
func (b *Budget) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
