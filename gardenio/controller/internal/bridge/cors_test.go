package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetCORSOrigin(t *testing.T) {
	tests := []struct {
		name           string
		origin         string
		allowedOrigins []string
		wantACAO       string
		wantACACred    string
		wantVary       string
	}{
		{
			name:           "matching origin",
			origin:         "https://bridge.example.com",
			allowedOrigins: []string{"https://bridge.example.com"},
			wantACAO:       "https://bridge.example.com",
			wantACACred:    "true",
			wantVary:       "Origin",
		},
		{
			name:           "non-matching origin",
			origin:         "https://evil.example.com",
			allowedOrigins: []string{"https://bridge.example.com"},
			wantACAO:       "",
			wantACACred:    "",
			wantVary:       "",
		},
		{
			name:           "no origin header",
			origin:         "",
			allowedOrigins: []string{"https://bridge.example.com"},
			wantACAO:       "",
			wantACACred:    "",
			wantVary:       "",
		},
		{
			name:           "empty allowlist",
			origin:         "https://bridge.example.com",
			allowedOrigins: nil,
			wantACAO:       "",
			wantACACred:    "",
			wantVary:       "",
		},
		{
			name:           "multiple allowed origins",
			origin:         "https://staging.example.com",
			allowedOrigins: []string{"https://bridge.example.com", "https://staging.example.com"},
			wantACAO:       "https://staging.example.com",
			wantACACred:    "true",
			wantVary:       "Origin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}

			setCORSOrigin(w, r, tt.allowedOrigins)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != tt.wantACAO {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantACAO)
			}
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != tt.wantACACred {
				t.Errorf("Access-Control-Allow-Credentials = %q, want %q", got, tt.wantACACred)
			}
			if got := w.Header().Get("Vary"); got != tt.wantVary {
				t.Errorf("Vary = %q, want %q", got, tt.wantVary)
			}
		})
	}
}
