package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
)

// heraldry.go is the daemon side of a bead's arms (heraldry spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §2 R5-R7,
// R11, §3.1; steps 1.1 gd-pTtoEKamiI and 5.3 gd-InkEAAqGNM): the value is
// parsed and stored as the run (three to five emoji in order), allowed only
// on the bead kinds R6 names, and unique within a project (the register).
// createBead and updateBead call it, which covers HTTP,
// gRPC, escalation replay, formula pour and undo; the label endpoints re-check
// a project move. Every message uses plain words: the canonical form and the
// heraldic terms never reach a person.

// heraldryTypes are the bead types that may bear arms in v1 (R6, narrowed in
// round 1): epics, which plans and phases are, and projects. Arms are for the
// beads that last; tasks, decisions, escalations and agents may come later.
var heraldryTypes = map[model.BeadType]bool{
	model.TypeEpic:    true,
	model.TypeProject: true,
}

// heraldryNone is the value that removes arms (R7, `kd update --heraldry none`).
const heraldryNone = "none"

// heraldryRegisterIndex is migration 057's unique index; a 23505 naming it is
// a register collision that lost the race past the pre-write check.
const heraldryRegisterIndex = "idx_beads_heraldry_register"

// heraldryRegisterLimit bounds the register read. A project's register is the
// beads that were given arms by hand, so it is small; the bound only keeps a
// runaway from reading the whole table.
const heraldryRegisterLimit = 5000

// setHeraldryFilter applies the heraldry list filter (HTTP ?heraldry=, gRPC
// ListBeadsRequest.heraldry) to filter. The value is normalised to the run,
// which matches the beads bearing it, also those still stored in the shield
// form. "" sets nothing.
func setHeraldryFilter(filter *model.BeadFilter, raw string) error {
	if raw == "" {
		return nil
	}
	canonical, err := normalizeHeraldry(raw)
	if err != nil {
		return err
	}
	filter.Heraldry = canonical
	return nil
}

// normalizeHeraldry turns arms as given (the run, or an old shield form) into
// the stored form, the run. "" and "none" mean no arms and return "".
func normalizeHeraldry(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, heraldryNone) {
		return "", nil
	}
	arms, err := heraldry.Parse(raw)
	if err != nil {
		return "", inputError("invalid arms: " + err.Error() + ". " + model.HeraldryHelp)
	}
	return heraldry.Canonical(arms), nil
}

// heraldryConflictError is a register collision (R5): the arms are already
// borne by another bead of the project. It is a 409 on HTTP and AlreadyExists
// on gRPC, and names the bead that bears them.
type heraldryConflictError struct {
	Run        string // the arms as an emoji run, the only form a person sees
	Project    string
	HolderID   string // "" when the race backstop could not re-read the holder
	ShortTitle string
}

// heraldryConflictCode is the stable machine-readable code on the 409 body.
const heraldryConflictCode = "heraldry_taken"

func (e *heraldryConflictError) Error() string {
	holder := "another bead"
	if e.HolderID != "" {
		holder = e.HolderID
		if e.ShortTitle != "" {
			holder += " (" + e.ShortTitle + ")"
		}
	}
	return fmt.Sprintf("the arms %s are already borne by %s in project %s; arms are unique in a project, so choose other emoji or another order",
		e.Run, holder, e.Project)
}

// heraldryProject is the project whose register a bead's arms belong to: its
// one project:<x> label. A bead with no project label, or with more than one,
// has no single register and may not bear arms (R5).
func heraldryProject(labels []string) (string, error) {
	projects := projectLabelValues(labels)
	switch len(projects) {
	case 1:
		return projects[0], nil
	case 0:
		return "", inputError("arms are unique within a project, so a bead with arms needs a project:<name> label")
	default:
		return "", inputError("arms are unique within one project, so a bead with arms may carry only one project: label (this one has " + strings.Join(projects, ", ") + ")")
	}
}

// checkHeraldry enforces R5, R6 and R11 for a bead about to be written with
// bead.Heraldry already normalised: the type may bear arms, the bead has one
// project, and no other live bead of that project bears the same run. The
// same emoji in another order are allowed with an X-Beads-Warning. Register
// values still in the shield form are read as their run. A bead without arms
// passes.
func (s *BeadsServer) checkHeraldry(ctx context.Context, bead *model.Bead) error {
	if bead.Heraldry == "" {
		return nil
	}
	if !heraldryTypes[bead.Type] {
		return inputError("a " + string(bead.Type) + " bead cannot bear arms yet, because arms are for the beads that last. " + model.HeraldryHelp)
	}
	project, err := heraldryProject(bead.Labels)
	if err != nil {
		return err
	}
	// Read, not Parse: a label change re-checks arms the bead already bears,
	// which may be one or two emoji stored before arms were a run.
	arms, err := heraldry.Read(bead.Heraldry)
	if err != nil {
		return inputError("invalid arms: " + err.Error() + ". " + model.HeraldryHelp)
	}
	register, err := s.heraldryRegister(ctx, project)
	if err != nil {
		return err
	}
	run, set := heraldry.Run(arms), heraldry.EmojiSet(arms)
	var near []string
	for _, other := range register {
		if other.ID == bead.ID {
			continue
		}
		otherArms, err := heraldry.Read(other.Heraldry)
		if err != nil {
			continue
		}
		switch {
		case heraldry.Run(otherArms) == run:
			return &heraldryConflictError{Run: run, Project: project, HolderID: other.ID, ShortTitle: other.ShortTitle}
		case heraldry.EmojiSet(otherArms) == set:
			near = append(near, other.ID)
		}
	}
	if len(near) > 0 {
		addWarning(ctx, fmt.Sprintf("the arms %s use the same emoji as %s in project %s, in another order; they are allowed, but may be hard to tell apart",
			run, strings.Join(near, ", "), project))
	}
	return nil
}

// heraldryRegister reads the beads bearing arms in project's register, across
// every fleet (fleet is attribution only; one project's beads span many).
func (s *BeadsServer) heraldryRegister(ctx context.Context, project string) ([]*model.Bead, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		HeraldryRegister: project,
		Fleet:            model.FleetAll,
		Limit:            heraldryRegisterLimit,
		OmitDescription:  true,
		OmitNotes:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the arms register for project %s: %w", project, err)
	}
	return beads, nil
}

// isHeraldryRegisterViolation reports whether err is the register index's
// unique violation.
func isHeraldryRegisterViolation(err error) bool {
	return isUniqueViolation(err) && strings.Contains(err.Error(), heraldryRegisterIndex)
}

// heraldryRaceConflict turns a register 23505 into the same conflict the
// pre-write check returns, re-reading the register for the bead that won.
// Any other error is returned unchanged.
func (s *BeadsServer) heraldryRaceConflict(ctx context.Context, bead *model.Bead, err error) error {
	if bead.Heraldry == "" || !isHeraldryRegisterViolation(err) {
		return err
	}
	conflict := &heraldryConflictError{Run: bead.Heraldry}
	project, perr := heraldryProject(bead.Labels)
	if perr != nil {
		return conflict
	}
	conflict.Project = project
	if register, rerr := s.heraldryRegister(ctx, project); rerr == nil {
		for _, other := range register {
			if other.ID != bead.ID && other.Heraldry == bead.Heraldry {
				conflict.HolderID, conflict.ShortTitle = other.ID, other.ShortTitle
				break
			}
		}
	}
	return conflict
}

// heraldryLabelChangeError re-checks the register when a single label add or
// remove (the label endpoints) moves a bead with arms between projects, or
// leaves it with no project or two. Labels other than project: never matter.
func (s *BeadsServer) heraldryLabelChangeError(ctx context.Context, target *model.Bead, label string, adding bool) error {
	if target == nil || target.Heraldry == "" || !strings.HasPrefix(label, "project:") {
		return nil
	}
	labels := make([]string, 0, len(target.Labels)+1)
	for _, l := range target.Labels {
		if l != label {
			labels = append(labels, l)
		}
	}
	if adding {
		labels = append(labels, label)
	}
	moved := *target
	moved.Labels = labels
	return s.checkHeraldry(ctx, &moved)
}

// asHeraldryConflict unwraps a register conflict from err.
func asHeraldryConflict(err error) (*heraldryConflictError, bool) {
	var hc *heraldryConflictError
	if errors.As(err, &hc) {
		return hc, true
	}
	return nil, false
}

// writeHeraldryConflict writes the register 409: the plain-words message, the
// stable code, and the bead that bears the arms.
func writeHeraldryConflict(w http.ResponseWriter, hc *heraldryConflictError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":       hc.Error(),
		"code":        heraldryConflictCode,
		"project":     hc.Project,
		"bead_id":     hc.HolderID,
		"short_title": hc.ShortTitle,
	})
}

// writeHeraldryCheckError writes a checkHeraldry failure on an HTTP label
// endpoint: 409 for a register collision, 400 for a refused value.
func writeHeraldryCheckError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if hc, ok := asHeraldryConflict(err); ok {
		writeHeraldryConflict(w, hc)
		return
	}
	var ie inputError
	if errors.As(err, &ie) {
		writeError(w, http.StatusBadRequest, ie.Error())
		return
	}
	writeServerError(r.Context(), w, op, err, "failed to check the arms register")
}

// heraldryGRPCError is writeHeraldryCheckError for the gRPC label endpoints.
func heraldryGRPCError(err error) error {
	if hc, ok := asHeraldryConflict(err); ok {
		return status.Error(codes.AlreadyExists, hc.Error())
	}
	var ie inputError
	if errors.As(err, &ie) {
		return status.Error(codes.InvalidArgument, ie.Error())
	}
	return status.Errorf(codes.Internal, "%v", err)
}
