// Command incident-api serves health endpoints and, once configured, the investigation API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/inbound/httpapi"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/modelhttp"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/profilefile"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/redisstate"
	"github.com/hughtaiyaki/incident-agent/internal/app"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/secretenv"
)

const (
	defaultPort = "8080"
	// Plain HTTP carries the bearer token, so only local listeners are the default.
	defaultBind   = "127.0.0.1"
	minTokenBytes = 32
	// flowEnvVar chooses who picks the queries of new runs; unset keeps the agent flow.
	flowEnvVar = "INCIDENT_INVESTIGATION_FLOW"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Getenv))
	}
	os.Exit(run())
}

// healthcheck probes the local listener for container runtimes; the image has no shell or curl.
func healthcheck(getenv func(string) string) int {
	_, port, err := net.SplitHostPort(address(getenv))
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz") //nolint:noctx // Bounded by the client timeout.
	if err != nil {
		return 1
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

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
	token, err := secretenv.Lookup(getenv, "INCIDENT_API_TOKEN")
	if err != nil {
		return nil, nil, err
	}
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
	profile, err := modelhttp.ParseProfile(getenv("INCIDENT_MODEL_API_PROFILE"))
	if err != nil {
		return nil, nil, err
	}
	if profile == modelhttp.ProfileDeepSeekThinking {
		return nil, nil, errors.New("deepseek-thinking is for evaluation: its limits are not available here")
	}
	prometheus, loki, jaeger := getenv("INCIDENT_PROMETHEUS_URL"), getenv("INCIDENT_LOKI_URL"), getenv("INCIDENT_JAEGER_URL")
	if prometheus == "" || loki == "" || jaeger == "" {
		return nil, nil, errors.New("explicit INCIDENT_PROMETHEUS_URL, INCIDENT_LOKI_URL and INCIDENT_JAEGER_URL required")
	}
	redisPassword, err := secretenv.Lookup(getenv, "INCIDENT_REDIS_PASSWORD")
	if err != nil {
		return nil, nil, err
	}
	store, err := redisstate.New(redisstate.Config{Network: getenv("INCIDENT_REDIS_NETWORK"), Address: getenv("INCIDENT_REDIS_ADDRESS"), Namespace: getenv("INCIDENT_REDIS_NAMESPACE"), Username: getenv("INCIDENT_REDIS_USERNAME"), Password: redisPassword})
	if err != nil {
		return nil, nil, err
	}
	system, err := profilefile.Load(getenv(profilefile.EnvVar))
	if err != nil {
		return nil, nil, errors.Join(err, store.Close())
	}
	flow := getenv(flowEnvVar)
	if !investigation.ValidFlow(flow) {
		return nil, nil, errors.Join(errors.New("unknown "+flowEnvVar), store.Close())
	}
	deps := &httpapi.Deps{
		Token: token, Store: store, MaxActive: maxActive, Readiness: store.Readiness,
		Now: time.Now, Stats: store.Stats,
		Template: worker.Template{
			LogSweep: true,
			Model:    model, Window: window, ProviderID: worker.ProviderIdentity(endpoint, profile),
			TelemetryID: worker.TelemetryIdentity(prometheus, loki, jaeger), Services: system.Services, Environments: system.Environments,
			Flow: flow, SystemFacts: system.FactsText(), ProfileVersion: system.Version,
		},
	}
	return deps, func() { _ = store.Close() }, nil
}
