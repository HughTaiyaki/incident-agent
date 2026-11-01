package scripted

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/app/investigation"
)

const envelope = `{"request":{"service":"order-service","services":["order-service"],"environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:05:00Z","summary":"核查"},"decisions":[{"kind":"unknown"}]}`

func TestRead(t *testing.T) {
	script, err := Read(strings.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	// Unknown decisions reach the harness so their rejection still consumes a decision.
	if len(script.Decisions) != 1 {
		t.Fatal(script)
	}
	for _, tt := range []struct{ name, input string }{
		{"unknown envelope field", strings.Replace(envelope, `"request":`, `"path":"/tmp/private","request":`, 1)},
		{"trailing", envelope + ` {}`},
		{"oversized", strings.Repeat("x", MaxScriptBytes+1)},
		{"no decisions", strings.Replace(envelope, `[{"kind":"unknown"}]`, `[]`, 1)},
		{"too many decisions", strings.Replace(envelope, `[{"kind":"unknown"}]`, `[`+strings.Repeat(`{},`, 12)+`{}]`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(tt.input)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
func TestScriptDecide(t *testing.T) {
	s, err := Read(strings.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Decide(ctx, investigation.View{}); err == nil || s.next != 0 {
		t.Fatal("cancel consumed a decision")
	}
	b, err := s.Decide(t.Context(), investigation.View{})
	if err != nil {
		t.Fatal(err)
	}
	b[0] = 'x'
	if !json.Valid(s.Decisions[0]) {
		t.Fatal("shared decision memory")
	}
	if _, err = s.Decide(t.Context(), investigation.View{}); err == nil {
		t.Fatal("script repeated after exhaustion")
	}
}
