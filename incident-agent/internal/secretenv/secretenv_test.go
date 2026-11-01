package secretenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Lookup(env(map[string]string{"K": "direct"}), "K"); err != nil || got != "direct" {
		t.Fatal(got, err)
	}
	if got, err := Lookup(env(map[string]string{"K_FILE": path}), "K"); err != nil || got != "from-file" {
		t.Fatal(got, err)
	}
	if got, err := Lookup(env(nil), "K"); err != nil || got != "" {
		t.Fatal(got, err)
	}
	if _, err := Lookup(env(map[string]string{"K": "a", "K_FILE": path}), "K"); err == nil {
		t.Fatal("both sources accepted")
	}
	large := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxSecretBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{filepath.Join(t.TempDir(), "absent"), t.TempDir(), large} {
		if _, err := Lookup(env(map[string]string{"K_FILE": bad}), "K"); err == nil {
			t.Fatal("unusable file accepted:", bad)
		}
	}
}
