// `gb grafana users` — bump the org role of EXISTING Grafana users via the
// Grafana HTTP API (task ka-0000003341).
//
// Motivation. gardenio Grafana provisions every AVA-authenticated user with a
// default org role (GF_USERS_AUTO_ASSIGN_ORG_ROLE) — Admin on fresh prod
// (grafana-viewer-proxy-values.yaml, decision ka-0000002146 / operator request
// ka-0000005124). But that default only applies to users provisioned AFTER the
// value flips (at first login, or on auth-proxy header re-sync). A user who was
// already provisioned as Viewer keeps Viewer until they are bumped out of band.
//
// Historically that bump was a hand-rolled curl against the Grafana org-users
// API (thread thread-1781621532-248629, 2026-06-16: matthew.baker bumped
// Viewer→Admin live). This turns that one-off into a durable, idempotent
// affordance so the next "everyone who's still Viewer should be Admin" is one
// command, not archaeology.
//
// It drives Grafana's org-users API:
//
//	GET   <base>/api/org/users             → list org users (userId, login, role)
//	PATCH <base>/api/org/users/<userId>    → {"role":"Admin"} set a user's role
//
// reached through the same base + auth resolution `gb grafana render` uses
// (grafanaRenderBaseURL + applyGrafanaAuth). The org-users write endpoints
// require the caller to be an Org Admin: supply GRAFANA_TOKEN (an Admin
// service-account token) or run behind the auth-proxy as an Admin identity —
// which, with defaultOrgRole=Admin, the operator now is.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// grafanaUsersTimeout bounds each org-users API call. These are small JSON
// calls against the in-cluster Grafana, so the render timeout would be overkill.
const grafanaUsersTimeout = 20 * time.Second

// grafanaUsersHTTPClient is the production hook for the org-users calls; tests
// override it to avoid standing up a real Grafana.
var grafanaUsersHTTPClient = func() *http.Client {
	return &http.Client{Timeout: grafanaUsersTimeout}
}

// grafanaOrgRoles is the set of valid Grafana org roles, lowercased for
// case-insensitive validation. Grafana stores/returns them title-cased.
var grafanaOrgRoles = map[string]string{
	"viewer": "Viewer",
	"editor": "Editor",
	"admin":  "Admin",
}

// canonicalOrgRole normalises a user-supplied role to Grafana's title-case form
// (Viewer/Editor/Admin), or returns ok=false when it is not a real org role.
func canonicalOrgRole(role string) (string, bool) {
	canon, ok := grafanaOrgRoles[strings.ToLower(strings.TrimSpace(role))]
	return canon, ok
}

// grafanaOrgUser is one entry from GET /api/org/users. Grafana returns more
// fields; we decode only what the list/bump surface needs.
type grafanaOrgUser struct {
	UserID int    `json:"userId"`
	Email  string `json:"email"`
	Login  string `json:"login"`
	Role   string `json:"role"`
}

// ── gb grafana users ──────────────────────────────────────────────────────

var grafanaUsersCmd = &cobra.Command{
	Use:   "users",
	Short: "List and bump the org role of existing Grafana users",
	Long: `List existing Grafana org users and bump their org role via the Grafana API.

GF_USERS_AUTO_ASSIGN_ORG_ROLE only sets the role for users provisioned AFTER it
flips; users already provisioned at the old default keep it. These commands
reconcile the existing users to the role you want.`,
}

// ── gb grafana users list ─────────────────────────────────────────────────

var grafanaUsersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List existing Grafana org users and their roles",
	Long: `List the users in the Grafana org (GET /api/org/users) with their roles.

Use this to see who is still on the old default before a bump. --role filters
to a single role (e.g. --role Viewer to see exactly who a bump would touch).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		baseFlag, _ := cmd.Flags().GetString("grafana-url")
		roleFilter, _ := cmd.Flags().GetString("role")

		base := grafanaRenderBaseURL(baseFlag)
		if base == "" {
			return errNoGrafanaBase
		}
		users, err := listGrafanaOrgUsers(cmd.Context(), base)
		if err != nil {
			return err
		}
		if canon := strings.TrimSpace(roleFilter); canon != "" {
			c, ok := canonicalOrgRole(canon)
			if !ok {
				return fmt.Errorf("invalid --role %q (want Viewer, Editor, or Admin)", roleFilter)
			}
			users = filterUsersByRole(users, c)
		}

		if jsonOutput {
			printJSON(users)
			return nil
		}
		if len(users) == 0 {
			fmt.Fprintln(os.Stderr, "no users match")
			return nil
		}
		for _, u := range users {
			fmt.Printf("%d\t%s\t%s\n", u.UserID, u.Role, userLabel(u))
		}
		return nil
	},
}

// ── gb grafana users bump ─────────────────────────────────────────────────

var grafanaUsersBumpCmd = &cobra.Command{
	Use:   "bump",
	Short: "Bump existing Grafana users' org role (default Viewer→Admin)",
	Long: `Bump the org role of existing Grafana users via the Grafana API.

By default this promotes every user currently at --from (Viewer) to --to (Admin)
— the "existing Viewers should be Admin like new users are" reconcile. The
operation is idempotent: users already at --to are skipped, so re-running is a
no-op once everyone is bumped.

Scope it down with --login/--email (repeatable) to bump only named users, or
widen it with --from "" to bump every user regardless of current role. --dry-run
shows exactly which users would change without writing anything.

The org-users write endpoint requires an Org Admin caller: set GRAFANA_TOKEN to
an Admin service-account token, or run behind the auth-proxy as an Admin.

Examples:
  gb grafana users bump                          # all Viewers → Admin
  gb grafana users bump --dry-run                # preview only
  gb grafana users bump --to Editor              # all Viewers → Editor
  gb grafana users bump --login matthew.baker    # just this user → Admin
  gb grafana users bump --from "" --to Admin     # everyone → Admin`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		baseFlag, _ := cmd.Flags().GetString("grafana-url")
		toFlag, _ := cmd.Flags().GetString("to")
		fromFlag, _ := cmd.Flags().GetString("from")
		logins, _ := cmd.Flags().GetStringArray("login")
		emails, _ := cmd.Flags().GetStringArray("email")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		to, ok := canonicalOrgRole(toFlag)
		if !ok {
			return fmt.Errorf("invalid --to %q (want Viewer, Editor, or Admin)", toFlag)
		}
		var from string
		if strings.TrimSpace(fromFlag) != "" {
			from, ok = canonicalOrgRole(fromFlag)
			if !ok {
				return fmt.Errorf("invalid --from %q (want Viewer, Editor, or Admin, or \"\" for any)", fromFlag)
			}
		}

		base := grafanaRenderBaseURL(baseFlag)
		if base == "" {
			return errNoGrafanaBase
		}

		users, err := listGrafanaOrgUsers(cmd.Context(), base)
		if err != nil {
			return err
		}

		names := newUserSelector(logins, emails)
		targets := selectBumpTargets(users, from, to, names)

		if err := names.unmatchedErr(); err != nil {
			return err
		}

		results := make([]map[string]any, 0, len(targets))
		bumped := 0
		for _, u := range targets {
			res := map[string]any{
				"user_id": u.UserID,
				"login":   u.Login,
				"email":   u.Email,
				"from":    u.Role,
				"to":      to,
			}
			if dryRun {
				res["action"] = "would-bump"
				results = append(results, res)
				continue
			}
			if err := setGrafanaOrgUserRole(cmd.Context(), base, u.UserID, to); err != nil {
				return fmt.Errorf("bumping %s (userId=%d): %w", userLabel(u), u.UserID, err)
			}
			res["action"] = "bumped"
			results = append(results, res)
			bumped++
		}

		if jsonOutput {
			printJSON(map[string]any{
				"to":      to,
				"from":    from,
				"dry_run": dryRun,
				"count":   len(targets),
				"bumped":  bumped,
				"results": results,
			})
			return nil
		}

		if len(targets) == 0 {
			fmt.Fprintf(os.Stderr, "no users to bump — everyone selected is already %s\n", to)
			return nil
		}
		for _, u := range targets {
			verb := "bumped"
			if dryRun {
				verb = "would bump"
			}
			fmt.Printf("%s %s: %s → %s\n", verb, userLabel(u), u.Role, to)
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "dry-run: %d user(s) would be bumped to %s\n", len(targets), to)
		} else {
			fmt.Fprintf(os.Stderr, "bumped %d user(s) to %s\n", bumped, to)
		}
		return nil
	},
}

// errNoGrafanaBase is the shared "no base URL" error for the users subcommands.
var errNoGrafanaBase = fmt.Errorf("no Grafana base URL: set GRAFANA_URL (in-cluster) or GRAFANA_BASE_URL (public), or pass --grafana-url")

// userLabel picks a human-readable label for a user: login if present, else
// email, else the numeric id. Keeps output readable regardless of which fields
// Grafana populated.
func userLabel(u grafanaOrgUser) string {
	if s := strings.TrimSpace(u.Login); s != "" {
		return s
	}
	if s := strings.TrimSpace(u.Email); s != "" {
		return s
	}
	return fmt.Sprintf("userId=%d", u.UserID)
}

// filterUsersByRole returns the subset of users whose role matches (case-insensitively).
func filterUsersByRole(users []grafanaOrgUser, role string) []grafanaOrgUser {
	out := make([]grafanaOrgUser, 0, len(users))
	for _, u := range users {
		if strings.EqualFold(u.Role, role) {
			out = append(out, u)
		}
	}
	return out
}

// userSelector matches users by login or email (case-insensitive) and tracks
// which requested names were never seen, so a typo'd --login fails loudly
// instead of silently bumping nobody.
type userSelector struct {
	logins map[string]bool // lower-cased → matched?
	emails map[string]bool
	byName bool
}

func newUserSelector(logins, emails []string) *userSelector {
	s := &userSelector{logins: map[string]bool{}, emails: map[string]bool{}}
	for _, l := range logins {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			s.logins[l] = false
			s.byName = true
		}
	}
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			s.emails[e] = false
			s.byName = true
		}
	}
	return s
}

// matches reports whether u is selected by name. When no names were given every
// user matches (selection falls to the role filter instead). Matching a user
// marks its name(s) seen for unmatchedErr.
func (s *userSelector) matches(u grafanaOrgUser) bool {
	if !s.byName {
		return true
	}
	hit := false
	if l := strings.ToLower(strings.TrimSpace(u.Login)); l != "" {
		if _, ok := s.logins[l]; ok {
			s.logins[l] = true
			hit = true
		}
	}
	if e := strings.ToLower(strings.TrimSpace(u.Email)); e != "" {
		if _, ok := s.emails[e]; ok {
			s.emails[e] = true
			hit = true
		}
	}
	return hit
}

// unmatchedErr returns a non-nil error naming any --login/--email that did not
// match any org user, so a typo does not silently bump nothing.
func (s *userSelector) unmatchedErr() error {
	var missing []string
	for l, seen := range s.logins {
		if !seen {
			missing = append(missing, "login="+l)
		}
	}
	for e, seen := range s.emails {
		if !seen {
			missing = append(missing, "email="+e)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("no Grafana user matched: %s", strings.Join(missing, ", "))
}

// selectBumpTargets picks the users to bump: those matched by the name selector,
// filtered to role==from (when from is non-empty), and excluding anyone already
// at the target role (idempotency).
func selectBumpTargets(users []grafanaOrgUser, from, to string, sel *userSelector) []grafanaOrgUser {
	out := make([]grafanaOrgUser, 0, len(users))
	for _, u := range users {
		if !sel.matches(u) {
			continue
		}
		if from != "" && !strings.EqualFold(u.Role, from) {
			continue
		}
		if strings.EqualFold(u.Role, to) {
			continue // already there — idempotent skip
		}
		out = append(out, u)
	}
	return out
}

// listGrafanaOrgUsers GETs /api/org/users and decodes the org-user list.
func listGrafanaOrgUsers(ctx context.Context, base string) ([]grafanaOrgUser, error) {
	reqCtx, cancel := context.WithTimeout(ctx, grafanaUsersTimeout)
	defer cancel()

	url := strings.TrimRight(base, "/") + "/api/org/users"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building org-users request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	applyGrafanaAuth(req)

	resp, err := grafanaUsersHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("grafana org-users list: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("grafana org-users list: status %d (need an Org Admin caller — set GRAFANA_TOKEN to an Admin service-account token): %s", resp.StatusCode, snippetBody(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grafana org-users list: status %d: %s", resp.StatusCode, snippetBody(body))
	}
	var users []grafanaOrgUser
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, fmt.Errorf("grafana org-users list: decode: %w", err)
	}
	return users, nil
}

// setGrafanaOrgUserRole PATCHes /api/org/users/<userId> with the new role.
func setGrafanaOrgUserRole(ctx context.Context, base string, userID int, role string) error {
	reqCtx, cancel := context.WithTimeout(ctx, grafanaUsersTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/api/org/users/%d", strings.TrimRight(base, "/"), userID)
	payload, err := json.Marshal(map[string]string{"role": role})
	if err != nil {
		return fmt.Errorf("encoding role payload: %w", err)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building role PATCH: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyGrafanaAuth(req)

	resp, err := grafanaUsersHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("grafana role PATCH: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("status %d (need an Org Admin caller — set GRAFANA_TOKEN to an Admin service-account token): %s", resp.StatusCode, snippetBody(body))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, snippetBody(body))
	}
	return nil
}

func init() {
	grafanaCmd.AddCommand(grafanaUsersCmd)
	grafanaUsersCmd.AddCommand(grafanaUsersListCmd)
	grafanaUsersCmd.AddCommand(grafanaUsersBumpCmd)

	grafanaUsersListCmd.Flags().String("grafana-url", "", "override Grafana base URL (default: GRAFANA_URL or GRAFANA_BASE_URL)")
	grafanaUsersListCmd.Flags().String("role", "", "only list users with this org role (Viewer|Editor|Admin)")

	grafanaUsersBumpCmd.Flags().String("grafana-url", "", "override Grafana base URL (default: GRAFANA_URL or GRAFANA_BASE_URL)")
	grafanaUsersBumpCmd.Flags().String("to", "Admin", "target org role to bump users to (Viewer|Editor|Admin)")
	grafanaUsersBumpCmd.Flags().String("from", "Viewer", "only bump users currently at this role; \"\" bumps every user regardless of current role")
	grafanaUsersBumpCmd.Flags().StringArray("login", nil, "bump only this login (repeatable); default: all matching --from")
	grafanaUsersBumpCmd.Flags().StringArray("email", nil, "bump only this email (repeatable); default: all matching --from")
	grafanaUsersBumpCmd.Flags().Bool("dry-run", false, "print which users would be bumped without writing anything")
}
