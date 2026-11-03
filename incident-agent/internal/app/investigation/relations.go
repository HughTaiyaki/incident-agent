package investigation

import (
	"slices"
	"strconv"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// relationMinExcess keeps ordinary clock and network jitter out of the relations.
const relationMinExcess = 10 * time.Millisecond

// ChildTime says where the most recorded time inside a callee went.
type ChildTime struct {
	Operation string `json:"operation"`
	Ms        int64  `json:"ms"`
}

// CallRelation compares one call across services as each side recorded it. It holds numbers
// and links only and never concludes why something happened: a caller may end first because
// the callee was slow or because the caller was cancelled, and the spans cannot tell which.
// Empty span handles mean that side left no record, which is not the same as it not running.
type CallRelation struct {
	Operation    string `json:"operation"`
	Caller       string `json:"caller,omitempty"`
	CallerSpan   string `json:"caller_span,omitempty"`
	CallerMs     *int64 `json:"caller_ms,omitempty"`
	CallerStatus string `json:"caller_status,omitempty"`
	Callee       string `json:"callee,omitempty"`
	CalleeSpan   string `json:"callee_span,omitempty"`
	CalleeMs     *int64 `json:"callee_ms,omitempty"`
	CalleeStatus string `json:"callee_status,omitempty"`
	// CalleeEndAfterCallerMs is nil when timing is unreliable: start times from two hosts
	// can only be compared when the callee's start falls inside the caller's span.
	CalleeEndAfterCallerMs *int64     `json:"callee_end_after_caller_ms,omitempty"`
	TimingUnreliable       bool       `json:"timing_unreliable,omitempty"`
	LongestChild           *ChildTime `json:"longest_child,omitempty"`
	at                     time.Time
	failed                 bool
}

func spanMs(d time.Duration) int64 { return d.Milliseconds() }

func spanDuration(record telemetry.Record) (time.Duration, bool) {
	us, err := strconv.ParseInt(record.Fields["duration_us"], 10, 64)
	if err != nil || us < 0 {
		return 0, false
	}
	return time.Duration(us) * time.Microsecond, true
}

func spanFailed(record telemetry.Record) bool {
	return record.Fields["error"] == "true" || record.Fields["otel.status_code"] == "ERROR"
}

func isRPC(record telemetry.Record) bool { return record.Fields["rpc.method"] != "" }

func spanStatus(record telemetry.Record) string {
	if description := record.Fields["status_description"]; description != "" {
		return description
	}
	if spanFailed(record) {
		return "error"
	}
	return "ok"
}

// CallRelations derives the calls worth comparing from every span in the evidence, whether or
// not it fits in a prompt: those with a failed side, a side without a record, or a callee that
// outlived its caller. Failures come first, then the largest overrun.
func CallRelations(evidence []telemetry.Result) []CallRelation {
	type key struct{ trace, span string }
	spans := map[key]telemetry.Record{}
	order := []key{}
	for _, entry := range evidence {
		for _, record := range entry.Records {
			k := key{record.TraceID, record.SpanID}
			if _, seen := spans[k]; record.Type != "span" || record.SpanID == "" || seen {
				continue
			}
			if _, valid := spanDuration(record); !valid {
				continue
			}
			spans[k] = record
			order = append(order, k)
		}
	}
	parentOf := func(record telemetry.Record) (telemetry.Record, bool, bool) {
		for _, reference := range record.References {
			if reference.Type == "CHILD_OF" {
				parent, found := spans[key{reference.TraceID, reference.SpanID}]
				return parent, found, true
			}
		}
		return telemetry.Record{}, false, false
	}
	longest := map[key]ChildTime{}
	hasRemoteChild := map[key]bool{}
	for _, k := range order {
		record := spans[k]
		parent, found, _ := parentOf(record)
		if !found {
			continue
		}
		parentKey := key{parent.TraceID, parent.SpanID}
		hasRemoteChild[parentKey] = hasRemoteChild[parentKey] || parent.Service != record.Service
		if d, _ := spanDuration(record); d.Milliseconds() >= longest[parentKey].Ms {
			longest[parentKey] = ChildTime{Operation: record.Fields["operation"], Ms: spanMs(d)}
		}
	}
	relations := []CallRelation{}
	for _, k := range order {
		record := spans[k]
		parent, found, linked := parentOf(record)
		d, _ := spanDuration(record)
		ms := spanMs(d)
		switch {
		case !isRPC(record):
			// A consumer starts after its publisher ended by design, so timing means nothing here.
		case found && parent.Service != record.Service && isRPC(parent):
			parentDuration, _ := spanDuration(parent)
			parentMs := spanMs(parentDuration)
			relation := CallRelation{Operation: record.Fields["operation"], Caller: parent.Service, CallerSpan: shortID(parent.ID), CallerMs: &parentMs,
				CallerStatus: spanStatus(parent), Callee: record.Service, CalleeSpan: shortID(record.ID), CalleeMs: &ms, CalleeStatus: spanStatus(record),
				at: parent.At, failed: spanFailed(parent) || spanFailed(record)}
			if child, ok := longest[k]; ok {
				relation.LongestChild = &child
			}
			parentEnd := parent.At.Add(parentDuration)
			excess := d - parentDuration
			if record.At.Before(parent.At) || record.At.After(parentEnd) {
				relation.TimingUnreliable = true
			} else {
				excess = record.At.Add(d).Sub(parentEnd)
				overrun := spanMs(excess)
				relation.CalleeEndAfterCallerMs = &overrun
			}
			if relation.failed || excess >= relationMinExcess {
				relations = append(relations, relation)
			}
		case linked && !found && spanFailed(record):
			relation := CallRelation{Operation: record.Fields["operation"], Callee: record.Service, CalleeSpan: shortID(record.ID), CalleeMs: &ms,
				CalleeStatus: spanStatus(record), at: record.At, failed: true}
			if child, ok := longest[k]; ok {
				relation.LongestChild = &child
			}
			relations = append(relations, relation)
		case found && spanFailed(record) && !hasRemoteChild[k]:
			relations = append(relations, CallRelation{Operation: record.Fields["operation"], Caller: record.Service, CallerSpan: shortID(record.ID),
				CallerMs: &ms, CallerStatus: spanStatus(record), at: record.At, failed: true})
		}
	}
	overrun := func(r CallRelation) int64 {
		if r.CalleeEndAfterCallerMs != nil {
			return *r.CalleeEndAfterCallerMs
		}
		return 0
	}
	slices.SortStableFunc(relations, func(a, b CallRelation) int {
		if a.failed != b.failed {
			if a.failed {
				return -1
			}
			return 1
		}
		if diff := overrun(b) - overrun(a); diff != 0 {
			if diff > 0 {
				return 1
			}
			return -1
		}
		return a.at.Compare(b.at)
	})
	return relations
}
