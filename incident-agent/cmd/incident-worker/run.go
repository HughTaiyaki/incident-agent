package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/modelhttp"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/redisstate"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/capture"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/worker"
	"github.com/hughtaiyaki/incident-agent/internal/secretenv"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// exitOverloaded lets callers retry a refused enqueue; nothing was written.
const exitOverloaded = 3

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("incident-worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", "", "enqueue, work, status, evidence, export, cancel")
	id := flags.String("id", "", "operator run ID (enqueue/status/evidence/export/cancel)")
	requestPath := flags.String("request", "", "bounded request JSON (enqueue)")
	fixturePath := flags.String("fixture", "", "verified telemetry bundle (enqueue/work)")
	live := flags.Bool("live", false, "query live telemetry backends instead of a fixture (enqueue/work)")
	output := flags.String("out", "", "new anonymized evidence bundle file (export)")
	upstream := flags.String("upstream-sha", "unknown", "verified running upstream revision, or unknown (export)")
	once := flags.Bool("once", false, "process one candidate scan and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	fail := func(err error) int { _, _ = fmt.Fprintln(stderr, err); return 1 }
	switch *mode {
	case "enqueue", "status", "evidence", "export", "cancel":
		if *id == "" {
			return 2
		}
	case "work":
	default:
		return 2
	}
	if (*mode == "export") != (*output != "") {
		return 2
	}
	if (*mode == "enqueue" || *mode == "work") && *live == (*fixturePath != "") {
		_, _ = fmt.Fprintln(stderr, "exactly one of -fixture and -live is required")
		return 2
	}
	redisPassword, err := secretenv.Lookup(os.Getenv, "INCIDENT_REDIS_PASSWORD")
	if err != nil {
		return fail(err)
	}
	store, err := redisstate.New(redisstate.Config{Network: os.Getenv("INCIDENT_REDIS_NETWORK"), Address: os.Getenv("INCIDENT_REDIS_ADDRESS"), Namespace: os.Getenv("INCIDENT_REDIS_NAMESPACE"), Username: os.Getenv("INCIDENT_REDIS_USERNAME"), Password: redisPassword})
	if err != nil {
		return fail(err)
	}
	defer func() { _ = store.Close() }()
	if *mode == "status" || *mode == "evidence" || *mode == "cancel" {
		var view any
		var err error
		switch *mode {
		case "status":
			view, err = worker.ReadResult(ctx, store, *id)
		case "evidence":
			view, err = worker.ReadEvidence(ctx, store, *id)
		default:
			view, err = worker.CancelRun(ctx, store, *id)
		}
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(stdout).Encode(view); err != nil {
			return fail(err)
		}
		return 0
	}
	if *mode == "export" {
		view, err := worker.ReadEvidence(ctx, store, *id)
		if err != nil {
			return fail(err)
		}
		bundle, err := capture.Freeze(view.Origin, *upstream, view.Evidence)
		if err != nil {
			return fail(err)
		}
		if err := telemetry.WriteBundle(*output, bundle); err != nil {
			return fail(err)
		}
		return 0
	}
	if err := store.CheckPersistence(ctx); err != nil {
		return fail(err)
	}
	maxActive := 0
	if *mode == "enqueue" {
		// No default: a safe limit depends on the deployment's memory budget.
		maxActive, err = strconv.Atoi(os.Getenv("INCIDENT_MAX_ACTIVE_RUNS"))
		if err != nil || maxActive < 1 || maxActive > worker.MaxActiveLimit {
			return fail(errors.New("explicit INCIDENT_MAX_ACTIVE_RUNS required"))
		}
	}
	src, err := openSource(*live, *fixturePath)
	if err != nil {
		return fail(err)
	}
	defer src.close()
	model := os.Getenv("INCIDENT_MODEL_NAME")
	endpoint := os.Getenv("INCIDENT_MODEL_ENDPOINT")
	window, err := strconv.Atoi(os.Getenv("INCIDENT_MODEL_CONTEXT_WINDOW"))
	if err != nil {
		return fail(errors.New("explicit model window required"))
	}
	profile, err := modelhttp.ParseProfile(os.Getenv("INCIDENT_MODEL_API_PROFILE"))
	if err != nil {
		return fail(err)
	}
	apiKey, err := secretenv.Lookup(os.Getenv, "INCIDENT_MODEL_API_KEY")
	if err != nil {
		return fail(err)
	}
	provider, err := modelhttp.New(modelhttp.Config{Endpoint: endpoint, Model: model, APIKey: apiKey, Profile: profile})
	if err != nil {
		return fail(err)
	}
	defer provider.Close()
	providerID := worker.ProviderIdentity(endpoint, profile)
	if *mode == "enqueue" {
		f, err := os.Open(*requestPath)
		if err != nil {
			return fail(errors.New("cannot open request"))
		}
		data, err := io.ReadAll(io.LimitReader(f, 16385))
		_ = f.Close()
		if err != nil || len(data) > 16384 {
			return fail(errors.New("invalid request size"))
		}
		var request investigation.Request
		if err := investigation.Decode(data, &request); err != nil {
			return fail(err)
		}
		job := worker.Job{Request: request, Model: model, Window: window, FixtureDigest: src.digest, TelemetryID: src.telemetryID, ProviderID: providerID}
		if err := worker.Enqueue(ctx, store, *id, job, maxActive); err != nil {
			fail(err)
			if errors.Is(err, worker.ErrOverloaded) {
				return exitOverloaded
			}
			return 1
		}
		_, err = fmt.Fprintln(stdout, "queued", *id)
		if err != nil {
			return fail(err)
		}
		return 0
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fail(err)
	}
	w := worker.Worker{Store: store, Owner: hex.EncodeToString(random[:]), Logger: slog.New(slog.NewJSONHandler(stderr, nil)), Build: func(_ context.Context, job worker.Job) (investigation.Provider, investigation.Querier, error) {
		if job.Model != model || job.Window != window || job.ProviderID != providerID || job.FixtureDigest != src.digest || job.TelemetryID != src.telemetryID {
			return nil, nil, errors.New("worker configuration does not match frozen job")
		}
		policy, err := investigation.ValidateRequest(job.Request)
		if err != nil {
			return nil, nil, err
		}
		querier, err := src.querier(policy)
		return provider, querier, err
	}}
	if *once {
		ids, err := store.Candidates(ctx, 32)
		if err != nil {
			return fail(err)
		}
		for _, id := range ids {
			if err := w.Process(ctx, id); err != nil {
				return fail(err)
			}
		}
		return 0
	}
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fail(err)
	}
	return 0
}

// telemetrySource is either a frozen bundle or the configured live backends.
// Its identity is frozen into each job so recovery cannot silently switch data.
type telemetrySource struct {
	digest      string
	telemetryID string
	querier     func(telemetry.Policy) (investigation.Querier, error)
	close       func()
}

func openSource(live bool, fixturePath string) (telemetrySource, error) {
	if !live {
		// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
		file, err := os.Open(fixturePath)
		if err != nil {
			return telemetrySource{}, errors.New("cannot open fixture")
		}
		bundle, err := telemetry.ReadBundle(file)
		_ = file.Close()
		if err != nil {
			return telemetrySource{}, err
		}
		return telemetrySource{digest: bundle.Digest, close: func() {}, querier: func(policy telemetry.Policy) (investigation.Querier, error) {
			return fixture.New(bundle, policy)
		}}, nil
	}
	// No default addresses: every backend must be chosen by the operator.
	prometheus, loki, jaeger := os.Getenv("INCIDENT_PROMETHEUS_URL"), os.Getenv("INCIDENT_LOKI_URL"), os.Getenv("INCIDENT_JAEGER_URL")
	if prometheus == "" || loki == "" || jaeger == "" {
		return telemetrySource{}, errors.New("explicit INCIDENT_PROMETHEUS_URL, INCIDENT_LOKI_URL and INCIDENT_JAEGER_URL required")
	}
	client, err := telemetryhttp.New(telemetryhttp.Config{PrometheusURL: prometheus, LokiURL: loki, JaegerURL: jaeger, Policy: telemetry.DefaultPolicy()})
	if err != nil {
		return telemetrySource{}, err
	}
	// The harness narrows each query to the job's request before it reaches the client.
	return telemetrySource{telemetryID: worker.TelemetryIdentity(prometheus, loki, jaeger), close: client.Close, querier: func(telemetry.Policy) (investigation.Querier, error) {
		return client, nil
	}}, nil
}
