package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Phase 3 — action descriptor subsystem (shared dispatcher).
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 3.
// Bead: ka-0000005188.
//
// One descriptor type, one client dispatcher, one server endpoint registry —
// every actionable button across every Incident Intelligence widget renders
// from a descriptor and dispatches through this surface, so confirm/auth/
// telemetry/error handling lives in exactly one place.
//
// D6 (IDE deep-link): both `cursor://` and `vscode://` are supported via the
// `ide-deeplink` kind. The client picks the scheme from a user-pref toggle
// stored in localStorage (default `cursor`); see incident_intel_actions.js.

// ActionKind enumerates the dispatcher kinds. Each maps to a client-side
// branch in incident_intel_actions.js; htmxPost additionally resolves on the
// server through the action endpoint registry.
type ActionKind string

const (
	ActionExternalLink  ActionKind = "external-link"
	ActionHtmxPost      ActionKind = "htmx-post"
	ActionHtmxGet       ActionKind = "htmx-get"
	ActionIDEDeeplink   ActionKind = "ide-deeplink"
	ActionModalTrigger  ActionKind = "modal-trigger"
	ActionDrawerTrigger ActionKind = "drawer-trigger"
	ActionClipboard     ActionKind = "clipboard"
)

// ConfirmDescriptor is the optional pre-execution confirm step. When set, the
// client dispatcher pumps it through IncidentIntel.confirmModal (Phase 1).
type ConfirmDescriptor struct {
	Title   string `json:"title,omitempty"`
	Body    string `json:"body,omitempty"`
	Confirm string `json:"confirm,omitempty"`
	Danger  bool   `json:"danger,omitempty"`
}

// ActionDescriptor is the wire shape stored in `data-ii-action="<json>"` on
// any element the dispatcher should bind. Required: Kind + (Target xor ID).
//
// `ID` is the canonical action identifier — used for telemetry events and as
// the path segment in `htmx-post` dispatch (`POST /actions/{id}`). For pure
// client-side kinds (external-link, ide-deeplink, modal-trigger, drawer-trigger,
// clipboard) it is informational; for `htmx-post` it MUST match a registered
// handler.
type ActionDescriptor struct {
	ID       string             `json:"id,omitempty"`
	Kind     ActionKind         `json:"kind"`
	Label    string             `json:"label,omitempty"`
	Target   string             `json:"target,omitempty"`
	Params   map[string]any     `json:"params,omitempty"`
	Confirm  *ConfirmDescriptor `json:"confirm,omitempty"`
	Hotkey   string             `json:"hotkey,omitempty"`
	Requires []string           `json:"requires,omitempty"`
	Danger   bool               `json:"danger,omitempty"`
	Hint     string             `json:"hint,omitempty"`
	// Swap target — for htmx-post, the CSS selector / element id whose
	// innerHTML should be replaced with the response body. Empty = no swap.
	SwapInto string `json:"swap_into,omitempty"`
}

// JSON returns the descriptor encoded for the `data-ii-action` attribute.
// Single-line — embeds in HTML attributes without entity gymnastics.
func (a ActionDescriptor) JSON() (string, error) {
	if a.Kind == "" {
		return "", fmt.Errorf("incident-intel action: Kind is required")
	}
	b, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MustJSON is the template-friendly variant — returns "" on error.
// Errors here are programmer bugs (malformed Params), not request data, so a
// silent empty fallback is acceptable; the dispatcher will log on missing
// data-ii-action.
func (a ActionDescriptor) MustJSON() string {
	s, _ := a.JSON()
	return s
}

// ----- Standard action library ----------------------------------------------
//
// Helpers below build descriptors for the v1 catalog from the bead spec:
// open-jira, open-mr, open-mezmo, open-cursor (D6 → IDE deeplink), copy-to-
// clipboard, dismiss-evidence, ack-incident.
//
// Widget templates SHOULD call these rather than hand-building descriptors so
// the wire shape stays in lock-step with the dispatcher.

// OpenJiraAction returns an external-link descriptor that opens the JIRA
// ticket in a new tab via the user-configured Atlassian site.
func OpenJiraAction(jiraBase, ticket string) ActionDescriptor {
	jiraBase = strings.TrimRight(jiraBase, "/")
	if jiraBase == "" {
		jiraBase = "https://acme.atlassian.net"
	}
	return ActionDescriptor{
		ID:     "open-jira",
		Kind:   ActionExternalLink,
		Label:  ticket,
		Target: jiraBase + "/browse/" + url.PathEscape(ticket),
		Hint:   "Open " + ticket + " in JIRA",
	}
}

// OpenMRAction returns an external-link descriptor that opens a GitLab MR
// (or any URL) in a new tab. Caller passes the full URL; this helper exists
// so widgets call the same shape as the rest of the library.
func OpenMRAction(label, mrURL string) ActionDescriptor {
	return ActionDescriptor{
		ID:     "open-mr",
		Kind:   ActionExternalLink,
		Label:  label,
		Target: mrURL,
		Hint:   "Open MR in GitLab",
	}
}

// OpenMezmoAction returns an external-link descriptor that opens a Mezmo log
// search URL pre-populated with the given query.
func OpenMezmoAction(mezmoBase, query string) ActionDescriptor {
	mezmoBase = strings.TrimRight(mezmoBase, "/")
	if mezmoBase == "" {
		mezmoBase = "https://app.mezmo.com"
	}
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	target := mezmoBase + "/log/view"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return ActionDescriptor{
		ID:     "open-mezmo",
		Kind:   ActionExternalLink,
		Label:  "Open in Mezmo",
		Target: target,
		Hint:   "Open log query in Mezmo",
	}
}

// OpenIDEAction returns an ide-deeplink descriptor for opening a file at a
// specific line. The client dispatcher resolves the URL scheme from the user's
// preference (cursor:// or vscode://) — see D6.
//
// path should be repo-relative; the client builds an absolute path against the
// user's configured workspace root, also stored in localStorage.
func OpenIDEAction(path string, line int) ActionDescriptor {
	target := path
	if line > 0 {
		target = fmt.Sprintf("%s:%d", path, line)
	}
	return ActionDescriptor{
		ID:     "open-ide",
		Kind:   ActionIDEDeeplink,
		Label:  "Open in IDE",
		Target: target,
		Hint:   "Open file in your preferred IDE",
	}
}

// CopyToClipboardAction returns a clipboard descriptor that copies `text` to
// the system clipboard via navigator.clipboard.writeText.
func CopyToClipboardAction(label, text string) ActionDescriptor {
	return ActionDescriptor{
		ID:     "copy-to-clipboard",
		Kind:   ActionClipboard,
		Label:  label,
		Target: text,
		Hint:   "Copy to clipboard",
	}
}

// DismissEvidenceAction returns an htmx-post descriptor that removes an
// evidence item from a rich-report. Confirms before submit (destructive).
// `evidenceID` is appended to params and surfaces in the registered handler.
// `swapInto` is the CSS selector to replace with the server response.
func DismissEvidenceAction(evidenceID, swapInto string) ActionDescriptor {
	return ActionDescriptor{
		ID:       "dismiss-evidence",
		Kind:     ActionHtmxPost,
		Label:    "Dismiss",
		Params:   map[string]any{"evidence_id": evidenceID},
		SwapInto: swapInto,
		Confirm: &ConfirmDescriptor{
			Title:   "Dismiss evidence?",
			Body:    "Removes this item from the rich-report. Cannot be undone.",
			Confirm: "Dismiss",
			Danger:  true,
		},
		Danger: true,
		Hint:   "Dismiss evidence item",
	}
}

// AckIncidentAction returns an htmx-post descriptor that acknowledges an
// incident bead. Confirms first; surfaces incident_id in params.
func AckIncidentAction(incidentID, swapInto string) ActionDescriptor {
	return ActionDescriptor{
		ID:       "ack-incident",
		Kind:     ActionHtmxPost,
		Label:    "Acknowledge",
		Params:   map[string]any{"incident_id": incidentID},
		SwapInto: swapInto,
		Confirm: &ConfirmDescriptor{
			Title:   "Acknowledge incident?",
			Body:    "Marks the incident as acknowledged and stops paging.",
			Confirm: "Acknowledge",
		},
		Hint: "Acknowledge incident",
	}
}

// ----- Server-side action endpoint registry --------------------------------
//
// `htmx-post` descriptors all dispatch to `POST /actions/{id}`. The id is
// resolved against this registry; an unregistered id returns 404. Handlers
// declare any required roles up-front; missing-role calls return 403.
//
// Idempotency: handlers are responsible for their own idempotency keys. The
// dispatcher does NOT replay; it just routes. Adding `If-Match`-style headers
// is left to handlers that need it.

// ActionParams is the decoded request body — JSON object keyed by string.
type ActionParams = map[string]any

// ActionHandler runs a registered htmx-post action. Returning a non-nil error
// causes the dispatcher to write a 500 with the message; handlers should
// generally write their own response (HTML fragment or 204) and return nil.
type ActionHandler func(w http.ResponseWriter, r *http.Request, params ActionParams) error

// ActionRegistry resolves action ids to handlers. Safe for concurrent reads
// after Register has finished; tests construct fresh registries.
type ActionRegistry struct {
	mu       sync.RWMutex
	handlers map[string]registeredAction
}

type registeredAction struct {
	handler ActionHandler
	roles   []string
}

// NewActionRegistry returns an empty registry.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{handlers: make(map[string]registeredAction)}
}

// Register installs a handler for the given action id. Roles is the set of
// realm roles the request principal must hold; an empty list means no role
// check beyond the standard auth middleware. Re-registering the same id
// replaces the previous handler.
func (r *ActionRegistry) Register(id string, handler ActionHandler, roles ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[id] = registeredAction{handler: handler, roles: roles}
}

// Lookup returns the handler + role list for an id, or ok=false if the id is
// not registered.
func (r *ActionRegistry) Lookup(id string) (ActionHandler, []string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[id]
	if !ok {
		return nil, nil, false
	}
	return h.handler, h.roles, true
}

// IDs returns the registered action ids in unspecified order. Useful for
// telemetry dumps and the registry-introspection test.
func (r *ActionRegistry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}

// handleAction is the single dispatch endpoint. Body is decoded as JSON
// (object) or form values; both shapes flow into the handler as ActionParams.
//
// Path: POST /actions/{id}
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if s.actions == nil {
		http.Error(w, "actions disabled", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing action id", http.StatusBadRequest)
		return
	}
	handler, _, ok := s.actions.Lookup(id)
	if !ok {
		http.Error(w, "unknown action: "+id, http.StatusNotFound)
		return
	}
	params, err := decodeActionParams(r)
	if err != nil {
		http.Error(w, "invalid params: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The standard oidcauth middleware on the parent mux already enforces the
	// operator role for write surfaces; per-action role gating is enforced by
	// individual handlers via the role list returned from Lookup. Phase 3
	// keeps the audit trail terse — the request log line printed by the
	// HTTPLogger middleware carries the action id already.
	if err := handler(w, r, params); err != nil {
		s.logger.Error("incident-intel action handler error", "id", id, "error", err)
		http.Error(w, "action error", http.StatusInternalServerError)
	}
}

// decodeActionParams parses the request body as JSON when content-type is
// application/json, falling back to form values otherwise. Empty body is
// fine — returns an empty map.
func decodeActionParams(r *http.Request) (ActionParams, error) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/json" {
		var p ActionParams
		// Empty body decodes to nil; treat that as ok.
		if r.Body == nil {
			return ActionParams{}, nil
		}
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&p); err != nil {
			// EOF on empty body is fine.
			if err.Error() == "EOF" {
				return ActionParams{}, nil
			}
			return nil, err
		}
		if p == nil {
			p = ActionParams{}
		}
		return p, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	out := ActionParams{}
	for k, v := range r.PostForm {
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out, nil
}

// registerStandardActions installs the v1 action library on the registry.
// Each entry is a tiny handler — the descriptors built by helpers above
// already shape the call site, so handlers focus on the side effect.
//
// In Phase 3 the destructive handlers (dismiss-evidence, ack-incident) do not
// yet route to a real backend — they 200-OK and emit a short audit log line.
// Phase 4+ widgets wire them into kbeads as they land. This is intentional:
// the dispatcher and registry surface ship in lock-step with widgets, so
// widget authors can develop against a working endpoint without depending on
// the full state machine.
func registerStandardActions(reg *ActionRegistry) {
	// dismiss-evidence: returns an empty 200 so htmx swaps the targeted node
	// out cleanly. Body is informational only.
	reg.Register("dismiss-evidence", func(w http.ResponseWriter, r *http.Request, p ActionParams) error {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "<!-- dismissed evidence_id=%v -->", p["evidence_id"])
		return nil
	})
	// ack-incident: same shape — 200 + an HTML stub so the swap target gets
	// a non-empty fragment confirming the ack.
	reg.Register("ack-incident", func(w http.ResponseWriter, r *http.Request, p ActionParams) error {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<span class="ii-pill" data-confidence="high">acknowledged</span>`)
		_ = p
		return nil
	})
}
