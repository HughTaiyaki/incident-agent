package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/fixture"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/modelhttp"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/profilefile"
	"github.com/hughtaiyaki/incident-agent/internal/adapter/outbound/telemetryhttp"
	"github.com/hughtaiyaki/incident-agent/internal/app/capture"
	"github.com/hughtaiyaki/incident-agent/internal/app/evaluation"
	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/secretenv"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	bundleFile   = "bundle.json"
	requestFile  = "request.json"
	manifestFile = "manifest.json"
	// Generous for the plan's queries and their retries; a capture has no model cost.
	captureRequests = 400
	captureTimeout  = 5 * time.Minute
)

// thinkingTuning leaves room for reasoning in the output and for the minutes it takes.
var thinkingTuning = investigation.ModelTuning{MaxOutput: 6000, DecisionTimeout: 150 * time.Second, RunTimeout: 15 * time.Minute}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: incident-eval capture|derive|investigate|sheet [flags]")
		return 2
	}
	flags := flag.NewFlagSet("incident-eval "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "sample directory (created by capture and derive)")
	from := flags.String("from", "", "derive: sample directory to start from")
	drop := flags.String("drop", "", "derive: comma-separated query kinds to remove")
	rename := flags.String("rename", "", "derive: comma-separated old=new service names, for testing that results do not depend on names")
	startText := flags.String("start", "", "capture: window start, RFC3339")
	endText := flags.String("end", "", "capture: window end, RFC3339")
	environment := flags.String("environment", "experiment", "capture: telemetry environment")
	upstream := flags.String("upstream-sha", "unknown", "capture: verified upstream revision")
	attempts := flags.Int("attempts", 3, "investigate: repeated investigations of this sample")
	thinking := flags.Bool("thinking", false, "investigate: keep model thinking on, with the longer limits it needs")
	relations := flags.Bool("relations", false, "investigate, fixed: add computed call relations to the report call")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dir == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "dir is required; positional arguments are not accepted")
		return 2
	}
	var err error
	switch args[0] {
	case "capture":
		err = captureSample(ctx, *dir, *startText, *endText, *environment, *upstream, stdout)
	case "derive":
		err = deriveSample(*from, *dir, strings.Split(*drop, ","), *rename, stdout)
	case "investigate":
		err = investigate(ctx, *dir, *attempts, *thinking, *relations, evaluation.FlowAgent, stdout)
	case "hybrid":
		err = investigate(ctx, *dir, *attempts, *thinking, *relations, evaluation.FlowHybrid, stdout)
	case "fixed":
		err = investigate(ctx, *dir, *attempts, *thinking, *relations, evaluation.FlowFixed, stdout)
	case "sheet":
		err = sheet(*dir, stdout)
	default:
		_, _ = fmt.Fprintln(stderr, "unknown command")
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func writeNew(path string, value any) error {
	data, err := evaluation.Encode(value)
	if err != nil {
		return err
	}
	// #nosec G304,G703 -- Operator-selected CLI path, not model or telemetry input.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create %s: use a new path", filepath.Base(path))
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(data)
	return err
}

func captureSample(ctx context.Context, dir, startText, endText, environment, upstream string, stdout io.Writer) error {
	start, err := time.Parse(time.RFC3339, startText)
	if err != nil {
		return errors.New("start must be RFC3339")
	}
	end, err := time.Parse(time.RFC3339, endText)
	if err != nil {
		return errors.New("end must be RFC3339")
	}
	// #nosec G703 -- Operator-selected CLI path, not model or telemetry input.
	if err = os.Mkdir(dir, 0o700); err != nil {
		return errors.New("cannot create sample directory: use a new path")
	}
	policy := telemetry.DefaultPolicy()
	client, err := telemetryhttp.New(telemetryhttp.Config{
		PrometheusURL: envOr("INCIDENT_PROMETHEUS_URL", "http://127.0.0.1:9090"),
		LokiURL:       envOr("INCIDENT_LOKI_URL", "http://127.0.0.1:3100"),
		JaegerURL:     envOr("INCIDENT_JAEGER_URL", "http://127.0.0.1:16686"),
		Policy:        policy,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	normalized := []telemetry.Query{}
	for _, q := range evaluation.PlanQueries(policy.Services, environment, start, end) {
		if q, err = policy.Normalize(q); err != nil {
			return err
		}
		normalized = append(normalized, q)
	}
	// Failed queries are kept as evidence of the gap, so collection errors are not fatal here.
	entries, _ := capture.CollectWithin(ctx, client, normalized, captureRequests)
	details := []telemetry.Query{}
	for _, q := range evaluation.TraceQueries(entries) {
		if q, err = policy.Normalize(q); err != nil {
			return err
		}
		details = append(details, q)
	}
	more, _ := capture.CollectWithin(ctx, client, details, captureRequests)
	entries = append(entries, more...)
	if err = ctx.Err(); err != nil {
		return err
	}
	bundle, err := capture.Freeze("live", upstream, entries)
	if err != nil {
		return err
	}
	if err = telemetry.WriteBundle(filepath.Join(dir, bundleFile), bundle); err != nil {
		return err
	}
	manifest := evaluation.Manifest{SampleID: filepath.Base(dir), UpstreamSHA: upstream, Environment: environment, Start: start, End: end,
		CapturedAt: bundle.CapturedAt, BundleDigest: bundle.Digest, Sources: evaluation.Summarize(bundle.Entries), LeakHits: evaluation.ScanLeaks(bundle.Entries)}
	if err = writeNew(filepath.Join(dir, manifestFile), manifest); err != nil {
		return err
	}
	if err = writeNew(filepath.Join(dir, requestFile), evaluation.Request(policy.Services, environment, start, end)); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(manifest)
}

func readSample(dir string) (telemetry.Bundle, evaluation.Manifest, investigation.Request, error) {
	var manifest evaluation.Manifest
	var request investigation.Request
	// #nosec G304,G703 -- Operator-selected CLI path, not model or telemetry input.
	file, err := os.Open(filepath.Join(dir, bundleFile))
	if err != nil {
		return telemetry.Bundle{}, manifest, request, errors.New("cannot open sample bundle")
	}
	defer func() { _ = file.Close() }()
	bundle, err := telemetry.ReadBundle(file)
	if err != nil {
		return telemetry.Bundle{}, manifest, request, err
	}
	for name, value := range map[string]any{manifestFile: &manifest, requestFile: &request} {
		// #nosec G304,G703 -- Operator-selected CLI path, not model or telemetry input.
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return telemetry.Bundle{}, manifest, request, fmt.Errorf("cannot read %s", name)
		}
		if err = json.Unmarshal(data, value); err != nil {
			return telemetry.Bundle{}, manifest, request, fmt.Errorf("invalid %s", name)
		}
	}
	if manifest.BundleDigest != bundle.Digest {
		return telemetry.Bundle{}, manifest, request, errors.New("bundle does not match its manifest")
	}
	return bundle, manifest, request, nil
}

func deriveSample(from, dir string, drop []string, rename string, stdout io.Writer) error {
	if from == "" {
		return errors.New("from is required")
	}
	bundle, manifest, request, err := readSample(from)
	if err != nil {
		return err
	}
	derived, err := bundle, error(nil)
	if rename != "" {
		names := map[string]string{}
		for _, pair := range strings.Split(rename, ",") {
			oldName, newName, ok := strings.Cut(pair, "=")
			if !ok {
				return errors.New("rename takes old=new pairs")
			}
			names[oldName] = newName
		}
		if derived, err = evaluation.RenameServices(bundle, names); err != nil {
			return err
		}
		if request, err = evaluation.RenameRequest(request, names); err != nil {
			return err
		}
		drop = nil
	} else if derived, err = evaluation.Derive(bundle, drop); err != nil {
		return err
	}
	// #nosec G703 -- Operator-selected CLI path, not model or telemetry input.
	if err = os.Mkdir(dir, 0o700); err != nil {
		return errors.New("cannot create sample directory: use a new path")
	}
	if err = telemetry.WriteBundle(filepath.Join(dir, bundleFile), derived); err != nil {
		return err
	}
	manifest.DerivedFrom, manifest.Dropped = manifest.SampleID, drop
	manifest.SampleID, manifest.BundleDigest, manifest.CapturedAt = filepath.Base(dir), derived.Digest, derived.CapturedAt
	manifest.Sources, manifest.LeakHits = evaluation.Summarize(derived.Entries), evaluation.ScanLeaks(derived.Entries)
	if err = writeNew(filepath.Join(dir, manifestFile), manifest); err != nil {
		return err
	}
	if err = writeNew(filepath.Join(dir, requestFile), request); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(manifest)
}

// recorder notes each model call for the outcome. It embeds the client so the decider still
// finds its token counter.
type recorder struct {
	*modelhttp.Client
	calls []evaluation.CallRecord
}

func (r *recorder) Complete(ctx context.Context, input investigation.CompletionRequest) (investigation.CompletionResponse, error) {
	var shown struct {
		Phase   string `json:"phase"`
		Records []struct {
			ID string `json:"id"`
		} `json:"records"`
	}
	call := evaluation.CallRecord{Shown: []string{}}
	if json.Unmarshal(input.Input, &shown) == nil {
		call.Phase = shown.Phase
		for _, record := range shown.Records {
			call.Shown = append(call.Shown, record.ID)
		}
	}
	started := time.Now()
	response, err := r.Client.Complete(ctx, input)
	call.Seconds, call.BusyRetries = time.Since(started).Seconds(), response.BusyRetries
	var failure *investigation.ProviderError
	if errors.As(err, &failure) {
		call.Failure, call.BusyRetries = failure.Code, failure.BusyRetries
	}
	if ctx.Err() != nil {
		call.Failure = "TIMEOUT"
	}
	r.calls = append(r.calls, call)
	return response, err
}

func investigate(ctx context.Context, dir string, attempts int, thinking, relations bool, flow string, stdout io.Writer) error {
	if attempts < 1 || attempts > 20 {
		return errors.New("attempts must be between 1 and 20")
	}
	bundle, manifest, request, err := readSample(dir)
	if err != nil {
		return err
	}
	policy, err := investigation.ValidateRequest(request)
	if err != nil {
		return err
	}
	replay, err := fixture.New(bundle, policy)
	if err != nil {
		return err
	}
	system, err := profilefile.Load(os.Getenv(profilefile.EnvVar))
	if err != nil {
		return err
	}
	model := os.Getenv("INCIDENT_MODEL_NAME")
	window, err := strconv.Atoi(os.Getenv("INCIDENT_MODEL_CONTEXT_WINDOW"))
	if err != nil {
		return errors.New("INCIDENT_MODEL_CONTEXT_WINDOW must be configured explicitly")
	}
	apiKey, err := secretenv.Lookup(os.Getenv, "INCIDENT_MODEL_API_KEY")
	if err != nil {
		return err
	}
	config := modelhttp.Config{Endpoint: os.Getenv("INCIDENT_MODEL_ENDPOINT"), APIKey: apiKey, Model: model, Profile: os.Getenv("INCIDENT_MODEL_API_PROFILE")}
	if thinking {
		config.Profile, config.Timeout = modelhttp.ProfileDeepSeekThinking, thinkingTuning.DecisionTimeout
	}
	client, err := modelhttp.New(config)
	if err != nil {
		return err
	}
	defer client.Close()
	for attempt := 1; attempt <= attempts; attempt++ {
		path := filepath.Join(dir, fmt.Sprintf("run-%02d.json", attempt))
		// #nosec G703 -- Operator-selected CLI path, not model or telemetry input.
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%s exists: results are never overwritten", filepath.Base(path))
		}
		recording := &recorder{Client: client}
		decider, err := investigation.NewModelDecider(recording, model, window)
		if err != nil {
			return err
		}
		if thinking {
			if err = decider.Tune(thinkingTuning); err != nil {
				return err
			}
		}
		decider.UseFacts(system.FactsText())
		if relations {
			decider.UseRelations()
		}
		var flowDecider investigation.Decider = decider
		var hybrid *evaluation.HybridFlow
		switch flow {
		case evaluation.FlowFixed:
			flowDecider = evaluation.FixedFlow{ModelDecider: decider}
		case evaluation.FlowHybrid:
			decider.UseCoverage()
			hybrid = &evaluation.HybridFlow{ModelDecider: decider}
			flowDecider = hybrid
		}
		result, err := investigation.Execute(ctx, request, flowDecider, replay)
		if err != nil {
			return err
		}
		outcome := evaluation.NewOutcome(manifest.SampleID, attempt, bundle.Digest, model, result)
		outcome.Flow, outcome.Calls, outcome.Relations = flow, recording.calls, relations
		if hybrid != nil {
			outcome.Fills = hybrid.Fills
		}
		if err = writeNew(path, outcome); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(stdout, "%s #%d %s %s facts=%d tokens=%d calls=%d\n", manifest.SampleID, attempt, outcome.Report.Result,
			outcome.Report.StopReason, len(outcome.Report.Facts), outcome.ChargedTokens, outcome.ModelCalls); err != nil {
			return err
		}
	}
	return nil
}

// sheet prints every result under dir for a human to score, without any case label.
func sheet(dir string, stdout io.Writer) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "run-*.json"))
	if err != nil || len(paths) == 0 {
		return errors.New("no results found")
	}
	for _, path := range paths {
		// #nosec G304,G703 -- Paths come from the operator-selected directory.
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read result")
		}
		var outcome evaluation.Outcome
		if err = json.Unmarshal(data, &outcome); err != nil {
			return fmt.Errorf("invalid result %s", filepath.Base(path))
		}
		report, err := evaluation.Encode(outcome.Report)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(stdout, "## %s #%d\n\n%s，停止原因 %s，%d 次模型调用，%d Token，%.1f 秒\n\n```json\n%s```\n\n评分：主假设命中（是/否/不适用）＿＿　无依据断言数＿＿　关键断言数＿＿　备注＿＿\n\n",
			outcome.Sample, outcome.Attempt, outcome.Report.Result, outcome.Report.StopReason, outcome.ModelCalls, outcome.ChargedTokens, outcome.Seconds, report); err != nil {
			return err
		}
	}
	return nil
}
