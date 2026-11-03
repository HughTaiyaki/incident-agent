package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// ErrIdempotencyConflict means an alert ID was reused for a different request.
var ErrIdempotencyConflict = errors.New("alert ID already used by a different request")

var alertIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

// Submission is one manually reported incident. Scope comes from the server's
// template; a submission can only choose inside it.
type Submission struct {
	AlertID              string    `json:"alert_id"`
	Service              string    `json:"service"`
	Environment          string    `json:"environment"`
	StartedAt            time.Time `json:"started_at"`
	Summary              string    `json:"summary"`
	InitialWindowMinutes int       `json:"initial_window_minutes"`
}

// Template holds the server-side identity frozen into every submitted job.
type Template struct {
	// LogSweep reads every service\'s logs before the model chooses any query.
	LogSweep      bool
	Model         string
	Window        int
	ProviderID    string
	FixtureDigest string
	TelemetryID   string
	Services      []string
	Environments  []string
	// Flow, SystemFacts and ProfileVersion are frozen into every job.
	Flow           string
	SystemFacts    string
	ProfileVersion string
}

// Submitted reports the run behind an alert ID and whether this call created it.
type Submitted struct {
	RunID   string `json:"run_id"`
	Created bool   `json:"created"`
}

// ProviderIdentity identifies a model endpoint and request shape without persisting its address.
func ProviderIdentity(endpoint, profile string) string {
	return telemetry.Hash([]byte(endpoint + "\x00" + profile))
}

// TelemetryIdentity identifies the live backends without persisting their addresses.
func TelemetryIdentity(prometheus, loki, jaeger string) string {
	return telemetry.Hash([]byte(prometheus + "\n" + loki + "\n" + jaeger))
}

// Submit creates the run for an alert ID, or finds the one a retry already made.
// The ID is derived from the alert ID, so concurrent retries resolve to one run.
func Submit(ctx context.Context, store runstate.Store, template Template, input Submission, now time.Time, maxActive int) (Submitted, error) {
	if !alertIDPattern.MatchString(input.AlertID) || !slices.Contains(template.Services, input.Service) ||
		!slices.Contains(template.Environments, input.Environment) || input.StartedAt.IsZero() {
		return Submitted{}, investigation.ErrInvalidInput
	}
	if input.InitialWindowMinutes == 0 {
		input.InitialWindowMinutes = 10
	}
	if input.InitialWindowMinutes < 1 || input.InitialWindowMinutes > 50 {
		return Submitted{}, investigation.ErrInvalidInput
	}
	input.StartedAt = input.StartedAt.UTC()
	// Digest the submission, not the window: clipping to now differs between retries.
	canonical, err := json.Marshal(input)
	if err != nil {
		return Submitted{}, investigation.ErrInvalidInput
	}
	end := input.StartedAt.Add(5 * time.Minute)
	if now.Before(end) {
		end = now.UTC()
	}
	start := input.StartedAt.Add(-time.Duration(input.InitialWindowMinutes) * time.Minute)
	if !start.Before(end) {
		return Submitted{}, investigation.ErrInvalidInput
	}
	job := Job{
		Request: investigation.Request{LogSweep: template.LogSweep, Service: input.Service, Services: slices.Clone(template.Services), Environment: input.Environment, Start: start, End: end, Summary: input.Summary},
		Model:   template.Model, Window: template.Window, ProviderID: template.ProviderID,
		FixtureDigest: template.FixtureDigest, TelemetryID: template.TelemetryID, InputDigest: telemetry.Hash(canonical),
		Flow: template.Flow, SystemFacts: template.SystemFacts, ProfileVersion: template.ProfileVersion,
	}
	id := "inc-" + telemetry.Hash([]byte("manual\x00" + input.AlertID))[:32]
	err = Enqueue(ctx, store, id, job, maxActive)
	if err == nil {
		return Submitted{RunID: id, Created: true}, nil
	}
	if !errors.Is(err, errRunExists) {
		return Submitted{}, err
	}
	reply, err := store.Execute(ctx, runstate.Command{Action: "read", ID: id})
	if err != nil {
		return Submitted{}, err
	}
	if reply.Code == "NOT_FOUND" {
		// Retention ended between create and read. Nothing was created; the client may retry.
		return Submitted{}, fmt.Errorf("%w: %s", investigation.ErrPersistence, reply.Code)
	}
	state, _, err := decode(reply)
	if err != nil {
		return Submitted{}, err
	}
	var existing Job
	if json.Unmarshal([]byte(state.Request), &existing) != nil || existing.InputDigest != job.InputDigest {
		return Submitted{}, ErrIdempotencyConflict
	}
	return Submitted{RunID: id}, nil
}
