package capture_test

import (
	"strings"
	"testing"

	"github.com/hughtaiyaki/incident-agent/internal/app/capture"
	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func TestReadQueriesRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	t.Parallel()
	base := `[{"kind":"logs","service":"order-service","environment":"experiment","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:01:00Z"}]`
	tests := []struct {
		name  string
		input string
	}{
		{name: "unknown field", input: strings.Replace(base, `"kind"`, `"url":"http://example.com","kind"`, 1)},
		{name: "trailing data", input: base + "{}"},
		{name: "oversized", input: strings.Repeat(" ", 16385)},
		{name: "empty array", input: "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := capture.ReadQueries(strings.NewReader(tt.input), telemetry.DefaultPolicy()); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if _, err := capture.ReadQueries(strings.NewReader(base), telemetry.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
}
