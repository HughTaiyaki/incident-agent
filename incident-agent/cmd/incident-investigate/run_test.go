package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testScript = "../../testdata/investigation/observed-script.json"
const testBundle = "../../testdata/telemetry/observed-20261028/bundle.json"

func TestRun(t *testing.T) {
	t.Run("real historical bundle stays offline", func(t *testing.T) {
		// Even unusable online endpoint configuration must not affect this command.
		t.Setenv("INCIDENT_PROMETHEUS_URL", "http://invalid.example")
		output := filepath.Join(t.TempDir(), "run.json")
		args := []string{"-script", testScript, "-fixture", testBundle, "-out", output}
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%s", code, stderr.String())
		}
		// #nosec G304 -- Test-owned path within a temporary directory.
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		var a artifact
		if err = json.Unmarshal(data, &a); err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, e := range a.Run.Evidence {
			total += len(e.Records)
		}
		if a.Mode != "scripted_replay" || a.UpstreamSHA != "unknown" || a.Run.Decisions != 7 || a.Run.BackendRequests != 0 || total != 169 || a.Run.Report.Result != "INCONCLUSIVE" || a.Run.Report.StopReason != "REPORT_ACCEPTED" {
			t.Fatalf("unexpected result: %+v total=%d", a.Run, total)
		}
		if len(a.Run.Report.Facts) != 1 || len(a.Run.Report.Facts[0].EvidenceIDs) != 4 {
			t.Fatal("report lost citations")
		}
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatal(info.Mode())
		}
		if code := run(t.Context(), args, &stdout, &stderr); code != 1 {
			t.Fatal("overwrote artifact")
		}
		// #nosec G304 -- Test-owned path within a temporary directory.
		after, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatal("existing artifact changed")
		}
	})
	t.Run("cancel produces inspectable artifact", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		output := filepath.Join(t.TempDir(), "cancel.json")
		var stdout, stderr bytes.Buffer
		if code := run(ctx, []string{"-script", testScript, "-fixture", testBundle, "-out", output}, &stdout, &stderr); code != 1 {
			t.Fatalf("code=%d", code)
		}
		// #nosec G304 -- Test-owned path within a temporary directory.
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		var a artifact
		if err = json.Unmarshal(data, &a); err != nil {
			t.Fatal(err)
		}
		if a.Run.Status != "CANCELLED" || a.Run.Decisions != 0 || a.Run.Report.StopReason != "CANCELLED" {
			t.Fatal(a.Run)
		}
	})
	for _, tt := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"-h"}, 0}, {"missing", nil, 2}, {"no live mode", []string{"-mode", "live"}, 2},
		{"invalid file", []string{"-script", "missing", "-fixture", testBundle, "-out", "unused"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := run(t.Context(), tt.args, &out, &out); code != tt.code {
				t.Fatalf("code=%d want=%d", code, tt.code)
			}
		})
	}
}
