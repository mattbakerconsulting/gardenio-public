package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// escalationApproveCapability is the capability a viewer's type=user bead must
// carry to approve or reject an escalation from the bead page. It is the SAME
// capability string the bridge's escalation SPA enforces
// (internal/bridge/api_escalations.go), so the two approval surfaces cannot
// diverge on who is allowed to act.
const escalationApproveCapability = "approve:escalation"

// maxEscalationReasonLen bounds the free-text rejection reason and the
// approval's optional comment (the daemon's maxApprovalCommentLen).
const maxEscalationReasonLen = 4000

// userCapsTTL is how long the email → capabilities index is reused before it
// is refetched. Short enough that granting someone approve:escalation takes
// effect within a minute; long enough that a page render is not one 500-bead
// list per request.
const userCapsTTL = 60 * time.Second

// userCapsCache memoizes the normalized-email → capabilities index built from
// type=user kind=config beads. Concurrency-safe; a stale-but-valid snapshot is
// preferred over failing the render when the daemon errors.
type userCapsCache struct {
	mu       sync.RWMutex
	byEmail  map[string][]string
	fetched  time.Time
	hasValue bool
}

// get returns the cached index when it is younger than userCapsTTL.
func (c *userCapsCache) get(now time.Time) (map[string][]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.hasValue || now.Sub(c.fetched) > userCapsTTL {
		return nil, false
	}
	return c.byEmail, true
}

// put stores a freshly-built index.
func (c *userCapsCache) put(idx map[string][]string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byEmail = idx
	c.fetched = now
	c.hasValue = true
}

// stale returns the last-known index regardless of age. Used when the daemon
// fetch fails so an approval button does not vanish on a transient blip.
func (c *userCapsCache) stale() (map[string][]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byEmail, c.hasValue
}

// viewerCapabilities returns the capabilities recorded on the type=user bead
// whose `email` field matches the authenticated viewer. Returns nil when the
// viewer is unauthenticated or has no user bead — both of which mean "cannot
// approve", but the caller distinguishes them for the blocked-reason copy.
func (s *Server) viewerCapabilities(ctx context.Context, email string) []string {
	email = normalizeProfileEmail(email)
	if email == "" {
		return nil
	}
	now := s.now()
	idx, ok := s.userCaps.get(now)
	if !ok {
		built, err := s.buildUserCapsIndex(ctx)
		if err != nil {
			// Degrade to the last-known index rather than silently reporting
			// "no capability" — a daemon blip must not look like a revoked
			// grant.
			if prev, had := s.userCaps.stale(); had {
				idx = prev
			} else {
				s.logger.Warn("building user capability index", "error", err)
				return nil
			}
		} else {
			s.userCaps.put(built, now)
			idx = built
		}
	}
	return idx[email]
}

// buildUserCapsIndex reads type=user kind=config beads and indexes their
// `capabilities` JSON array by normalized email — the same bead shape and the
// same field the bridge's UserDirectory reads.
func (s *Server) buildUserCapsIndex(ctx context.Context) (map[string][]string, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"user"},
		Kinds: []string{"config"},
		// A closed user bead is a removed person: it grants nothing, and must
		// not make a live bead's email look duplicated.
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	idx := make(map[string][]string)
	if res == nil {
		return idx, nil
	}
	ambiguous := make(map[string]bool)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		key := normalizeProfileEmail(b.Fields["email"])
		if key == "" {
			continue
		}
		var caps []string
		if raw := strings.TrimSpace(b.Fields["capabilities"]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &caps); err != nil {
				continue
			}
		}
		if _, dup := idx[key]; dup || ambiguous[key] {
			// Two open user beads share one email: unioning their capabilities
			// would let a second bead add grants to the first, and picking one
			// depends on daemon list order. The email grants nothing until an
			// admin closes the duplicate (fail closed, gd-W4FkF80BlX).
			if !ambiguous[key] {
				s.logger.Error("duplicate open user beads share an email; it grants no capabilities until one is closed",
					"email", key, "other_bead", b.ID)
			}
			ambiguous[key] = true
			delete(idx, key)
			continue
		}
		idx[key] = caps
	}
	return idx, nil
}

// viewerCanApproveEscalation reports whether the authenticated viewer may
// approve/reject escalations, and returns operator-facing copy explaining a
// "no". Capability matching reuses bridge.UserIdentity.HasCapability so the
// wildcard semantics ("approve:*") are identical to the SPA's.
func (s *Server) viewerCanApproveEscalation(ctx context.Context, email string) (bool, string) {
	if strings.TrimSpace(email) == "" {
		return false, "Sign in to approve or reject this escalation."
	}
	caps := s.viewerCapabilities(ctx, email)
	u := bridge.UserIdentity{Capabilities: caps}
	if !u.HasCapability(escalationApproveCapability) {
		return false, "Your user bead does not carry the " + escalationApproveCapability + " capability."
	}
	return true, ""
}

// escalationApproveGate decides whether the viewer is offered Approve/Reject on
// one escalation, and why not. It asks the daemon (can-approve: lanes,
// operator_only, self-approval), so the buttons and the POST agree with what
// the daemon's approve will accept; the viewer keeps no list of approvers.
//
// Fails closed: a daemon error hides the buttons with a reason. The one
// exception is a 404 from a daemon rolled before can-approve existed: then the
// legacy approve:escalation capability check applies, which is what that
// daemon's approve endpoint expects. Remove the fallback once every daemon
// serves can-approve (escalation routing Task 14).
func (s *Server) escalationApproveGate(ctx context.Context, id, email string) (bool, string) {
	if strings.TrimSpace(email) == "" {
		return false, "Sign in to approve or reject this escalation."
	}
	res, err := s.daemon.CanApproveEscalation(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return s.viewerCanApproveEscalation(ctx, email)
		}
		s.logger.Warn("can-approve check failed; hiding escalation actions", "id", id, "error", err)
		return false, "Couldn't check your approval rights with the daemon."
	}
	if !res.Allowed {
		if reason := strings.TrimSpace(res.Reason); reason != "" {
			return false, "You can't approve this escalation: " + reason
		}
		return false, "You are not an approver for this escalation."
	}
	return true, ""
}

// handleApproveEscalation handles POST /b/{id}/escalation/approve. It approves
// via the daemon's native /v1/escalations/{id}/approve, which records the
// authorization trail (approved_by / approval_method / audit record) and flips
// the bead open -> in_progress.
//
// Auto-replay is limited: the daemon's replayEscalation
// (kbeads/internal/server/http_escalations.go) implements exactly three
// operation_type arms — bead create, update, and close — and only a successful
// bead-op replay auto-closes the escalation. Every OTHER operation_type
// (gitlab_mr_merge — the large majority of escalations — plus
// gardenio_user_capability and every IAM/deploy/terraform type) hits the default
// arm, is WARN-logged as "unknown operation type", and is left in_progress for a
// human to execute by hand. So "approve" here means "authorization recorded";
// for a non-bead operation the thing the agent asked for (the merge, the IAM
// grant) does NOT auto-happen. Escalation ka-0000002462 set that honest
// expectation; epics ka-0000004327 / ka-0000000455 would make full auto-replay
// true.
func (s *Server) handleApproveEscalation(w http.ResponseWriter, r *http.Request) {
	s.escalationAction(w, r, false)
}

// handleRejectEscalation handles POST /b/{id}/escalation/reject.
func (s *Server) handleRejectEscalation(w http.ResponseWriter, r *http.Request) {
	s.escalationAction(w, r, true)
}

// escalationAction is the shared approve/reject path: authenticate, re-read the
// bead to catch an already-resolved race, ask the daemon's can-approve
// (escalationApproveGate), call the daemon, then re-render the card partial so htmx can swap it in place.
func (s *Server) escalationAction(w http.ResponseWriter, r *http.Request, reject bool) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense-in-depth: the OIDC middleware should already block this.
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	reason, comment, tooLong := escalationNotes(r.PostForm, reject)
	if tooLong != "" {
		s.renderEscalationError(w, r, id, email, tooLong, http.StatusRequestEntityTooLarge)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for escalation action", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if bead.Type != "escalation" {
		http.Error(w, "Bead is not an escalation", http.StatusBadRequest)
		return
	}
	if bead.Status != "open" && bead.Status != "in_progress" {
		s.renderEscalationError(w, r, id, email, "This escalation is already "+bead.Status+".", http.StatusConflict)
		return
	}
	if ok, why := s.escalationApproveGate(r.Context(), id, email); !ok {
		s.renderEscalationError(w, r, id, email, why, http.StatusForbidden)
		return
	}

	if reject {
		if reason == "" {
			reason = "Rejected from the bead page"
		}
		err = s.daemon.RejectEscalation(r.Context(), id, beadsapi.RejectEscalationRequest{
			RejectedBy: email,
			Reason:     reason,
		})
	} else {
		err = s.daemon.ApproveEscalation(r.Context(), id, beadsapi.ApproveEscalationRequest{
			ApprovedBy:     email,
			ApprovalMethod: "web_ui",
			Comment:        comment,
		})
	}
	if err != nil {
		verb := "approve"
		if reject {
			verb = "reject"
		}
		// A daemon refusal is an answer, not an outage: surface it as the same
		// 403 the can-approve gate above returns, with the daemon's reason, so
		// the card says why instead of a generic 502 (gd-WJ09iJNvFM).
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
			s.logger.Warn("escalation action refused by daemon", "id", id, "verb", verb, "actor", email, "error", err)
			s.renderEscalationError(w, r, id, email, escalationRefusalMessage(verb, apiErr), http.StatusForbidden)
			return
		}
		s.logger.Error("escalation action failed", "id", id, "verb", verb, "actor", email, "error", err)
		s.renderEscalationError(w, r, id, email, "Failed to "+verb+" this escalation.", http.StatusBadGateway)
		return
	}
	s.logger.Info("escalation resolved from bead page", "id", id, "reject", reject, "actor", email)

	// Re-fetch so the approval trail the daemon merged in (approved_by,
	// approved_at, and any replay result) flows into the swapped-in card.
	updated, ferr := s.daemon.GetBead(r.Context(), id)
	if ferr != nil {
		s.logger.Warn("re-fetching escalation after action", "id", id, "error", ferr)
		updated = bead
	}
	s.renderEscalationPartial(w, r, updated, email, "", http.StatusOK)
}

// escalationNotes reads the form's reason= (a rejection's) and comment=, an
// approval's optional note ("approve with comment", gd-yuOG6cX6VI) from the
// garden's answer box; on approve the bead card's reason= box, which has
// always offered a note, is the comment when no comment= is sent. tooLong is
// the card's error when either passes maxEscalationReasonLen, else "".
func escalationNotes(form url.Values, reject bool) (reason, comment, tooLong string) {
	reason = strings.TrimSpace(form.Get("reason"))
	comment = strings.TrimSpace(form.Get("comment"))
	if comment == "" && !reject {
		comment = reason
	}
	switch {
	case len(reason) > maxEscalationReasonLen:
		tooLong = "Reason is too long (max 4000 characters)."
	case len(comment) > maxEscalationReasonLen:
		tooLong = "Comment is too long (max 4000 characters)."
	}
	return reason, comment, tooLong
}

// escalationRefusalMessage is the card copy for a daemon 401/403 on approve or
// reject. The daemon's own reason (e.g. "four-eyes required: ...") is shown
// verbatim; an empty one falls back to a generic line.
func escalationRefusalMessage(verb string, apiErr *beadsapi.APIError) string {
	msg := "The daemon refused this " + verb + "."
	if reason := strings.TrimSpace(apiErr.Message); reason != "" {
		msg += " " + reason
		if !strings.HasSuffix(reason, ".") {
			msg += "."
		}
	}
	return msg
}

// renderEscalationError re-renders the card with an inline error banner. The
// bead is re-read so the swapped card still shows current state; a read
// failure degrades to a plain-text error.
func (s *Server) renderEscalationError(w http.ResponseWriter, r *http.Request, id, email, msg string, status int) {
	if respondAction(w, r, id, actionFailure(msg, status)) {
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil || bead == nil {
		http.Error(w, msg, status)
		return
	}
	s.renderEscalationPartial(w, r, bead, email, msg, status)
}

// renderEscalationPartial renders templates/partials/type_escalation.html as a
// standalone fragment so the client can replace the card in place.
func (s *Server) renderEscalationPartial(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email, errMsg string, status int) {
	ev := buildEscalationView(bead, s.md, s.basePath, s.now())
	if ev != nil {
		ev.Error = errMsg
		if ev.Open {
			ev.CanApprove, ev.ApproveBlockedReason = s.escalationApproveGate(r.Context(), ev.BeadID, email)
		}
		ev.CanRoute = viewerMayRouteEscalation(r) && ev.Open && !ev.AudienceForced
		ev.Origin = s.resolveRequestOrigin(r.Context(), bead)
		s.fillLaneOffline(r.Context(), ev)
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/type_escalation.html", "type_escalation", map[string]any{
		"Escalation": ev,
		"Bead":       bead,
		"BasePath":   s.basePath,
	})
}
