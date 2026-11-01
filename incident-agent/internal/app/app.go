// Package app owns process lifecycle and dependency composition behavior.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const shutdownTimeout = 5 * time.Second

// App runs the incident API and shuts it down when the parent context ends.
type App struct {
	server *http.Server
	logger *slog.Logger
}

// New constructs an App from explicitly injected dependencies.
func New(addr string, handler http.Handler, logger *slog.Logger) (*App, error) {
	if addr == "" {
		return nil, errors.New("address is required")
	}
	if handler == nil {
		return nil, errors.New("handler is required")
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}

	return &App{
		server: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger: logger,
	}, nil
}

// Run blocks until the server fails or the parent context is cancelled.
func (a *App) Run(ctx context.Context) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- a.server.ListenAndServe()
	}()

	a.logger.Info("incident API listening", "address", a.server.Addr)

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listen and serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}

	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("stop HTTP server: %w", err)
	}

	return nil
}
