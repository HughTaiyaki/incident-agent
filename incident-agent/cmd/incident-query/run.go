package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/capture"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("incident-query", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", "", "live or replay (required)")
	input := flags.String("input", "", "JSON query array (required)")
	output := flags.String("out", "", "new evidence bundle file (required; parent must exist)")
	fixturePath := flags.String("fixture", "", "bundle to replay")
	upstream := flags.String("upstream-sha", "unknown", "verified running upstream revision, or unknown")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	validMode := *mode == "live" || *mode == "replay"
	if !validMode || flags.NArg() != 0 || *input == "" || *output == "" {
		_, _ = fmt.Fprintln(stderr, "mode, input and out are required; positional arguments are not accepted")
		return 2
	}
	if (*mode == "replay") != (*fixturePath != "") {
		_, _ = fmt.Fprintln(stderr, "fixture is required only in replay mode")
		return 2
	}
	policy := telemetry.DefaultPolicy()
	queries, err := readQueries(*input, policy)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if _, err = os.Lstat(*output); err == nil {
		_, _ = fmt.Fprintln(stderr, "output already exists; choose a new capture path")
		return 1
	}
	if !errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintln(stderr, "cannot inspect output path")
		return 1
	}
	// Validate provenance before making any requests.
	if _, err = telemetry.NewBundle(*mode, *upstream, []telemetry.Result{}); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	querier, closeSource, err := source(*mode, *fixturePath, policy)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer closeSource()
	collectionCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	entries, collectionErr := capture.Collect(collectionCtx, querier, queries)
	bundle, err := capture.Freeze(*mode, *upstream, entries)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err = telemetry.WriteBundle(*output, bundle); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	summary := struct {
		Entries  int    `json:"entries"`
		Requests int    `json:"backend_requests"`
		Records  int    `json:"records"`
		Digest   string `json:"digest"`
	}{Entries: len(entries), Digest: bundle.Digest}
	for _, entry := range entries {
		summary.Requests += entry.Requests
		summary.Records += len(entry.Records)
	}
	if err = json.NewEncoder(stdout).Encode(summary); err != nil {
		return 1
	}
	if collectionErr != nil {
		_, _ = fmt.Fprintln(stderr, "collection incomplete; inspect saved source_status and error_code")
		return 1
	}
	return 0
}

func readQueries(path string, policy telemetry.Policy) ([]telemetry.Query, error) {
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open query file")
	}
	defer func() { _ = file.Close() }()
	return capture.ReadQueries(file, policy)
}

func source(mode, path string, policy telemetry.Policy) (capture.Querier, func(), error) {
	if mode == "replay" {
		// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
		file, err := os.Open(path)
		if err != nil {
			return nil, nil, errors.New("cannot open fixture")
		}
		defer func() { _ = file.Close() }()
		bundle, err := telemetry.ReadBundle(file)
		if err != nil {
			return nil, nil, err
		}
		replay, err := fixture.New(bundle, policy)
		return replay, func() {}, err
	}
	client, err := telemetryhttp.New(telemetryhttp.Config{
		PrometheusURL: envOr("INCIDENT_PROMETHEUS_URL", "http://127.0.0.1:9090"),
		LokiURL:       envOr("INCIDENT_LOKI_URL", "http://127.0.0.1:3100"),
		JaegerURL:     envOr("INCIDENT_JAEGER_URL", "http://127.0.0.1:16686"),
		Policy:        policy,
	})
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
