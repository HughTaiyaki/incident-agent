package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/scripted"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// artifact records replay provenance separately from the report's diagnostic conclusions.
type artifact struct {
	SchemaVersion     string            `json:"schema_version"`
	Mode              string            `json:"mode"`
	InputBundleDigest string            `json:"input_bundle_digest"`
	UpstreamSHA       string            `json:"upstream_sha"`
	Run               investigation.Run `json:"run"`
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("incident-investigate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("script", "", "local JSON request and scripted decisions (required; no model calls)")
	fixturePath := flags.String("fixture", "", "sanitized evidence bundle (required; offline only)")
	output := flags.String("out", "", "new run artifact (required; parent must exist)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *input == "" || *fixturePath == "" || *output == "" {
		_, _ = fmt.Fprintln(stderr, "script, fixture and out are required; this command only runs offline")
		return 2
	}
	script, bundle, err := readInputs(*input, *fixturePath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	policy, err := investigation.ValidateRequest(script.Request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	replay, err := fixture.New(bundle, policy)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	// Reserve output before execution so an existing artifact is never overwritten.
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot create output; choose a new path with an existing parent directory")
		return 1
	}
	complete := false
	defer func() {
		_ = out.Close()
		if !complete {
			_ = os.Remove(*output)
		}
	}()
	result, err := investigation.Execute(ctx, script.Request, script, replay)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	saved := artifact{SchemaVersion: "1", Mode: "scripted_replay", InputBundleDigest: bundle.Digest, UpstreamSHA: bundle.UpstreamSHA, Run: result}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(saved); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot write run artifact")
		return 1
	}
	if err := out.Sync(); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot sync run artifact")
		return 1
	}
	if err := out.Close(); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot close run artifact")
		return 1
	}
	complete = true
	summary := struct {
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
		Result     string `json:"result"`
		StopReason string `json:"stop_reason"`
		Decisions  int    `json:"decisions"`
		Requests   int    `json:"backend_requests"`
	}{result.ID, result.Status, result.Report.Result, result.Report.StopReason, result.Decisions, result.BackendRequests}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return 1
	}
	if result.Status == "CANCELLED" {
		return 1
	}
	return 0
}

func readInputs(scriptPath, bundlePath string) (*scripted.Script, telemetry.Bundle, error) {
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	scriptFile, err := os.Open(scriptPath)
	if err != nil {
		return nil, telemetry.Bundle{}, errors.New("cannot open investigation script")
	}
	defer func() { _ = scriptFile.Close() }()
	script, err := scripted.Read(scriptFile)
	if err != nil {
		return nil, telemetry.Bundle{}, err
	}
	// #nosec G304 -- Operator-selected CLI path, not model or telemetry input.
	bundleFile, err := os.Open(bundlePath)
	if err != nil {
		return nil, telemetry.Bundle{}, errors.New("cannot open evidence bundle")
	}
	defer func() { _ = bundleFile.Close() }()
	bundle, err := telemetry.ReadBundle(bundleFile)
	return script, bundle, err
}
