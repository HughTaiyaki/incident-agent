package main

import (
	"strings"
	"testing"
)

func TestFindings(t *testing.T) {
	fake := "sk-" + strings.Repeat("a1", 16)
	got := findings("f.env", []byte("first\nKEY="+fake+"\n"))
	if len(got) != 1 || got[0] != "f.env:2: API key" {
		t.Fatal(got)
	}
	for _, text := range got {
		if strings.Contains(text, fake) {
			t.Fatal("finding repeats the credential")
		}
	}
	clean := []string{"INCIDENT_MODEL_API_KEY=private-test-key", "task-0009-online", "Bearer $TOKEN", "disk-usage-summary-of-the-whole-system"}
	for _, text := range clean {
		if got := findings("f", []byte(text)); len(got) != 0 {
			t.Fatal(text, got)
		}
	}
	if got := findings("binary", []byte{0, 's', 'k', '-'}); got != nil {
		t.Fatal(got)
	}
}
