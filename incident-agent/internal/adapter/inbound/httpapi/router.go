// Package httpapi provides inbound HTTP adapters for the incident service.
package httpapi

import (
	"encoding/json"
	"net/http"
)

// NewRouter returns the HTTP routes exposed by the incident API.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", statusHandler("ok"))
	mux.HandleFunc("GET /readyz", statusHandler("ready"))

	return mux
}

func statusHandler(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"status": status}); err != nil {
			http.Error(w, "encode response", http.StatusInternalServerError)
		}
	}
}
