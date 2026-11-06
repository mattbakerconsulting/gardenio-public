package main

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// operatorMiddleware attaches the OIDC operator to the request context so all
// downstream daemon write calls (CreateBead, AddLabel, AddDependency,
// UpdateBead, AddComment, ResolveDecision, …) automatically carry the
// X-Operated-By header. Without this, kbeads R2 enforcement
// (ka-0000005772) fail-closes write paths with HTTP 400 "operator could
// not be resolved" because beads-viewer talks to the daemon with a static
// service token whose identity has no OIDC subject.
//
// When the request carries no OIDC email (anonymous /healthz, the dev
// bypass middleware, etc.) we don't stamp anything — let downstream
// handlers reject as they see fit.
//
// CSRF posture (ka-0000004498, source ka-0000002836 §5.E): the viewer does
// not mint its own CSRF tokens. Browser-originated writes are protected by
// the combined effect of (a) SameSite=Lax on every viewer-set cookie (see
// cookieSecure in dev_auth.go and the SetCookie sites in agents.go /
// migration_banner.go), and (b) kbeads R2 enforcement on the daemon: every
// write requires an X-Operated-By value resolvable to a real operator. A
// cross-origin form post would either be dropped by SameSite or rejected by
// the daemon for a missing/unrecognised operator — there is no anonymous
// write surface. AVA-routed writes additionally inherit the AWS Verified
// Access edge signature gate before reaching the pod.
// It also attaches the caller's OIDC access token (when present) to the
// context via beadsapi.WithUserBearer. This is inert until write-bearer
// enforcement is enabled (BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES); once on,
// the daemon client authenticates mutating calls AS THE USER so the daemon's
// OIDC RBAC + attribution path runs instead of the privileged static-token
// bypass, and a write with no user bearer fails closed (ka-0000006379).
func operatorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if email := oidcauth.EmailFromContext(ctx); email != "" {
			// Read the VERIFIED identity from context, never the raw
			// X-Auth-Request-Email header: on the AVA edge that header is
			// client-supplied and spoofable, so stamping it would forge the
			// X-Operated-By on every daemon write (ka-0000002979). The AVA and
			// Keycloak auth branches populate ctx from a validated credential;
			// the dev-bypass path promotes the trusted forwardauth-stamped header
			// into the same ctx slot (devBypassMiddleware).
			//
			// canonicalOIDC lowercases the oidc form so every viewer-driven write
			// stamps the SAME canonical operator id the fleet uses
			// (oidc:<lowercased-email>). Stamping the raw mixed-case email forked a
			// second operator row per human and fragmented "my work" filters
			// (ka-0000002505).
			ctx = beadsapi.WithOperator(ctx, canonicalOIDC(email))
			// Edge-auth envelope operator (decision ka-0000000960 →
			// edge-auth-envelope): also carry the verified human as the
			// edge-auth operator. This is INERT until an HMAC key is wired
			// (main.go: BEADS_VIEWER_EDGE_AUTH_HMAC_KEY → SetEdgeAuthHMACKey):
			// beadsapi.sendOnce only stamps the envelope headers when a key is
			// set, otherwise it falls back to the X-Operated-By path above. Once
			// the key is present, an AVA-routed write — which has NO forwardable
			// bearer, so the write-bearer path can't authenticate it — signs an
			// envelope the daemon verifies to AuthIdentity{Type:"edge",
			// Subject:oidc:<user>}, attributing the write to the human rather
			// than the viewer's shared daemon token. canonicalOIDC(email) is
			// already the required human-operator shape (oidc:<lowercased>).
			ctx = beadsapi.WithEdgeAuthOperator(ctx, canonicalOIDC(email))
		}
		if bearer := oidcauth.AccessTokenFromRequest(r); bearer != "" {
			ctx = beadsapi.WithUserBearer(ctx, bearer)
		}
		// Install the per-request user-bead memo so a handler's own
		// findUserBeadByEmail (settings / feed default project / pinned set)
		// and the render seam's VibeMode injection (ka-0000007201) share a
		// single daemon scan instead of re-scanning per lookup.
		ctx = withUserBeadCache(ctx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ipWhitelistMiddleware restricts access to the given CIDR ranges.
// When allowedCIDRs is empty, all IPs are allowed (dev mode).
//
// trustedProxyHops is the number of trusted reverse proxies in front of this
// listener. It controls how X-Forwarded-For is interpreted (see clientIP):
// clients spoof XFF freely, so the header can only be trusted for exactly the
// hops we know sit in front of us. With the default 0, XFF is ignored entirely
// and the gate decides on the direct RemoteAddr — the only value an attacker
// cannot forge.
func ipWhitelistMiddleware(next http.Handler, allowedCIDRs string, trustedProxyHops int, logger *slog.Logger) http.Handler {
	if allowedCIDRs == "" {
		return next
	}

	var nets []*net.IPNet
	for _, cidr := range strings.Split(allowedCIDRs, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Warn("invalid CIDR in ALLOWED_CIDRS, skipping", "cidr", cidr, "error", err)
			continue
		}
		nets = append(nets, network)
	}

	if len(nets) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, trustedProxyHops)
		if ip == nil || !containsIP(nets, ip) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client IP the access gate should decide on.
//
// X-Forwarded-For is fully attacker-controlled: any caller can send
// `X-Forwarded-For: <an-allowed-ip>` to spoof it, so the old "trust the first
// (left-most) XFF entry" behaviour made the CIDR allow-list illusory — an
// attacker just names an allowed address. We only trust the header for the
// exact number of reverse proxies we know sit in front of us (trustedProxyHops):
//
//   - hops == 0 (default): ignore XFF entirely; decide on r.RemoteAddr, the
//     TCP peer address, which cannot be forged.
//   - hops == N > 0: each trusted proxy APPENDS the address it observed to the
//     right of XFF, so the last N entries are the ones our own proxies wrote.
//     The right-most-minus-N entry is the address the OUTERMOST trusted proxy
//     saw — i.e. the real client (or the next untrusted hop). Anything the
//     client itself pre-seeded sits further LEFT and is ignored. If the header
//     is shorter than N (missing/stripped), fall back to RemoteAddr rather than
//     trusting a client-supplied left-most entry.
func clientIP(r *http.Request, trustedProxyHops int) net.IP {
	if trustedProxyHops > 0 {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// The address written by the outermost trusted proxy is the
			// (len-hops)th element (0-indexed). Guard against a header shorter
			// than the configured hop count.
			if idx := len(parts) - trustedProxyHops; idx >= 0 && idx < len(parts) {
				if ip := net.ParseIP(strings.TrimSpace(parts[idx])); ip != nil {
					return ip
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

func containsIP(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// coarsePointerUASignals are case-insensitive substrings that strongly imply a
// touch/coarse-pointer device. The list is intentionally short — a long
// allow-list of phone UAs gets stale fast. False negatives degrade to the
// desktop defaults (more scrollback, larger queue), which is graceful: the
// session works, it just buffers a bit more aggressively than ideal.
var coarsePointerUASignals = []string{
	"Mobi",       // Firefox/Chrome on phones
	"Android",    // Android Chrome / WebView
	"iPhone",     // iOS Safari / WKWebView
	"iPad",       // iPadOS
	"iPod",       // legacy iPod touch
	"Touch",      // Edge mobile, Surface w/ touch
	"BlackBerry", // BB10 + legacy
	"Tablet",     // Kindle Fire / Silk
	"Silk",       // Kindle browser
	"webOS",      // legacy
	"Opera Mini", // proxy browser
	"Opera Mobi", // mobile Opera
}

// isCoarsePointer reports whether the request's User-Agent matches a
// coarse-pointer device signature. Templates and JS read this to lower the
// xterm scrollback fetch and the WS queue high-watermark, so an operator
// glancing at an agent on their phone doesn't pay desktop-scale buffering
// for a screen they can't really scroll. Mirrors the
// `(pointer: coarse)` CSS media query at the request layer so the choice
// can affect server-rendered output (initial scrollback fetch size).
func isCoarsePointer(r *http.Request) bool {
	if r == nil {
		return false
	}
	// Explicit override for testing + for clients that opt in via query
	// (covers the `(pointer: coarse)` JS feature-detect path: when the JS
	// matches the media query it can append `?coarse=1` to the next nav).
	if v := strings.TrimSpace(r.URL.Query().Get("coarse")); v != "" {
		return v == "1" || v == "true"
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		return false
	}
	for _, sig := range coarsePointerUASignals {
		if strings.Contains(ua, sig) {
			return true
		}
	}
	return false
}
