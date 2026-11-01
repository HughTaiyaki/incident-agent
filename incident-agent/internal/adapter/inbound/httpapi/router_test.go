package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantStatus string
	}{
		{name: "liveness", path: "/healthz", wantStatus: `"status":"ok"`},
		{name: "readiness", path: "/readyz", wantStatus: `"status":"ready"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			NewRouter().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status code = %d, want %d", response.Code, http.StatusOK)
			}
			if !strings.Contains(response.Body.String(), test.wantStatus) {
				t.Fatalf("response body = %q, want it to contain %q", response.Body.String(), test.wantStatus)
			}
		})
	}
}
