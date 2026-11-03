// Package modelhttp implements an explicitly configured Chat Completions JSON provider.
package modelhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/deepseektoken"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

// MaxResponseBytes bounds the complete provider response body.
const MaxResponseBytes = 256 << 10

// busyDelays spaces the retries of a request the provider refused because it is overloaded.
// They fit inside one decision's limit and leave most of it for the request that gets through.
var busyDelays = []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second}

var modelName = regexp.MustCompile(`^[a-zA-Z0-9_./:-]{1,128}$`)

// Request shapes differ between providers that all accept the Chat Completions path.
const (
	ProfileOpenAI   = "openai"
	ProfileDeepSeek = "deepseek"
	// ProfileDeepSeekThinking keeps thinking on. Thinking counts against max_tokens and takes
	// minutes, so it needs larger limits than a deployment has; only evaluation sets them.
	ProfileDeepSeekThinking = "deepseek-thinking"
)

// ParseProfile normalizes the operator's choice; empty selects the original request shape.
func ParseProfile(value string) (string, error) {
	switch value {
	case "", ProfileOpenAI:
		return ProfileOpenAI, nil
	case ProfileDeepSeek, ProfileDeepSeekThinking:
		return value, nil
	}
	return "", errors.New("unknown model API profile")
}

// Config is operator-controlled; no model decision or source text can change it.
type Config struct {
	Endpoint string
	APIKey   string
	Model    string
	Profile  string
	// Timeout overrides the single-request limit; zero keeps the 30-second default.
	Timeout time.Duration
}

// Client sends bounded completions to one operator-configured endpoint.
type Client struct {
	endpoint  string
	apiKey    string
	model     string
	profile   string
	http      *http.Client
	transport *http.Transport
	counter   investigation.TokenCounter
	busy      []time.Duration
}

// New disables proxy inheritance and redirects. The only retry is of a request the provider
// refused as overloaded, which it did not process; a timeout's cost is unknown, so it is never retried here.
func New(config Config) (*Client, error) {
	profile, err := ParseProfile(config.Profile)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" ||
		!modelName.MatchString(config.Model) || strings.ContainsAny(config.APIKey, "\r\n") || len(config.APIKey) > 4096 {
		return nil, errors.New("invalid model provider configuration")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("model endpoint requires HTTPS or local loopback HTTP")
	}
	if config.APIKey == "" && !loopback {
		return nil, errors.New("model API key is required")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout, MaxConnsPerHost: 1,
		MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}
	client := &http.Client{Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c := &Client{endpoint: u.String(), apiKey: config.APIKey, model: config.Model, profile: profile, http: client, transport: transport, busy: busyDelays}
	if profile == ProfileDeepSeek || profile == ProfileDeepSeekThinking {
		counter, err := deepseektoken.Load()
		if err != nil {
			return nil, err
		}
		c.counter = counter
	}
	return c, nil
}

// TokenCounter returns the profile's exact counter, or nil when its tokenizer is unknown.
func (c *Client) TokenCounter() investigation.TokenCounter { return c.counter }

// Close releases idle provider connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type completionBody struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
	MaxTokens           int       `json:"max_tokens,omitempty"`
	ResponseFormat      struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Stream      bool      `json:"stream"`
	Store       *bool     `json:"store,omitempty"`
	Thinking    *thinking `json:"thinking,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type thinking struct {
	Type string `json:"type"`
}

// requestBody builds the provider's shape. DeepSeek ignores max_completion_tokens and
// counts reasoning against max_tokens, so its limit only holds with thinking disabled.
func (c *Client) requestBody(input investigation.CompletionRequest) completionBody {
	body := completionBody{Model: c.model, Messages: []message{{Role: "system", Content: input.Instructions}, {Role: "user", Content: string(input.Input)}}}
	body.ResponseFormat.Type = "json_object"
	if c.profile == ProfileDeepSeekThinking {
		body.MaxTokens = input.MaxOutputTokens
		body.Thinking = &thinking{Type: "enabled"}
		return body
	}
	if c.profile == ProfileDeepSeek {
		body.MaxTokens = input.MaxOutputTokens
		body.Thinking = &thinking{Type: "disabled"}
		// Same evidence should lead to the same investigation as far as the provider allows.
		temperature := 0.0
		body.Temperature = &temperature
		return body
	}
	store := false
	body.MaxCompletionTokens = input.MaxOutputTokens
	body.Store = &store
	return body
}

type completionResult struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string          `json:"content"`
			Refusal   *string         `json:"refusal"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

func failure(code string, busyRetries int) error {
	return &investigation.ProviderError{Code: code, BusyRetries: busyRetries}
}

// statusFailure labels a non-200 status by class; the body is untrusted and is not read.
func statusFailure(status int) string {
	switch {
	case status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests:
		return "BUSY"
	case status >= 500:
		return "HTTP_5XX"
	case status >= 400:
		return "HTTP_4XX"
	}
	return "HTTP_OTHER"
}

// post sends the request, waiting and resending while the provider reports it is overloaded.
// It also returns how many of those resends it made.
func (c *Client) post(ctx context.Context, encoded []byte) (*http.Response, int, error) {
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
		if err != nil {
			return nil, attempt, err
		}
		request.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		response, err := c.http.Do(request)
		if err != nil {
			return nil, attempt, err
		}
		busy := response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusTooManyRequests
		if !busy || attempt >= len(c.busy) {
			return response, attempt, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponseBytes))
		_ = response.Body.Close()
		wait := time.NewTimer(c.busy[attempt])
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, attempt, ctx.Err()
		case <-wait.C:
		}
	}
}

// Complete performs one bounded request without redirect.
func (c *Client) Complete(ctx context.Context, input investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return investigation.CompletionResponse{}, err
	}
	if input.MaxOutputTokens < 1 || input.MaxOutputTokens > investigation.MaxConfigurableOutputTokens ||
		len(input.Instructions)+len(input.Input) > 64000 || !json.Valid(input.Input) {
		return investigation.CompletionResponse{}, failure("REQUEST", 0)
	}
	encoded, err := json.Marshal(c.requestBody(input))
	if err != nil {
		return investigation.CompletionResponse{}, failure("REQUEST", 0)
	}
	response, retries, err := c.post(ctx, encoded)
	if err != nil {
		return investigation.CompletionResponse{}, failure("NETWORK", retries)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return investigation.CompletionResponse{}, failure(statusFailure(response.StatusCode), retries)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(data) > MaxResponseBytes {
		return investigation.CompletionResponse{}, failure("RESPONSE", retries)
	}
	var decoded completionResult
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Choices) != 1 {
		return investigation.CompletionResponse{}, failure("RESPONSE", retries)
	}
	choice := decoded.Choices[0]
	if choice.FinishReason == "length" {
		return investigation.CompletionResponse{}, failure("TRUNCATED", retries)
	}
	if choice.Message.Refusal != nil || choice.FinishReason == "content_filter" {
		return investigation.CompletionResponse{}, failure("REFUSED", retries)
	}
	if choice.FinishReason != "stop" || choice.Message.Content == "" ||
		(len(choice.Message.ToolCalls) > 0 && string(choice.Message.ToolCalls) != "null" && string(choice.Message.ToolCalls) != "[]") {
		return investigation.CompletionResponse{}, failure("RESPONSE", retries)
	}
	result := investigation.CompletionResponse{Decision: json.RawMessage(choice.Message.Content), BusyRetries: retries}
	if decoded.Usage != nil {
		if decoded.Usage.PromptTokens == nil || decoded.Usage.CompletionTokens == nil {
			return investigation.CompletionResponse{}, failure("RESPONSE", retries)
		}
		result.Usage = &investigation.ModelUsage{InputTokens: *decoded.Usage.PromptTokens, OutputTokens: *decoded.Usage.CompletionTokens}
	}
	return result, nil
}
