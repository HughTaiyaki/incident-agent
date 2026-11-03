package deepseektoken

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// golden.json holds counts produced by the official Rust tokenizers library from the same file.
func TestCountMatchesOfficialLibrary(t *testing.T) {
	counter, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text   string `json:"text"`
		Tokens int    `json:"tokens"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no golden cases")
	}
	failures := 0
	for _, c := range cases {
		if got := counter.Count(c.Text); got != c.Tokens {
			failures++
			if failures <= 20 {
				t.Errorf("Count(%.80q) = %d, official %d", c.Text, got, c.Tokens)
			}
		}
	}
	t.Logf("%d cases, %d differ", len(cases), failures)
}

func TestCountPromptAddsTemplateTokens(t *testing.T) {
	counter, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	system, user := "You are a helpful assistant.", []byte(`{"kind":"logs"}`)
	want := counter.Count("<｜begin▁of▁sentence｜>" + system + "<｜User｜>" + string(user) + "<｜Assistant｜></think>")
	if got := counter.CountPrompt(system, user); got != want {
		t.Fatalf("CountPrompt = %d, rendered template = %d", got, want)
	}
}

func TestCountSurvivesInvalidUTF8AndLongRuns(t *testing.T) {
	counter, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := counter.Count("a\xffb"); got < 1 {
		t.Fatalf("invalid UTF-8 counted %d", got)
	}
	if got := counter.Count(strings.Repeat("=", 2000)); got < 1 {
		t.Fatalf("long run counted %d", got)
	}
}

func TestParseRejectsCorruptTokenizer(t *testing.T) {
	if _, err := parse([]byte("not gzip")); err == nil {
		t.Fatal("corrupt tokenizer accepted")
	}
}

func BenchmarkCountPrompt(b *testing.B) {
	counter, err := Load()
	if err != nil {
		b.Fatal(err)
	}
	input := []byte(strings.Repeat(`{"service":"order-service","message":"order 123: reserve outcome unknown","severity":"WARN"},`, 1000))
	b.SetBytes(int64(len(input)))
	for b.Loop() {
		counter.CountPrompt("rules", input)
	}
}
