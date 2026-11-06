package main

// htmx widget surface — P3 of free-htmx (ka-0000003324).
//
// Route shape: /htmx/widget/{widget-name}/{instance-id}/{action}.
//
// Each request is gated by:
//   1. OIDC ForwardAuth (X-Auth-Request-Email / Authorization Bearer) — wired
//      via oidcauth.RequireRole at mux registration time. GETs require viewer
//      (any authenticated user); POSTs require operator.
//   2. A per-instance signed token. The token binds (widget, instance, email,
//      expiry) under HMAC-SHA256 with a process-local secret. Required on
//      every request via the X-Widget-Token header (preferred) or ?t=… query.
//      The signed token defends against URL leakage: even if a poll URL ends
//      up in a log or referer header, an attacker cannot reuse it without the
//      bearer's email matching the OIDC-injected header.
//   3. A per-instance rate limit. Polls (GET) are clamped to N >= 5s per
//      (widget, instance, action) tuple via a 1-token bucket that refills at
//      0.2 tokens/sec. Writes (POST) get a small steady-state burst with
//      faster refill so legit operator clicks don't trip the gate.
//
// Action handlers may emit OOB swap markers — the response is HTML and may
// contain sibling fragments with `hx-swap-oob="true"` to update other parts
// of the page in addition to the primary swap target. They may also set
// HX-Trigger on the response writer for client-side event broadcast.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/ratelimit"
)

// htmxTokenTTL is the validity window of a per-instance widget token. Long
// enough that a polling page held open does not need to refresh tokens during
// normal session activity, short enough that a leaked URL becomes useless
// within an operator shift.
const htmxTokenTTL = 30 * time.Minute

// htmxPollMinIntervalSecs enforces the minimum poll cadence per
// (widget, instance, action) tuple. The runbook fixes it at 5s — anything
// faster is rejected with HTTP 429.
const htmxPollMinIntervalSecs = 5

// htmxWriteBurst / htmxWriteRatePerSec govern POSTs. A small burst tolerates
// a flurry of click-throughs (e.g. operator confirms a multi-step action)
// without blocking, with steady-state refill that drains slow probing.
const (
	htmxWriteBurst      = 10
	htmxWriteRatePerSec = 2.0
)

// htmxActionFunc renders an HTML fragment for one (widget, instance, action)
// invocation. Handlers may also set response headers (HX-Trigger, etc.) on w
// before returning. Returning a non-nil error degrades the request to HTTP
// 500 — the framework writes the body so handlers don't need to.
type htmxActionFunc func(w http.ResponseWriter, r *http.Request, ctx *htmxActionCtx) (template.HTML, error)

// htmxAction is one entry in the per-widget action catalog.
type htmxAction struct {
	// Method is "GET" or "POST". Mismatching the request method returns 405.
	Method string
	// Handler renders the fragment. Required.
	Handler htmxActionFunc
}

// htmxActionCtx carries per-request fields the handler usually needs without
// reaching back into r.
type htmxActionCtx struct {
	Widget   string
	Instance string
	Action   string
	Email    string
	BasePath string
}

// htmxWidgetActions is the in-memory catalog: widget -> action -> definition.
// Populated at init() time by registrar funcs in this file (and any future
// htmx_widget_*.go that wants to register more actions).
var htmxWidgetActions = map[string]map[string]htmxAction{}

// registerHtmxWidget installs an action. Duplicate (widget, action) panics —
// programmer error at startup beats silent shadowing in production.
func registerHtmxWidget(widget, action string, def htmxAction) {
	if def.Method == "" {
		def.Method = http.MethodGet
	}
	if def.Handler == nil {
		// PANIC: programmer error at init() — registrar passed a nil handler.
		// Fail loud at startup rather than NPE on first request.
		panic("registerHtmxWidget: nil handler for " + widget + "/" + action)
	}
	if _, ok := htmxWidgetActions[widget]; !ok {
		htmxWidgetActions[widget] = map[string]htmxAction{}
	}
	if _, dup := htmxWidgetActions[widget][action]; dup {
		// PANIC: programmer error at init() — duplicate (widget, action)
		// registration would silently shadow the earlier one in production.
		panic("registerHtmxWidget: duplicate action " + widget + "/" + action)
	}
	htmxWidgetActions[widget][action] = def
}

// lookupHtmxAction returns the registered action, or ok=false if missing.
func lookupHtmxAction(widget, action string) (htmxAction, bool) {
	actions, ok := htmxWidgetActions[widget]
	if !ok {
		return htmxAction{}, false
	}
	def, ok := actions[action]
	return def, ok
}

// htmxState carries the per-process token signer and rate-limiter pair used
// by every htmx widget request. Lives on Server; tests build their own to
// drive deterministic clocks.
type htmxState struct {
	signer       []byte
	pollLimiter  *ratelimit.Limiter
	writeLimiter *ratelimit.Limiter
}

// newHtmxState mints a fresh signer + limiter pair. The signer is 32 random
// bytes; tokens minted with this state are valid only for the lifetime of
// this process — pod restarts invalidate every outstanding token, which is
// the desired blast radius for short-lived widget instances.
func newHtmxState() *htmxState {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// PANIC: crypto/rand failure means the process cannot generate
		// secrets at all — refusing to start is safer than running with
		// a zero or weak signer.
		panic("htmx: crypto/rand failed: " + err.Error())
	}
	return &htmxState{
		signer:       secret,
		pollLimiter:  ratelimit.New(1, 1.0/float64(htmxPollMinIntervalSecs)),
		writeLimiter: ratelimit.New(htmxWriteBurst, htmxWriteRatePerSec),
	}
}

// signHtmxToken returns a base64-url-encoded token binding
// (widget, instance, email, expiry) under HMAC-SHA256 with secret.
//
// Token wire format (after base64url decode):
//
//	widget|instance|email|expiry_unix|sig
//
// '|' is forbidden in widget / instance / email (validated at verify time)
// so a simple SplitN parse round-trips cleanly without ambiguity.
func signHtmxToken(secret []byte, widget, instance, email string, expiry time.Time) string {
	payload := htmxTokenPayload(widget, instance, email, expiry.Unix())
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig))
}

func htmxTokenPayload(widget, instance, email string, expUnix int64) string {
	return widget + "|" + instance + "|" + email + "|" + strconv.FormatInt(expUnix, 10)
}

// verifyHtmxToken returns nil iff token is well-formed, unexpired, signed by
// secret, and bound to the given (widget, instance, email).
func verifyHtmxToken(secret []byte, token, widget, instance, email string, now time.Time) error {
	if widget == "" || instance == "" || email == "" {
		return errors.New("missing principal")
	}
	if strings.ContainsAny(widget, "|") || strings.ContainsAny(instance, "|") || strings.ContainsAny(email, "|") {
		return errors.New("invalid principal")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return errors.New("malformed token")
	}
	parts := strings.SplitN(string(raw), "|", 5)
	if len(parts) != 5 {
		return errors.New("malformed token")
	}
	tWidget, tInstance, tEmail, expStr, sig := parts[0], parts[1], parts[2], parts[3], parts[4]
	if tWidget != widget || tInstance != instance || tEmail != email {
		return errors.New("token principal mismatch")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errors.New("malformed expiry")
	}
	if now.Unix() > exp {
		return errors.New("token expired")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(htmxTokenPayload(tWidget, tInstance, tEmail, exp)))
	expectSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectSig)) {
		return errors.New("bad signature")
	}
	return nil
}

// extractHtmxToken pulls the per-instance signed token from the request.
// Header form is preferred (htmx hx-headers) so the token does not appear in
// access logs; query form is a fallback for non-htmx callers (e.g. a manual
// curl during smoke testing).
func extractHtmxToken(r *http.Request) string {
	if t := r.Header.Get("X-Widget-Token"); t != "" {
		return strings.TrimSpace(t)
	}
	return strings.TrimSpace(r.URL.Query().Get("t"))
}

// handleHtmxWidget dispatches /htmx/widget/{widget}/{instance}/{action} to
// the registered action handler. Verifies the per-instance signed token and
// applies the per-instance rate limit before invoking the handler.
//
// The OIDC role gate is applied by the route registration in RegisterRoutes
// — GETs route through RequireRole(viewer), POSTs through RequireRole(operator)
// — so this handler treats roles as already-checked.
func (s *Server) handleHtmxWidget(w http.ResponseWriter, r *http.Request) {
	if s.htmx == nil {
		http.Error(w, "htmx surface disabled", http.StatusServiceUnavailable)
		return
	}
	widget := strings.TrimSpace(r.PathValue("widget"))
	instance := strings.TrimSpace(r.PathValue("instance"))
	action := strings.TrimSpace(r.PathValue("action"))
	if widget == "" || instance == "" || action == "" {
		http.NotFound(w, r)
		return
	}
	def, ok := lookupHtmxAction(widget, action)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if def.Method != r.Method {
		w.Header().Set("Allow", def.Method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// OIDC ForwardAuth should have injected an email header by the
		// time we get here; missing means either the proxy is bypassed
		// in dev (use a placeholder so the signer round-trips) or the
		// caller is unauthenticated. Fail closed with 401 — mint a
		// token for "" and the caller still has to know the secret.
		http.Error(w, "missing authenticated email", http.StatusUnauthorized)
		return
	}

	tok := extractHtmxToken(r)
	if tok == "" {
		http.Error(w, "missing widget token", http.StatusUnauthorized)
		return
	}
	if err := verifyHtmxToken(s.htmx.signer, tok, widget, instance, email, time.Now()); err != nil {
		s.logger.Debug("htmx widget token rejected", "widget", widget, "instance", instance, "action", action, "error", err)
		http.Error(w, "invalid widget token", http.StatusUnauthorized)
		return
	}

	limiter := s.htmx.pollLimiter
	if r.Method == http.MethodPost {
		limiter = s.htmx.writeLimiter
	}
	rlKey := widget + "/" + instance + "/" + action
	if !limiter.Allow(rlKey) {
		w.Header().Set("Retry-After", strconv.Itoa(htmxPollMinIntervalSecs))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	body, err := def.Handler(w, r, &htmxActionCtx{
		Widget:   widget,
		Instance: instance,
		Action:   action,
		Email:    email,
		BasePath: s.basePath,
	})
	if err != nil {
		s.logger.Warn("htmx widget action failed", "widget", widget, "instance", instance, "action", action, "error", err)
		http.Error(w, "action failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
	}
}

// init registers the baseline `noop` widget. P3 ships the framework + an
// always-available demo action that exercises both rendering and OOB swap;
// later phases add real widgets in their own files.
func init() {
	registerHtmxWidget("noop", "render", htmxAction{
		Method: http.MethodGet,
		Handler: func(_ http.ResponseWriter, _ *http.Request, ctx *htmxActionCtx) (template.HTML, error) {
			return template.HTML(
				`<div id="htmx-widget-` + escapeAttr(ctx.Instance) +
					`" class="htmx-widget" data-widget="` + escapeAttr(ctx.Widget) +
					`" data-instance="` + escapeAttr(ctx.Instance) +
					`" data-state="rendered">noop@` + escapeText(ctx.Email) + `</div>`,
			), nil
		},
	})
	registerHtmxWidget("noop", "poke", htmxAction{
		Method: http.MethodPost,
		Handler: func(w http.ResponseWriter, _ *http.Request, ctx *htmxActionCtx) (template.HTML, error) {
			// HX-Trigger lets client-side listeners react without
			// scraping the swapped fragment.
			w.Header().Set("HX-Trigger", `{"widget:poked":{"widget":"`+escapeAttr(ctx.Widget)+`","instance":"`+escapeAttr(ctx.Instance)+`"}}`)
			ts := time.Now().UTC().Format(time.RFC3339)
			// Two fragments: the primary swap target (the widget itself)
			// followed by an OOB fragment that updates the page-level
			// status banner without the client having to know about it.
			return template.HTML(
				`<div id="htmx-widget-` + escapeAttr(ctx.Instance) +
					`" class="htmx-widget" data-widget="` + escapeAttr(ctx.Widget) +
					`" data-instance="` + escapeAttr(ctx.Instance) +
					`" data-state="poked">poked at ` + ts + `</div>` +
					`<div id="htmx-status-` + escapeAttr(ctx.Instance) +
					`" hx-swap-oob="true" class="htmx-widget-status" data-state="poked">poked by ` + escapeText(ctx.Email) + `</div>`,
			), nil
		},
	})
}

// escapeAttr is a tiny helper for HTML-attribute escaping in inline strings.
// We avoid html/template here because the framework returns template.HTML
// (already-escaped); using template/html across both for one-off attribute
// values would force every action to set up a *template.Template just to
// emit a tag.
func escapeAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
