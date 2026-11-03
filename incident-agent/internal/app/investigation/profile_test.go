package investigation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mallProfile(t *testing.T) SystemProfile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "profiles", "goframeshop.json"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ParseProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// Moving the knowledge out of the code must not change a single character the model sees.
func TestMallProfileReproducesTheEvaluatedInstructions(t *testing.T) {
	facts := mallProfile(t).FactsText()
	for phase, file := range map[string]string{"INVESTIGATING": "goframeshop-instructions-investigating.txt", "SYNTHESIZING": "goframeshop-instructions-synthesizing.txt"} {
		// #nosec G304 -- Test-owned file name from the table above.
		want, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		if got := instructionsFor(phase, facts); got != string(want) {
			t.Fatalf("%s instructions changed:\n%s", phase, got)
		}
	}
}

func TestRulesNameNoSystemWithoutAProfile(t *testing.T) {
	mallWords := []string{"goods-service", "order-service", "payment-simulator", "gateway-h5", "user-service", "CreateOrder", "1062", "800 ms",
		"INSUFFICIENT_STOCK", "ALREADY_RELEASED", "message.redelivered", "replayed", "inventory", "System facts"}
	for _, phase := range []string{"INVESTIGATING", "SYNTHESIZING"} {
		text := instructionsFor(phase, "") + relationInstructions + coverageInstructions
		for _, word := range mallWords {
			if strings.Contains(text, word) {
				t.Errorf("%s rules mention %q", phase, word)
			}
		}
	}
}

func TestParseProfileRejectsIncompleteOrUnsafeInput(t *testing.T) {
	valid := `{"version":"v1","source":"s","services":["a"],"environments":["e"],"facts":[{"kind":"error_code","text":"t"}]}`
	if _, err := ParseProfile([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	withoutFacts := `{"version":"v1","source":"s","services":["a"],"environments":["e"],"facts":[]}`
	profile, err := ParseProfile([]byte(withoutFacts))
	if err != nil || profile.FactsText() != "" {
		t.Fatalf("%q %v", profile.FactsText(), err)
	}
	for name, input := range map[string]string{
		"not json":         `{`,
		"trailing":         valid + `{}`,
		"unknown field":    strings.Replace(valid, `"source"`, `"extra":1,"source"`, 1),
		"no version":       strings.Replace(valid, `"version":"v1"`, `"version":""`, 1),
		"no source":        strings.Replace(valid, `"source":"s"`, `"source":""`, 1),
		"no services":      strings.Replace(valid, `["a"]`, `[]`, 1),
		"too many":         strings.Replace(valid, `["a"]`, `["a","b","c","d","e","f"]`, 1),
		"repeated service": strings.Replace(valid, `["a"]`, `["a","a"]`, 1),
		"unsafe service":   strings.Replace(valid, `["a"]`, `["a\"}|x"]`, 1),
		"no environments":  strings.Replace(valid, `["e"]`, `[]`, 1),
		"unknown kind":     strings.Replace(valid, `"error_code"`, `"what_happened"`, 1),
		"empty fact":       strings.Replace(valid, `"text":"t"`, `"text":" "`, 1),
		"control text":     strings.Replace(valid, `"text":"t"`, `"text":"t\u0007"`, 1),
	} {
		if _, err := ParseProfile([]byte(input)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestProfileAllowsOnlyItsScope(t *testing.T) {
	profile := SystemProfile{Services: []string{"a", "b"}, Environments: []string{"e"}}
	for _, tt := range []struct {
		request Request
		want    bool
	}{
		{Request{Services: []string{"a"}, Environment: "e"}, true},
		{Request{Services: []string{"a", "c"}, Environment: "e"}, false},
		{Request{Services: []string{"a"}, Environment: "other"}, false},
	} {
		if got := profile.Allows(tt.request); got != tt.want {
			t.Errorf("%+v: %v", tt.request, got)
		}
	}
}
