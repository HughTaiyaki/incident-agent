package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/modelhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

type artifact struct {
	SchemaVersion     string            `json:"schema_version"`
	Mode              string            `json:"mode"`
	InputBundleDigest string            `json:"input_bundle_digest"`
	UpstreamSHA       string            `json:"upstream_sha"`
	Run               investigation.Run `json:"run"`
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("incident-model", flag.ContinueOnError)
	flags.SetOutput(stderr)
	requestPath := flags.String("request", "", "JSON investigation request (required)")
	fixturePath := flags.String("fixture", "", "sanitized telemetry bundle (required)")
	output := flags.String("out", "", "new run artifact (required; parent must exist)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *requestPath == "" || *fixturePath == "" || *output == "" {
		_, _ = fmt.Fprintln(stderr, "request, fixture and out are required; model mode sends selected data to the configured endpoint")
		return 2
	}
	request, bundle, err := readInputs(*requestPath, *fixturePath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	policy, err := investigation.ValidateRequest(request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	replay, err := fixture.New(bundle, policy)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	model := os.Getenv("INCIDENT_MODEL_NAME")
	window, err := strconv.Atoi(os.Getenv("INCIDENT_MODEL_CONTEXT_WINDOW"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "INCIDENT_MODEL_CONTEXT_WINDOW must be configured explicitly")
		return 2
	}
	client, err := modelhttp.New(modelhttp.Config{Endpoint: os.Getenv("INCIDENT_MODEL_ENDPOINT"),
		APIKey: os.Getenv("INCIDENT_MODEL_API_KEY"), Model: model})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer client.Close()
	decider, err := investigation.NewModelDecider(client, model, window)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	// Reserve the name before any paid call; no retries can silently overwrite a previous run.
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot create output; use a new path with an existing parent directory")
		return 1
	}
	complete := false
	defer func() {
		_ = out.Close()
		if !complete {
			_ = os.Remove(*output)
		}
	}()
	result, err := investigation.Execute(ctx, request, decider, replay)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	saved := artifact{SchemaVersion: "1", Mode: "model_replay", InputBundleDigest: bundle.Digest, UpstreamSHA: bundle.UpstreamSHA, Run: result}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(saved); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot write model artifact")
		return 1
	}
	if err := out.Sync(); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot sync model artifact")
		return 1
	}
	if err := out.Close(); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot close model artifact")
		return 1
	}
	complete = true
	charged := 0
	for _, call := range result.ModelCalls {
		charged += call.Charged
	}
	summary := struct {
		RunID         string `json:"run_id"`
		Status        string `json:"status"`
		Result        string `json:"result"`
		StopReason    string `json:"stop_reason"`
		ModelCalls    int    `json:"model_calls"`
		ChargedTokens int    `json:"charged_tokens"`
	}{result.ID, result.Status, result.Report.Result, result.Report.StopReason, len(result.ModelCalls), charged}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return 1
	}
	if result.Status == "CANCELLED" {
		return 1
	}
	return 0
}

func readInputs(requestPath, bundlePath string) (investigation.Request, telemetry.Bundle, error) {
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	file, err := os.Open(requestPath)
	if err != nil {
		return investigation.Request{}, telemetry.Bundle{}, errors.New("cannot open request")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return investigation.Request{}, telemetry.Bundle{}, errors.New("cannot read bounded request")
	}
	var request investigation.Request
	if err := investigation.Decode(data, &request); err != nil {
		return request, telemetry.Bundle{}, err
	}
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	source, err := os.Open(bundlePath)
	if err != nil {
		return request, telemetry.Bundle{}, errors.New("cannot open fixture")
	}
	defer func() { _ = source.Close() }()
	bundle, err := telemetry.ReadBundle(source)
	return request, bundle, err
}
