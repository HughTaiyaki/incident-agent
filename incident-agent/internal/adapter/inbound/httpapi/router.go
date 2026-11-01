// Package httpapi provides inbound HTTP adapters for the incident service.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	maxSubmissionBytes = 16 << 10
	// Long enough for active runs to reach their 180-second deadline.
	overloadRetrySeconds = "60"
)

var runIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Deps enables the investigation API. Every field is required; Token is the
// single-tenant operator credential, compared by digest and never logged.
type Deps struct {
	Token     string
	Store     runstate.Store
	Template  worker.Template
	MaxActive int
	Readiness func(context.Context) []string
	Now       func() time.Time
	// Stats enables GET /metrics; the endpoint requires the same token as the API.
	Stats func(context.Context) (runstate.Stats, error)
}

// NewRouter returns the routes exposed by the incident API. Without deps only
// the health routes exist and the process reports itself as not configured.
func NewRouter(deps *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	readiness := func(context.Context) []string { return []string{"not_configured"} }
	if deps != nil {
		readiness = deps.Readiness
		a := &api{deps: deps, token: sha256.Sum256([]byte(deps.Token))}
		mux.Handle("POST /v1/incidents", a.authenticated(a.submit))
		mux.Handle("GET /v1/runs/{id}", a.authenticated(a.run))
		mux.Handle("GET /v1/runs/{id}/report", a.authenticated(a.report))
		mux.Handle("GET /v1/runs/{id}/evidence/{query_id}", a.authenticated(a.evidence))
		mux.Handle("POST /v1/runs/{id}/cancel", a.authenticated(a.cancel))
		if deps.Stats != nil {
			mux.Handle("GET /metrics", a.authenticated(a.metrics))
		}
	}
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if reasons := readiness(r.Context()); len(reasons) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reasons": reasons})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return mux
}

type api struct {
	deps  *Deps
	token [sha256.Size]byte
}

func (a *api) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
		digest := sha256.Sum256([]byte(credential))
		if !found || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(digest[:], a.token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
			return
		}
		next(w, r)
	})
}

func (a *api) submit(w http.ResponseWriter, r *http.Request) {
	if media, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(media) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubmissionBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	var input worker.Submission
	if investigation.Decode(data, &input) != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	submitted, err := worker.Submit(r.Context(), a.deps.Store, a.deps.Template, input, a.deps.Now(), a.deps.MaxActive)
	if err != nil {
		writeFailure(w, err)
		return
	}
	status := http.StatusOK
	if submitted.Created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, submitted)
}

// runID rejects malformed IDs before any storage call.
func runID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !runIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return "", false
	}
	return id, true
}

func (a *api) run(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	result, err := worker.ReadResult(r.Context(), a.deps.Store, id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *api) report(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	result, err := worker.ReadResult(r.Context(), a.deps.Store, id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if result.Report != nil {
		writeJSON(w, http.StatusOK, result.Report)
		return
	}
	code := "NOT_READY"
	if result.Terminal {
		// A cancelled or expired run has no report and never will.
		code = "NO_REPORT"
	}
	writeJSON(w, http.StatusConflict, map[string]string{"code": code, "status": result.Status})
}

func (a *api) evidence(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	view, err := worker.ReadEvidence(r.Context(), a.deps.Store, id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	// The same query may run twice; each saved result is its own observation.
	matches := []telemetry.Result{}
	for _, entry := range view.Evidence {
		if entry.QueryID == r.PathValue("query_id") {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		writeError(w, http.StatusNotFound, "EVIDENCE_NOT_FOUND")
		return
	}
	view.Evidence, view.LogPatterns = matches, nil
	writeJSON(w, http.StatusOK, view)
}

func (a *api) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	result, err := worker.CancelRun(r.Context(), a.deps.Store, id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeFailure maps application errors to fixed codes; internal detail stays out of responses.
func writeFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, investigation.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
	case errors.Is(err, worker.ErrNotFound):
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND")
	case errors.Is(err, worker.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, worker.ErrOverloaded):
		w.Header().Set("Retry-After", overloadRetrySeconds)
		writeError(w, http.StatusServiceUnavailable, "OVERLOADED")
	case errors.Is(err, worker.ErrCorrupt):
		writeError(w, http.StatusInternalServerError, "STATE_CORRUPT")
	case errors.Is(err, worker.ErrUnknownSchema):
		writeError(w, http.StatusInternalServerError, "STATE_UNKNOWN_SCHEMA")
	default:
		// Includes unconfirmed writes: the client may retry, and the alert ID keeps that safe.
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE")
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"code": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
