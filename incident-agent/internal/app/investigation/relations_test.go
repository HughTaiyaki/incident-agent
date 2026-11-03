package investigation

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

var relationStart = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

// span builds a record starting at atMs that lasted ms, under parent ("" for a root).
func span(id, service, parent string, atMs, ms int, extra ...string) telemetry.Record {
	fields := map[string]string{"duration_us": strconv.Itoa(ms * 1000), "operation": "svc/Call", "rpc.method": "Call"}
	for i := 0; i+1 < len(extra); i += 2 {
		fields[extra[i]] = extra[i+1]
	}
	record := telemetry.Record{ID: "id-" + id, Type: "span", TraceID: "t", SpanID: id, Service: service,
		At: relationStart.Add(time.Duration(atMs) * time.Millisecond), Fields: fields}
	if parent != "" {
		record.References = []telemetry.Reference{{Type: "CHILD_OF", TraceID: "t", SpanID: parent}}
	}
	return record
}

func relationsOf(records ...telemetry.Record) []CallRelation {
	return CallRelations([]telemetry.Result{{Records: records}})
}

func failed(description string) []string {
	return []string{"error", "true", "status_description", description}
}

func TestCallRelationsCounterexamples(t *testing.T) {
	root := span("root", "a", "", 0, 2000)
	t.Run("slow callee and caller deadline are only numbers", func(t *testing.T) {
		got := relationsOf(root, span("c", "a", "root", 0, 800, failed("context deadline exceeded")...), span("s", "b", "c", 5, 1200, failed("internal error")...),
			span("db", "b", "s", 1190, 3, "operation", "DB.Begin"))
		if len(got) != 1 || *got[0].CallerMs != 800 || *got[0].CalleeMs != 1200 || *got[0].CalleeEndAfterCallerMs != 405 || got[0].LongestChild == nil || got[0].LongestChild.Ms != 3 {
			t.Fatalf("%+v", got)
		}
		encoded, _ := json.Marshal(got)
		for _, word := range []string{"slow", "cause", "root"} {
			if strings.Contains(strings.ToLower(string(encoded)), word) {
				t.Fatalf("relation concludes %q: %s", word, encoded)
			}
		}
	})
	t.Run("caller cancelled first looks the same and claims nothing more", func(t *testing.T) {
		got := relationsOf(root, span("c", "a", "root", 0, 10, failed("context canceled")...), span("s", "b", "c", 2, 50))
		if len(got) != 1 || *got[0].CalleeEndAfterCallerMs != 42 || got[0].CalleeStatus != "ok" || got[0].CallerStatus != "context canceled" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("callee stopped by the deadline does not overrun", func(t *testing.T) {
		got := relationsOf(root, span("c", "a", "root", 0, 800, failed("deadline")...), span("s", "b", "c", 5, 793, failed("internal error")...),
			span("db", "b", "s", 40, 739, "operation", "DB.Query"))
		if len(got) != 1 || *got[0].CalleeEndAfterCallerMs != -2 || got[0].LongestChild.Operation != "DB.Query" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("retries stay separate calls", func(t *testing.T) {
		got := relationsOf(root, span("c1", "a", "root", 0, 800, failed("deadline")...), span("s1", "b", "c1", 1, 1200),
			span("c2", "a", "root", 1000, 800, failed("deadline")...), span("s2", "b", "c2", 1001, 1200))
		if len(got) != 2 || got[0].CallerSpan == got[1].CallerSpan {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("parallel calls each compare with their own caller", func(t *testing.T) {
		got := relationsOf(root, span("c1", "a", "root", 0, 100), span("s1", "b", "c1", 1, 300), span("c2", "a", "root", 0, 500), span("s2", "c", "c2", 1, 40, failed("boom")...))
		if len(got) != 2 || got[0].Callee != "c" || got[1].Callee != "b" || *got[1].CalleeEndAfterCallerMs != 201 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("clock skew keeps durations and drops the ordering", func(t *testing.T) {
		for name, at := range map[string]int{"starts before caller": -50, "starts after caller ended": 900} {
			got := relationsOf(root, span("c", "a", "root", 0, 800, failed("deadline")...), span("s", "b", "c", at, 1200))
			if len(got) != 1 || !got[0].TimingUnreliable || got[0].CalleeEndAfterCallerMs != nil || *got[0].CalleeMs != 1200 {
				t.Fatalf("%s: %+v", name, got)
			}
		}
	})
	t.Run("skewed clocks do not make a quick call look long", func(t *testing.T) {
		if got := relationsOf(root, span("c", "a", "root", 0, 100), span("s", "b", "c", -500, 90)); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("missing callee record is reported as missing", func(t *testing.T) {
		got := relationsOf(root, span("c", "a", "root", 0, 800, failed("unavailable")...))
		if len(got) != 1 || got[0].Callee != "" || got[0].CalleeSpan != "" || got[0].CalleeMs != nil || got[0].CalleeEndAfterCallerMs != nil || got[0].TimingUnreliable {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("missing caller record is reported as missing", func(t *testing.T) {
		got := relationsOf(span("s", "b", "gone", 0, 50, failed("internal error")...))
		if len(got) != 1 || got[0].Caller != "" || got[0].CallerMs != nil || got[0].Callee != "b" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("healthy calls and local work are left out", func(t *testing.T) {
		got := relationsOf(root, span("c", "a", "root", 0, 100), span("s", "b", "c", 2, 90), span("local", "a", "root", 0, 900, "error", "true", "rpc.method", ""))
		if len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("async follows-from is not treated as a call", func(t *testing.T) {
		consumer := span("m", "b", "", 5000, 50, failed("boom")...)
		consumer.References = []telemetry.Reference{{Type: "FOLLOWS_FROM", TraceID: "t", SpanID: "root"}}
		if got := relationsOf(root, consumer); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("message consumer under its publisher is not a call", func(t *testing.T) {
		publisher := span("p", "a", "root", 0, 4, "rpc.method", "")
		consumer := span("m", "b", "p", 5000, 7, "rpc.method", "", "error", "true")
		if got := relationsOf(root, publisher, consumer); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("service names do not change the result", func(t *testing.T) {
		rename := func(a, b string) []CallRelation {
			return relationsOf(span("root", a, "", 0, 2000), span("c", a, "root", 0, 800, failed("deadline")...), span("s", b, "c", 5, 1200))
		}
		first, second := rename("order-service", "goods-service")[0], rename("alpha", "beta")[0]
		if *first.CallerMs != *second.CallerMs || *first.CalleeEndAfterCallerMs != *second.CalleeEndAfterCallerMs || first.failed != second.failed {
			t.Fatalf("%+v %+v", first, second)
		}
	})
	t.Run("the same span from two queries counts once", func(t *testing.T) {
		records := []telemetry.Record{root, span("c", "a", "root", 0, 800, failed("deadline")...), span("s", "b", "c", 5, 1200)}
		if got := CallRelations([]telemetry.Result{{Records: records}, {Records: records}}); len(got) != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("unusable duration is skipped", func(t *testing.T) {
		bad := span("s", "b", "c", 5, 1200)
		bad.Fields["duration_us"] = "soon"
		if got := relationsOf(root, span("c", "a", "root", 0, 800), bad); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
}
