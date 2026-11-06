package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestIPWhitelist_Disabled(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "", 0, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when whitelist disabled, got %d", w.Code)
	}
}

func TestIPWhitelist_AllowedIP(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 0, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.1.5:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for allowed IP, got %d", w.Code)
	}
}

func TestIPWhitelist_BlockedIP(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 0, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.5:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for blocked IP, got %d", w.Code)
	}
}

// TestIPWhitelist_XForwardedForSpoofBlocked is the security regression for
// ka-0000006111: with the default trustedProxyHops=0, a client-supplied
// X-Forwarded-For MUST NOT be able to bypass the CIDR gate. Here the real TCP
// peer (RemoteAddr) is OUTSIDE the allowed range but the attacker spoofs an
// in-range address in XFF; the request must still be rejected.
func TestIPWhitelist_XForwardedForSpoofBlocked(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 0, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.5:1234" // real peer, outside allow-list
	req.Header.Set("X-Forwarded-For", "10.0.1.5")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("XFF spoof bypassed the CIDR gate: expected 403, got %d", w.Code)
	}
}

// TestIPWhitelist_TrustedProxyHopsHonorsXFF: with one trusted proxy in front,
// the client address written by that proxy (the last XFF entry, since each hop
// APPENDS) is used. Real client in range -> allowed even though the proxy's own
// RemoteAddr is not in the list.
func TestIPWhitelist_TrustedProxyHopsHonorsXFF(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 1, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.16.0.9:1234" // the trusted proxy, not in allow-list
	// Client 10.0.1.5 hit our single proxy, which appended what it saw.
	req.Header.Set("X-Forwarded-For", "10.0.1.5")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for in-range client behind 1 trusted proxy, got %d", w.Code)
	}
}

// TestIPWhitelist_TrustedProxyHopsRejectsPrependedSpoof: with one trusted
// proxy, an attacker who PREPENDS a fake in-range address to XFF must still be
// rejected — the trusted proxy appends the attacker's real (out-of-range)
// address to the right, and only that right-most entry is trusted.
func TestIPWhitelist_TrustedProxyHopsRejectsPrependedSpoof(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 1, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.16.0.9:1234" // trusted proxy
	// Attacker (203.0.113.7, out of range) pre-seeded a spoofed in-range entry;
	// the trusted proxy appended the attacker's real address on the right.
	req.Header.Set("X-Forwarded-For", "10.0.1.5, 203.0.113.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("prepended XFF spoof bypassed the gate: expected 403, got %d", w.Code)
	}
}

// TestIPWhitelist_TrustedProxyHopsShortHeaderFallsBackToRemoteAddr: if the XFF
// header is shorter than the configured hop count (missing/stripped), we must
// NOT trust a client-supplied left-most entry — fall back to RemoteAddr.
func TestIPWhitelist_TrustedProxyHopsShortHeaderFallsBackToRemoteAddr(t *testing.T) {
	logger := slog.Default()
	handler := ipWhitelistMiddleware(okHandler(), "10.0.0.0/8", 2, logger)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.5:1234"           // out of range
	req.Header.Set("X-Forwarded-For", "10.0.1.5") // too short for 2 hops
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("short XFF should fall back to RemoteAddr: expected 403, got %d", w.Code)
	}
}
