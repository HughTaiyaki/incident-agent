// Package profilefile reads the operator's system profile from disk.
package profilefile

import (
	"errors"
	"io"
	"os"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

const maxBytes = 64 << 10

// EnvVar names the setting every investigating process requires: without it the agent would
// run without knowing the system, and that must be a choice, not an omission.
const EnvVar = "INCIDENT_SYSTEM_PROFILE_FILE"

// Load reads and validates the profile at path.
func Load(path string) (investigation.SystemProfile, error) {
	if path == "" {
		return investigation.SystemProfile{}, errors.New("explicit " + EnvVar + " required")
	}
	// #nosec G304 -- Operator-selected path, not model or telemetry input.
	file, err := os.Open(path)
	if err != nil {
		return investigation.SystemProfile{}, errors.New("cannot open system profile")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return investigation.SystemProfile{}, errors.New("invalid system profile size")
	}
	return investigation.ParseProfile(data)
}
