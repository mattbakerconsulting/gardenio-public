package server

import (
	"context"
	"net/http"
	"testing"
)

func TestAuthIdentityContext(t *testing.T) {
	// Default (no identity set) returns anonymous.
	id := AuthIdentityFromContext(context.Background())
	if id.Type != "anonymous" {
		t.Errorf("expected anonymous, got %q", id.Type)
	}
	if id.Subject != "" {
		t.Errorf("expected empty subject, got %q", id.Subject)
	}

	// Set and retrieve identity.
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	id = AuthIdentityFromContext(ctx)
	if id.Type != "oidc" {
		t.Errorf("expected oidc, got %q", id.Type)
	}
	if id.Subject != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %q", id.Subject)
	}
}

func TestVerifyAndIdentify(t *testing.T) {
	ctx := context.Background()

	// Non-IdentityVerifier returns empty string.
	plain := &mockVerifier{err: nil}
	email := verifyAndIdentify(ctx, plain, "tok")
	if email != "" {
		t.Errorf("expected empty, got %q", email)
	}

	// IdentityVerifier returns email.
	iv := &mockIdentityVerifier{email: "bob@example.com"}
	email = verifyAndIdentify(ctx, iv, "tok")
	if email != "bob@example.com" {
		t.Errorf("expected bob@example.com, got %q", email)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		xff        string
		xri        string
		remoteAddr string
		want       string
	}{
		{
			name:       "X-Forwarded-For single",
			xff:        "1.2.3.4",
			remoteAddr: "10.0.0.1:12345",
			want:       "1.2.3.4",
		},
		{
			name:       "X-Forwarded-For multiple",
			xff:        "1.2.3.4, 5.6.7.8",
			remoteAddr: "10.0.0.1:12345",
			want:       "1.2.3.4",
		},
		{
			name:       "X-Real-Ip",
			xri:        "9.8.7.6",
			remoteAddr: "10.0.0.1:12345",
			want:       "9.8.7.6",
		},
		{
			name:       "RemoteAddr only",
			remoteAddr: "10.0.0.1:12345",
			want:       "10.0.0.1",
		},
		{
			name:       "RemoteAddr no port",
			remoteAddr: "10.0.0.1",
			want:       "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				r.Header.Set("X-Real-Ip", tt.xri)
			}
			// nil TrustedProxyConfig trusts all peers (backwards-compatible).
			got := clientIP(r, nil)
			if got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	// Only trust 10.0.0.0/8 as reverse proxy.
	tp, err := NewTrustedProxyConfig([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		xff        string
		xri        string
		remoteAddr string
		want       string
	}{
		{
			name:       "trusted proxy forwards XFF",
			xff:        "203.0.113.50",
			remoteAddr: "10.0.0.1:12345",
			want:       "203.0.113.50",
		},
		{
			name:       "trusted proxy forwards XRI",
			xri:        "203.0.113.51",
			remoteAddr: "10.0.0.1:12345",
			want:       "203.0.113.51",
		},
		{
			name:       "untrusted peer XFF ignored",
			xff:        "203.0.113.50",
			remoteAddr: "192.168.1.1:12345",
			want:       "192.168.1.1",
		},
		{
			name:       "untrusted peer XRI ignored",
			xri:        "203.0.113.51",
			remoteAddr: "192.168.1.1:12345",
			want:       "192.168.1.1",
		},
		{
			name:       "untrusted peer falls back to RemoteAddr",
			xff:        "1.2.3.4",
			remoteAddr: "172.16.0.1:54321",
			want:       "172.16.0.1",
		},
		{
			name:       "trusted proxy no headers",
			remoteAddr: "10.0.0.5:12345",
			want:       "10.0.0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				r.Header.Set("X-Real-Ip", tt.xri)
			}
			got := clientIP(r, tp)
			if got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// mockVerifier implements TokenVerifier (not IdentityVerifier).
type mockVerifier struct {
	err error
}

func (m *mockVerifier) Verify(_ context.Context, _ string) error {
	return m.err
}

// mockIdentityVerifier implements IdentityVerifier.
type mockIdentityVerifier struct {
	email string
	err   error
}

func (m *mockIdentityVerifier) Verify(_ context.Context, _ string) error {
	return m.err
}

func (m *mockIdentityVerifier) VerifyAndIdentify(_ context.Context, _ string) (string, error) {
	return m.email, m.err
}
