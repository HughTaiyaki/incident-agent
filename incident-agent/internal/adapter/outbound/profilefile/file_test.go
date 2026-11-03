package profilefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), EnvVar) {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", maxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil {
		t.Fatal("oversized file accepted")
	}
	profile, err := Load(filepath.Join("..", "..", "..", "..", "deploy", "profiles", "goframeshop.json"))
	if err != nil || len(profile.Services) != 5 || profile.FactsText() == "" {
		t.Fatalf("%+v %v", profile, err)
	}
}
