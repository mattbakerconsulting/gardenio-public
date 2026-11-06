package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Meadow adoption (gd-6HQyB0jgmO, Homestead World phase 3): an operator takes a loose bead from the meadow into a plan,
// or into one of its phases, from the in-garden bead view (web/adopt.js). It is one parent-child edge from the bead to
// the plan or phase, the same edge `gb plan add-step` and the plan page's "Add step" write, so the plan's gardener hears
// about its new step through the one PlanStepReactor. The route is the viewer's to gate: beads-viewer registers Adopt
// behind its operator role and classes it admin-only, so a scoped principal never reaches it (route_classes.go). The
// write goes on the request context, which carries the operator's own bearer and X-Operated-By, so the daemon checks and
// audits it as the operator. The route takes a write only from this page's own origin (crossOrigin): the operator gate
// and the bearer say who, not which page asked.

// BeadLinker is the slice of the daemon client adoption writes with.
type BeadLinker interface {
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

const adoptTimeout = 10 * time.Second

// adoptResult is the JSON body of every answer from POST /garden/bead/{id}/adopt. Error is the sentence the bead view
// shows; Parent is the bead's plan or phase when the refusal is that it already has one.
type adoptResult struct {
	OK      bool   `json:"ok"`
	ID      string `json:"id,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Already bool   `json:"already,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Error   string `json:"error,omitempty"`
}

type adoptError struct {
	code   int
	msg    string
	parent string
}

func (e *adoptError) Error() string { return e.msg }

// Adopt serves POST /garden/bead/{id}/adopt with the form fields plan (required) and phase (optional, one of the plan's
// phase_order). It answers 200 when the bead is now a step of the plan or phase, including when it already was (Already),
// and refuses, without writing, a target that is not an open plan or one of its open phases, a bead that is not open
// loose work, a bead in another project than the plan, and a bead that already sits under a plan, phase or other epic.
// A non-epic parent (a Slack thread bead, say) is provenance, not structure, and leaves the bead in the meadow, so it does
// not stop adoption.
func (h *Handler) Adopt(w http.ResponseWriter, r *http.Request) {
	if crossOrigin(r) {
		writeAdopt(w, http.StatusForbidden, adoptResult{Error: "Adopt from the garden page itself."})
		return
	}
	if h.Beads == nil || h.Links == nil {
		writeAdopt(w, http.StatusServiceUnavailable, adoptResult{Error: "Adoption is not enabled here."})
		return
	}
	me := ""
	if h.Viewer != nil {
		me = h.Viewer(r)
	}
	ctx := r.Context()
	if me == "" {
		me = beadsapi.OperatorFromContext(ctx)
	}
	if me == "" {
		writeAdopt(w, http.StatusUnauthorized, adoptResult{Error: "Your session ended. Reload the page to sign in again."})
		return
	}
	if beadsapi.OperatorFromContext(ctx) == "" {
		ctx = beadsapi.WithOperator(ctx, me)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		writeAdopt(w, http.StatusBadRequest, adoptResult{Error: "The form was rejected."})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	plan := strings.TrimSpace(r.PostForm.Get("plan"))
	phase := strings.TrimSpace(r.PostForm.Get("phase"))
	if id == "" || len(id) > 200 || plan == "" || len(plan) > 200 || len(phase) > 200 {
		writeAdopt(w, http.StatusBadRequest, adoptResult{Error: "Say which bead and which plan."})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, adoptTimeout)
	defer cancel()

	res := adoptResult{ID: id, Plan: plan, Phase: phase}
	already, err := adoptCheck(ctx, h.Beads, id, plan, phase)
	if err == nil && !already {
		target := plan
		if phase != "" {
			target = phase
		}
		err = h.Links.AddDependency(ctx, id, target, "parent-child", me)
		if err != nil {
			var ae *beadsapi.APIError
			if errors.As(err, &ae) && ae.StatusCode == http.StatusForbidden {
				err = &adoptError{code: http.StatusForbidden, msg: "The beads daemon did not let you link " + id + "."}
			} else {
				err = &adoptError{code: http.StatusBadGateway, msg: "The beads daemon did not take the link. Try again."}
			}
		} else if h.Farm != nil {
			h.Farm.Invalidate() // the flower leaves the meadow on the next refresh, not the next timer
		}
	}
	if err != nil {
		ae := &adoptError{code: http.StatusBadGateway, msg: "The beads daemon did not answer. Try again."}
		errors.As(err, &ae)
		res.Error, res.Parent = ae.msg, ae.parent
		writeAdopt(w, ae.code, res)
		return
	}
	res.OK, res.Already = true, already
	writeAdopt(w, http.StatusOK, res)
}

// adoptCheck reads the bead, the plan, the phase and the bead's parents, and says whether the edge is already there
// (true) or may be written (false), or why not (an *adoptError).
func adoptCheck(ctx context.Context, src BeadReader, id, plan, phase string) (bool, error) {
	b, err := readOne(ctx, src, id, "There is no bead "+id+", or you cannot see it.")
	if err != nil {
		return false, err
	}
	if !meadowTypes[b.Type] {
		return false, &adoptError{code: http.StatusConflict, msg: "Only a task, feature, bug or chore can join a plan; " + id + " is " + article(b.Type) + "."}
	}
	if b.Status == "closed" {
		return false, &adoptError{code: http.StatusConflict, msg: id + " is closed."}
	}
	p, err := readOne(ctx, src, plan, "There is no plan "+plan+", or you cannot see it.")
	if err != nil {
		return false, err
	}
	if p.Type != "epic" || !hasLabel(p, "plan") {
		return false, &adoptError{code: http.StatusBadRequest, msg: plan + " is not a plan."}
	}
	if p.Status == "closed" {
		return false, &adoptError{code: http.StatusConflict, msg: "The plan " + plan + " is closed."}
	}
	// No project label is no project, not every project: the check refuses it rather than letting it through.
	switch bp, pp := projectOf(b), projectOf(p); {
	case bp == "":
		return false, &adoptError{code: http.StatusConflict, msg: id + " has no project, so it cannot join a plan."}
	case pp == "":
		return false, &adoptError{code: http.StatusConflict, msg: "The plan " + plan + " has no project."}
	case bp != pp:
		return false, &adoptError{code: http.StatusConflict, msg: id + " belongs to " + bp + " and the plan to " + pp + "."}
	}
	target := plan
	if phase != "" {
		if !slices.Contains(phaseOrderOf(p), phase) {
			return false, &adoptError{code: http.StatusBadRequest, msg: phase + " is not a phase of " + plan + "."}
		}
		ph, err := readOne(ctx, src, phase, "There is no phase "+phase+", or you cannot see it.")
		if err != nil {
			return false, err
		}
		if ph.Type != "epic" {
			return false, &adoptError{code: http.StatusBadRequest, msg: phase + " is not a phase of " + plan + "."}
		}
		if ph.Status == "closed" {
			return false, &adoptError{code: http.StatusConflict, msg: "The phase " + phase + " is closed."}
		}
		target = phase
	}

	deps, err := src.GetDependencies(ctx, id)
	if err != nil {
		return false, &adoptError{code: http.StatusBadGateway, msg: "The beads daemon did not send " + id + "'s links. Try again."}
	}
	for _, d := range deps {
		if d.Type != "parent-child" || d.DependsOnID == "" {
			continue
		}
		if d.DependsOnID == target {
			return true, nil
		}
	}
	for _, d := range deps {
		if d.Type != "parent-child" || d.DependsOnID == "" {
			continue
		}
		// A parent we cannot read is refused, not ignored: it may be a plan this viewer cannot see.
		pb, err := src.GetBead(ctx, d.DependsOnID)
		if err != nil || pb == nil || pb.Type == "epic" {
			return false, &adoptError{code: http.StatusConflict, msg: id + " is already part of " + d.DependsOnID + ".", parent: d.DependsOnID}
		}
	}
	return false, nil
}

// crossOrigin reports a write another page sent. The edge session cookie is SameSite=Lax, which keeps a cross-site page
// out but not a same-site one, so without this any *.clonk.tv page could re-parent a bead on the operator's session.
// A browser says where a request came from: Sec-Fetch-Site must be same-origin when it is sent, and an Origin must name
// the host the request came in on (directly or through the ingress). A request with neither header is not a browser's
// (curl, a test) and passes; the operator gate and the daemon still decide it.
func crossOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true // "null" (a sandboxed frame, a file: page) or junk
	}
	if strings.EqualFold(u.Host, r.Host) {
		return false
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	fwd = strings.TrimSpace(fwd)
	return fwd == "" || !strings.EqualFold(u.Host, fwd)
}

// readOne reads one bead, turning "not there" and "not yours" into the same 404 the bead view gives.
func readOne(ctx context.Context, src BeadReader, id, missing string) (*beadsapi.BeadDetail, error) {
	b, err := src.GetBead(ctx, id)
	if err != nil {
		var ae *beadsapi.APIError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
			return nil, &adoptError{code: http.StatusNotFound, msg: missing}
		}
		return nil, err
	}
	if b == nil {
		return nil, &adoptError{code: http.StatusNotFound, msg: missing}
	}
	return b, nil
}

func article(typ string) string {
	switch {
	case typ == "":
		return "not work"
	case strings.ContainsRune("aeiou", rune(typ[0])):
		return "an " + typ
	default:
		return "a " + typ
	}
}

func writeAdopt(w http.ResponseWriter, code int, res adoptResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}
