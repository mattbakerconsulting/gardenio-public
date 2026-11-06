package main

// Admin-gated project config editor (task ka-0000007456, epic ka-0000005923,
// design doc §6). Today projects are edited CLI-only (kd update against the
// type:project bead). This file ships the in-UI editor that closes the gap the
// operator named: "adding a secret to a project should be easier."
//
//   GET  /p/{project}/edit    — the config-edit form (admin-gated)
//   POST /p/{project}/config  — persist the edited config (admin-gated)
//
// AUTH is the load-bearing difference from the gardener editor (gardener_ops.go):
// type:project WritePolicy is min_role gardenio-admin (lvl 3) and is NOT
// project_scoped, so the routes register behind RequireRole(RoleAdmin) — a
// GLOBAL admin, no per-project delegation (design doc §6, §9). The daemon
// enforces min_role gardenio-admin for OIDC callers; viewerCanEditProjectConfig
// (project_hub.go) mirrors the route gate so the page never shows a lever the
// route rejects.
//
// FLAGSHIP affordance: a structured add-secret form (env / secret / key →
// appends a SecretEntry to the secrets JSON) so the operator never hand-edits
// the secrets blob. The same scalar fields (git_url, image, account, model,
// budget, resources, flags) are editable inline. Validation is LAYERED: the editor
// pre-validates via the canonical projectValidityReport (the same health
// signal the hub/index render), and on success PATCHes the config bead's
// typed fields — the daemon then re-validates against its TypeConfig (the
// single source of truth). On a validation error we re-render the form with
// inline field errors and the operator's raw input preserved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"k8s.io/apimachinery/pkg/api/resource"
)

// projectConfigView is the data passed to the config-edit form. It carries the
// editable scalar knobs plus the current secret list (rendered by NAME only —
// never a value) and the project identity so the form can post back and
// re-render in place. Draft input + FieldErrors are populated when a POST
// round-trips back to the form on a validation failure, so the admin never
// loses their input.
type projectConfigView struct {
	Project      string
	ConfigBeadID string

	// Editable scalar knobs. Stored as strings so the form re-renders the
	// admin's raw input verbatim on a validation error.
	GitURL        string
	DefaultBranch string
	Image         string
	Account       string
	Model         string
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
	StorageClass  string
	// CardTheme selects the Slack card visual system for this project's
	// channels: "garden" (default) or "business". See
	// gardenio/plans/slack-greenhouse-restyle.md and bridge/theme.go.
	CardTheme string
	// BudgetUSD is the per-project budget in US dollars (decision
	// gd-PtNuTyjQoN). Blank = no budget; 0 is rejected, never stored.
	// BudgetPeriod is the window it covers: "month" (default) or "7d".
	BudgetUSD    string
	BudgetPeriod string

	// Boolean flags (checkboxes).
	DockerEnabled    bool
	PostgresEnabled  bool
	NeedsK8sAPI      bool
	SchedulesEnabled bool
	TriggersEnabled  bool

	// Secrets is the current secret list (read from the config bead), rendered
	// as env→secret/key references. The add-secret form appends one entry.
	Secrets []beadsapi.SecretEntry

	// AddSecret holds the raw add-secret form input, preserved on a validation
	// error so the admin does not retype it.
	AddSecretEnv    string
	AddSecretSecret string
	AddSecretKey    string

	// FieldErrors maps a form field name (e.g. "cpu_limit", "add_secret_env")
	// to an inline error message. Non-empty → the form re-renders with the
	// errors shown next to their fields and the draft preserved.
	FieldErrors map[string]string

	// Error is a top-level alert (e.g. a persist failure not tied to a field).
	Error string
	// Saved flips true after a successful POST so the partial renders a
	// confirmation in place of (or above) the form.
	Saved bool
}

// hasFieldErrors reports whether any inline field error is set.
func (v projectConfigView) hasFieldErrors() bool { return len(v.FieldErrors) > 0 }

// handleProjectConfigForm renders the config-edit form (GET /p/{project}/edit).
// Admin-gated at the route layer. Loads the project's config bead and pre-fills
// the form from its current fields.
func (s *Server) handleProjectConfigForm(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("project"))
	if project == "" {
		http.Error(w, "Missing project", http.StatusBadRequest)
		return
	}
	bead, ok := s.loadProjectConfigBead(w, r, project)
	if !ok {
		return
	}
	s.renderProjectConfigForm(w, r, projectConfigViewFromBead(project, bead))
}

// handleProjectConfigSave persists the edited project config back to the
// type:project bead (POST /p/{project}/config). Admin-gated. The handler reads
// the form, applies the add-secret append when present, pre-validates the whole
// resulting config via the canonical projectValidityReport, and only then
// PATCHes the changed typed fields. The daemon re-validates against its
// TypeConfig. On any validation failure we re-render with inline field errors
// and the raw input preserved (no persist).
func (s *Server) handleProjectConfigSave(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("project"))
	if project == "" {
		http.Error(w, "Missing project", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadProjectConfigBead(w, r, project)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	// Decode the current secret list off the bead's raw fields so an add-secret
	// append composes with what's already there (and survives a re-render). A
	// malformed existing secrets blob is surfaced as a field error rather than
	// silently dropped — the admin must fix it via CLI before the form can save.
	existingSecrets, secretsErr := decodeSecrets(bead.Fields["secrets"])

	// card_theme is new enough that an old daemon (pre-rollout of the
	// card_theme schema field, ka-0000001549 means kbeads never reads config
	// beads back out of the bead store to pick up a mid-rollout schema change)
	// may reject it as an unknown field. The <select> has no blank option, so
	// every submit carries an explicit "garden"/"business" even when the admin
	// never touched that control — only PATCH it when it actually changed from
	// the stored value, so an edit to an unrelated field (git_url, a secret,
	// ...) never 400s against an old daemon on a project that never set
	// card_theme. An unset stored value is treated as the "garden" default.
	originalCardTheme := strings.TrimSpace(bead.Fields["card_theme"])
	// The budget fields follow the same rule for the same reason: only PATCH
	// them when they changed, so a project without a budget never sends them.
	originalBudgetUSD := strings.TrimSpace(bead.Fields["budget_usd"])
	originalBudgetPeriod := strings.TrimSpace(bead.Fields["budget_period"])

	// Build the draft view from the submitted form so we can re-render with the
	// admin's raw input on a validation error.
	draft := projectConfigView{
		Project:          project,
		ConfigBeadID:     bead.ID,
		GitURL:           strings.TrimSpace(r.PostForm.Get("git_url")),
		DefaultBranch:    strings.TrimSpace(r.PostForm.Get("default_branch")),
		Image:            strings.TrimSpace(r.PostForm.Get("image")),
		Account:          strings.TrimSpace(r.PostForm.Get("account")),
		Model:            strings.TrimSpace(r.PostForm.Get("model")),
		CPURequest:       strings.TrimSpace(r.PostForm.Get("cpu_request")),
		CPULimit:         strings.TrimSpace(r.PostForm.Get("cpu_limit")),
		MemoryRequest:    strings.TrimSpace(r.PostForm.Get("memory_request")),
		MemoryLimit:      strings.TrimSpace(r.PostForm.Get("memory_limit")),
		StorageClass:     strings.TrimSpace(r.PostForm.Get("storage_class")),
		CardTheme:        strings.TrimSpace(r.PostForm.Get("card_theme")),
		BudgetUSD:        strings.TrimSpace(r.PostForm.Get("budget_usd")),
		BudgetPeriod:     strings.TrimSpace(r.PostForm.Get("budget_period")),
		DockerEnabled:    r.PostForm.Get("docker") != "",
		PostgresEnabled:  r.PostForm.Get("postgres") != "",
		NeedsK8sAPI:      r.PostForm.Get("needs_k8s_api") != "",
		SchedulesEnabled: r.PostForm.Get("schedules_enabled") != "",
		TriggersEnabled:  r.PostForm.Get("triggers_enabled") != "",
		Secrets:          existingSecrets,
		AddSecretEnv:     strings.TrimSpace(r.PostForm.Get("add_secret_env")),
		AddSecretSecret:  strings.TrimSpace(r.PostForm.Get("add_secret_secret")),
		AddSecretKey:     strings.TrimSpace(r.PostForm.Get("add_secret_key")),
		FieldErrors:      map[string]string{},
	}

	if secretsErr != nil {
		draft.FieldErrors["secrets"] = fmt.Sprintf("existing secrets JSON is malformed — fix via `kd update %s` before editing here: %v", bead.ID, secretsErr)
		s.renderProjectConfigForm(w, r, draft)
		return
	}

	// Add-secret: the flagship affordance. Any non-empty add-secret field arms
	// the append; all three (env / secret / key) are then required. An empty
	// trio is a no-op (the admin only edited scalars).
	secrets := existingSecrets
	if draft.AddSecretEnv != "" || draft.AddSecretSecret != "" || draft.AddSecretKey != "" {
		if draft.AddSecretEnv == "" {
			draft.FieldErrors["add_secret_env"] = "Env var name is required."
		}
		if draft.AddSecretSecret == "" {
			draft.FieldErrors["add_secret_secret"] = "K8s Secret name is required."
		}
		if draft.AddSecretKey == "" {
			draft.FieldErrors["add_secret_key"] = "Secret key is required."
		}
		// Reject a duplicate env var name — two SecretEntry rows mapping the
		// same env var is a silent footgun (last one wins at pod spec time).
		if draft.AddSecretEnv != "" {
			for _, e := range existingSecrets {
				if e.Env == draft.AddSecretEnv {
					draft.FieldErrors["add_secret_env"] = fmt.Sprintf("Env var %q already maps a secret — remove it first via CLI.", draft.AddSecretEnv)
					break
				}
			}
		}
		if draft.hasFieldErrors() {
			s.renderProjectConfigForm(w, r, draft)
			return
		}
		secrets = append(append([]beadsapi.SecretEntry{}, existingSecrets...), beadsapi.SecretEntry{
			Env:    draft.AddSecretEnv,
			Secret: draft.AddSecretSecret,
			Key:    draft.AddSecretKey,
		})
	}

	// K8s-quantity pre-validation for the resource knobs. Empty = inherit, which
	// is valid. We mirror the daemon-tolerated/silently-broken class the
	// projectValidityReport catches — flag it here BEFORE persisting so a bad
	// quantity never reaches the bead.
	for field, val := range map[string]string{
		"cpu_request":    draft.CPURequest,
		"cpu_limit":      draft.CPULimit,
		"memory_request": draft.MemoryRequest,
		"memory_limit":   draft.MemoryLimit,
	} {
		if val == "" {
			continue
		}
		if _, err := resource.ParseQuantity(val); err != nil {
			draft.FieldErrors[field] = fmt.Sprintf("%q is not a valid Kubernetes quantity (e.g. 500m, 2Gi).", val)
		}
	}
	if draft.hasFieldErrors() {
		s.renderProjectConfigForm(w, r, draft)
		return
	}

	// card_theme has no blank option in the <select> (it defaults to "garden"),
	// but normalize/validate defensively against a non-browser POST — the
	// daemon's enum field rejects anything outside garden/business, including
	// an empty string.
	if draft.CardTheme == "" {
		draft.CardTheme = "garden"
	} else if draft.CardTheme != "garden" && draft.CardTheme != "business" {
		draft.FieldErrors["card_theme"] = fmt.Sprintf("%q is not a valid card theme (must be garden or business).", draft.CardTheme)
	}
	if draft.hasFieldErrors() {
		s.renderProjectConfigForm(w, r, draft)
		return
	}

	// budget_period has no blank option either; an omitted value is the
	// "month" default. Its validity, and budget_usd's (> 0 or blank), are
	// checked by the validity report below.
	if draft.BudgetPeriod == "" {
		draft.BudgetPeriod = "month"
	}

	// Compose the candidate raw-field map (existing fields overlaid with the
	// edits) and run the canonical validity report over it — the SAME health
	// signal the hub/index render. A red finding blocks the save with the
	// finding surfaced inline on its field; this is the editor's pre-POST gate
	// (design doc §5.3) that complements the daemon's authoritative re-validate.
	candidate := candidateRawFields(bead.Fields, draft, secrets)
	report := buildProjectValidityReport(candidate, s.projectConfigAccountSet(r.Context()))
	for _, c := range report.Checks {
		if c.Severity != severityError {
			continue // warnings don't block the save
		}
		if _, taken := draft.FieldErrors[c.Field]; !taken {
			draft.FieldErrors[c.Field] = c.Message
		}
	}
	if draft.hasFieldErrors() {
		s.renderProjectConfigForm(w, r, draft)
		return
	}

	// Build the typed PATCH. We send ONLY the schema-declared keys we manage —
	// the daemon's additive merge leaves every other field (and any concurrent
	// write) untouched. JSON fields go in as proper Go values; the daemon's
	// type:json check accepts any valid JSON and re-validates the whole bead.
	patch := map[string]any{
		"git_url":           draft.GitURL,
		"default_branch":    draft.DefaultBranch,
		"image":             draft.Image,
		"account":           draft.Account,
		"model":             draft.Model,
		"cpu_request":       draft.CPURequest,
		"cpu_limit":         draft.CPULimit,
		"memory_request":    draft.MemoryRequest,
		"memory_limit":      draft.MemoryLimit,
		"storage_class":     draft.StorageClass,
		"docker":            draft.DockerEnabled,
		"postgres":          draft.PostgresEnabled,
		"needs_k8s_api":     draft.NeedsK8sAPI,
		"schedules_enabled": draft.SchedulesEnabled,
		"triggers_enabled":  draft.TriggersEnabled,
		// secrets is the flagship JSON field; pass the typed slice so the daemon
		// stores well-formed JSON. An empty slice serializes to [] (valid).
		"secrets": secrets,
	}
	if draft.CardTheme != originalCardTheme && !(originalCardTheme == "" && draft.CardTheme == "garden") {
		patch["card_theme"] = draft.CardTheme
	}
	maps.Copy(patch, budgetPatch(draft, originalBudgetUSD, originalBudgetPeriod))
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), bead.ID, patch); err != nil {
		s.logger.Error("project config save: persist", "project", project, "bead", bead.ID, "actor", email, "error", err)
		draft.Error = "Failed to save the project config (the daemon rejected the update). " + daemonRejectHint(err)
		s.renderProjectConfigForm(w, r, draft)
		return
	}
	if err := s.daemon.AddComment(r.Context(), bead.ID, email, fmt.Sprintf("project config edited by %s", email)); err != nil {
		s.logger.Warn("project config save audit comment", "project", project, "bead", bead.ID, "actor", email, "error", err)
	}

	// Re-render from the persisted state so the secret list reflects the append
	// and the add-secret inputs clear.
	saved := projectConfigView{
		Project:          project,
		ConfigBeadID:     bead.ID,
		GitURL:           draft.GitURL,
		DefaultBranch:    draft.DefaultBranch,
		Image:            draft.Image,
		Account:          draft.Account,
		Model:            draft.Model,
		CPURequest:       draft.CPURequest,
		CPULimit:         draft.CPULimit,
		MemoryRequest:    draft.MemoryRequest,
		MemoryLimit:      draft.MemoryLimit,
		StorageClass:     draft.StorageClass,
		CardTheme:        draft.CardTheme,
		BudgetUSD:        draft.BudgetUSD,
		BudgetPeriod:     draft.BudgetPeriod,
		DockerEnabled:    draft.DockerEnabled,
		PostgresEnabled:  draft.PostgresEnabled,
		NeedsK8sAPI:      draft.NeedsK8sAPI,
		SchedulesEnabled: draft.SchedulesEnabled,
		TriggersEnabled:  draft.TriggersEnabled,
		Secrets:          secrets,
		FieldErrors:      map[string]string{},
		Saved:            true,
	}
	s.renderProjectConfigForm(w, r, saved)
}

// loadProjectConfigBead loads the type:project config bead behind a project
// name. Returns an HTTP-mapped error via the writer and (nil, false) when the
// project has no config bead (label-only — there is nothing to edit) or on a
// daemon error; callers return immediately when ok is false.
func (s *Server) loadProjectConfigBead(w http.ResponseWriter, r *http.Request, project string) (*beadsapi.BeadDetail, bool) {
	info, err := s.daemon.ListProjectBeads(r.Context())
	if err != nil {
		s.logger.Error("project config: list project beads", "project", project, "error", err)
		http.Error(w, "Failed to load project config", http.StatusBadGateway)
		return nil, false
	}
	pi, ok := info[project]
	if !ok || pi.BeadID == "" {
		// Label-only project: no type:project bead exists to edit. The editor
		// only manages an existing config bead; creating one is a CLI operation.
		http.Error(w, "This project has no type:project config bead to edit (it is label-only). Create one via the CLI first.", http.StatusNotFound)
		return nil, false
	}
	bead, err := s.daemon.GetBead(r.Context(), pi.BeadID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Project config bead not found", http.StatusNotFound)
			return nil, false
		}
		s.logger.Error("project config: get bead", "project", project, "bead", pi.BeadID, "error", err)
		http.Error(w, "Failed to load project config", http.StatusInternalServerError)
		return nil, false
	}
	if bead == nil {
		http.Error(w, "Project config bead not found", http.StatusNotFound)
		return nil, false
	}
	return bead, true
}

// renderProjectConfigForm renders the config-edit partial as a standalone htmx
// fragment. Rendered at HTTP 200 (not 4xx) even on a validation error because
// htmx 2's default responseHandling does not swap on 4xx/5xx — same rationale
// as renderGardenerConfigError (ka-0000005181).
//
// A JSON caller (the garden's project panel, Lands WB-10) gets the outcome
// instead: {"ok":true,"bead":<config bead>} once saved, else the top-level
// error or "Fix the fields marked." with the inline errors under "fields".
func (s *Server) renderProjectConfigForm(w http.ResponseWriter, r *http.Request, v projectConfigView) {
	if v.FieldErrors == nil {
		v.FieldErrors = map[string]string{}
	}
	if wantsActionJSON(r) {
		res := projectConfigAction{actionResponse: actionResponse{OK: true, Bead: v.ConfigBeadID}}
		switch {
		case v.Error != "":
			res.OK, res.Error, res.Status = false, v.Error, http.StatusBadGateway
		case v.hasFieldErrors():
			res.OK, res.Error, res.Status = false, "Fix the fields marked.", http.StatusUnprocessableEntity
		}
		if !res.OK {
			res.Fields = v.FieldErrors
		}
		code := http.StatusOK
		if !res.OK {
			code = res.Status
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	s.renderPartial(w, r, "templates/partials/project_config.html", "project_config", map[string]any{"Config": v})
}

// projectConfigAction is the config save's JSON answer: a write route's
// (actionResponse) with the form's inline errors, by field.
type projectConfigAction struct {
	actionResponse
	Fields map[string]string `json:"fields,omitempty"`
}

// projectConfigViewFromBead builds the edit-form view from a config bead's
// current raw fields. Secrets are decoded best-effort: a malformed blob renders
// an empty list (the admin sees the add-secret form but the existing-secrets
// section is blank — the validity report surfaces the malformed-JSON finding on
// the hub).
func projectConfigViewFromBead(project string, b *beadsapi.BeadDetail) projectConfigView {
	f := b.Fields
	secrets, _ := decodeSecrets(f["secrets"])
	return projectConfigView{
		Project:          project,
		ConfigBeadID:     b.ID,
		GitURL:           f["git_url"],
		DefaultBranch:    f["default_branch"],
		Image:            f["image"],
		Account:          f["account"],
		Model:            f["model"],
		CPURequest:       f["cpu_request"],
		CPULimit:         f["cpu_limit"],
		MemoryRequest:    f["memory_request"],
		MemoryLimit:      f["memory_limit"],
		StorageClass:     f["storage_class"],
		CardTheme:        f["card_theme"],
		BudgetUSD:        f["budget_usd"],
		BudgetPeriod:     f["budget_period"],
		DockerEnabled:    f["docker"] == "true",
		PostgresEnabled:  f["postgres"] == "true",
		NeedsK8sAPI:      f["needs_k8s_api"] == "true",
		SchedulesEnabled: f["schedules_enabled"] != "false", // default true
		TriggersEnabled:  f["triggers_enabled"] != "false",  // default true
		Secrets:          secrets,
		FieldErrors:      map[string]string{},
	}
}

// decodeSecrets parses the raw secrets JSON field into a SecretEntry slice. An
// empty field returns (nil, nil) — absent secrets is valid. A non-empty but
// malformed blob returns the parse error so callers can surface it rather than
// silently drop the secrets (which would let a save erase them).
func decodeSecrets(raw string) ([]beadsapi.SecretEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []beadsapi.SecretEntry
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("unexpected trailing data after JSON value")
	}
	return out, nil
}

// marshalSecrets serializes a SecretEntry slice to its JSON field form. A nil
// or empty slice serializes to "[]" (a valid, empty secrets list) so the
// candidate-field map and the validity report agree on the post-PATCH shape.
func marshalSecrets(secrets []beadsapi.SecretEntry) string {
	if len(secrets) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		// SecretEntry is a flat string struct — marshal cannot fail in practice.
		return "[]"
	}
	return string(raw)
}

// candidateRawFields composes the raw-field map the validity report runs over:
// the bead's existing fields overlaid with the edited scalars, flags, and the
// (possibly appended) secrets JSON. Mirrors how the daemon will see the bead
// after the PATCH so the pre-POST report matches the post-PATCH state.
func candidateRawFields(existing map[string]string, draft projectConfigView, secrets []beadsapi.SecretEntry) map[string]string {
	out := make(map[string]string, len(existing)+1)
	maps.Copy(out, existing)
	out["git_url"] = draft.GitURL
	out["default_branch"] = draft.DefaultBranch
	out["image"] = draft.Image
	out["account"] = draft.Account
	out["model"] = draft.Model
	out["cpu_request"] = draft.CPURequest
	out["cpu_limit"] = draft.CPULimit
	out["memory_request"] = draft.MemoryRequest
	out["memory_limit"] = draft.MemoryLimit
	out["storage_class"] = draft.StorageClass
	out["card_theme"] = draft.CardTheme
	if draft.BudgetUSD == "" {
		// No budget: the save clears both fields.
		delete(out, "budget_usd")
		delete(out, "budget_period")
	} else {
		out["budget_usd"] = draft.BudgetUSD
		out["budget_period"] = draft.BudgetPeriod
	}
	out["secrets"] = marshalSecrets(secrets)
	return out
}

// budgetPatch returns the budget_usd / budget_period keys the save must PATCH,
// given an already-validated draft and the stored values. A blank budget means
// "no budget": any stored budget_usd / budget_period is deleted (a JSON null
// unsets the key), never written as 0 or "". A set budget is sent as a number
// only when it differs from the stored amount, and its period only when it
// differs from the stored period. Unchanged fields are left out, so a project
// that never had a budget sends neither key.
func budgetPatch(draft projectConfigView, storedUSD, storedPeriod string) map[string]any {
	out := map[string]any{}
	if draft.BudgetUSD == "" {
		if storedUSD != "" {
			out["budget_usd"] = nil
		}
		if storedPeriod != "" {
			out["budget_period"] = nil
		}
		return out
	}
	v, err := parseBudgetUSD(draft.BudgetUSD)
	if err != nil {
		return out // unreachable after the validity report; never send a bad value
	}
	if prev, err := strconv.ParseFloat(storedUSD, 64); err != nil || prev != v {
		out["budget_usd"] = v
	}
	if draft.BudgetPeriod != storedPeriod {
		out["budget_period"] = draft.BudgetPeriod
	}
	return out
}

// projectConfigAccountSet loads the set of live type:account bead names so the
// validity report's account-reference check can resolve the edited account.
// Reuses the index's account-set loader. Best-effort: nil on error skips the
// reference check rather than flagging a valid account as dangling.
func (s *Server) projectConfigAccountSet(ctx context.Context) map[string]struct{} {
	return s.indexAccountSet(ctx)
}

// daemonRejectHint extracts a short, admin-readable hint from a daemon update
// error (e.g. an unknown-field or bad-enum rejection from ValidateTypeFields)
// so the inline alert points at the cause without dumping the raw error.
func daemonRejectHint(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Keep it short: the daemon wraps the field error; surface the tail.
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return strings.TrimSpace(msg[i+2:])
	}
	return strings.TrimSpace(msg)
}
