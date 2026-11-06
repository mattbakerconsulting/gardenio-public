package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// taintDetailListLimit caps each tab's daemon list call. The tabs are meant
// for interactive triage, not bulk export — a viewer needing more reaches
// for `kd` directly.
const taintDetailListLimit = 200

// taintDetailTab is one entry in the detail page's tab strip, mirroring
// widgetTypeTab's ?tab= pattern (widget_types.go).
type taintDetailTab struct {
	Label  string
	Slug   string
	Href   string
	Active bool
}

var taintDetailTabDefs = []struct{ Label, Slug string }{
	{"Policy", "policy"},
	{"Grants", "grants"},
	{"Beads", "beads"},
	{"Scrubs", "scrubs"},
	{"Audit", "audit"},
}

func taintDetailTabs(basePath, name, activeTab string) []taintDetailTab {
	tabs := make([]taintDetailTab, 0, len(taintDetailTabDefs))
	for _, d := range taintDetailTabDefs {
		href := basePath + "/taints/" + url.PathEscape(name)
		if d.Slug != "policy" {
			href += "?tab=" + d.Slug
		}
		tabs = append(tabs, taintDetailTab{Label: d.Label, Slug: d.Slug, Href: href, Active: d.Slug == activeTab})
	}
	return tabs
}

func normalizeTaintTab(tab string) string {
	switch strings.ToLower(strings.TrimSpace(tab)) {
	case "grants":
		return "grants"
	case "beads":
		return "beads"
	case "scrubs":
		return "scrubs"
	case "audit":
		return "audit"
	default:
		return "policy"
	}
}

// taintDetailView is the full /taints/{name} page view model (task
// gd-nOnc1Q8Zcv, ADR docs/adr/taints.md §9.1: "Taint detail"). Exactly one of
// Policy/Grants/Beads/Scrubs/Audit is populated, matching the active Tab.
//
// CanManage mirrors the daemon's own canSeeTaintDetail/callerMayManageTaint
// (owner-team-or-admin) exactly via Restricted: GetTaint omits owner_team
// (and beadsapi sets Restricted=true) for exactly the callers those two
// daemon checks would refuse (kbeads' http_taints.go /
// http_taint_grants.go), so this client-side flag never grants more than the
// daemon itself would. It gates the Policy-edit form, grant/revoke actions,
// and whether the Grants/Audit tabs even attempt their daemon calls (which
// would otherwise 403 noisily).
type taintDetailView struct {
	// BasePath/ViewerEmail duplicate the top-level render map's fields of the
	// same name (server.go's render sets both). Every named sub-template
	// below ("taint_detail_policy" etc.) is invoked via {{template "name" .}}
	// from inside {{with .Taint}}, which resets $ to the taintDetailView
	// value passed in — so $.BasePath / $.ViewerEmail inside those
	// sub-templates resolve against this struct, not the outer map.
	BasePath    string
	ViewerEmail string
	Name        string
	Description string
	OwnerTeam   string
	Restricted  bool
	CanManage   bool
	Effects     []string
	Retired     bool
	UpdatedAgo  string
	Tab         string
	Tabs        []taintDetailTab
	Error       string

	Policy *taintPolicyTabView
	Grants *taintGrantsTabView
	Beads  *taintBeadsTabView
	Scrubs *taintScrubsTabView
	Audit  *taintAuditTabView
}

// handleTaintDetail handles GET /taints/{name} — the full taint detail page
// (task gd-nOnc1Q8Zcv). Ungated at the route level, matching GET /taints:
// the daemon's own scopeListFilter (Beads/Scrubs tabs) and
// canSeeTaintDetail/callerMayManageTaint (Policy/Grants/Audit tabs) are the
// real gates, so an extra viewer-role route wrapper would only duplicate
// them without adding anything.
func (s *Server) handleTaintDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Missing taint name", http.StatusBadRequest)
		return
	}
	tab := normalizeTaintTab(r.URL.Query().Get("tab"))
	email := oidcauth.EmailFromContext(r.Context())

	det, err := s.daemon.GetTaint(r.Context(), name)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Taint not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting taint", "name", name, "error", err)
		http.Error(w, "Failed to load taint", http.StatusBadGateway)
		return
	}

	view := s.buildTaintDetailView(r, det, tab, email)
	s.render(w, r, "taint_detail.html", map[string]any{
		"Taint":       view,
		"ViewerEmail": email,
	})
}

// buildTaintDetailView assembles the full page view, populating exactly the
// sub-view matching tab. det.Restricted short-circuits the Policy/Grants/
// Audit tabs' daemon calls (see taintDetailView's doc comment) — Beads and
// Scrubs always call through, since those are scopeListFilter-gated per row
// rather than all-or-nothing per taint.
func (s *Server) buildTaintDetailView(r *http.Request, det *beadsapi.TaintDetail, tab, viewerEmail string) *taintDetailView {
	view := &taintDetailView{
		BasePath:    s.basePath,
		ViewerEmail: viewerEmail,
		Name:        det.Name,
		Description: det.Description,
		OwnerTeam:   det.OwnerTeam,
		Restricted:  det.Restricted,
		CanManage:   !det.Restricted,
		Effects:     det.Effects,
		Retired:     det.RetiredAt != nil,
		UpdatedAgo:  humanizeTime(det.UpdatedAt),
		Tab:         tab,
		Tabs:        taintDetailTabs(s.basePath, det.Name, tab),
	}

	ctx := r.Context()
	switch tab {
	case "grants":
		if det.Restricted {
			view.Grants = &taintGrantsTabView{Error: taintRestrictedMessage(det.OwnerTeam)}
		} else {
			view.Grants = s.buildTaintGrantsTab(ctx, det.Name)
		}
	case "beads":
		view.Beads = s.buildTaintBeadsTab(ctx, det.Name)
	case "scrubs":
		view.Scrubs = s.buildTaintScrubsTab(ctx, det.Name)
	case "audit":
		if det.Restricted {
			view.Audit = &taintAuditTabView{Error: taintRestrictedMessage(det.OwnerTeam)}
		} else {
			action := strings.TrimSpace(r.URL.Query().Get("action"))
			actor := strings.TrimSpace(r.URL.Query().Get("actor"))
			view.Audit = s.buildTaintAuditTab(ctx, det.Name, action, actor)
		}
	default:
		if det.Restricted {
			view.Policy = &taintPolicyTabView{Error: taintRestrictedMessage(det.OwnerTeam)}
		} else {
			view.Policy = buildTaintPolicyTab(det)
		}
	}
	return view
}

func taintRestrictedMessage(ownerTeam string) string {
	if ownerTeam != "" {
		return "You don't hold a grant or membership on this taint's owner team (" + ownerTeam + ") — contact them to request access."
	}
	return "You don't have access to this taint's management views — contact its owner team to request access."
}

// renderTaintDetailError re-fetches the taint and re-renders the page for
// tab with an inline error, mirroring handleProjectConfigSave's re-render-
// with-FieldErrors convention: no special HTTP status, since these are
// ordinary browser form posts with no htmx swap target of their own — the
// error is surfaced in-page instead.
func (s *Server) renderTaintDetailError(w http.ResponseWriter, r *http.Request, name, tab, viewerEmail, errMsg string) {
	det, err := s.daemon.GetTaint(r.Context(), name)
	if err != nil {
		s.logger.Error("re-loading taint after error", "name", name, "error", err)
		http.Error(w, errMsg, http.StatusBadGateway)
		return
	}
	view := s.buildTaintDetailView(r, det, tab, viewerEmail)
	view.Error = errMsg
	s.render(w, r, "taint_detail.html", map[string]any{
		"Taint":       view,
		"ViewerEmail": viewerEmail,
	})
}

// ---------------------------------------------------------------------------
// Policy tab

// viewerScrubRule/viewerScrubPolicy mirror kbeads' internal/model.ScrubRule /
// ScrubPolicy (docs/adr/taints.md §3) closely enough to edit a policy from
// the viewer — the viewer module cannot import the daemon's internal/model
// package, the same constraint taintPresets (taints_registry.go) documents.
type viewerScrubRule struct {
	When      map[string]any `json:"when,omitempty"`
	Mode      string         `json:"mode"`
	Approvers []string       `json:"approvers,omitempty"`
	MaxBeads  *int           `json:"max_beads,omitempty"`
	MaxPerDay *int           `json:"max_per_day,omitempty"`
}

type viewerScrubPolicy struct {
	Default     string            `json:"default"`
	DetaintMode string            `json:"detaint_mode,omitempty"`
	Approvers   []string          `json:"approvers,omitempty"`
	Rules       []viewerScrubRule `json:"rules,omitempty"`
}

// taintPolicyRuleView is one editable rule row. Kind is the `when.kind`
// quick-select field pulled out on its own; any other `when` keys round-trip
// through Predicate as pretty-printed JSON.
type taintPolicyRuleView struct {
	Kind      string
	Predicate string
	Mode      string
	Approvers string
	MaxBeads  string
	MaxPerDay string
}

// taintPolicyBlankRuleRows pads the editable rule table with empty rows so a
// no-JS viewer can add a rule by filling one in, without any "add row"
// scripting.
const taintPolicyBlankRuleRows = 2

type taintPolicyTabView struct {
	Default     string
	DetaintMode string
	Approvers   string
	Rules       []taintPolicyRuleView
	RawJSON     string
	Error       string
}

func buildTaintPolicyTab(det *beadsapi.TaintDetail) *taintPolicyTabView {
	view := &taintPolicyTabView{Default: "human", RawJSON: prettyJSON(string(det.ScrubPolicy))}
	if len(det.ScrubPolicy) == 0 {
		for i := 0; i < taintPolicyBlankRuleRows; i++ {
			view.Rules = append(view.Rules, taintPolicyRuleView{})
		}
		return view
	}
	var policy viewerScrubPolicy
	if err := json.Unmarshal(det.ScrubPolicy, &policy); err != nil {
		view.Error = "Stored policy is not valid JSON: " + err.Error()
		return view
	}
	view.Default = policy.Default
	view.DetaintMode = policy.DetaintMode
	view.Approvers = strings.Join(policy.Approvers, ", ")
	for _, rule := range policy.Rules {
		view.Rules = append(view.Rules, ruleToView(rule))
	}
	for i := 0; i < taintPolicyBlankRuleRows; i++ {
		view.Rules = append(view.Rules, taintPolicyRuleView{})
	}
	return view
}

func ruleToView(rule viewerScrubRule) taintPolicyRuleView {
	row := taintPolicyRuleView{
		Mode:      rule.Mode,
		Approvers: strings.Join(rule.Approvers, ", "),
	}
	if rule.MaxBeads != nil {
		row.MaxBeads = strconv.Itoa(*rule.MaxBeads)
	}
	if rule.MaxPerDay != nil {
		row.MaxPerDay = strconv.Itoa(*rule.MaxPerDay)
	}
	when := make(map[string]any, len(rule.When))
	for k, v := range rule.When {
		when[k] = v
	}
	if kind, ok := when["kind"].(string); ok {
		row.Kind = kind
		delete(when, "kind")
	}
	if len(when) > 0 {
		if raw, err := json.MarshalIndent(when, "", "  "); err == nil {
			row.Predicate = string(raw)
		}
	}
	return row
}

// scrubPolicyFromForm builds a policy from the Policy tab's submitted form,
// mirroring model.ValidateScrubPolicy's "an auto rule needs a cap" rule
// (kbeads/internal/model/taint.go) client-side. This is a UX nicety only —
// per acceptance criteria the daemon's own PATCH validation is authoritative
// and is re-checked regardless of what passes here.
func scrubPolicyFromForm(form url.Values) (*viewerScrubPolicy, string) {
	def := strings.TrimSpace(form.Get("default"))
	if def == "" {
		def = "human"
	}
	if def == "auto" {
		return nil, `the policy default may not be "auto" (docs/adr/taints.md §3)`
	}
	policy := &viewerScrubPolicy{
		Default:     def,
		DetaintMode: strings.TrimSpace(form.Get("detaint_mode")),
	}
	if approvers := strings.TrimSpace(form.Get("approvers")); approvers != "" {
		policy.Approvers = splitAndTrim(approvers)
	}

	kinds := form["rule_kind"]
	predicates := form["rule_predicate"]
	modes := form["rule_mode"]
	approversList := form["rule_approvers"]
	maxBeadsList := form["rule_max_beads"]
	maxPerDayList := form["rule_max_per_day"]

	n := len(kinds)
	for _, list := range [][]string{predicates, modes, approversList, maxBeadsList, maxPerDayList} {
		if len(list) > n {
			n = len(list)
		}
	}

	for i := 0; i < n; i++ {
		kind := trimAt(kinds, i)
		predicate := trimAt(predicates, i)
		mode := trimAt(modes, i)
		approvers := trimAt(approversList, i)
		maxBeadsRaw := trimAt(maxBeadsList, i)
		maxPerDayRaw := trimAt(maxPerDayList, i)

		if kind == "" && predicate == "" && mode == "" && approvers == "" && maxBeadsRaw == "" && maxPerDayRaw == "" {
			continue
		}
		if mode == "" {
			return nil, fmt.Sprintf("rule %d: mode is required", i+1)
		}

		rule := viewerScrubRule{Mode: mode}
		when := map[string]any{}
		if predicate != "" {
			if err := json.Unmarshal([]byte(predicate), &when); err != nil {
				return nil, fmt.Sprintf("rule %d: predicate is not valid JSON: %v", i+1, err)
			}
		}
		if kind != "" {
			when["kind"] = kind
		}
		if len(when) > 0 {
			rule.When = when
		}
		if approvers != "" {
			rule.Approvers = splitAndTrim(approvers)
		}
		if maxBeadsRaw != "" {
			v, err := strconv.Atoi(maxBeadsRaw)
			if err != nil {
				return nil, fmt.Sprintf("rule %d: max_beads must be a whole number", i+1)
			}
			rule.MaxBeads = &v
		}
		if maxPerDayRaw != "" {
			v, err := strconv.Atoi(maxPerDayRaw)
			if err != nil {
				return nil, fmt.Sprintf("rule %d: max_per_day must be a whole number", i+1)
			}
			rule.MaxPerDay = &v
		}
		if mode == "auto" && rule.MaxBeads == nil && rule.MaxPerDay == nil {
			return nil, fmt.Sprintf(`rule %d: an "auto" rule requires max_beads or max_per_day (docs/adr/taints.md §3) — the daemon will refuse this too`, i+1)
		}
		policy.Rules = append(policy.Rules, rule)
	}

	return policy, ""
}

func trimAt(s []string, i int) string {
	if i >= len(s) {
		return ""
	}
	return strings.TrimSpace(s[i])
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleTaintPolicyUpdate handles POST /taints/{name}/policy — the Policy
// tab's save action (task gd-nOnc1Q8Zcv acceptance criteria: "Policy tab
// save rejects an auto rule with no max_beads/max_per_day, matching
// p1-taint-store's server-side validation; client mirrors it, server is
// authoritative"). Route-gated operatorOnly; the daemon's PATCH
// /v1/taints/{name} additionally requires callerMayManageTaint
// (owner-team-or-admin), enforced there regardless of this route gate.
func (s *Server) handleTaintPolicyUpdate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Missing taint name", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	policy, errMsg := scrubPolicyFromForm(r.PostForm)
	if errMsg != "" {
		s.renderTaintDetailError(w, r, name, "policy", email, errMsg)
		return
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		s.renderTaintDetailError(w, r, name, "policy", email, "Failed to encode policy: "+err.Error())
		return
	}

	if _, err := s.daemon.UpdateTaintPolicy(r.Context(), name, raw); err != nil {
		s.logger.Error("updating taint policy", "name", name, "actor", email, "error", err)
		s.renderTaintDetailError(w, r, name, "policy", email, "Failed to save policy: "+err.Error())
		return
	}

	http.Redirect(w, r, s.basePath+"/taints/"+url.PathEscape(name), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Grants tab

// taintGrantRowView is one row of the Grants tab. AgentBead is populated
// best-effort for an agent-kind grant so the template can render
// beadOperatorChip (operator_chip.go) on it — the daemon's own spawn-chain
// walk (audit.go) already resolved and stamped that agent's operated_by
// fields at write time, so no chain-walking happens here (acceptance
// criteria: "agent grants shown greyed with their spawn-chain root human").
type taintGrantRowView struct {
	PrincipalKind string
	Principal     string
	AgentBead     *beadsapi.BeadDetail
	GrantedBy     string
	ExpiresLabel  string
	Expired       bool
	RevokeValue   string
}

type taintGrantsTabView struct {
	Rows  []taintGrantRowView
	Error string
}

func (s *Server) buildTaintGrantsTab(ctx context.Context, name string) *taintGrantsTabView {
	view := &taintGrantsTabView{}
	grants, err := s.daemon.ListTaintGrants(ctx, name)
	if err != nil {
		view.Error = "Failed to load grants: " + err.Error()
		return view
	}

	opDisplay := s.operatorDisplayMap(ctx)
	now := time.Now()

	rows := make([]taintGrantRowView, 0, len(grants))
	for _, g := range grants {
		row := taintGrantRowView{
			PrincipalKind: g.PrincipalKind,
			Principal:     g.Principal,
			RevokeValue:   g.PrincipalKind + ":" + g.Principal,
		}
		if label, ok := opDisplay[g.GrantedBy]; ok {
			row.GrantedBy = label
		} else {
			row.GrantedBy = fmt.Sprintf("operator #%d", g.GrantedBy)
		}
		if g.ExpiresAt != nil {
			row.ExpiresLabel = humanizeTime(*g.ExpiresAt)
			row.Expired = g.ExpiresAt.Before(now)
		} else {
			row.ExpiresLabel = "never"
		}
		if g.PrincipalKind == "agent" {
			if bead, err := s.daemon.GetBead(ctx, g.Principal); err == nil {
				row.AgentBead = bead
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].PrincipalKind != rows[j].PrincipalKind {
			return rows[i].PrincipalKind < rows[j].PrincipalKind
		}
		return rows[i].Principal < rows[j].Principal
	})
	view.Rows = rows
	return view
}

// operatorDisplayMap resolves operator IDs to display names for the Grants
// tab's "Granted by" column, via the public operator directory
// (client_operators.go). Best-effort: a lookup failure just means every row
// falls back to "operator #<id>" instead of failing the whole tab.
func (s *Server) operatorDisplayMap(ctx context.Context) map[int64]string {
	out := map[int64]string{}
	resp, err := s.daemon.ListOperators(ctx, 0)
	if err != nil || resp == nil {
		return out
	}
	for _, op := range resp.Operators {
		label := op.Display
		if label == "" {
			label = op.Email
		}
		if label == "" {
			label = op.Subject
		}
		if label != "" {
			out[op.ID] = label
		}
	}
	return out
}

// handleTaintGrantCreate handles POST /taints/{name}/grants — the Grants
// tab's grant action, owner-team/admin only (task gd-nOnc1Q8Zcv). Route-gated
// operatorOnly; the daemon's own callerMayManageTaint check on
// POST /v1/taints/{name}/grants is authoritative.
func (s *Server) handleTaintGrantCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Missing taint name", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	kind := strings.TrimSpace(r.PostForm.Get("principal_kind"))
	value := strings.TrimSpace(r.PostForm.Get("principal_value"))
	if kind == "" || value == "" {
		s.renderTaintDetailError(w, r, name, "grants", email, "Principal kind and value are both required.")
		return
	}
	req := beadsapi.GrantTaintRequest{Principal: kind + ":" + value}
	if until := strings.TrimSpace(r.PostForm.Get("until")); until != "" {
		t, err := time.Parse("2006-01-02", until)
		if err != nil {
			s.renderTaintDetailError(w, r, name, "grants", email, "Expiry date must be YYYY-MM-DD.")
			return
		}
		req.Until = &t
	}

	if _, err := s.daemon.GrantTaint(r.Context(), name, req); err != nil {
		s.logger.Error("granting taint", "name", name, "principal", req.Principal, "actor", email, "error", err)
		s.renderTaintDetailError(w, r, name, "grants", email, "Failed to grant: "+err.Error())
		return
	}

	http.Redirect(w, r, s.basePath+"/taints/"+url.PathEscape(name)+"?tab=grants", http.StatusSeeOther)
}

// handleTaintGrantRevoke handles POST /taints/{name}/grants/revoke. Revoke
// is idempotent daemon-side (always 200, docs/adr/taints.md), so this never
// surfaces a "no such grant" error to the caller.
func (s *Server) handleTaintGrantRevoke(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Missing taint name", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	principal := strings.TrimSpace(r.PostForm.Get("principal"))
	if principal == "" {
		s.renderTaintDetailError(w, r, name, "grants", email, "Missing principal to revoke.")
		return
	}

	if _, err := s.daemon.RevokeTaint(r.Context(), name, principal); err != nil {
		s.logger.Error("revoking taint grant", "name", name, "principal", principal, "actor", email, "error", err)
		s.renderTaintDetailError(w, r, name, "grants", email, "Failed to revoke: "+err.Error())
		return
	}

	http.Redirect(w, r, s.basePath+"/taints/"+url.PathEscape(name)+"?tab=grants", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Beads tab

// taintBeadRowView is one row of the Beads tab. Rows come only from
// ListTaintBeads, which applies the same scopeListFilter read-gate as every
// other bead list in the viewer (acceptance criteria: "never lists a bead
// the viewer cannot read") — there is no separate client-side filter here.
type taintBeadRowView struct {
	ID           string
	Href         string
	Title        string
	Status       string
	Source       string
	SourceBeadID string
	SourceHref   string
	UpdatedAgo   string
}

type taintBeadsTabView struct {
	Rows  []taintBeadRowView
	Total int
	Error string
}

func (s *Server) buildTaintBeadsTab(ctx context.Context, name string) *taintBeadsTabView {
	view := &taintBeadsTabView{}
	entries, total, err := s.daemon.ListTaintBeads(ctx, name, taintDetailListLimit, 0)
	if err != nil {
		view.Error = "Failed to load tainted beads: " + err.Error()
		return view
	}
	view.Total = total
	for _, e := range entries {
		if e.BeadDetail == nil {
			continue
		}
		row := taintBeadRowView{
			ID:         e.ID,
			Href:       s.basePath + "/" + e.ID,
			Title:      e.Title,
			Status:     e.Status,
			Source:     taintSourceLabel(e.Source),
			UpdatedAgo: humanizeTime(e.UpdatedAt),
		}
		if e.SourceBeadID != "" {
			row.SourceBeadID = e.SourceBeadID
			row.SourceHref = s.basePath + "/" + e.SourceBeadID
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// ---------------------------------------------------------------------------
// Scrubs tab

// taintScrubRowView is one row of the Scrubs tab. CanApprove/CanReject mirror
// the daemon's own approve/reject authorization (beadsapi.
// TaintScrubRequestEntry's doc comment) except the four-eyes
// same-approver-as-requester check, which depends on which specific human
// clicks and so is only enforced at call time — a CanApprove=true row can
// still be refused server-side.
type taintScrubRowView struct {
	ID           string
	Href         string
	Kind         string
	ScopeSize    string
	State        string
	ResolvedMode string
	Bead         *beadsapi.BeadDetail
	CanApprove   bool
	CanReject    bool
	CreatedAgo   string
}

type taintScrubsTabView struct {
	Rows  []taintScrubRowView
	Total int
	Error string
}

func (s *Server) buildTaintScrubsTab(ctx context.Context, name string) *taintScrubsTabView {
	view := &taintScrubsTabView{}
	entries, total, err := s.daemon.ListTaintScrubRequests(ctx, name, taintDetailListLimit, 0)
	if err != nil {
		view.Error = "Failed to load scrub requests: " + err.Error()
		return view
	}
	view.Total = total
	for _, e := range entries {
		if e.BeadDetail == nil {
			continue
		}
		row := taintScrubRowView{
			ID:           e.ID,
			Href:         s.basePath + "/" + e.ID,
			Kind:         e.Fields["kind"],
			ScopeSize:    scrubScopeSizeDisplay(e.Fields["scope"]),
			State:        scrubStateDisplay(e.Status, e.Fields["approval_status"]),
			ResolvedMode: e.ResolvedMode,
			Bead:         e.BeadDetail,
			CanApprove:   e.CanApprove,
			CanReject:    e.CanReject,
			CreatedAgo:   humanizeTime(e.CreatedAt),
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// scrubScopeSizeDisplay renders a scrub-request's scope field (a JSON blob,
// http_scrub_requests_create.go) as a human-readable count. The bead_ids
// shape (the only shape "Request scrub" on a bead page produces,
// taint_badges.go) shows a count; anything else — a future predicate-shaped
// scope (gd-u1zizVKGmz) — shows "predicate" rather than guessing at a size.
func scrubScopeSizeDisplay(scopeJSON string) string {
	scopeJSON = strings.TrimSpace(scopeJSON)
	if scopeJSON == "" {
		return "—"
	}
	var parsed struct {
		BeadIDs []string `json:"bead_ids"`
	}
	if err := json.Unmarshal([]byte(scopeJSON), &parsed); err == nil && len(parsed.BeadIDs) > 0 {
		if len(parsed.BeadIDs) == 1 {
			return "1 bead"
		}
		return strconv.Itoa(len(parsed.BeadIDs)) + " beads"
	}
	return "predicate"
}

func scrubStateDisplay(status, approvalStatus string) string {
	if approvalStatus != "" {
		return status + " · " + approvalStatus
	}
	return status
}

// handleTaintScrubApprove handles POST
// /taints/{name}/scrub-requests/{id}/approve — the Scrubs tab's approve
// action (task gd-nOnc1Q8Zcv acceptance criteria: "call
// p2-scrub-approve-path and are visible only to callers in the relevant
// rule's approvers"). Route-gated operatorOnly, but that is not the real
// gate: ApproveScrubRequest is forced-OIDC (doJSONAsUser — see
// beadsapi.ErrScrubApprovalRequiresUserBearer's doc comment) rather than
// relayed like every other write on this page, so a caller reached through a
// relay-only edge (no forwardable user bearer) fails closed here instead of
// silently approving as whoever's shared token is configured. The daemon's
// own four-eyes / approvers-membership check is authoritative regardless of
// CanApprove on the row that rendered this button.
func (s *Server) handleTaintScrubApprove(w http.ResponseWriter, r *http.Request) {
	s.handleTaintScrubDecision(w, r, true)
}

// handleTaintScrubReject handles POST
// /taints/{name}/scrub-requests/{id}/reject — same forced-OIDC
// authentication as approve, but no four-eyes check (http_scrub_requests.go).
func (s *Server) handleTaintScrubReject(w http.ResponseWriter, r *http.Request) {
	s.handleTaintScrubDecision(w, r, false)
}

func (s *Server) handleTaintScrubDecision(w http.ResponseWriter, r *http.Request, approve bool) {
	name := strings.TrimSpace(r.PathValue("name"))
	id := strings.TrimSpace(r.PathValue("id"))
	if name == "" || id == "" {
		http.Error(w, "Missing taint name or scrub-request id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))

	action := "reject"
	var err error
	if approve {
		action = "approve"
		_, err = s.daemon.ApproveScrubRequest(r.Context(), id, reason)
	} else {
		_, err = s.daemon.RejectScrubRequest(r.Context(), id, reason)
	}
	if err != nil {
		if errors.Is(err, beadsapi.ErrScrubApprovalRequiresUserBearer) {
			s.logger.Warn("scrub-request "+action+": no forwardable user bearer", "operator", email, "bead", id)
			s.renderTaintDetailError(w, r, name, "scrubs", email, "Your session has no forwardable identity token — approving or rejecting a scrub requires signing in directly (not available through this access path).")
			return
		}
		s.logger.Error("scrub-request "+action, "id", id, "actor", email, "error", err)
		s.renderTaintDetailError(w, r, name, "scrubs", email, "Failed to "+action+": "+err.Error())
		return
	}

	http.Redirect(w, r, s.basePath+"/taints/"+url.PathEscape(name)+"?tab=scrubs", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Audit tab

// taintAuditHashTruncate bounds ContentSHA display so a hash reads as an
// identifier, never as enough of the content to reconstruct it (acceptance
// criteria: "hashes truncated, no content ever rendered").
const taintAuditHashTruncate = 12

type taintAuditRowView struct {
	ID           int64
	Action       string
	BeadID       string
	BeadHref     string
	RequestID    string
	Actor        string
	ApprovalMode string
	HashDisplay  string
	Ago          string
}

type taintAuditTabView struct {
	Rows         []taintAuditRowView
	Total        int
	ActionFilter string
	ActorFilter  string
	Error        string
}

func (s *Server) buildTaintAuditTab(ctx context.Context, name, action, actor string) *taintAuditTabView {
	view := &taintAuditTabView{ActionFilter: action, ActorFilter: actor}
	entries, total, err := s.daemon.ListTaintAudit(ctx, name, beadsapi.TaintAuditFilter{
		Action: action,
		Actor:  actor,
		Limit:  taintDetailListLimit,
	})
	if err != nil {
		view.Error = "Failed to load audit log: " + err.Error()
		return view
	}
	view.Total = total
	for _, e := range entries {
		row := taintAuditRowView{
			ID:           e.ID,
			Action:       e.Action,
			BeadID:       e.BeadID,
			RequestID:    e.RequestID,
			Actor:        e.Actor,
			ApprovalMode: e.ApprovalMode,
			HashDisplay:  truncateHash(e.ContentSHA),
			Ago:          humanizeTime(e.CreatedAt),
		}
		if e.BeadID != "" {
			row.BeadHref = s.basePath + "/" + e.BeadID
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

func truncateHash(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return "—"
	}
	if len(h) <= taintAuditHashTruncate {
		return h
	}
	return h[:taintAuditHashTruncate] + "…"
}
