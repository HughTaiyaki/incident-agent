package investigation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func logSample(t *testing.T, count int) telemetry.Result {
	t.Helper()
	q, err := telemetry.DefaultPolicy().Normalize(query())
	if err != nil {
		t.Fatal(err)
	}
	result := observation(q, "sample")
	result.Records = []telemetry.Record{}
	for i := range count {
		result.Records = append(result.Records, telemetry.Record{Type: "log", At: q.Start.Add(time.Duration(i) * time.Second),
			Service: q.Service, Environment: q.Environment, Fields: map[string]string{"severity_text": "INFO", "event_name": "event.one", "order_id": fmt.Sprint(i)}})
	}
	result.Finish()
	return result
}

func TestSummarizeLogs(t *testing.T) {
	t.Run("correlation IDs do not split patterns and representatives preserve outcomes", func(t *testing.T) {
		entry := logSample(t, 6)
		for i := range entry.Records {
			entry.Records[i].Fields["outcome"] = "success"
		}
		entry.Records[3].Fields["outcome"] = "failed"
		entry.Finish()
		first, last, middle := entry.Records[0].ID, entry.Records[5].ID, entry.Records[3].ID
		got, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Groups) != 1 || got.Groups[0].ObservedCount != 6 || got.ScannedRecords != 6 || got.CountBasis != "OBSERVED_SAMPLE" {
			t.Fatal(got)
		}
		if !slices.Equal(got.Groups[0].RecordIDs, []string{first, middle, last}) {
			t.Fatal(got.Groups[0])
		}
		if got.Groups[0].Key.ErrorType != "unknown" || got.ResultDigest != entry.Digest || got.QueryID != entry.QueryID {
			t.Fatal(got)
		}
		// Shuffle while preserving record identity; grouping and representatives stay stable.
		slices.Reverse(entry.Records)
		entry.Finish()
		shuffled, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw(shuffled.Groups), raw(got.Groups)) {
			t.Fatal("unstable aggregation")
		}
	})
	t.Run("bounded groups reserve success candidate", func(t *testing.T) {
		entry := logSample(t, 25)
		for i := range entry.Records {
			entry.Records[i].Fields["event_name"] = fmt.Sprintf("event.%02d", i/2)
			entry.Records[i].Fields["severity_text"] = "ERROR"
		}
		entry.Records[24].Fields["event_name"] = "zzz.normal"
		entry.Records[24].Fields["severity_text"] = "INFO"
		entry.Records[24].Fields["outcome"] = "success"
		entry.Quality.Truncated = true
		entry.Missing("record_limit_reached")
		entry.Finish()
		got, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Groups) != 10 || got.OmittedGroups != 3 || !got.Truncated || got.Groups[9].Key.EventName != "zzz.normal" {
			t.Fatal(got)
		}
		if got.SourceStatus != "PARTIAL" || got.SourceQuality.Coverage != "PARTIAL" {
			t.Fatal("quality upgraded")
		}
		got.SourceQuality.MissingReasons[0] = "changed"
		*got.SourceQuality.LatestEventAt = time.Time{}
		if entry.Quality.MissingReasons[0] == "changed" || entry.Quality.LatestEventAt.IsZero() {
			t.Fatal("shared quality memory")
		}
	})
	t.Run("unknown fields stay unknown", func(t *testing.T) {
		entry := logSample(t, 1)
		entry.Records[0].Fields = map[string]string{}
		entry.Finish()
		got, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got.Groups[0].Key.EventName != "unknown" || got.Groups[0].Key.Severity != "unknown" || len(got.Groups[0].RecordIDs) != 1 {
			t.Fatal(got)
		}
	})
	for _, status := range []string{"empty", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			entry := logSample(t, 0)
			if status == "unavailable" {
				entry.Quality.QueryOutcome = "FAILED"
				entry.Status = "UNAVAILABLE"
			}
			entry.Finish()
			got, err := SummarizeLogs(entry)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Groups) != 0 || got.ScannedRecords != 0 || got.SourceStatus != entry.Status || got.SourceQuality.Coverage != "UNKNOWN" {
				t.Fatal(got)
			}
		})
	}
	t.Run("non logs and unverified evidence rejected", func(t *testing.T) {
		entry := logSample(t, 1)
		entry.Query.Kind = "metrics"
		if _, err := SummarizeLogs(entry); err == nil {
			t.Fatal("accepted non logs")
		}
		entry = logSample(t, 101)
		if _, err := SummarizeLogs(entry); err == nil {
			t.Fatal("accepted >100 records")
		}
		entry = logSample(t, 1)
		entry.Records[0].Service = "goods-service"
		entry.Finish()
		if _, err := SummarizeLogs(entry); err == nil {
			t.Fatal("accepted out of scope")
		}
		entry = logSample(t, 1)
		entry.Digest = "forged"
		if _, err := SummarizeLogs(entry); err == nil {
			t.Fatal("accepted bad digest")
		}
	})
	t.Run("same timestamp ties sorted by ID", func(t *testing.T) {
		entry := logSample(t, 4)
		for i := range entry.Records {
			entry.Records[i].At = entry.Query.Start
		}
		entry.Finish()
		a, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		slices.Reverse(entry.Records)
		entry.Finish()
		b, err := SummarizeLogs(entry)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw(a.Groups), raw(b.Groups)) {
			t.Fatal("unstable tie")
		}
	})
}

func TestExecuteLogPatterns(t *testing.T) {
	calls := 0
	result, err := Execute(t.Context(), request(), decideFunc(func(_ context.Context, v View) (json.RawMessage, error) {
		if v.Step == 1 {
			q := query()
			return raw(Decision{Kind: "query", Query: &q}), nil
		}
		if len(v.LogPatterns) != 1 || len(v.LogPatterns[0].Groups) != 1 {
			t.Fatal(v.LogPatterns)
		}
		id := v.LogPatterns[0].Groups[0].RecordIDs[0]
		v.LogPatterns[0].Groups[0].RecordIDs[0] = "forged"
		v.LogPatterns[0].SourceQuality.MissingReasons = append(v.LogPatterns[0].SourceQuality.MissingReasons, "mutated")
		return raw(Decision{Kind: "report", Report: ptr(validDraft(id))}), nil
	}), queryFunc(func(_ context.Context, q telemetry.Query, _ *telemetry.Budget) (telemetry.Result, error) {
		calls++
		return observation(q, "1"), nil
	}))
	if err != nil || calls != 1 || result.BackendRequests != 0 || len(result.LogPatterns) != 1 {
		t.Fatalf("%+v calls=%d err=%v", result, calls, err)
	}
	summary := result.LogPatterns[0]
	if summary.Groups[0].RecordIDs[0] != result.Evidence[0].Records[0].ID || len(summary.SourceQuality.MissingReasons) != 0 {
		t.Fatal("summary was mutated")
	}
	if result.Report.StopReason != "REPORT_ACCEPTED" {
		t.Fatal(result.Report)
	}
}
