// Package avareauth detects when an AVA-gated HTTP redirect chain has landed
// on a GitLab interactive sign-in form — the signal that AVA's silent
// re-auth dance has failed and the user's GitLab session itself is dead.
//
// Failure mode this package exists to recognise:
//
//   - The CLI replays its captured AVA session cookie against an AVA-gated
//     endpoint (e.g. beads-viewer).
//   - AVA's cookie has expired, so AVA 302s the browser to GitLab.
//   - Normally GitLab's own session cookie is still valid, GitLab silent-
//     re-auths, and the chain redirects back to AVA with a fresh cookie. The
//     CLI sees nothing.
//   - But when the GitLab session has ALSO expired, GitLab serves its
//     interactive sign-in form (HTML, 200 OK, URL contains /users/sign_in).
//     A non-interactive CLI cannot fill in that form — it must exit
//     non-zero and tell the user to run `gb login`.
//
// This package is consumed by the kd/gb/coop HTTP client (the redirect-
// follow path landing in A3, ka-0000006815). It provides two detection
// surfaces:
//
//   - URL-based, for cheap inspection inside http.Client.CheckRedirect
//     before the body has even been fetched.
//   - Body-based, for the final response after the chain settles. The body
//     check is needed because GitLab may serve sign_in at a host/path that
//     doesn't carry the literal substring (custom-mounted GitLab, vanity
//     hosts, etc.) — the embedded HTML form is the definitive marker.
//
// Both surfaces return the same ErrReauthRequired sentinel so callers can
// `errors.Is` once and surface a single user-facing message.
package avareauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ErrReauthRequired is the sentinel returned when the redirect chain has
// landed on a GitLab sign-in page or a Cloudflare challenge interstitial in
// front of the IdP. The message is user-facing: it is printed verbatim by
// CLIs that wrap it with %w. It stays IdP-agnostic because the redirect can
// settle on either GitLab's own sign-in form or Cloudflare's bot-check page —
// both mean the same thing: the captured session expired and the operator
// must re-auth in a real browser via `gb login`.
var ErrReauthRequired = errors.New("login session expired — please run `gb login` to re-authenticate")

// signInPath is the canonical GitLab interactive sign-in path. Matched
// case-sensitively because GitLab serves a fixed lowercase path; case-folding
// would risk false positives against unrelated `/Sign_In`-style URLs.
const signInPath = "/users/sign_in"

// bodyScanLimit caps how many bytes of a candidate response body we sniff
// for the sign-in form marker. 64 KiB comfortably covers GitLab's <head>
// plus the opening of <body> where the sign-in <form> sits. We do NOT scan
// the whole body because:
//   - The marker, when present, appears in the document prefix.
//   - Letting the scan run unbounded would let a hostile (or merely
//     enormous) response burn CPU on substring matching.
const bodyScanLimit = 64 * 1024

// IsSignInURL reports whether u points at a GitLab interactive sign-in
// page. Cheap enough to call inside http.Client.CheckRedirect on every hop.
//
// The match is path-based: any URL whose path is exactly "/users/sign_in"
// or whose path begins with "/users/sign_in/" (sign_in/two_factor, etc.).
// We deliberately do NOT match on the host name — GitLab self-hosted
// instances can live anywhere, and the path shape is the reliable
// invariant. AVA's own sign-in page lives at a different path
// ("/oauth/authorize" etc.) so a path match is safe.
func IsSignInURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	p := u.Path
	if p == signInPath {
		return true
	}
	return strings.HasPrefix(p, signInPath+"/")
}

// signInBodyMarkers are HTML substrings that, when present in the response
// body of a text/html response, signal a GitLab interactive sign-in page.
// Each marker on its own is a strong indicator — GitLab's sign-in form
// always posts to /users/sign_in, and the user_login field is the canonical
// username input. We match case-insensitively because GitLab's HTML uses
// mixed case for attribute values across versions.
var signInBodyMarkers = []string{
	`action="/users/sign_in"`,
	`action='/users/sign_in'`,
	`id="new_user"`,
	`name="user[login]"`,
}

// LooksLikeSignInPage reports whether (contentType, body) is consistent
// with a GitLab interactive sign-in HTML page.
//
// Returns true only when BOTH:
//   - contentType begins with "text/html" (case-insensitive, ignoring
//     parameters like "; charset=utf-8")
//   - the first bodyScanLimit bytes of body contain at least one
//     signInBodyMarker (case-insensitive substring match)
//
// Body bytes beyond bodyScanLimit are ignored even if they would match —
// the marker we care about is in the document prefix, and unbounded
// scanning is a CPU footgun on giant responses.
func LooksLikeSignInPage(contentType string, body []byte) bool {
	if !strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return false
	}
	if len(body) == 0 {
		return false
	}
	scan := body
	if len(scan) > bodyScanLimit {
		scan = scan[:bodyScanLimit]
	}
	// ToLower allocates; do it once and reuse for every marker check.
	lower := strings.ToLower(string(scan))
	for _, m := range signInBodyMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// cloudflareChallengeMarkers are substrings that, in a text/html response,
// signal a Cloudflare interstitial (managed challenge, JS browser-check, or
// block page) rather than the real backend. When an AVA→GitLab redirect lands
// here the IdP login is Cloudflare-walled and a non-interactive CLI cannot
// pass it — same remediation as a GitLab sign-in landing: re-auth in a real
// browser via `gb login`. These markers are Cloudflare-internal identifiers
// (challenge-platform script path, the cf_chl_* token namespace, the
// _cf_chl_opt bootstrap object) that do not appear in ordinary app HTML, so
// false positives against a legitimate beads JSON/HTML response are
// vanishingly unlikely.
var cloudflareChallengeMarkers = []string{
	"/cdn-cgi/challenge-platform/",
	"cf-browser-verification",
	"window._cf_chl_opt",
	"cf_chl_",
}

// LooksLikeCloudflareChallenge reports whether (contentType, body) is a
// Cloudflare challenge/block interstitial. Mirrors LooksLikeSignInPage:
// requires a text/html content type and at least one marker within the first
// bodyScanLimit bytes.
func LooksLikeCloudflareChallenge(contentType string, body []byte) bool {
	if !strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return false
	}
	if len(body) == 0 {
		return false
	}
	scan := body
	if len(scan) > bodyScanLimit {
		scan = scan[:bodyScanLimit]
	}
	lower := strings.ToLower(string(scan))
	for _, m := range cloudflareChallengeMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// DetectFromResponse inspects a finalised http.Response — URL plus an
// already-read body — and returns ErrReauthRequired when it looks like a
// GitLab sign-in landing or a Cloudflare challenge in front of the IdP.
//
// The caller is expected to pass the response that survived the redirect
// chain (i.e. the one returned by http.Client.Do) along with its body
// bytes. We do not read resp.Body ourselves because typical callers already
// need the body for their own decoding path; double-reading is awkward and
// loses the bytes.
//
// Returns nil when the response is not a re-auth landing — callers should
// proceed to their normal status/body handling.
func DetectFromResponse(resp *http.Response, body []byte) error {
	if resp == nil {
		return nil
	}
	if resp.Request != nil && IsSignInURL(resp.Request.URL) {
		return ErrReauthRequired
	}
	// Cloudflare stamps its managed-challenge responses with this header
	// regardless of body shape — a definitive signal that survives even if the
	// body is empty or minified beyond our marker set.
	if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") {
		return ErrReauthRequired
	}
	ct := resp.Header.Get("Content-Type")
	if LooksLikeSignInPage(ct, body) {
		return ErrReauthRequired
	}
	if LooksLikeCloudflareChallenge(ct, body) {
		return ErrReauthRequired
	}
	return nil
}
