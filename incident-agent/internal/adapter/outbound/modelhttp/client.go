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

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

// MaxResponseBytes bounds the complete provider response body.
const MaxResponseBytes = 256 << 10

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
}

// New disables proxy inheritance, redirects and automatic application retries.
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
	return &Client{endpoint: u.String(), apiKey: config.APIKey, model: config.Model, profile: profile, http: client, transport: transport}, nil
}

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

// Complete performs one request without redirect or automatic retry.
func (c *Client) Complete(ctx context.Context, input investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return investigation.CompletionResponse{}, err
	}
	if input.MaxOutputTokens < 1 || input.MaxOutputTokens > investigation.MaxConfigurableOutputTokens ||
		len(input.Instructions)+len(input.Input) > 64000 || !json.Valid(input.Input) {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	encoded, err := json.Marshal(c.requestBody(input))
	if err != nil {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	request.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(data) > MaxResponseBytes {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	var decoded completionResult
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Choices) != 1 {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	choice := decoded.Choices[0]
	if choice.FinishReason != "stop" || choice.Message.Refusal != nil || choice.Message.Content == "" ||
		(len(choice.Message.ToolCalls) > 0 && string(choice.Message.ToolCalls) != "null" && string(choice.Message.ToolCalls) != "[]") {
		return investigation.CompletionResponse{}, investigation.ErrModelProvider
	}
	result := investigation.CompletionResponse{Decision: json.RawMessage(choice.Message.Content)}
	if decoded.Usage != nil {
		if decoded.Usage.PromptTokens == nil || decoded.Usage.CompletionTokens == nil {
			return investigation.CompletionResponse{}, investigation.ErrModelProvider
		}
		result.Usage = &investigation.ModelUsage{InputTokens: *decoded.Usage.PromptTokens, OutputTokens: *decoded.Usage.CompletionTokens}
	}
	return result, nil
}
