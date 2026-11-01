package app

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
)

func TestNewValidatesDependencies(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := http.NewServeMux()

	tests := []struct {
		name    string
		addr    string
		handler http.Handler
		logger  *slog.Logger
	}{
		{name: "missing address", handler: handler, logger: logger},
		{name: "missing handler", addr: ":8080", logger: logger},
		{name: "missing logger", addr: ":8080", handler: handler},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := New(test.addr, test.handler, test.logger); err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
		})
	}
}

func TestNewCreatesApplication(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := http.NewServeMux()

	application, err := New(":8080", handler, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if application.server.Addr != ":8080" {
		t.Fatalf("server address = %q, want %q", application.server.Addr, ":8080")
	}
}
