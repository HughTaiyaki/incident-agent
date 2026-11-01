// Package secretenv reads a credential from NAME or from the file named by NAME_FILE.
package secretenv

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// maxSecretBytes bounds what a misconfigured path can pull into memory.
const maxSecretBytes = 8 << 10

// Lookup prefers neither source silently: setting both is a configuration error.
// Mounted files keep credentials out of the process environment and container metadata.
func Lookup(getenv func(string) string, name string) (string, error) {
	value, path := getenv(name), getenv(name+"_FILE")
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, errors.Unwrap(err))
	}
	if !info.Mode().IsRegular() || info.Size() > maxSecretBytes {
		return "", fmt.Errorf("read %s_FILE: not a small regular file", name)
	}
	// #nosec G304 -- The path is operator configuration, not request input.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, errors.Unwrap(err))
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
