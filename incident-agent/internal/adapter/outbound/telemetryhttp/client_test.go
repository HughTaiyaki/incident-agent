package telemetryhttp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func testQuery(kind string) telemetry.Query {
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	return telemetry.Query{Kind: kind, Service: "order-service", Environment: "experiment", Start: start, End: start.Add(time.Minute)}
}

func newClient(t *testing.T, handler http.HandlerFunc) *telemetryhttp.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := telemetryhttp.New(telemetryhttp.Config{
		PrometheusURL: server.URL, LokiURL: server.URL, JaegerURL: server.URL, Policy: telemetry.DefaultPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestClientRejectsBeforeRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	client := newClient(t, func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) })
	q := testQuery("logs")
	q.Service = "admin-service"
	budget := telemetry.NewBudget(16)
	_, err := client.Query(t.Context(), q, budget)
	if !errors.Is(err, telemetry.ErrInvalidQuery) || calls.Load() != 0 || budget.Used() != 0 {
		t.Fatal("invalid query reached backend")
	}
}

func TestClientRetryBudgetAndFailureQuality(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		limit   int
		want    int
		wantErr error
	}{
		{name: "one retry", limit: 16, want: 2, wantErr: telemetry.ErrUnavailable},
		{name: "retry needs budget", limit: 1, want: 1, wantErr: telemetry.ErrBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "private upstream error", 429)
			})
			result, err := client.Query(t.Context(), testQuery("logs"), telemetry.NewBudget(tt.limit))
			if !errors.Is(err, tt.wantErr) || int(calls.Load()) != tt.want || result.Requests != tt.want {
				t.Fatalf("result=%+v calls=%d error=%v", result, calls.Load(), err)
			}
			if result.Status != "UNAVAILABLE" || result.Quality.Reachability != "REACHABLE" {
				t.Fatal("failure hidden")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("raw error leaked")
			}
		})
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer target.Close()
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	_, err := client.Query(t.Context(), testQuery("logs"), telemetry.NewBudget(16))
	if err == nil || redirected.Load() != 0 {
		t.Fatal("redirect escaped fixed endpoint")
	}
}

func TestClientBoundsResponseAndCancellation(t *testing.T) {
	t.Parallel()
	t.Run("large response", func(t *testing.T) {
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, strings.Repeat("x", telemetry.MaxResponseBytes+1))
		})
		_, err := client.Query(t.Context(), testQuery("logs"), telemetry.NewBudget(16))
		if !errors.Is(err, telemetry.ErrTooLarge) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("cancelled before request", func(t *testing.T) {
		client := newClient(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("unexpected request") })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		budget := telemetry.NewBudget(16)
		_, err := client.Query(ctx, testQuery("logs"), budget)
		if !errors.Is(err, context.Canceled) || budget.Used() != 0 {
			t.Fatalf("error=%v used=%d", err, budget.Used())
		}
	})
	t.Run("cancel in flight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		client := newClient(t, func(_ http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() })
		_, err := client.Query(ctx, testQuery("logs"), telemetry.NewBudget(16))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
}
