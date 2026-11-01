// Command incident-api serves health endpoints and, once configured, the investigation API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/inbound/httpapi"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/redisstate"
	"github.com/hughtaiyaki/incident-agent/internal/app"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	defaultPort = "8080"
	// Plain HTTP carries the bearer token, so only local listeners are the default.
	defaultBind   = "127.0.0.1"
	minTokenBytes = 32
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	deps, closeDeps, err := loadDeps(os.Getenv)
	if err != nil {
		logger.Error("load configuration", "error", err)
		return 1
	}
	defer closeDeps()

	application, err := app.New(address(os.Getenv), httpapi.NewRouter(deps), logger)
	if err != nil {
		logger.Error("create application", "error", err)
		return 1
	}

	if err := application.Run(ctx); err != nil {
		logger.Error("run application", "error", err)
		return 1
	}
	return 0
}

func address(getenv func(string) string) string {
	bind, port := getenv("INCIDENT_API_BIND"), getenv("PORT")
	if bind == "" {
		bind = defaultBind
	}
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(bind, port)
}

// loadDeps enables the investigation API only when a token is set. Identity must
// match the worker's configuration, or the worker will refuse the jobs it finds.
func loadDeps(getenv func(string) string) (*httpapi.Deps, func(), error) {
	token := getenv("INCIDENT_API_TOKEN")
	if token == "" {
		return nil, func() {}, nil
	}
	if len(token) < minTokenBytes {
		return nil, nil, errors.New("INCIDENT_API_TOKEN must be at least 32 bytes")
	}
	maxActive, err := strconv.Atoi(getenv("INCIDENT_MAX_ACTIVE_RUNS"))
	if err != nil || maxActive < 1 || maxActive > worker.MaxActiveLimit {
		return nil, nil, errors.New("explicit INCIDENT_MAX_ACTIVE_RUNS required")
	}
	model, endpoint := getenv("INCIDENT_MODEL_NAME"), getenv("INCIDENT_MODEL_ENDPOINT")
	window, err := strconv.Atoi(getenv("INCIDENT_MODEL_CONTEXT_WINDOW"))
	if err != nil || model == "" || endpoint == "" {
		return nil, nil, errors.New("explicit INCIDENT_MODEL_NAME, INCIDENT_MODEL_ENDPOINT and INCIDENT_MODEL_CONTEXT_WINDOW required")
	}
	prometheus, loki, jaeger := getenv("INCIDENT_PROMETHEUS_URL"), getenv("INCIDENT_LOKI_URL"), getenv("INCIDENT_JAEGER_URL")
	if prometheus == "" || loki == "" || jaeger == "" {
		return nil, nil, errors.New("explicit INCIDENT_PROMETHEUS_URL, INCIDENT_LOKI_URL and INCIDENT_JAEGER_URL required")
	}
	store, err := redisstate.New(redisstate.Config{Network: getenv("INCIDENT_REDIS_NETWORK"), Address: getenv("INCIDENT_REDIS_ADDRESS"), Namespace: getenv("INCIDENT_REDIS_NAMESPACE"), Username: getenv("INCIDENT_REDIS_USERNAME"), Password: getenv("INCIDENT_REDIS_PASSWORD")})
	if err != nil {
		return nil, nil, err
	}
	deps := &httpapi.Deps{
		Token: token, Store: store, MaxActive: maxActive, Readiness: store.Readiness,
		Now: time.Now,
		Template: worker.Template{
			Model: model, Window: window, ProviderID: worker.ProviderIdentity(endpoint),
			TelemetryID: worker.TelemetryIdentity(prometheus, loki, jaeger), Services: telemetry.DefaultPolicy().Services,
		},
	}
	return deps, func() { _ = store.Close() }, nil
}
