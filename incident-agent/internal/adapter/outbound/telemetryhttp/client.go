// Package telemetryhttp queries fixed telemetry endpoints using guarded HTTP GET requests.
package telemetryhttp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// Config comes from trusted process configuration, never from a Query.
type Config struct {
	PrometheusURL string
	LokiURL       string
	JaegerURL     string
	Policy        telemetry.Policy
	Timeout       time.Duration
}

// Client holds immutable endpoint configuration; request budgets are supplied per collection.
type Client struct {
	endpoints map[string]*url.URL
	policy    telemetry.Policy
	http      *http.Client
	timeout   time.Duration
}

// New validates fixed endpoints and disables redirects and ambient HTTP proxies.
func New(cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Timeout < 0 || cfg.Timeout > 5*time.Second {
		return nil, errors.New("timeout must be within five seconds")
	}
	if len(cfg.Policy.Services) == 0 || len(cfg.Policy.Environments) == 0 {
		return nil, errors.New("query policy is required")
	}
	endpoints := map[string]*url.URL{}
	for source, raw := range map[string]string{"prometheus": cfg.PrometheusURL, "loki": cfg.LokiURL, "jaeger": cfg.JaegerURL} {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, errors.New("invalid telemetry endpoint")
		}
		validScheme := u.Scheme == "http" || u.Scheme == "https"
		unsafeParts := u.User != nil || u.RawQuery != "" || u.Fragment != ""
		if !validScheme || u.Host == "" || unsafeParts {
			return nil, errors.New("invalid telemetry endpoint")
		}
		endpoints[source] = u
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: cfg.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: cfg.Timeout, ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConns: 6, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
	}
	policy := telemetry.Policy{Services: slices.Clone(cfg.Policy.Services), Environments: slices.Clone(cfg.Policy.Environments)}
	return &Client{endpoints: endpoints, policy: policy, timeout: cfg.Timeout, http: &http.Client{
		Transport: transport, Timeout: cfg.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Close releases idle connections. In-flight calls remain controlled by their contexts.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Query validates first, charges every request, filters responses and seals evidence.
func (c *Client) Query(ctx context.Context, input telemetry.Query, budget *telemetry.Budget) (telemetry.Result, error) {
	q, err := c.policy.Normalize(input)
	if err != nil {
		return telemetry.Result{}, err
	}
	source, path, values := request(q)
	endpoint := *c.endpoints[source]
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = values.Encode()
	canonical := path + "?" + values.Encode()
	result := telemetry.NewResult(q, source, canonical)
	data, attempts, status, err := c.get(ctx, endpoint.String(), budget)
	result.Requests = attempts
	if status != 0 {
		result.Quality.Reachability = "REACHABLE"
	}
	if err != nil {
		result.ErrorCode = errorCode(err)
		if status == 0 && !errors.Is(err, telemetry.ErrBudget) {
			result.Quality.Reachability = "UNREACHABLE"
		}
		result.Finish()
		return result, err
	}
	switch source {
	case "prometheus":
		err = c.metrics(data, &result)
	case "loki":
		err = c.logs(data, &result)
	case "jaeger":
		if status == http.StatusNotFound && q.Kind == "trace" {
			data = []byte(`{"data":[]}`)
		}
		err = c.traces(data, &result)
	}
	if err != nil {
		result.Records = []telemetry.Record{}
		result.ErrorCode = "INVALID_RESPONSE"
		result.Finish()
		return result, telemetry.ErrResponse
	}
	result.Quality.QueryOutcome = "SUCCEEDED"
	result.Finish()
	return result, nil
}

func request(q telemetry.Query) (string, string, url.Values) {
	v := url.Values{}
	switch q.Kind {
	case "metrics":
		expression, _ := telemetry.MetricExpression(q) // Normalize already validated the template.
		v.Set("query", expression)
		v.Set("start", q.Start.Format(time.RFC3339Nano))
		v.Set("end", q.End.Format(time.RFC3339Nano))
		v.Set("step", strconv.Itoa(q.StepSeconds))
		v.Set("timeout", "5s")
		v.Set("limit", strconv.Itoa(q.Limit))
		return "prometheus", "/api/v1/query_range", v
	case "logs":
		expression := "{service_name=" + strconv.Quote(q.Service) + ",deployment_environment_name=" + strconv.Quote(q.Environment) + "}"
		for _, filter := range logFilters(q) {
			if filter.value != "" {
				expression += " | " + filter.key + "=" + strconv.Quote(filter.value)
			}
		}
		if q.Notable {
			expression += ` | regexp "^(?P<lifecycle>` + serviceStartLog + `)" | event_name!="" or severity_text=~"WARN|ERROR|FATAL" or lifecycle!=""`
		}
		v.Set("query", expression)
		v.Set("start", strconv.FormatInt(q.Start.UnixNano(), 10))
		v.Set("end", strconv.FormatInt(q.End.UnixNano(), 10))
		v.Set("limit", strconv.Itoa(q.Limit))
		v.Set("direction", "forward")
		return "loki", "/loki/api/v1/query_range", v
	case "trace":
		return "jaeger", "/api/traces/" + q.TraceID, v
	default:
		v.Set("service", q.Service)
		v.Set("start", strconv.FormatInt(q.Start.UnixMicro(), 10))
		v.Set("end", strconv.FormatInt(q.End.UnixMicro(), 10))
		v.Set("limit", strconv.Itoa(q.Limit))
		tags, _ := json.Marshal(map[string]string{"deployment.environment.name": q.Environment})
		v.Set("tags", string(tags))
		return "jaeger", "/api/traces", v
	}
}

func (c *Client) get(ctx context.Context, address string, budget *telemetry.Budget) ([]byte, int, int, error) {
	var status int
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, attempt, status, err
		}
		if err := budget.Reserve(); err != nil {
			return nil, attempt, status, err
		}
		requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
		data, code, err := c.getOnce(requestCtx, address)
		cancel()
		status = code
		retry := code == 429 || code == 502 || code == 503 || code == 504
		if !retry || attempt == 1 {
			return data, attempt + 1, status, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, attempt + 1, status, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, 2, status, telemetry.ErrUnavailable
}

func (c *Client) getOnce(ctx context.Context, address string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, 0, telemetry.ErrInvalidQuery
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Never propagate url.Error: its URL can contain private correlation IDs.
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, telemetry.ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, resp.StatusCode, nil
		}
		return nil, resp.StatusCode, telemetry.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, telemetry.MaxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, telemetry.ErrUnavailable
	}
	if len(data) > telemetry.MaxResponseBytes {
		return nil, resp.StatusCode, telemetry.ErrTooLarge
	}
	return data, resp.StatusCode, nil
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, telemetry.ErrBudget):
		return "BUDGET_EXHAUSTED"
	case errors.Is(err, telemetry.ErrTooLarge):
		return "RESPONSE_TOO_LARGE"
	default:
		return "SOURCE_UNAVAILABLE"
	}
}
