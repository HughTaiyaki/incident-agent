package investigation

import (
	"strings"
	"testing"
	"time"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestDecode(t *testing.T) {
	for _, s := range []string{`{"kind":"query","command":"shell"}`, `{} {}`, strings.Repeat("x", MaxDecisionBytes+1)} {
		t.Run(s[:min(len(s), 30)], func(t *testing.T) {
			var d Decision
			if Decode([]byte(s), &d) == nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
}
func TestValidateRequest(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Request)
	}{
		{"unknown service", func(r *Request) { r.Services = []string{"unknown"} }},
		{"duplicate scope", func(r *Request) { r.Services = append(r.Services, r.Service) }},
		{"credential", func(r *Request) { r.Summary = "token=secret-value" }},
		{"large summary", func(r *Request) { r.Summary = strings.Repeat("中", 2001) }},
		{"long window", func(r *Request) { r.End = r.Start.Add(2 * time.Hour) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := request()
			tt.change(&r)
			if _, err := ValidateRequest(r); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
func TestValidateReport(t *testing.T) {
	q, _ := telemetry.DefaultPolicy().Normalize(query())
	entry := observation(q, "1")
	id := entry.Records[0].ID
	good := func() Draft {
		d := validDraft(id)
		d.Timeline = []TimelineItem{{At: q.Start, Claim: d.Facts[0]}}
		return d
	}
	if err := ValidateReport(good(), []telemetry.Result{entry}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		change func(*Draft)
	}{
		{"foreign citation", func(d *Draft) { d.Facts[0].EvidenceIDs = []string{"foreign"} }},
		{"empty citation", func(d *Draft) { d.Facts[0].EvidenceIDs = nil }},
		{"duplicate citation", func(d *Draft) { d.Facts[0].EvidenceIDs = []string{id, id} }},
		{"unsubstantiated diagnosis", func(d *Draft) { d.Result = "DIAGNOSED" }},
		{"recovery assertion", func(d *Draft) { d.Recovery = "RECOVERED" }},
		{"secret", func(d *Draft) { d.NextActions = []string{"password=private"} }},
		{"unknown omitted", func(d *Draft) { d.Unknowns = nil }},
		{"invented timestamp", func(d *Draft) { d.Timeline[0].At = q.Start.Add(time.Second) }},
		{"unsupported strength", func(d *Draft) { d.Primary = &Hypothesis{Claim: d.Facts[0], Strength: "100%"} }},
		{"contradiction overlaps support", func(d *Draft) {
			d.Primary = &Hypothesis{Claim: d.Facts[0], Strength: "LOW", ContradictingIDs: []string{id}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := good()
			tt.change(&d)
			if ValidateReport(d, []telemetry.Result{entry}) == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
	t.Run("same record ID from failed source is not support", func(t *testing.T) {
		entry.Quality.QueryOutcome = "FAILED"
		if ValidateReport(good(), []telemetry.Result{entry}) == nil {
			t.Fatal("accepted")
		}
	})
}
