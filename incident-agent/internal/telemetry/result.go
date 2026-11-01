package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

// Reference preserves Jaeger reference semantics instead of inventing parentage.
type Reference struct {
	Type    string `json:"type"`
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// Record is a whitelisted, bounded piece of evidence; arbitrary log bodies are omitted.
type Record struct {
	CollectedAt time.Time         `json:"collected_at"`
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	At          time.Time         `json:"at"`
	Service     string            `json:"service"`
	Environment string            `json:"environment"`
	TraceID     string            `json:"trace_id,omitempty"`
	SpanID      string            `json:"span_id,omitempty"`
	Fields      map[string]string `json:"fields"`
	Value       *float64          `json:"value,omitempty"`
	References  []Reference       `json:"references,omitempty"`
}

// Quality separates source reachability from query coverage and freshness.
type Quality struct {
	Reachability    string     `json:"reachability"`
	QueryOutcome    string     `json:"query_outcome"`
	ReturnedRecords int        `json:"returned_records"`
	Truncated       bool       `json:"truncated"`
	Freshness       string     `json:"freshness"`
	Coverage        string     `json:"coverage"`
	MissingReasons  []string   `json:"missing_reasons"`
	LatestEventAt   *time.Time `json:"latest_event_at,omitempty"`
}

// Result freezes query scope, provenance and sanitized records together.
type Result struct {
	Query          Query     `json:"query"`
	Source         string    `json:"source"`
	QueryID        string    `json:"query_id"`
	CanonicalQuery string    `json:"canonical_query"`
	CollectedAt    time.Time `json:"collected_at"`
	PolicyVersion  string    `json:"policy_version"`
	Status         string    `json:"source_status"`
	Quality        Quality   `json:"source_quality"`
	CountBasis     string    `json:"count_basis"`
	Records        []Record  `json:"records"`
	Requests       int       `json:"requests"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Digest         string    `json:"digest"`
}

// NewResult creates conservative quality defaults. An empty result is never proof of recovery.
func NewResult(q Query, source, canonical string) Result {
	return Result{Query: q, Source: source, QueryID: Hash([]byte(canonical)), CanonicalQuery: canonical,
		CollectedAt: time.Now().UTC(), PolicyVersion: PolicyVersion, Status: "UNAVAILABLE", CountBasis: "OBSERVED_SAMPLE",
		Records: []Record{}, Quality: Quality{Reachability: "UNKNOWN", QueryOutcome: "FAILED",
			Freshness: "UNKNOWN", Coverage: "UNKNOWN", MissingReasons: []string{}},
	}
}

// Missing records a deterministic quality reason without copying upstream error text.
func (r *Result) Missing(reason string) {
	if !slices.Contains(r.Quality.MissingReasons, reason) {
		r.Quality.MissingReasons = append(r.Quality.MissingReasons, reason)
	}
	r.Quality.Coverage = "PARTIAL"
}

// Finish seals a result after all filtering. Digest detects modification, not malicious tampering.
func (r *Result) Finish() {
	r.Quality.ReturnedRecords = len(r.Records)
	if r.Quality.QueryOutcome == "SUCCEEDED" {
		r.Status = "AVAILABLE"
		if len(r.Records) == 0 {
			r.Status = "EMPTY"
		}
		if r.Quality.Coverage == "PARTIAL" {
			r.Status = "PARTIAL"
		}
	}
	for i := range r.Records {
		record := &r.Records[i]
		if record.CollectedAt.IsZero() {
			record.CollectedAt = r.CollectedAt
		}
		record.ID = ""
		data, _ := json.Marshal(record) // Record contains only JSON-safe values after adapter validation.
		record.ID = Hash(data)
		if r.Quality.LatestEventAt == nil || record.At.After(*r.Quality.LatestEventAt) {
			at := record.At
			r.Quality.LatestEventAt = &at
		}
	}
	r.Digest = ""
	data, _ := json.Marshal(r)
	r.Digest = Hash(data)
}

// Hash returns the SHA-256 content digest used by snapshots and manifests.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
