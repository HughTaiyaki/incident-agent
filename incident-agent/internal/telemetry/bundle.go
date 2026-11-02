package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxBundleEntries bounds a bundle's queries. Evaluation captures cover every service,
// metric and severity so a replay can answer what a live backend could.
const MaxBundleEntries = 128

// MaxBundleBytes bounds an imported or exported evidence bundle.
const MaxBundleBytes = 16 << 20

// Bundle contains only sanitized query evidence; evaluator truth is deliberately absent.
type Bundle struct {
	SchemaVersion string    `json:"schema_version"`
	PolicyVersion string    `json:"policy_version"`
	Origin        string    `json:"origin"`
	UpstreamSHA   string    `json:"upstream_sha"`
	CapturedAt    time.Time `json:"captured_at"`
	Entries       []Result  `json:"entries"`
	Digest        string    `json:"digest"`
}

// NewBundle seals a collection. Unknown runtime provenance must remain "unknown".
func NewBundle(origin, upstreamSHA string, entries []Result) (Bundle, error) {
	if origin != "live" && origin != "synthetic" && origin != "replay" {
		return Bundle{}, errors.New("invalid bundle origin")
	}
	if upstreamSHA != "unknown" && !revisionPattern.MatchString(upstreamSHA) {
		return Bundle{}, errors.New("invalid upstream revision")
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return Bundle{}, errors.New("invalid evidence entries")
	}
	if len(encoded) > MaxBundleBytes {
		return Bundle{}, ErrTooLarge
	}
	detached := []Result{}
	if err = json.Unmarshal(encoded, &detached); err != nil {
		return Bundle{}, errors.New("invalid evidence entries")
	}
	b := Bundle{SchemaVersion: "1", PolicyVersion: PolicyVersion, Origin: origin, UpstreamSHA: upstreamSHA,
		CapturedAt: time.Now().UTC(), Entries: detached}
	if b.Entries == nil {
		b.Entries = []Result{}
	}
	if err := b.seal(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func (b *Bundle) seal() error {
	b.Digest = ""
	data, err := json.Marshal(b)
	if err != nil {
		return errors.New("bundle contains invalid values")
	}
	b.Digest = Hash(data)
	return nil
}

// IsIdentifierField reports whether a record field holds a business identifier.
func IsIdentifierField(key string) bool {
	switch key {
	case "order_id", "operation_id", "event_id", "order.no", "payment.id", "messaging.message.id":
		return true
	}
	return false
}

// Anonymize replaces correlation identifiers consistently across queries and records.
// The caller supplies a fresh collection salt; it is not included in the bundle.
func (b *Bundle) Anonymize(salt []byte) error {
	if len(salt) < 16 {
		return errors.New("anonymization salt is too short")
	}
	replacements := map[string]string{}
	register := func(value string, size int) {
		if value == "" {
			return
		}
		replacements[value] = Hash(append(append([]byte{}, salt...), []byte(value)...))[:size]
	}
	for _, entry := range b.Entries {
		q := entry.Query
		for _, value := range []string{q.OrderID, q.OperationID, q.EventID} {
			register(value, 32)
		}
		register(q.TraceID, 32)
		for _, record := range entry.Records {
			register(record.TraceID, 32)
			register(record.SpanID, 16)
			for key, value := range record.Fields {
				if IsIdentifierField(key) {
					register(value, 32)
				}
			}
			for _, reference := range record.References {
				register(reference.TraceID, 32)
				register(reference.SpanID, 16)
			}
		}
	}
	for i := range b.Entries {
		entry := &b.Entries[i]
		q := &entry.Query
		q.OrderID = replacements[q.OrderID]
		q.OperationID = replacements[q.OperationID]
		q.EventID = replacements[q.EventID]
		q.TraceID = replacements[q.TraceID]
		// Decode URL values before replacing IDs so percent-escaped values cannot leak.
		entry.CanonicalQuery = anonymizeQuery(entry.CanonicalQuery, replacements)
		entry.QueryID = Hash([]byte(entry.CanonicalQuery))
		for j := range entry.Records {
			record := &entry.Records[j]
			record.TraceID = replacements[record.TraceID]
			record.SpanID = replacements[record.SpanID]
			for key, value := range record.Fields {
				if IsIdentifierField(key) {
					record.Fields[key] = replacements[value]
				}
			}
			for k := range record.References {
				reference := &record.References[k]
				reference.TraceID = replacements[reference.TraceID]
				reference.SpanID = replacements[reference.SpanID]
			}
		}
		entry.Finish()
	}
	return b.seal()
}

// WriteBundle creates an owner-readable file without overwriting an existing capture.
func WriteBundle(path string, b Bundle) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return errors.New("encode evidence bundle")
	}
	if len(data) > MaxBundleBytes {
		return ErrTooLarge
	}
	// #nosec G304 -- Operator-selected export path; O_EXCL and 0600 prevent overwrite and broad access.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence bundle: %w", err)
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write evidence bundle: %w", err)
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync evidence bundle: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close evidence bundle: %w", err)
	}
	return nil
}

// ReadBundle verifies size, schema, and content hashes before returning a frozen collection.
func ReadBundle(reader io.Reader) (Bundle, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxBundleBytes+1))
	if err != nil {
		return Bundle{}, fmt.Errorf("read evidence bundle: %w", err)
	}
	if len(data) > MaxBundleBytes {
		return Bundle{}, ErrTooLarge
	}
	var b Bundle
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&b); err != nil {
		return Bundle{}, errors.New("invalid evidence bundle")
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return Bundle{}, errors.New("trailing evidence bundle data")
	}
	if b.SchemaVersion != "1" || b.PolicyVersion != PolicyVersion || len(b.Entries) > MaxBundleEntries {
		return Bundle{}, errors.New("unsupported evidence bundle")
	}
	digest := b.Digest
	if err = b.seal(); err != nil || b.Digest != digest {
		return Bundle{}, errors.New("bundle digest mismatch")
	}
	if b.Origin != "live" && b.Origin != "synthetic" && b.Origin != "replay" {
		return Bundle{}, errors.New("invalid bundle origin")
	}
	policy := DefaultPolicy()
	for _, entry := range b.Entries {
		q, err := policy.Normalize(entry.Query)
		if err != nil {
			return Bundle{}, err
		}
		source := strings.TrimPrefix(entry.Source, "fixture:")
		for strings.HasPrefix(source, "fixture:") {
			source = strings.TrimPrefix(source, "fixture:")
		}
		if source != "prometheus" && source != "loki" && source != "jaeger" && source != "fixture" && source != "harness" {
			return Bundle{}, errors.New("invalid evidence source")
		}
		for _, record := range entry.Records {
			if err := ValidateRecord(record, q, policy); err != nil {
				return Bundle{}, err
			}
		}
		allowedReasons := []string{"backend_warning", "series_limit_reached", "out_of_scope_series", "filter_mismatch",
			"point_limit_reached", "out_of_scope_point", "undefined_metric_value", "record_limit_reached", "out_of_scope_record",
			"free_text_omitted", "trace_limit_reached", "trace_id_mismatch", "requested_service_not_visible", "span_limit_reached",
			"missing_process", "out_of_scope_span", "span_crosses_window", "invalid_reference", "reference_target_not_visible",
			"recorded_source_unavailable", "frozen_sample_only"}
		for _, reason := range entry.Quality.MissingReasons {
			if !slices.Contains(allowedReasons, reason) {
				return Bundle{}, errors.New("invalid source quality reason")
			}
		}
		expected := entry.Digest
		entry.Finish()
		if entry.Digest != expected {
			return Bundle{}, errors.New("evidence digest mismatch")
		}
	}
	return b, nil
}

func anonymizeQuery(canonical string, replacements map[string]string) string {
	rewrite := func(q *Query) {
		q.TraceID = replacements[q.TraceID]
		q.OrderID = replacements[q.OrderID]
		q.OperationID = replacements[q.OperationID]
		q.EventID = replacements[q.EventID]
	}
	if strings.HasPrefix(canonical, "{") {
		var q Query
		if json.Unmarshal([]byte(canonical), &q) != nil {
			return "opaque:" + Hash([]byte(canonical))
		}
		rewrite(&q)
		data, _ := json.Marshal(q)
		return string(data)
	}
	path, raw, hasQuery := strings.Cut(canonical, "?")
	if strings.HasPrefix(path, "/api/traces/") {
		original := strings.TrimPrefix(path, "/api/traces/")
		if replacement, ok := replacements[original]; ok {
			path = "/api/traces/" + replacement
		}
	}
	if !hasQuery {
		if strings.HasPrefix(path, "/api/traces/") {
			return path
		}
		return "opaque:" + Hash([]byte(canonical))
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "opaque:" + Hash([]byte(canonical))
	}
	expression := values.Get("query")
	pairs := []string{}
	for original, replacement := range replacements {
		for _, key := range []string{"trace_id", "order_id", "operation_id", "event_id"} {
			pairs = append(pairs, key+"="+strconv.Quote(original), key+"="+strconv.Quote(replacement))
		}
	}
	if expression != "" {
		values.Set("query", strings.NewReplacer(pairs...).Replace(expression))
	}
	return path + "?" + values.Encode()
}
