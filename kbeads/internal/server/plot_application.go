package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Lands plot applications (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-track-b-design.md §2.4, decision B-D4; step 7.4).
//
// A stranger who signed in with GitHub and accepted the terms applies for a
// plot through the Gate (beads-viewer /lands/apply, step 7.7). The Gate files
// one type=plot_application bead per application, never under the applicant's
// own bearer: the type is admin-gated, so only an OIDC gardenio-admin or a
// machine principal granted the write may create or change one.
//
// Every application lives in the operator-only project lands-intake. A bead
// with no project label is visible to every scoped principal (the read cut is
// LabelsOrNoProject), and one with a second project label is visible in that
// land, so an application carries exactly one project label and it is
// project:lands-intake. No Keycloak group maps to lands-intake, so no tenant
// can read another applicant's application.
//
// The rules a FieldDef cannot express are checked here: the label rule, the
// shape of land_name and repo, the purpose length, non-negative amounts, the
// born-submitted rule and the state machine.

const typePlotApplication model.BeadType = "plot_application"

// landsIntakeProject is the project every plot application belongs to.
const landsIntakeProject = "lands-intake"

// Plot application states (§2.4).
const (
	plotStateSubmitted    = "submitted"
	plotStateApproved     = "approved"
	plotStateDenied       = "denied"
	plotStateWithdrawn    = "withdrawn"
	plotStateProvisioning = "provisioning"
	plotStateProvisioned  = "provisioned"
	plotStateFailed       = "failed"
	plotStateClosed       = "closed"
)

// plotStateEdges is the application lifecycle: submitted → approved / denied /
// withdrawn → provisioning → provisioned / failed → closed. An approved
// application can still be closed before provisioning starts (the operator
// changes their mind), and a failed one may be provisioned again. closed is
// terminal.
var plotStateEdges = map[string][]string{
	plotStateSubmitted:    {plotStateApproved, plotStateDenied, plotStateWithdrawn},
	plotStateApproved:     {plotStateProvisioning, plotStateClosed},
	plotStateDenied:       {plotStateClosed},
	plotStateWithdrawn:    {plotStateClosed},
	plotStateProvisioning: {plotStateProvisioned, plotStateFailed},
	plotStateProvisioned:  {plotStateClosed},
	plotStateFailed:       {plotStateProvisioning, plotStateClosed},
	plotStateClosed:       nil,
}

// plotApplicationDecisionFields are written from the approval decision (§2.5),
// by the provisioner or by gb lands close, never by the Gate when it files the
// application.
var plotApplicationDecisionFields = []string{
	"asked_at", "credits_granted_usd", "decided_at", "decided_by", "deny_reason", "model", "project", "question",
	"provision_step", "provision_attempt", "provision_error", "provision_failed_at", "provision_escalation_id",
	"close_requested_at", "close_requested_by", "close_reason", "close_step",
}

// landNamePattern is a DNS label of 3 to 30 characters: the land name becomes
// the project name, the Keycloak group project-<land> and the namespace
// land-<land>.
var landNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,28}[a-z0-9]$`)

// reservedLandNames are names a land may never take: the intake project
// itself, the operator's own project and names that read as the platform.
var reservedLandNames = map[string]bool{
	landsIntakeProject: true,
	"gardenio":         true,
	"admin":            true,
	"lands":            true,
	"system":           true,
	"default":          true,
	"operator":         true,
	"support":          true,
	"kube-system":      true,
}

// githubRepoPattern is owner/name on github.com: a GitHub login (letters,
// digits and single hyphens, at most 39 characters) and a repository name.
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}$`)

// plotApplicationPurposeMax is the purpose limit in characters (§2.4).
const plotApplicationPurposeMax = 1000

// plotApplicationFields is the subset of type:plot_application fields the
// cross-field rules read. Types and enum values are checked by the type
// config first.
type plotApplicationFields struct {
	State               string   `json:"state"`
	LandName            string   `json:"land_name"`
	Repo                string   `json:"repo"`
	Purpose             string   `json:"purpose"`
	CreditsRequestedUSD *float64 `json:"credits_requested_usd"`
	CreditsGrantedUSD   *float64 `json:"credits_granted_usd"`
}

func decodePlotApplicationFields(raw json.RawMessage) (plotApplicationFields, map[string]any) {
	var f plotApplicationFields
	all := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
		_ = json.Unmarshal(raw, &all)
	}
	return f, all
}

// plotApplicationLabelError reports a label set that breaks the intake rule:
// exactly one project label, project:lands-intake.
func plotApplicationLabelError(labels []string) error {
	projects := projectLabelValues(labels)
	if len(projects) == 1 && projects[0] == landsIntakeProject {
		return nil
	}
	if len(projects) == 0 {
		return fmt.Errorf("a plot_application needs the label project:%s (an application with no project label is visible in every land)", landsIntakeProject)
	}
	return fmt.Errorf("a plot_application carries exactly one project label, project:%s; got %s", landsIntakeProject, strings.Join(projects, ", "))
}

// validatePlotApplicationShape checks the field rules that hold in every
// state: land_name, repo, purpose and the amounts.
func validatePlotApplicationShape(f plotApplicationFields) error {
	if !landNamePattern.MatchString(f.LandName) {
		return fmt.Errorf("land_name %q must be a DNS label: 3 to 30 characters, lowercase letters, digits and '-', starting with a letter and not ending with '-'", f.LandName)
	}
	if reservedLandNames[f.LandName] {
		return fmt.Errorf("land_name %q is reserved", f.LandName)
	}
	if !githubRepoPattern.MatchString(f.Repo) {
		return fmt.Errorf("repo %q must be owner/name on github.com", f.Repo)
	}
	if n := utf8.RuneCountInString(f.Purpose); n > plotApplicationPurposeMax {
		return fmt.Errorf("purpose is %d characters; the limit is %d", n, plotApplicationPurposeMax)
	}
	if f.CreditsRequestedUSD != nil && *f.CreditsRequestedUSD < 0 {
		return fmt.Errorf("credits_requested_usd must not be negative")
	}
	if f.CreditsGrantedUSD != nil && *f.CreditsGrantedUSD < 0 {
		return fmt.Errorf("credits_granted_usd must not be negative")
	}
	return nil
}

// validatePlotApplicationCreate enforces the create-time rules: the intake
// label, the field shape, state submitted, no decision field set, and a
// land_name that is not already a project.
func validatePlotApplicationCreate(ctx context.Context, st store.Store, bead *model.Bead) error {
	if err := plotApplicationLabelError(bead.Labels); err != nil {
		return err
	}
	f, all := decodePlotApplicationFields(bead.Fields)
	if f.State != plotStateSubmitted {
		return fmt.Errorf("a plot_application is created in state %q, not %q", plotStateSubmitted, f.State)
	}
	var set []string
	for _, k := range plotApplicationDecisionFields {
		if v, ok := all[k]; ok && !isEmptyFieldValue(v) {
			set = append(set, k)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("a new plot_application cannot carry %s: those are set by the approval decision", strings.Join(set, ", "))
	}
	if err := validatePlotApplicationShape(f); err != nil {
		return err
	}
	return landNameTakenError(ctx, st, f.LandName)
}

// landNameTakenError refuses a land_name that is already in use: a project
// bead names it (live or soft-deleted), or any bead carries project:<name>. A
// name known only by its label is still a fleet project, and a land granted
// it would get owner tier over that project's beads (gd-i74esUHCTg). The
// lookups read through the store, never the caller's scope.
func landNameTakenError(ctx context.Context, st store.Store, name string) error {
	project, err := st.FindProjectByName(ctx, name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("project lookup failed: %w", err)
	case project != nil:
		return fmt.Errorf("land_name %q is already a project", name)
	}
	for _, f := range []model.BeadFilter{
		{Type: []model.BeadType{"project"}, Titles: []string{name, "Project: " + name}},
		{Labels: []string{"project:" + name}},
	} {
		f.IncludeDeleted, f.Limit, f.NoTotal = true, 1, true
		beads, _, err := st.ListBeads(ctx, f)
		if err != nil {
			return fmt.Errorf("project lookup failed: %w", err)
		}
		if len(beads) > 0 {
			return fmt.Errorf("land_name %q is already in use: bead %s is named for it or carries project:%s", name, beads[0].ID, name)
		}
	}
	return nil
}

// plotApplicationNameRecheckError checks the land name again when an update
// approves an application or renames one that is not yet approved: a name
// free when the application was filed may have come into use since.
func plotApplicationNameRecheckError(ctx context.Context, st store.Store, prior, next json.RawMessage) error {
	from, _ := decodePlotApplicationFields(prior)
	to, _ := decodePlotApplicationFields(next)
	approving := to.State == plotStateApproved && from.State != plotStateApproved
	renamed := to.LandName != from.LandName && (to.State == plotStateSubmitted || to.State == plotStateApproved)
	if !approving && !renamed {
		return nil
	}
	return landNameTakenError(ctx, st, to.LandName)
}

// validatePlotApplicationUpdate checks a fields update against the post-merge
// bead: the field shape and a legal state move from prior.
func validatePlotApplicationUpdate(prior, next json.RawMessage) error {
	from, _ := decodePlotApplicationFields(prior)
	to, _ := decodePlotApplicationFields(next)
	if err := validatePlotApplicationShape(to); err != nil {
		return err
	}
	if from.State == to.State {
		return nil
	}
	if !slices.Contains(plotStateEdges[from.State], to.State) {
		legal := "none"
		if edges := plotStateEdges[from.State]; len(edges) > 0 {
			legal = strings.Join(edges, ", ")
		}
		return fmt.Errorf("plot_application state cannot move %s → %s; from %s it may move to %s", from.State, to.State, from.State, legal)
	}
	return nil
}

// plotApplicationLabelChangeError applies the intake rule to a single label
// add or remove. Only a project label can break it.
func plotApplicationLabelChangeError(bead *model.Bead, label string, adding bool) error {
	if bead == nil || bead.Type != typePlotApplication || !strings.HasPrefix(label, "project:") {
		return nil
	}
	next := slices.DeleteFunc(slices.Clone(bead.Labels), func(l string) bool { return l == label })
	if adding {
		next = append(next, label)
	}
	return plotApplicationLabelError(next)
}
