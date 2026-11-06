package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Open-PR close guard (gd-lIKFejVISw, plan gd-3F4W22h2nT).
//
// A task or bug whose mr_url names a pull request that is still open must not
// be closed by hand: the ledger would say the work landed while its code is off
// main, and the PR drops off every queue that walks open tasks (the merge desk,
// the stale reaper, gb news). That is how the fleet leaked ~90 open PRs. The
// epic auto-close already defers on unmerged MR work (firstUnmergedMR in
// auto_close.go); this applies the same rule to a direct close.
//
// The guard runs on the three generic close surfaces a caller can reach: POST
// /v1/beads/{id}/close (handleCloseBead), the gRPC CloseBead, and a status →
// closed PATCH (updateBead). The daemon's own transitions call store.CloseBead
// directly and never meet it: the close-on-execute release of a merge
// escalation's requesting task (closeEscalationWrapper), close-on-merge
// (autoCloseMergedLeaf) and an approved close replay all still close.
//
// It fires only on POSITIVE evidence the PR is open. A lookup error, an unknown
// URL or a missing merge-request bead lets the close through, matching kd's
// client-side guard (cmd/kd/close.go beadHasOpenMR): the guard never blocks a
// close on its own read failure.

// PRStateLookup reports a pull request's state as "open", "closed" or
// "merged" — the contract of the controller stale reaper's GitHub reader
// (statusreporter.PRStateLookup, gd-ovzZSuuHny). An error, or any other
// string, means the state is unknown.
type PRStateLookup func(ctx context.Context, prURL string) (string, error)

// SetPRStateLookup replaces the reader the open-PR close guard asks. nil
// restores the default, the github-bridge's merge-request bead
// (projectedPRState).
func (s *BeadsServer) SetPRStateLookup(l PRStateLookup) {
	s.prStateLookup = l
}

// openPRCloseGuardTypes are the bead types the guard covers: the work beads a
// crew claims, links a PR to and closes.
var openPRCloseGuardTypes = map[model.BeadType]bool{
	model.TypeTask: true,
	model.TypeBug:  true,
}

// forceCloseHeader is how `kd close --force` asks to close over an open PR.
// Only an operator's sign-in may use it (callerMayForceCloseOverOpenPR); from
// an agent token it is ignored and the guard refuses as usual.
const forceCloseHeader = "X-Force-Close"

type forceCloseKey struct{}

// withForceClose marks ctx as carrying a forced close.
func withForceClose(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceCloseKey{}, true)
}

func forceCloseFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(forceCloseKey{}).(bool)
	return v
}

// parseForceCloseHeader accepts the same truthy set as the other override
// headers (parseForceParentEdgeHeader).
func parseForceCloseHeader(raw string) bool {
	return parseForceParentEdgeHeader(raw)
}

// callerMayForceCloseOverOpenPR reports whether the caller may override the
// guard. The audience is the one that may re-route an escalation: a
// gardenio-operator or admin on their own sign-in, the daemon's machine
// writes, and auth-disabled mode — never a static or fleet token, which every
// agent pod holds.
func callerMayForceCloseOverOpenPR(ctx context.Context) bool {
	return callerMayRouteEscalation(ctx)
}

// openPRCloseError is the refusal. Its text names the PR and the two ways out.
type openPRCloseError struct {
	id    string
	prURL string
}

func (e *openPRCloseError) Error() string {
	return fmt.Sprintf("refusing to close %s: its pull request %s is still open, and a closed task "+
		"would say the work landed while its code is off main. Either:\n"+
		"  - have the merge close it: gb pr ready (or gb escalation create --operation-type gitlab_mr_merge "+
		"--requesting-task %s --close-on-execute), then gb stop; the merge closes %s; or\n"+
		"  - close the PR first if the work is abandoned: gh pr close %s --comment \"<why>\", then kd close %s.\n"+
		"An operator can override with kd close --force.",
		e.id, e.prURL, e.id, e.id, e.prURL, e.id)
}

// openPRCloseRefusal returns the refusal for closing bead, or nil when the
// close may go ahead: the bead is not a task or bug, is already closed, links
// no PR, its PR is not positively open, or an operator forced the close.
func (s *BeadsServer) openPRCloseRefusal(ctx context.Context, bead *model.Bead) *openPRCloseError {
	if bead == nil || bead.Status == model.StatusClosed || !openPRCloseGuardTypes[bead.Type] {
		return nil
	}
	prURL := strings.TrimSpace(parseFieldsToStringMap(bead.Fields)["mr_url"])
	if prURL == "" {
		return nil
	}
	lookup := s.prStateLookup
	if lookup == nil {
		lookup = s.projectedPRState
	}
	state, err := lookup(ctx, prURL)
	if err != nil {
		slog.Warn("open-PR close guard: PR state unknown; allowing the close",
			"bead", bead.ID, "pr", prURL, "err", err.Error(), "task", "gd-lIKFejVISw")
		return nil
	}
	if state != "open" {
		return nil
	}
	if forceCloseFromContext(ctx) && callerMayForceCloseOverOpenPR(ctx) {
		slog.Info("open-PR close guard: operator forced the close",
			"bead", bead.ID, "pr", prURL, "actor", actorFromContext(ctx), "task", "gd-lIKFejVISw")
		return nil
	}
	return &openPRCloseError{id: bead.ID, prURL: prURL}
}

// projectedPRState is the default PRStateLookup. It reads the canonical
// type=merge-request bead the github-bridge keeps for the URL from GitHub's
// pull_request webhooks (mr_state opened / closed / merged, GitLab's words),
// the same projection firstUnmergedMR trusts. The daemon holds no GitHub
// credential, so this is its view of what GitHub reports. No merge-request
// bead yet means unknown.
func (s *BeadsServer) projectedPRState(ctx context.Context, prURL string) (string, error) {
	mr, err := s.mrBeadForURL(ctx, prURL)
	if err != nil {
		return "", err
	}
	if mr == nil {
		return "", fmt.Errorf("no merge-request bead for %s", prURL)
	}
	f := parseFieldsToStringMap(mr.Fields)
	switch {
	case isMRMerged(f):
		return "merged", nil
	case strings.EqualFold(strings.TrimSpace(f["mr_state"]), "opened"):
		return "open", nil
	case strings.EqualFold(strings.TrimSpace(f["mr_state"]), "closed"):
		return "closed", nil
	}
	return "", fmt.Errorf("merge-request bead %s has mr_state %q", mr.ID, f["mr_state"])
}
