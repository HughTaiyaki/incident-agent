package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func completeEnv() map[string]string {
	return map[string]string{
		"INCIDENT_API_TOKEN": strings.Repeat("t", minTokenBytes), "INCIDENT_MAX_ACTIVE_RUNS": "2",
		"INCIDENT_SYSTEM_PROFILE_FILE": mallProfile, "INCIDENT_MODEL_NAME": "m", "INCIDENT_MODEL_ENDPOINT": "http://127.0.0.1:1/chat", "INCIDENT_MODEL_CONTEXT_WINDOW": "32000",
		"INCIDENT_PROMETHEUS_URL": "http://127.0.0.1:9090", "INCIDENT_LOKI_URL": "http://127.0.0.1:3100", "INCIDENT_JAEGER_URL": "http://127.0.0.1:16686",
		"INCIDENT_REDIS_NETWORK": "unix", "INCIDENT_REDIS_ADDRESS": "/tmp/unused.sock", "INCIDENT_REDIS_NAMESPACE": "test",
	}
}

const mallProfile = "../../deploy/profiles/goframeshop.json"

func TestNoTokenLeavesInvestigationAPIDisabled(t *testing.T) {
	deps, closeDeps, err := loadDeps(envOf(nil))
	if err != nil || deps != nil {
		t.Fatalf("deps=%v err=%v", deps, err)
	}
	closeDeps()
}

func TestConfigurationFailsClosed(t *testing.T) {
	for _, key := range []string{"INCIDENT_MAX_ACTIVE_RUNS", "INCIDENT_MODEL_NAME", "INCIDENT_MODEL_ENDPOINT", "INCIDENT_MODEL_CONTEXT_WINDOW", "INCIDENT_PROMETHEUS_URL", "INCIDENT_LOKI_URL", "INCIDENT_JAEGER_URL", "INCIDENT_REDIS_NETWORK", "INCIDENT_REDIS_ADDRESS", "INCIDENT_REDIS_NAMESPACE"} {
		env := completeEnv()
		delete(env, key)
		if _, _, err := loadDeps(envOf(env)); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	env := completeEnv()
	env["INCIDENT_API_TOKEN"] = "short"
	if _, _, err := loadDeps(envOf(env)); err == nil || strings.Contains(err.Error(), "short") {
		t.Fatalf("short token: %v", err)
	}
}

func TestCompleteConfigurationFreezesWorkerIdentity(t *testing.T) {
	deps, closeDeps, err := loadDeps(envOf(completeEnv()))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeps()
	if deps.MaxActive != 2 || deps.Template.TelemetryID == "" || deps.Template.ProviderID == "" || len(deps.Template.Services) == 0 {
		t.Fatalf("%+v", deps.Template)
	}
	if strings.Contains(deps.Template.ProviderID, "127.0.0.1") {
		t.Fatal("address stored in identity")
	}
}

func TestAddressDefaultsToLoopback(t *testing.T) {
	if got := address(envOf(nil)); got != "127.0.0.1:8080" {
		t.Fatal(got)
	}
	if got := address(envOf(map[string]string{"INCIDENT_API_BIND": "0.0.0.0", "PORT": "9000"})); got != "0.0.0.0:9000" {
		t.Fatal(got)
	}
}

func TestProfileAndFlowFreezeIntoTheTemplate(t *testing.T) {
	values := map[string]string{"INCIDENT_API_TOKEN": strings.Repeat("t", minTokenBytes), "INCIDENT_MAX_ACTIVE_RUNS": "2", "INCIDENT_SYSTEM_PROFILE_FILE": mallProfile,
		"INCIDENT_MODEL_NAME": "m", "INCIDENT_MODEL_ENDPOINT": "http://127.0.0.1:1/chat", "INCIDENT_MODEL_CONTEXT_WINDOW": "32000",
		"INCIDENT_PROMETHEUS_URL": "http://127.0.0.1:9090", "INCIDENT_LOKI_URL": "http://127.0.0.1:3100", "INCIDENT_JAEGER_URL": "http://127.0.0.1:16686",
		"INCIDENT_REDIS_NETWORK": "unix", "INCIDENT_REDIS_ADDRESS": filepath.Join(t.TempDir(), "unused.sock"), "INCIDENT_REDIS_NAMESPACE": "t", "INCIDENT_REDIS_PASSWORD": "p"}
	load := func(key, value string) error {
		copied := map[string]string{}
		for k, v := range values {
			copied[k] = v
		}
		copied[key] = value
		deps, closeDeps, err := loadDeps(envOf(copied))
		if err == nil {
			defer closeDeps()
			if deps.Template.ProfileVersion == "" || deps.Template.SystemFacts == "" || len(deps.Template.Environments) == 0 {
				t.Errorf("%+v", deps.Template)
			}
			if key == flowEnvVar && deps.Template.Flow != value {
				t.Errorf("flow %q", deps.Template.Flow)
			}
		}
		return err
	}
	if err := load(flowEnvVar, "hybrid"); err != nil {
		t.Fatal(err)
	}
	if err := load(flowEnvVar, "other"); err == nil {
		t.Fatal("unknown flow accepted")
	}
	if err := load("INCIDENT_SYSTEM_PROFILE_FILE", ""); err == nil {
		t.Fatal("missing profile accepted")
	}
}
