package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

const requestFixture = "../../testdata/investigation/observed-request.json"
const evidenceFixture = "../../testdata/telemetry/observed-20261028/bundle.json"

func TestRun(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var input struct {
			Request investigation.Request `json:"request"`
			Records []telemetry.Record    `json:"records"`
		}
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil {
			t.Error(err)
			return
		}
		var decision investigation.Decision
		if calls == 1 {
			decision = investigation.Decision{Kind: "query", Query: &telemetry.Query{Kind: "logs", Service: input.Request.Service,
				Environment: input.Request.Environment, Start: input.Request.Start, End: input.Request.End}}
		} else {
			if len(input.Records) == 0 {
				t.Error("context lost all records")
			}
			draft := investigation.Draft{Result: "INCONCLUSIVE", Unknowns: []string{"No fault evidence."}, Recovery: "UNKNOWN"}
			if len(input.Records) > 0 {
				draft.Facts = []investigation.Claim{{Text: "One INFO log record is visible.", EvidenceIDs: []string{input.Records[0].ID}}}
			}
			decision = investigation.Decision{Kind: "report", Report: &draft}
		}
		data, _ := json.Marshal(decision)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(data)}}}, "usage": map[string]int{"prompt_tokens": 300, "completion_tokens": 100}})
	}))
	defer server.Close()
	t.Setenv("INCIDENT_MODEL_ENDPOINT", server.URL+"/v1/chat/completions")
	t.Setenv("INCIDENT_MODEL_API_KEY", "private-test-key")
	t.Setenv("INCIDENT_MODEL_NAME", "fake-model")
	t.Setenv("INCIDENT_MODEL_CONTEXT_WINDOW", "16384")
	out := filepath.Join(t.TempDir(), "run.json")
	args := []string{"-request", requestFixture, "-fixture", evidenceFixture, "-out", out}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	// #nosec G304 -- Test-owned path within a temporary directory.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-test-key")) {
		t.Fatal("key leaked")
	}
	var saved artifact
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Mode != "model_replay" || calls != 2 || len(saved.Run.ModelCalls) != 2 || saved.Run.Report.StopReason != "REPORT_ACCEPTED" || len(saved.Run.Evidence) != 1 {
		t.Fatalf("calls=%d run=%+v", calls, saved.Run)
	}
	if saved.Run.ModelCalls[0].Charged != 400 || saved.Run.ModelCalls[1].Charged != 400 {
		t.Fatal("usage missing")
	}
	if code := run(t.Context(), args, &stdout, &stderr); code != 1 || calls != 2 {
		t.Fatal("existing output still triggered paid call")
	}
}

func TestRunConfigurationAndFailure(t *testing.T) {
	t.Run("missing config performs no call", func(t *testing.T) {
		t.Setenv("INCIDENT_MODEL_CONTEXT_WINDOW", "")
		var out bytes.Buffer
		code := run(t.Context(), []string{"-request", requestFixture, "-fixture", evidenceFixture, "-out", filepath.Join(t.TempDir(), "run.json")}, &out, &out)
		if code != 2 {
			t.Fatal(code, out.String())
		}
	})
	t.Run("failure still persists conservative audit", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(500)
			_, _ = fmt.Fprint(w, "secret=private-response")
		}))
		defer server.Close()
		t.Setenv("INCIDENT_MODEL_ENDPOINT", server.URL+"/chat")
		t.Setenv("INCIDENT_MODEL_API_KEY", "private-key")
		t.Setenv("INCIDENT_MODEL_NAME", "fake-model")
		t.Setenv("INCIDENT_MODEL_CONTEXT_WINDOW", "8192")
		path := filepath.Join(t.TempDir(), "run.json")
		var out bytes.Buffer
		if code := run(t.Context(), []string{"-request", requestFixture, "-fixture", evidenceFixture, "-out", path}, &out, &out); code != 0 {
			t.Fatal(code, out.String())
		}
		// #nosec G304 -- Test-owned path within a temporary directory.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "private") {
			t.Fatal("secret leaked")
		}
		var a artifact
		if err = json.Unmarshal(data, &a); err != nil {
			t.Fatal(err)
		}
		if calls != 3 || a.Run.Report.Result != "INCONCLUSIVE" || a.Run.ModelCalls[0].Charged != a.Run.ModelCalls[0].Reserved {
			t.Fatal(a.Run)
		}
	})
}
