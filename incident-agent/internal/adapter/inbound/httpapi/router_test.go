package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
)

const testToken = "test-token-with-enough-length-0123456789"

type fakeStore struct {
	reply    runstate.Reply
	err      error
	commands []runstate.Command
}

func (s *fakeStore) Execute(_ context.Context, cmd runstate.Command) (runstate.Reply, error) {
	s.commands = append(s.commands, cmd)
	return s.reply, s.err
}

func (*fakeStore) Candidates(context.Context, int) ([]string, error) { return nil, nil }

func testDeps(store *fakeStore) *Deps {
	return &Deps{
		Token: testToken, Store: store, MaxActive: 2,
		Template:  worker.Template{Model: "m", Window: 32000, ProviderID: "p", TelemetryID: "t", Services: []string{"order-service", "user-service"}, Environments: []string{"local", "experiment"}},
		Readiness: func(context.Context) []string { return nil },
		Now:       func() time.Time { return time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC) },
	}
}

const validBody = `{"alert_id":"alert-1","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"orders failing"}`

func call(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestHealthRoutes(t *testing.T) {
	t.Parallel()
	unconfigured := NewRouter(nil)
	if got := call(t, unconfigured, http.MethodGet, "/healthz", "", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz: %d %s", got.Code, got.Body)
	}
	got := call(t, unconfigured, http.MethodGet, "/readyz", "", "")
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "not_configured") {
		t.Fatalf("unconfigured readiness claimed ready: %d %s", got.Code, got.Body)
	}
	if got := call(t, unconfigured, http.MethodPost, "/v1/incidents", testToken, validBody); got.Code != http.StatusNotFound {
		t.Fatalf("investigation route without configuration: %d", got.Code)
	}
}

func TestReadinessReportsReasonCodes(t *testing.T) {
	t.Parallel()
	deps := testDeps(&fakeStore{})
	if got := call(t, NewRouter(deps), http.MethodGet, "/readyz", "", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"ready"`) {
		t.Fatalf("ready: %d %s", got.Code, got.Body)
	}
	deps.Readiness = func(context.Context) []string { return []string{"worker_absent"} }
	got := call(t, NewRouter(deps), http.MethodGet, "/readyz", "", "")
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "worker_absent") {
		t.Fatalf("not ready: %d %s", got.Code, got.Body)
	}
}

func TestRoutesRequireCredential(t *testing.T) {
	t.Parallel()
	store := &fakeStore{reply: runstate.Reply{Code: "OK"}}
	handler := NewRouter(testDeps(store))
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/incidents", validBody},
		{http.MethodGet, "/v1/runs/run1", ""},
		{http.MethodGet, "/v1/runs/run1/report", ""},
		{http.MethodGet, "/v1/runs/run1/evidence/q1", ""},
		{http.MethodPost, "/v1/runs/run1/cancel", ""},
	}
	for _, route := range routes {
		for _, token := range []string{"", "wrong", testToken + "x"} {
			got := call(t, handler, route.method, route.path, token, route.body)
			if got.Code != http.StatusUnauthorized || got.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s token=%q: %d", route.method, route.path, token, got.Code)
			}
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/runs/run1", nil)
	request.Header.Set("Authorization", "Basic "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("basic scheme accepted: %d", response.Code)
	}
	if len(store.commands) != 0 {
		t.Fatal("unauthenticated request reached storage")
	}
}

func TestSubmitRejectsBeforeStorage(t *testing.T) {
	t.Parallel()
	store := &fakeStore{reply: runstate.Reply{Code: "OK"}}
	handler := NewRouter(testDeps(store))
	long := strings.Repeat("x", 2001)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":        {`{`, http.StatusBadRequest},
		"unknown field":   {`{"alert_id":"a","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"s","query":"up"}`, http.StatusBadRequest},
		"bad alert id":    {`{"alert_id":"a b","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"s"}`, http.StatusBadRequest},
		"other service":   {`{"alert_id":"a","service":"admin-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"s"}`, http.StatusBadRequest},
		"other env":       {`{"alert_id":"a","service":"order-service","environment":"prod","started_at":"2026-10-28T06:30:00Z","summary":"s"}`, http.StatusBadRequest},
		"future start":    {`{"alert_id":"a","service":"order-service","environment":"local","started_at":"2026-10-28T09:00:00Z","summary":"s"}`, http.StatusBadRequest},
		"no start":        {`{"alert_id":"a","service":"order-service","environment":"local","summary":"s"}`, http.StatusBadRequest},
		"window too wide": {`{"alert_id":"a","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"s","initial_window_minutes":51}`, http.StatusBadRequest},
		"long summary":    {`{"alert_id":"a","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"` + long + `"}`, http.StatusBadRequest},
		"secret summary":  {`{"alert_id":"a","service":"order-service","environment":"local","started_at":"2026-10-28T06:30:00Z","summary":"token=abc"}`, http.StatusBadRequest},
		"too large":       {`{"summary":"` + strings.Repeat("x", maxSubmissionBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		if got := call(t, handler, http.MethodPost, "/v1/incidents", testToken, tc.body); got.Code != tc.want {
			t.Fatalf("%s: %d %s", name, got.Code, got.Body)
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/incidents", strings.NewReader(validBody))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", response.Code)
	}
	if len(store.commands) != 0 {
		t.Fatalf("invalid submissions reached storage: %+v", store.commands)
	}
}

func TestFailureMappingHidesDetail(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]struct {
		status int
		body   string
	}{
		"NOT_FOUND":      {http.StatusNotFound, "RUN_NOT_FOUND"},
		"CORRUPT":        {http.StatusInternalServerError, "STATE_CORRUPT"},
		"UNKNOWN_SCHEMA": {http.StatusInternalServerError, "STATE_UNKNOWN_SCHEMA"},
		"WRONGTYPE":      {http.StatusServiceUnavailable, "STORE_UNAVAILABLE"},
	} {
		handler := NewRouter(testDeps(&fakeStore{reply: runstate.Reply{Code: code}}))
		got := call(t, handler, http.MethodGet, "/v1/runs/run1", testToken, "")
		if got.Code != want.status || !strings.Contains(got.Body.String(), want.body) {
			t.Fatalf("%s: %d %s", code, got.Code, got.Body)
		}
	}
	store := &fakeStore{err: errors.New("dial unix /private/path: secret detail")}
	got := call(t, NewRouter(testDeps(store)), http.MethodGet, "/v1/runs/run1", testToken, "")
	if got.Code != http.StatusServiceUnavailable || strings.Contains(got.Body.String(), "secret") {
		t.Fatalf("store error leaked: %d %s", got.Code, got.Body)
	}
	if got := call(t, NewRouter(testDeps(store)), http.MethodGet, "/v1/runs/bad%20id", testToken, ""); got.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", got.Code)
	}
}

func TestOverloadTellsClientWhenToRetry(t *testing.T) {
	t.Parallel()
	handler := NewRouter(testDeps(&fakeStore{reply: runstate.Reply{Code: "OVER_CAPACITY"}}))
	got := call(t, handler, http.MethodPost, "/v1/incidents", testToken, validBody)
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "OVERLOADED") || got.Header().Get("Retry-After") == "" {
		t.Fatalf("overload: %d %s", got.Code, got.Body)
	}
}

func TestMetricsRequireCredentialAndReportOutage(t *testing.T) {
	t.Parallel()
	deps := testDeps(&fakeStore{})
	deps.Stats = func(context.Context) (runstate.Stats, error) {
		return runstate.Stats{Ready: 3, WorkerAlive: true, Counters: map[string]int64{"runs:COMPLETED": 2}}, nil
	}
	router := NewRouter(deps)
	if got := call(t, router, http.MethodGet, "/metrics", "", ""); got.Code != http.StatusUnauthorized {
		t.Fatal(got.Code)
	}
	got := call(t, router, http.MethodGet, "/metrics", testToken, "")
	for _, want := range []string{"incident_redis_up 1", `incident_runs_active{state="ready"} 3`, `incident_runs_finished_total{status="COMPLETED"} 2`, "# TYPE incident_runs_finished_total counter"} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Fatalf("%d missing %q: %s", got.Code, want, got.Body)
		}
	}
	deps.Stats = func(context.Context) (runstate.Stats, error) { return runstate.Stats{}, errors.New("private detail") }
	got = call(t, NewRouter(deps), http.MethodGet, "/metrics", testToken, "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "incident_redis_up 0") || strings.Contains(got.Body.String(), "private") {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if got := call(t, NewRouter(testDeps(&fakeStore{})), http.MethodGet, "/metrics", testToken, ""); got.Code != http.StatusNotFound {
		t.Fatal("metrics exposed without a source:", got.Code)
	}
}
