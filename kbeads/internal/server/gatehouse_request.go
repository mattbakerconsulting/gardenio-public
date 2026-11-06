package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The gatehouse steward's apply and demo tools (task gd-K62coPtKkX, decision
// D4 gd-pbDt7wyQ3z apply-and-demo). The gatehouse (step 9.12) is the tenant
// land every signed-in person with no land is a guest of, and its steward is
// the first agent that takes free text from people who are tenants of no
// land. So the steward never writes a plot_application or a land_invite, and
// never says who the person is:
//
//   - it files one type=gatehouse_request in the gatehouse naming the message
//     the person wrote (gb lands gatehouse demo|apply --message <id>), and for
//     an application the land name, purpose and credits it took down;
//   - the daemon stamps a message an OIDC person creates in the gatehouse
//     with the GitHub account of the verified token (author_github_id,
//     author_github_login), and nobody may set or change those fields
//     (stampMessageGitHubAuthor);
//   - the viewer's gatehouse reactor (beads-viewer lands_gatehouse_reactor.go),
//     as the Gate's machine principal, re-reads the message, takes the person
//     from its stamp, and runs the Gate's own checks: the demo pass's terms,
//     one pass per account and LANDS_DEMO_MAX_GUESTS; for an application the
//     form's checks, after which /lands/apply is prefilled from the request
//     and the person's own repo connection and Apply file it.
//
// gatehouse_request is admin-gated. The one other writer is the tenant
// carve-out below (restricted_ops.go opGatehouseRequest): the gatehouse's
// steward may create one in its own land, and only while the gatehouse
// project carries land_gatehouse_tools=true, the switch the operator turns on
// after the 10.14 security review (gd-Ahv1F4wPUO). The viewer updates its
// outcome as a granted machine principal.

const typeGatehouseRequest model.BeadType = "gatehouse_request"

// landGatehouseToolsField is the project field that turns the tools on for
// the gatehouse; unset or anything but true is off.
const landGatehouseToolsField = "land_gatehouse_tools"

// Gatehouse request kinds.
const (
	gatehouseKindDemo  = "demo"
	gatehouseKindApply = "apply"
)

// Gatehouse request states. A request moves pending → drafted / refused, then
// drafted → done (the person asked for the pass or applied themselves) /
// refused (the draft lapsed). Nothing a request leads to is filed until the
// person acts on the draft (gd-EaTO1lFPAG). pending → done stays an edge for
// requests answered before drafts.
const (
	gatehouseStatePending = "pending"
	gatehouseStateDrafted = "drafted"
	gatehouseStateDone    = "done"
	gatehouseStateRefused = "refused"
)

var gatehouseStateEdges = map[string][]string{
	gatehouseStatePending: {gatehouseStateDrafted, gatehouseStateDone, gatehouseStateRefused},
	gatehouseStateDrafted: {gatehouseStateDone, gatehouseStateRefused},
	gatehouseStateDone:    nil,
	gatehouseStateRefused: nil,
}

// gatehouseRequestFixedFields are what the steward filed; nothing changes
// them after.
var gatehouseRequestFixedFields = []string{"kind", "message_id", "land_name", "purpose", "credits_requested_usd"}

// gatehouseRequestOutcomeFields are the reactor's; a new request carries none.
var gatehouseRequestOutcomeFields = []string{"outcome", "result_id", "github_id", "github_login", "handled_at"}

type gatehouseRequestFields struct {
	Kind      string   `json:"kind"`
	MessageID string   `json:"message_id"`
	LandName  string   `json:"land_name"`
	Purpose   string   `json:"purpose"`
	Credits   *float64 `json:"credits_requested_usd"`
	State     string   `json:"state"`
}

func decodeGatehouseRequestFields(raw json.RawMessage) (gatehouseRequestFields, map[string]any) {
	var f gatehouseRequestFields
	all := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
		_ = json.Unmarshal(raw, &all)
	}
	return f, all
}

// gatehouseProject reads the project bead name and reports whether it is the
// gatehouse and whether its tools are on. A missing project is neither.
func gatehouseProject(ctx context.Context, st store.Store, name string) (gatehouse, toolsOn bool, err error) {
	if name == "" {
		return false, false, nil
	}
	p, err := st.FindProjectByName(ctx, name)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("project lookup failed: %w", err)
	}
	all := fieldsAsMap(p.Fields)
	return fieldIsTrue(all[landGatehouseField]), fieldIsTrue(all[landGatehouseToolsField]), nil
}

// landGatehouseField is the project field marking the gatehouse land (the
// provisioner's GatehouseField, config.go land_gatehouse).
const landGatehouseField = "land_gatehouse"

// fieldIsTrue reads a boolean project field, stored as JSON true or, written
// through a string-typed client, "true".
func fieldIsTrue(v any) bool {
	return v == true || v == "true"
}

// validateGatehouseRequestCreate enforces the create-time rules: exactly one
// project label and it names the gatehouse, born pending with no outcome, a
// known kind and a message id, and for an application the form's shape.
func validateGatehouseRequestCreate(ctx context.Context, st store.Store, bead *model.Bead) error {
	projects := projectLabelValues(bead.Labels)
	if len(projects) != 1 {
		return fmt.Errorf("a gatehouse_request carries exactly one project label, the gatehouse's; got %d", len(projects))
	}
	gatehouse, _, err := gatehouseProject(ctx, st, projects[0])
	if err != nil {
		return err
	}
	if !gatehouse {
		return fmt.Errorf("a gatehouse_request belongs to the gatehouse land; %q is not it", projects[0])
	}
	f, all := decodeGatehouseRequestFields(bead.Fields)
	if f.State != gatehouseStatePending {
		return fmt.Errorf("a gatehouse_request is created in state %q, not %q", gatehouseStatePending, f.State)
	}
	var set []string
	for _, k := range gatehouseRequestOutcomeFields {
		if v, ok := all[k]; ok && !isEmptyFieldValue(v) {
			set = append(set, k)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("a new gatehouse_request cannot carry %s: the reactor sets them", strings.Join(set, ", "))
	}
	if strings.TrimSpace(f.MessageID) == "" {
		return errors.New("a gatehouse_request names the message the person wrote (message_id)")
	}
	switch f.Kind {
	case gatehouseKindDemo:
		if f.LandName != "" || f.Purpose != "" || f.Credits != nil {
			return errors.New("a demo request carries no land_name, purpose or credits_requested_usd")
		}
	case gatehouseKindApply:
		if !landNamePattern.MatchString(f.LandName) {
			return fmt.Errorf("land_name %q must be a DNS label: 3 to 30 characters, lowercase letters, digits and '-', starting with a letter and not ending with '-'", f.LandName)
		}
		if reservedLandNames[f.LandName] {
			return fmt.Errorf("land_name %q is reserved", f.LandName)
		}
		if strings.TrimSpace(f.Purpose) == "" {
			return errors.New("an application request says what the person wants the agents to work on (purpose)")
		}
		if n := utf8.RuneCountInString(f.Purpose); n > plotApplicationPurposeMax {
			return fmt.Errorf("purpose is %d characters; the limit is %d", n, plotApplicationPurposeMax)
		}
		if f.Credits != nil && *f.Credits < 0 {
			return errors.New("credits_requested_usd must not be negative")
		}
	default:
		return fmt.Errorf("kind %q must be %s or %s", f.Kind, gatehouseKindDemo, gatehouseKindApply)
	}
	return nil
}

// validateGatehouseRequestUpdate keeps what the steward filed fixed and the
// state moving forward.
func validateGatehouseRequestUpdate(prior, next json.RawMessage) error {
	from, priorAll := decodeGatehouseRequestFields(prior)
	to, nextAll := decodeGatehouseRequestFields(next)
	for _, k := range gatehouseRequestFixedFields {
		pv, pok := priorAll[k]
		nv, nok := nextAll[k]
		if pok != nok || !reflect.DeepEqual(pv, nv) {
			return fmt.Errorf("gatehouse_request %s is fixed when the request is filed", k)
		}
	}
	if from.State == to.State {
		return nil
	}
	if !slices.Contains(gatehouseStateEdges[from.State], to.State) {
		return fmt.Errorf("gatehouse_request state cannot move %s → %s", from.State, to.State)
	}
	return nil
}

// gatehouseAnswerOnceError holds a request to one answer: a write that sets
// handled_at, the reactor's answer, is refused unless the stored request is
// still pending. Read from the store under gatehouseRequestMu, it is the claim
// that keeps two viewer replicas' reactors from both answering one request
// (gd-GPvddfQY6y).
func gatehouseAnswerOnceError(ctx context.Context, st store.Store, id string, patch json.RawMessage) error {
	if _, answers := fieldsAsMap(patch)["handled_at"]; !answers {
		return nil
	}
	stored, err := st.GetBead(ctx, id)
	if err != nil {
		return fmt.Errorf("gatehouse request lookup failed: %w", err)
	}
	if stored == nil {
		return nil
	}
	if f, _ := decodeGatehouseRequestFields(stored.Fields); f.State != gatehouseStatePending {
		return inputError(fmt.Sprintf("gatehouse_request %s is already answered (%s); it is answered once", id, f.State))
	}
	return nil
}

// gatehouseRequestsPerHour is how many requests the gatehouse files in an
// hour. The steward reads free text from strangers, so one led astray could
// otherwise file without bound and starve the reactor's sweep.
const gatehouseRequestsPerHour = 30

// admitGatehouseRequest holds a new request to one per (message_id, kind) and
// to gatehouseRequestsPerHour in its land (gd-EaTO1lFPAG). A request for a
// pair already asked for comes back as that request, so the steward's gb verb
// reads its answer; a refused one does not count, so a transient refusal can
// be asked again. The caller holds gatehouseRequestMu across this and the
// insert.
func admitGatehouseRequest(ctx context.Context, st store.Store, bead *model.Bead, now time.Time) (*model.Bead, error) {
	projects := projectLabelValues(bead.Labels)
	if len(projects) != 1 {
		return nil, nil
	}
	f, _ := decodeGatehouseRequestFields(bead.Fields)
	beads, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{typeGatehouseRequest},
		Labels: []string{"project:" + projects[0]},
		Fields: map[string]string{"message_id": f.MessageID, "kind": f.Kind},
		Fleet:  model.FleetAll,
		Limit:  50,
	})
	if err != nil {
		return nil, fmt.Errorf("gatehouse request lookup failed: %w", err)
	}
	for _, b := range beads {
		if prior, _ := decodeGatehouseRequestFields(b.Fields); prior.MessageID == f.MessageID && prior.Kind == f.Kind && prior.State != gatehouseStateRefused {
			return b, nil
		}
	}
	since := now.Add(-time.Hour)
	recent, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type:         []model.BeadType{typeGatehouseRequest},
		Labels:       []string{"project:" + projects[0]},
		CreatedAfter: &since,
		Fleet:        model.FleetAll,
		Limit:        gatehouseRequestsPerHour + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("gatehouse request count failed: %w", err)
	}
	n := 0
	for _, b := range recent {
		if b.CreatedAt.After(since) {
			n++
		}
	}
	if n >= gatehouseRequestsPerHour {
		return nil, inputError(fmt.Sprintf("the gatehouse has filed %d requests in the last hour, its limit; point the person to /lands/apply or /lands/demo", gatehouseRequestsPerHour))
	}
	return nil, nil
}

// gatehouseRequestCarveOut reports whether the caller is a lands-tenant agent
// whom opGatehouseRequest admits to create a gatehouse_request with these
// labels. It lifts the type's admin gate for that caller only; every other
// caller is held to the gate as usual.
func gatehouseRequestCarveOut(ctx context.Context, st store.Store, labels []string) bool {
	caller, err := landsTenantCallerFrom(ctx, st)
	if err != nil || caller == nil || caller.Home {
		return false
	}
	projects := projectLabelValues(labels)
	if len(projects) != 1 {
		return false
	}
	return landsTenantOpDenial(ctx, st, restrictedOpRequest{
		Op: opGatehouseRequest, Action: "create", BeadType: typeGatehouseRequest, Project: projects[0],
	}) == nil
}

// gatehouseStewardDenial is the opGatehouseRequest rule: the steward of the
// gatehouse, with its tools on, files a request in its own land.
func gatehouseStewardDenial(ctx context.Context, st store.Store, caller *landsTenantCaller, req restrictedOpRequest) *AuthzDenial {
	deny := func(why string) *AuthzDenial {
		return landsTenantDenial(req.Action, req.BeadType, req.Action+" denied: "+caller.holder()+" holds the lands-tenant tier; "+why)
	}
	if !caller.isSteward() {
		return deny("only the gatehouse's steward files a gatehouse_request")
	}
	if req.Project == "" || req.Project != caller.Project {
		return deny("a gatehouse_request is filed only in the steward's own land " + strconvQuote(caller.Project))
	}
	gatehouse, toolsOn, err := gatehouseProject(ctx, st, caller.Project)
	switch {
	case err != nil:
		return landsTenantLookupDenial(req.Action, req.BeadType, err)
	case !gatehouse:
		return deny("only the gatehouse's steward files a gatehouse_request")
	case !toolsOn:
		return deny("the gatehouse's apply and demo tools are off (" + landGatehouseToolsField + "); point the person to /lands/apply or /lands/demo")
	}
	return nil
}

// The message author's GitHub account. A gatehouse request names a message,
// and the reactor gives the pass or drafts the application for whoever the
// daemon verified wrote it. So the daemon stamps it, and nothing else may.

const (
	authorGitHubIDField    = "author_github_id"
	authorGitHubLoginField = "author_github_login"
)

func isMessageGitHubAuthorField(k string) bool {
	return strings.EqualFold(k, authorGitHubIDField) || strings.EqualFold(k, authorGitHubLoginField)
}

func messageGitHubAuthorDeny(action string) *AuthzDenial {
	return &AuthzDenial{
		Action:   action,
		BeadType: string(typeMessage),
		Message:  authorGitHubIDField + " and " + authorGitHubLoginField + " are stamped by the server from the writer's verified GitHub sign-in; they cannot be set or changed",
	}
}

// stampMessageGitHubAuthor refuses a message create whose fields name either
// field in any letter case, then stamps the caller's verified GitHub account
// on a message an OIDC person writes in the gatehouse: elsewhere it would show
// every channel reader each writer's account (gd-R7meWqNxqr). A no-op for
// every other bead, a caller with no GitHub account, an escalation replay
// (the daemon's write) and a failed project lookup.
func stampMessageGitHubAuthor(ctx context.Context, st store.Store, bead *model.Bead) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage {
		return nil
	}
	for k := range fieldsAsMap(bead.Fields) {
		if isMessageGitHubAuthorField(k) {
			return messageGitHubAuthorDeny("create")
		}
	}
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodLand:
	default:
		return nil
	}
	id := AuthIdentityFromContext(ctx)
	projects := projectLabelValues(bead.Labels)
	if id.GitHubID <= 0 || id.GitHubLogin == "" || systemOperatorFromContext(ctx) == operatorMachine || len(projects) != 1 {
		return nil
	}
	if gatehouse, _, err := gatehouseProject(ctx, st, projects[0]); err != nil || !gatehouse {
		return nil
	}
	bead.Fields = setField(bead.Fields, authorGitHubIDField, strconv.FormatInt(id.GitHubID, 10))
	bead.Fields = setField(bead.Fields, authorGitHubLoginField, id.GitHubLogin)
	return nil
}

// messageGitHubAuthorPatchDenial refuses an update to a message that touches
// either field, unless it echoes the stored value back unchanged.
func messageGitHubAuthorPatchDenial(action string, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage || len(patch) == 0 {
		return nil
	}
	var prior map[string]any
	for k, v := range fieldsAsMap(patch) {
		if !isMessageGitHubAuthorField(k) {
			continue
		}
		if prior == nil {
			prior = fieldsAsMap(bead.Fields)
		}
		if ev, had := prior[k]; had && fmt.Sprint(ev) == fmt.Sprint(v) {
			continue
		}
		return messageGitHubAuthorDeny(action)
	}
	return nil
}

// messageGitHubAuthorKeyDenial refuses the field endpoints on either field:
// they write no whole value to compare.
func messageGitHubAuthorKeyDenial(action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage {
		return nil
	}
	for _, k := range keys {
		if isMessageGitHubAuthorField(k) {
			return messageGitHubAuthorDeny(action)
		}
	}
	return nil
}
