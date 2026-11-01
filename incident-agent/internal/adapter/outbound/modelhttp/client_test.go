package modelhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

func testInput() investigation.CompletionRequest {
	return investigation.CompletionRequest{Instructions: "Return JSON", Input: json.RawMessage(`{"phase":"SYNTHESIZING"}`), MaxOutputTokens: 512}
}
func TestNew(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1/chat/completions", "https://user:pass@example.com/a", "https://example.com/a?key=secret", "https://example.com/a#secret", "file:///tmp/a", "https://example.com"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := New(Config{Endpoint: endpoint, APIKey: "private", Model: "test"}); err == nil {
				t.Fatal("unsafe endpoint")
			}
		})
	}
	if _, err := New(Config{Endpoint: "https://example.com/v1/chat/completions", APIKey: "x\ny", Model: "test"}); err == nil {
		t.Fatal("header injection")
	}
}
func TestComplete(t *testing.T) {
	t.Run("bounded request and usage", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer private" {
				t.Error("request headers")
			}
			var body completionBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.MaxCompletionTokens != 512 || body.Stream || body.Store || body.ResponseFormat.Type != "json_object" || len(body.Messages) != 2 {
				t.Error("request shape")
			}
			_, _ = fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"kind\":\"report\"}"}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
		}))
		defer server.Close()
		client, err := New(Config{Endpoint: server.URL + "/chat", APIKey: "private", Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		result, err := client.Complete(t.Context(), testInput())
		if err != nil || calls != 1 || result.Usage == nil || result.Usage.InputTokens != 100 || !json.Valid(result.Decision) {
			t.Fatal(result, err)
		}
	})
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", 429, `secret=private`}, {"server failure", 503, `secret=private`},
		{"truncated", 200, `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`},
		{"refusal", 200, `{"choices":[{"finish_reason":"stop","message":{"content":"{}","refusal":"private"}}]}`},
		{"invalid JSON", 200, `not JSON`}, {"oversized", 200, strings.Repeat("x", MaxResponseBytes+1)},
		{"partial usage", 200, `{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}],"usage":{"prompt_tokens":100}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			client, err := New(Config{Endpoint: server.URL + "/chat", Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Complete(t.Context(), testInput())
			if err == nil || strings.Contains(err.Error(), "private") || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
	t.Run("redirect is not followed", func(t *testing.T) {
		destinationCalls := 0
		destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls++ }))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		client, err := New(Config{Endpoint: server.URL + "/chat", APIKey: "private", Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err = client.Complete(t.Context(), testInput()); err == nil || destinationCalls != 0 {
			t.Fatal("redirect followed")
		}
	})
	t.Run("cancel before send", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		defer server.Close()
		client, err := New(Config{Endpoint: server.URL + "/chat", Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err = client.Complete(ctx, testInput()); err == nil || calls != 0 {
			t.Fatal("cancel ignored")
		}
	})
}
