// Command incident-api serves the local health endpoints.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hughtaiyaki/incident-agent/internal/adapter/inbound/httpapi"
	"github.com/hughtaiyaki/incident-agent/internal/app"
)

const defaultPort = "8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	handler := httpapi.NewRouter()
	application, err := app.New(":"+port, handler, logger)
	if err != nil {
		logger.Error("create application", "error", err)
		os.Exit(1)
	}

	if err := application.Run(ctx); err != nil {
		logger.Error("run application", "error", err)
		os.Exit(1)
	}
}
