package telemetry_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func bundle(t *testing.T) telemetry.Bundle {
	t.Helper()
	q, err := telemetry.DefaultPolicy().Normalize(query())
	if err != nil {
		t.Fatal(err)
	}
	q.OrderID = "case-t3-order"
	result := telemetry.NewResult(q, "loki", "query=case-t3-order")
	result.Quality.QueryOutcome = "SUCCEEDED"
	result.Records = []telemetry.Record{{Type: "log", At: q.Start, Service: q.Service, Environment: q.Environment,
		TraceID: strings.Repeat("a", 32), Fields: map[string]string{"order_id": "case-t3-order", "event_name": "order.transition"}}}
	result.Finish()
	b, err := telemetry.NewBundle("synthetic", "unknown", []telemetry.Result{result})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBundleAnonymizationAndIntegrity(t *testing.T) {
	t.Parallel()
	b := bundle(t)
	if err := b.Anonymize(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	record := b.Entries[0].Records[0]
	if record.Fields["order_id"] != b.Entries[0].Query.OrderID || record.TraceID == strings.Repeat("a", 32) {
		t.Fatal("correlation mapping broken")
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "case-t3-order") {
		t.Fatal("experiment identifier leaked")
	}
	if _, err = telemetry.ReadBundle(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(data, []byte("order.transition"), []byte("order.tampered"), 1)
	if _, err = telemetry.ReadBundle(bytes.NewReader(tampered)); err == nil {
		t.Fatal("tampering accepted")
	}
	if _, err = telemetry.ReadBundle(strings.NewReader(string(data) + "{}")); err == nil {
		t.Fatal("trailing object accepted")
	}
}

func TestWriteBundleDoesNotOverwriteAndRestrictsPermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "evidence.json")
	b := bundle(t)
	if err := telemetry.WriteBundle(path, b); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions=%v", info.Mode().Perm())
	}
	if err = telemetry.WriteBundle(path, b); err == nil {
		t.Fatal("overwrote immutable capture")
	}
}

func TestAnonymizeOnlyRewritesIdentifierSelectors(t *testing.T) {
	t.Parallel()
	b := bundle(t)
	entry := &b.Entries[0]
	entry.Query.OrderID = "order-service"
	entry.Records[0].Fields["order_id"] = "order-service"
	entry.CanonicalQuery = `/loki/api/v1/query_range?query=%7Bservice_name%3D%22order-service%22%7D+%7C+order_id%3D%22order-service%22&start=123`
	entry.Finish()
	if err := b.Anonymize(bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	canonical := b.Entries[0].CanonicalQuery
	if !strings.Contains(canonical, "service_name%3D%22order-service%22") || !strings.Contains(canonical, "start=123") {
		t.Fatalf("non-identifier selector changed: %s", canonical)
	}
	if strings.Contains(canonical, "order_id%3D%22order-service%22") {
		t.Fatal("original business identifier remained")
	}
}

func TestReadBundleRejectsUntrustedFieldsEvenWithValidHashes(t *testing.T) {
	t.Parallel()
	b := bundle(t)
	b.Entries[0].Records[0].Fields["password"] = "do-not-copy"
	b.Entries[0].Finish()
	forged, err := telemetry.NewBundle("synthetic", "unknown", b.Entries)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = telemetry.ReadBundle(bytes.NewReader(data)); err == nil {
		t.Fatal("a valid hash was treated as permission to import arbitrary fields")
	}
}
