package investigation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const (
	maxProfileFacts    = 24
	maxProfileFactText = 1000
	// A request carries at most this many services, so a profile cannot name more.
	maxProfileServices = 5
)

var profileVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Fact kinds separate what a system's records mean. None of them can say what happened in a
// run: that a duplicate request finished with no impact can only come from that run's evidence.
var factKinds = []string{"service_map", "field_meaning", "business_outcome", "event_meaning", "error_code", "deadline"}

// Fact is one operator-written explanation of this system's records.
type Fact struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// SystemProfile is everything the agent knows about the system under investigation beyond its
// telemetry. It is versioned configuration, written by the operator and never read from
// telemetry, so supporting another system does not mean changing the agent's rules.
type SystemProfile struct {
	Version      string   `json:"version"`
	Source       string   `json:"source"`
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
	Facts        []Fact   `json:"facts"`
}

// ParseProfile accepts only a complete profile: a typo must not silently drop knowledge.
func ParseProfile(data []byte) (SystemProfile, error) {
	var profile SystemProfile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return SystemProfile{}, errors.New("invalid system profile")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return SystemProfile{}, errors.New("invalid system profile: trailing data")
	}
	if !profileVersion.MatchString(profile.Version) || !safeText(profile.Source, 500) {
		return SystemProfile{}, errors.New("invalid system profile: version and source are required")
	}
	if len(profile.Services) == 0 || len(profile.Services) > maxProfileServices || len(profile.Environments) == 0 {
		return SystemProfile{}, errors.New("invalid system profile: services and environments are required")
	}
	for _, names := range [][]string{profile.Services, profile.Environments} {
		for i, name := range names {
			if !telemetry.ValidScopeName(name) || slices.Contains(names[:i], name) {
				return SystemProfile{}, errors.New("invalid system profile: bad or repeated name")
			}
		}
	}
	if len(profile.Facts) > maxProfileFacts {
		return SystemProfile{}, errors.New("invalid system profile: too many facts")
	}
	for _, fact := range profile.Facts {
		if !slices.Contains(factKinds, fact.Kind) || !safeText(fact.Text, maxProfileFactText) || !utf8.ValidString(fact.Text) {
			return SystemProfile{}, errors.New("invalid system profile: bad fact")
		}
	}
	return profile, nil
}

// FactsText is the block for the model's instructions; a profile without facts adds nothing.
func (p SystemProfile) FactsText() string {
	if len(p.Facts) == 0 {
		return ""
	}
	texts := make([]string, 0, len(p.Facts))
	for _, fact := range p.Facts {
		texts = append(texts, fact.Text)
	}
	return "System facts: " + strings.Join(texts, " ") + factsGuard
}

// Policy is the scope this deployment may investigate.
func (p SystemProfile) Policy() telemetry.Policy {
	return telemetry.Policy{Services: slices.Clone(p.Services), Environments: slices.Clone(p.Environments)}
}

// Allows says whether a request stays inside the profile's scope.
func (p SystemProfile) Allows(request Request) bool {
	if !slices.Contains(p.Environments, request.Environment) {
		return false
	}
	for _, service := range request.Services {
		if !slices.Contains(p.Services, service) {
			return false
		}
	}
	return true
}
