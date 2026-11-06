package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/oidcauth"
)

// The plot application (Track B spec §2.4 and §2.5, step 7.7). An applicant
// who accepted the terms and connected a repository (lands_github.go) applies
// for a plot; the operator answers a decision; the applicant reads the answer
// on /lands/applications and in their inbox.
//
//   - GET /lands/apply is the form; POST /lands/apply checks the §2.4 limits,
//     files one type=plot_application bead in project lands-intake and its
//     approval decision, and mails "received".
//   - GET /lands/applications lists the caller's own applications;
//     POST /lands/applications/{id}/withdraw withdraws one of them.
//   - The apply reactor (lands_apply_reactor.go) reads each resolved approval
//     decision back onto its application and mails the applicant.
//
// Every route is routeApplicant. plot_application is admin-gated (B-D4), so
// the Gate never writes it on the applicant's bearer: every intake call goes
// out as the viewer's machine principal with the gardenio-machine-lands-gate
// role, on a context that carries none of the request's identity
// (landsGate.callCtx). That makes the handlers the only thing standing between
// an applicant and everyone else's applications, so each one keys on the
// caller's verified Keycloak subject and re-checks applicant_sub on every bead
// the daemon returns, whatever the query asked for. The repository and the
// GitHub signals come from the signed setup result (landsGitHubResult), never
// from the form.

const (
	landsApplicationType = "plot_application"
	landsIntakeLabel     = "project:lands-intake"

	// Application states (kbeads plot_application.go).
	landsAppSubmitted    = "submitted"
	landsAppApproved     = "approved"
	landsAppDenied       = "denied"
	landsAppWithdrawn    = "withdrawn"
	landsAppProvisioning = "provisioning"
	landsAppProvisioned  = "provisioned"
	landsAppFailed       = "failed"
	landsAppClosed       = "closed"

	// The tenant agents' models (OQ3 gd-Se3rF0T5pu): Sonnet 5.5 unless the
	// approval says model=opus.
	landsModelSonnet = "claude-sonnet-5-5"
	landsModelOpus   = "claude-opus-5-5"

	landsApplyPurposeMax  = 1000
	landsApplyCreditsMax  = 10000.0
	landsGateCallTimeout  = 20 * time.Second
	landsGateListLimit    = 500
	landsDecisionPriority = 2

	// Environment (main.go, landsGateFromEnv).
	landsApplyDailyLimitEnv     = "LANDS_APPLY_DAILY_LIMIT"
	landsApplyDefaultCreditsEnv = "LANDS_APPLY_DEFAULT_CREDITS"
	landsApproverEnv            = "LANDS_APPROVER"
	landsApplyDailyLimitDefault = 20
	landsApplyCreditsDefault    = 25.0
)

// landsOpenStates are the states an applicant can hold only one of at a time:
// the application is still on its way to a land (§2.4 "one open application
// per applicant").
var landsOpenStates = []string{landsAppSubmitted, landsAppApproved, landsAppProvisioning, landsAppFailed}

// landsClaimingStates are the states in which an application holds its land
// name and repository: open, or already a land (§2.4 "the repo must not
// already back a land or an open application").
var landsClaimingStates = []string{landsAppSubmitted, landsAppApproved, landsAppProvisioning, landsAppFailed, landsAppProvisioned}

// landsNamePattern and landsReservedNames mirror kbeads' landNamePattern and
// reservedLandNames (kbeads/internal/server/plot_application.go), which stay
// the authority: these only let the form say what is wrong before a round trip.
var landsNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,28}[a-z0-9]$`)

var landsReservedNames = map[string]bool{
	"lands-intake": true, "gardenio": true, "admin": true, "lands": true, "system": true,
	"default": true, "operator": true, "support": true, "kube-system": true,
}

// landsIntakeAPI is the daemon surface the Gate uses, always as the viewer's
// machine principal. *beadsapi.Client implements it; tests fake it.
type landsIntakeAPI interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CancelDecision(ctx context.Context, decisionID, reason, canceledBy string) error
}

// landsGate is the configured application flow.
type landsGate struct {
	intake  landsIntakeAPI
	machine beadsapi.TokenSource // attached to every intake call; nil only in tests
	mail    mailer.Mailer
	tpl     *mailer.Templates
	logger  *slog.Logger
	now     func() time.Time

	dailyLimit     int     // submitted applications across all applicants per UTC day
	defaultCredits float64 // the grant when the approval names none
	approver       string  // operated_by on the approval decision, and the one person whose answer counts besides a gardenio-admin ("" = admins only)

	// mu serializes the limit checks with the create, so one viewer cannot
	// let two racing submissions both pass "one open application".
	mu sync.Mutex
}

// landsGateFromEnv builds the Gate. The caller wires it only when the viewer
// has a machine principal: without one the intake writes could only go out on
// the shared token, which kbeads refuses for plot_application anyway.
func landsGateFromEnv(getenv func(string) string, intake landsIntakeAPI, machine beadsapi.TokenSource, mail mailer.Mailer, tpl *mailer.Templates, logger *slog.Logger) (*landsGate, error) {
	g := &landsGate{
		intake: intake, machine: machine, mail: mail, tpl: tpl, logger: logger, now: time.Now,
		dailyLimit: landsApplyDailyLimitDefault, defaultCredits: landsApplyCreditsDefault,
		approver: strings.TrimSpace(getenv(landsApproverEnv)),
	}
	if v := strings.TrimSpace(getenv(landsApplyDailyLimitEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s=%q: want a positive integer", landsApplyDailyLimitEnv, v)
		}
		g.dailyLimit = n
	}
	if v := strings.TrimSpace(getenv(landsApplyDefaultCreditsEnv)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > landsApplyCreditsMax {
			return nil, fmt.Errorf("%s=%q: want a number of credits from 0 to %.0f", landsApplyDefaultCreditsEnv, v, landsApplyCreditsMax)
		}
		g.defaultCredits = f
	}
	if g.mail == nil {
		g.mail = mailer.Disabled{}
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	return g, nil
}

// landsMailerFromEnv is the Gate's mailer and templates (mailer.ConfigFromEnv:
// off, so Disabled, unless MAILER_ENABLED).
func landsMailerFromEnv(ctx context.Context) (mailer.Mailer, *mailer.Templates, error) {
	cfg := mailer.ConfigFromEnv()
	m, err := mailer.New(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return m, mailer.NewTemplates(cfg), nil
}

// callCtx is the context of one intake call: the request's deadline and
// cancellation, the viewer's machine principal, and nothing else. The request
// context carries the applicant's bearer, actor and edge-auth operator, and
// each of those would outrank or relabel the machine credential on the wire.
func (g *landsGate) callCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), landsGateCallTimeout)
	stop := context.AfterFunc(parent, cancel)
	return beadsapi.WithMachinePrincipal(ctx, g.machine), func() { stop(); cancel() }
}

// landsApplication is one plot_application bead as the Gate reads it.
type landsApplication struct {
	ID               string
	State            string
	ApplicantSub     string
	ApplicantEmail   string
	GitHubLogin      string
	LandName         string
	Repo             string
	RepoPrivate      bool
	Purpose          string
	CreditsRequested string
	AccountAgeDays   string
	PublicRepos      string
	CreditsGranted   float64
	Model            string
	DecisionID       string
	DecisionDigest   string
	DenyReason       string
	Question         string
	AskedAt          string
	DecidedAt        string
	CreatedAt        time.Time
}

// landsApplicationFromBead reads b, or reports false when b is not a plot
// application in lands-intake.
func landsApplicationFromBead(b *beadsapi.BeadDetail) (*landsApplication, bool) {
	if b == nil || b.Type != landsApplicationType || !slices.Contains(b.Labels, landsIntakeLabel) {
		return nil, false
	}
	f := b.Fields
	granted, _ := strconv.ParseFloat(f["credits_granted_usd"], 64)
	return &landsApplication{
		ID: b.ID, State: f["state"], ApplicantSub: f["applicant_sub"], ApplicantEmail: f["applicant_email"],
		GitHubLogin: f["github_login"], LandName: f["land_name"], Repo: f["repo"], RepoPrivate: f["repo_private"] == "true",
		Purpose: f["purpose"], CreditsRequested: f["credits_requested_usd"], CreditsGranted: granted, Model: f["model"],
		AccountAgeDays: f["github_account_age_days"], PublicRepos: f["github_public_repos"],
		DecisionID: f["decision_id"], DecisionDigest: f["decision_digest"], DenyReason: f["deny_reason"], Question: f["question"], AskedAt: f["asked_at"],
		DecidedAt: f["decided_at"], CreatedAt: b.CreatedAt,
	}, true
}

// list returns the applications whose field key equals value, re-checked here
// so a daemon that ignored the filter cannot widen the answer.
func (g *landsGate) list(ctx context.Context, key, value string) ([]*landsApplication, error) {
	res, err := g.intake.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{landsApplicationType}, Labels: []string{landsIntakeLabel},
		Fields: map[string]string{key: value}, Limit: landsGateListLimit,
	})
	if err != nil {
		return nil, err
	}
	var out []*landsApplication
	for _, b := range res.Beads {
		if a, ok := landsApplicationFromBead(b); ok && b.Fields[key] == value {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// mine is the caller's own applications, newest first.
func (g *landsGate) mine(ctx context.Context, sub string) ([]*landsApplication, error) {
	return g.list(ctx, "applicant_sub", sub)
}

// landsApplyInput is the form, parsed.
type landsApplyInput struct {
	LandName string
	Purpose  string
	Credits  string // as typed, for re-rendering
	credits  *float64
	// errField is the form field parseLandsApply refused (land_name, purpose,
	// credits), so the page shows the message at that field.
	errField string
}

// parseLandsApply reads and checks the form. The returned message is what the
// applicant reads; empty means the input is acceptable.
func parseLandsApply(form func(string) string) (landsApplyInput, string) {
	in := landsApplyInput{
		LandName: strings.ToLower(strings.TrimSpace(form("land_name"))),
		Purpose:  strings.TrimSpace(strings.ReplaceAll(form("purpose"), "\r\n", "\n")),
		Credits:  strings.TrimSpace(form("credits")),
	}
	switch {
	case in.LandName == "":
		in.errField = "land_name"
		return in, "Give your land a name."
	case !landsNamePattern.MatchString(in.LandName):
		in.errField = "land_name"
		return in, "A land name is 3 to 30 characters: lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen."
	case landsReservedNames[in.LandName]:
		in.errField = "land_name"
		return in, "That name is reserved. Please pick another."
	case in.Purpose == "":
		in.errField = "purpose"
		return in, "Tell the operator what you want the agents to work on."
	case utf8.RuneCountInString(in.Purpose) > landsApplyPurposeMax:
		in.errField = "purpose"
		return in, fmt.Sprintf("Please keep what you want to do to %d characters; it is %d now.", landsApplyPurposeMax, utf8.RuneCountInString(in.Purpose))
	}
	if in.Credits != "" {
		f, err := strconv.ParseFloat(strings.TrimPrefix(in.Credits, "$"), 64)
		if err != nil || f < 0 || f > landsApplyCreditsMax {
			in.errField = "credits"
			return in, fmt.Sprintf("Credits asked for must be a number from 0 to %.0f.", landsApplyCreditsMax)
		}
		in.credits = &f
	}
	return in, ""
}

// errLandsLimit is a §2.4 limit refusing an application; its message is for
// the applicant, and field names the form field it is about, if one.
type errLandsLimit struct {
	status int
	msg    string
	field  string
}

func (e *errLandsLimit) Error() string { return e.msg }

// checkLimits applies §2.4's limits to a new application: one open
// application per applicant, a land name and a repository nobody else holds,
// and the daily cap across all applicants.
func (g *landsGate) checkLimits(ctx context.Context, sub, landName, repo string) error {
	mine, err := g.mine(ctx, sub)
	if err != nil {
		return err
	}
	for _, a := range mine {
		if slices.Contains(landsOpenStates, a.State) {
			return &errLandsLimit{http.StatusConflict, "You already have an open application, for " + a.LandName + ". You can have one at a time: wait for the answer, or withdraw it first.", ""}
		}
	}
	byRepo, err := g.list(ctx, "repo", repo)
	if err != nil {
		return err
	}
	for _, a := range byRepo {
		if slices.Contains(landsClaimingStates, a.State) {
			return &errLandsLimit{http.StatusConflict, "The repository " + repo + " already has a land or an open application.", ""}
		}
	}
	byName, err := g.list(ctx, "land_name", landName)
	if err != nil {
		return err
	}
	for _, a := range byName {
		if slices.Contains(landsClaimingStates, a.State) {
			return &errLandsLimit{http.StatusConflict, "The name " + landName + " is taken. Please pick another.", "land_name"}
		}
	}
	now := g.now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	today, err := g.intake.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{landsApplicationType}, Labels: []string{landsIntakeLabel}, CreatedAfter: midnight, CountOnly: true,
	})
	if err != nil {
		return err
	}
	if today.Total >= g.dailyLimit {
		return &errLandsLimit{http.StatusTooManyRequests, "We have received as many applications as we can read today. Please apply again tomorrow.", ""}
	}
	return nil
}

// submit files the application and its decision. The decision is best-effort
// here: the reactor files one for any submitted application that has none.
func (g *landsGate) submit(ctx context.Context, sub, email, termsVersion string, gh *landsGitHubResult, in landsApplyInput) (*landsApplication, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkLimits(ctx, sub, in.LandName, gh.Repo); err != nil {
		return nil, err
	}
	fields := map[string]any{
		"applicant_sub": sub, "applicant_email": email,
		"github_login": gh.GitHubLogin, "github_id": strconv.FormatInt(gh.GitHubID, 10),
		"land_name": in.LandName, "repo": gh.Repo, "repo_private": gh.Private,
		"github_installation_id": strconv.FormatInt(gh.InstallationID, 10),
		"purpose":                in.Purpose,
		"terms_version":          termsVersion, "aup_version": termsVersion,
		"github_account_age_days": gh.AccountAgeDays(g.now()), "github_public_repos": gh.PublicRepos,
		"state": landsAppSubmitted,
	}
	if in.credits != nil {
		fields["credits_requested_usd"] = *in.credits
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	b, err := g.intake.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: "Plot application: " + in.LandName + " (" + gh.Repo + ")", Type: landsApplicationType, Kind: "data",
		Labels: []string{landsIntakeLabel}, Fields: raw,
	})
	if err != nil {
		if strings.Contains(err.Error(), "already a project") {
			return nil, &errLandsLimit{http.StatusConflict, "The name " + in.LandName + " is taken. Please pick another.", "land_name"}
		}
		return nil, err
	}
	app, ok := landsApplicationFromBead(b)
	if !ok {
		// The daemon answered with less than it stored; read what we know.
		app = &landsApplication{ID: b.ID, State: landsAppSubmitted, ApplicantSub: sub, ApplicantEmail: email,
			GitHubLogin: gh.GitHubLogin, LandName: in.LandName, Repo: gh.Repo, RepoPrivate: gh.Private, Purpose: in.Purpose,
			AccountAgeDays: strconv.Itoa(gh.AccountAgeDays(g.now())), PublicRepos: strconv.Itoa(gh.PublicRepos)}
	}
	if in.credits != nil {
		app.CreditsRequested = formatCredits(*in.credits)
	}
	if id, err := g.fileDecision(ctx, app, "", ""); err != nil {
		g.logger.Warn("lands apply: filing the approval decision failed; the reactor will retry", "application", app.ID, "error", err)
	} else {
		app.DecisionID = id
	}
	return app, nil
}

// fileDecision files the approval decision for app and points the
// application at it. after is the decision it replaces ("" for the first), and
// keys the create so a retry finds the decision a crashed attempt filed. note,
// when set, heads the context the operator reads.
//
// Nothing the daemon reports about who filed a decision is trusted: created_by
// is whatever X-Actor a static-token caller sent (gd-qZER8OOiIb), and the same
// header makes that caller the decision's owner, free to rewrite its prompt,
// options and context. Two things anchor the decision instead:
//
//   - decision_id on the application. plot_application is admin-gated, so only
//     the Gate (and an admin) writes it: a decision is the application's
//     because the application names it, not because of who created it.
//   - decision_digest beside it: the digest of what the operator is asked
//     (landsDecisionDigest). The reactor re-reads the decision and refuses it
//     unless it still says exactly that, so a relabelled option ("approve"
//     labelled "Deny") is never acted on.
//
// The key is predictable, and the daemon answers a keyed create with whatever
// bead already holds it. So the bead that comes back is recorded only when it
// is an open, unanswered decision for this application that asks exactly what
// the Gate sent (checkFiledDecision); anything else is logged and the decision
// filed again under a key nobody can guess.
func (g *landsGate) fileDecision(ctx context.Context, app *landsApplication, after, note string) (string, error) {
	prompt := "Plot application: " + app.LandName + " for " + app.Repo
	if app.GitHubLogin != "" {
		prompt += " from " + app.GitHubLogin
	}
	credits := formatCredits(g.defaultCredits)
	options := []map[string]string{
		{"id": "approve", "short": "Approve", "artifact_type": "report",
			"label": "Approve: provision the land with " + credits + " credits on Sonnet 5.5. To change either, reply credits=<n> and/or model=opus."},
		{"id": "deny", "short": "Deny", "artifact_type": "report",
			"label": "Deny: your reply is the reason the applicant reads."},
		{"id": "ask", "short": "Ask", "artifact_type": "report",
			"label": "Ask: your reply is emailed to the applicant as a question, and the application stays open."},
	}
	decisionCtx := g.decisionContext(app)
	if note != "" {
		decisionCtx = note + "\n\n" + decisionCtx
	}
	fields := map[string]any{
		"prompt": prompt, "options": options, "context": decisionCtx,
		"requested_by": "lands-gate", "requesting_task": app.ID,
	}
	if g.approver != "" {
		fields["operated_by"] = g.approver
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	digest, ok := landsDecisionDigest(map[string]string{
		"prompt": prompt, "options": string(optionsJSON), "context": decisionCtx, "operated_by": g.approver,
	})
	if !ok {
		return "", errors.New("digesting the decision the Gate files")
	}
	if after == "" {
		after = "none"
	}
	req := beadsapi.CreateBeadRequest{
		Title: prompt, Type: "decision", Kind: "data", Priority: landsDecisionPriority,
		Labels: []string{landsIntakeLabel}, Fields: raw,
		IdempotencyKey: "lands-gate:" + app.ID + ":after:" + after,
	}
	d, err := g.intake.CreateBeadDetail(ctx, req)
	if err != nil {
		return "", fmt.Errorf("filing the decision: %w", err)
	}
	if why := checkFiledDecision(app, d, digest); why != "" {
		g.logger.Warn("lands apply: refused the decision returned for the Gate's key; filing under a fresh key",
			"application", app.ID, "key", req.IdempotencyKey, "decision", d.ID, "why", why)
		req.IdempotencyKey += ":" + rand.Text()
		if d, err = g.intake.CreateBeadDetail(ctx, req); err != nil {
			return "", fmt.Errorf("filing the decision: %w", err)
		}
		if why := checkFiledDecision(app, d, digest); why != "" {
			return "", fmt.Errorf("refused decision %s: %s", d.ID, why)
		}
	}
	if err := g.intake.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{"decision_id": d.ID, "decision_digest": digest}); err != nil {
		return "", fmt.Errorf("recording decision %s on the application: %w", d.ID, err)
	}
	app.DecisionID, app.DecisionDigest = d.ID, digest
	return d.ID, nil
}

// checkFiledDecision says why d, returned for a create, is not the decision
// the Gate just filed for app, or "" when it is: an open, unanswered decision
// requesting app whose prompt, options, context and operated_by digest to
// digest. A decision another caller planted under the Gate's key with the same
// words is as good as the Gate's own: the read-back pin covers what its owner
// may change afterwards.
func checkFiledDecision(app *landsApplication, d *beadsapi.BeadDetail, digest string) string {
	switch {
	case d == nil:
		return "no bead"
	case d.Status != "open":
		return "status " + d.Status + ", not open"
	case d.Fields["chosen"] != "" || d.Fields["response_text"] != "" || d.Fields["resolver_subject"] != "":
		return "already answered"
	}
	return verifyLandsDecision(app, d, digest)
}

// verifyLandsDecision says why d is not app's approval decision as the Gate
// filed it, or "" when it is: a decision requesting app that still asks what
// the Gate pinned in digest (the application's decision_digest). The reactor
// calls it on every read-back, so a decision whose options, prompt, context or
// operated_by changed after filing is never acted on. created_by is not read:
// a static-token caller sets it with X-Actor.
//
// The digest only sees the decision as it is now, and a closed decision stays
// editable. So a closed one counts only if nothing wrote to it after it
// closed: otherwise its owner could relabel the options, let the approver
// answer, and put the Gate's words back before the sweep reads it. Every
// answer path in kbeads merges the answer and then closes, and the close
// stamps closed_at and updated_at with the same NOW(), so a decision answered
// and left alone has updated_at == closed_at.
func verifyLandsDecision(app *landsApplication, d *beadsapi.BeadDetail, digest string) string {
	switch {
	case d == nil:
		return "no bead"
	case d.Type != "decision":
		return "type " + d.Type + ", not decision"
	case d.Fields["requesting_task"] != app.ID:
		return "requesting_task " + strconv.Quote(d.Fields["requesting_task"]) + ", not " + app.ID
	case digest == "":
		return "the application pins no decision_digest"
	}
	if got, ok := landsDecisionDigest(d.Fields); !ok || got != digest {
		return "its prompt, options, context or operated_by changed since the Gate filed it"
	}
	if d.Status == "closed" {
		closedAt, err := time.Parse(time.RFC3339Nano, d.ClosedAt)
		switch {
		case err != nil:
			return "closed with no readable closed_at " + strconv.Quote(d.ClosedAt)
		case d.UpdatedAt.After(closedAt):
			return "it was written at " + d.UpdatedAt.UTC().Format(time.RFC3339Nano) + ", after it closed at " + closedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

// landsDecisionDigest is the sha256 of what a decision asks the operator: its
// prompt, options, context and operated_by. Options are compared as data, not
// bytes, since the daemon may store the array with its keys in another order;
// ok is false when they do not parse as the option objects the Gate files.
func landsDecisionDigest(f map[string]string) (string, bool) {
	var options []map[string]string
	if err := json.Unmarshal([]byte(f["options"]), &options); err != nil || len(options) == 0 {
		return "", false
	}
	canon, err := json.Marshal(struct {
		Prompt     string              `json:"prompt"`
		Options    []map[string]string `json:"options"`
		Context    string              `json:"context"`
		OperatedBy string              `json:"operated_by"`
	}{f["prompt"], options, f["context"], f["operated_by"]})
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), true
}

// decisionContext is what the operator reads before answering (§2.5): who
// applied, the GitHub signals that show a throwaway account, the repository
// and what they want to do.
//
// The context renders as Markdown wherever the decision is answered, so every
// value the applicant chose is fenced as code (gd-ZwnwonBxGS): the land name,
// login and email inline, the purpose as a code block. An applicant then
// cannot put a tracking image, a forged heading or "Approve grants" line, or
// a :::grafana embed in front of the operator. Only the Gate's own words and
// the operator's question are Markdown.
//
// It names the application in words, not by its id: the decision carries the
// id as its requesting_task, and an id in context prose reads bare on every
// surface that answers it (gd-nAlHLqdF4G). The facts are a Markdown list and each sentence
// its own paragraph, so a renderer without hard wraps (the bead panel, the
// viewer) does not join them into one line (gd-gKXlbPRw8i).
func (g *landsGate) decisionContext(app *landsApplication) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This application asks for a land called %s.\n\n", mdInlineCode(app.LandName))
	fmt.Fprintf(&b, "- Applicant: %s (GitHub %s)\n", mdInlineCode(orDash(app.ApplicantEmail)), mdInlineCode(orDash(app.GitHubLogin)))
	if app.AccountAgeDays != "" || app.PublicRepos != "" {
		fmt.Fprintf(&b, "- GitHub account: %s days old, %s public repositories\n", orDash(app.AccountAgeDays), orDash(app.PublicRepos))
	}
	vis := "public"
	if app.RepoPrivate {
		vis = "private"
	}
	fmt.Fprintf(&b, "- Repository: https://github.com/%s (%s)\n", app.Repo, vis)
	if app.CreditsRequested != "" {
		fmt.Fprintf(&b, "- Credits asked for: %s\n", app.CreditsRequested)
	}
	fmt.Fprintf(&b, "\nWhat they want to do:\n\n%s\n", mdCodeBlock(wrapLines(app.Purpose, 76)))
	if app.Question != "" {
		fmt.Fprintf(&b, "\nYou asked them (%s): %s\n\nTheir answer reaches you by email, if they reply.\n", app.AskedAt, app.Question)
	}
	fmt.Fprintf(&b, "\nApprove grants %s credits on Sonnet 5.5 unless your reply says credits=<n> or model=opus. Deny's reply is the reason the applicant reads. Ask's reply is emailed to them as a question. Dismissing this files it again: deny instead.", formatCredits(g.defaultCredits))
	return b.String()
}

// mdCodeBlock fences s as a Markdown code block, its fence one backtick longer
// than the longest backtick run in s (and at least three), so no line of s
// can close it and nothing in s renders as Markdown.
func mdCodeBlock(s string) string {
	fence := strings.Repeat("`", max(3, longestBacktickRun(s)+1))
	return fence + "\n" + strings.TrimRight(s, "\n") + "\n" + fence
}

// mdInlineCode puts s in a Markdown code span on one line, its delimiter one
// backtick longer than the longest backtick run in s, so s reads as literal
// text: never a link, a picture or emphasis.
func mdInlineCode(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	fence := strings.Repeat("`", longestBacktickRun(s)+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// wrapLines breaks each line of s longer than width at spaces, so quoted
// text in a code block (which no surface wraps) reads without scrolling
// sideways. A word longer than width stays whole on its own line.
func wrapLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if utf8.RuneCountInString(line) <= width {
			continue
		}
		var out strings.Builder
		n := 0
		for _, w := range strings.Fields(line) {
			wl := utf8.RuneCountInString(w)
			if n > 0 && n+1+wl > width {
				out.WriteByte('\n')
				n = 0
			} else if n > 0 {
				out.WriteByte(' ')
				n++
			}
			out.WriteString(w)
			n += wl
		}
		lines[i] = out.String()
	}
	return strings.Join(lines, "\n")
}

func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatCredits renders 25 as "25" and 12.5 as "12.50".
func formatCredits(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// sendMail renders and sends one Gate mail, logging rather than failing: the
// state change it reports has already happened, and the page says the same.
func (g *landsGate) sendMail(ctx context.Context, kind mailer.Kind, app *landsApplication, note string) {
	if g.tpl == nil || !mailer.Enabled(g.mail) || app.ApplicantEmail == "" {
		return
	}
	m, err := g.tpl.Render(kind, app.ApplicantEmail, mailer.Data{
		Name: app.GitHubLogin, ApplicationID: app.ID, Land: app.LandName, Repo: app.Repo,
		Note: note, GrantedUSD: app.CreditsGranted,
	})
	if err != nil {
		g.logger.Warn("lands mail: render failed", "kind", kind, "application", app.ID, "error", err)
		return
	}
	if err := g.mail.Send(ctx, m); err != nil {
		g.logger.Warn("lands mail: send failed", "kind", kind, "application", app.ID, "error", err)
	}
}

// withdraw withdraws the caller's own submitted application and cancels its
// decision. An id that is not the caller's reads as not found.
func (g *landsGate) withdraw(ctx context.Context, sub, id string) (*landsApplication, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, err := g.intake.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, errLandsNotYours
		}
		return nil, err
	}
	app, ok := landsApplicationFromBead(b)
	if !ok || sub == "" || app.ApplicantSub != sub {
		return nil, errLandsNotYours
	}
	if app.State != landsAppSubmitted {
		return app, &errLandsLimit{http.StatusConflict, "Only an application that is waiting for an answer can be withdrawn; this one is " + landsStateWords(app.State) + ".", ""}
	}
	if err := g.intake.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{"state": landsAppWithdrawn}); err != nil {
		return nil, err
	}
	app.State = landsAppWithdrawn
	if app.DecisionID != "" {
		if err := g.intake.CancelDecision(ctx, app.DecisionID, "the applicant withdrew application "+app.ID, "lands-gate"); err != nil {
			g.logger.Warn("lands withdraw: cancelling the decision failed", "application", app.ID, "decision", app.DecisionID, "error", err)
		}
	}
	g.sendMail(ctx, mailer.KindApplicationWithdrawn, app, "")
	return app, nil
}

// errLandsNotYours is every way an id fails to be the caller's application:
// missing, another type, or someone else's. One answer, so the page never
// says which.
var errLandsNotYours = errors.New("not the caller's application")

// landsStateWords is a state as the applicant reads it.
func landsStateWords(state string) string {
	switch state {
	case landsAppSubmitted:
		return "waiting for the operator"
	case landsAppApproved:
		return "approved"
	case landsAppDenied:
		return "not approved"
	case landsAppWithdrawn:
		return "withdrawn"
	case landsAppProvisioning:
		return "being set up"
	case landsAppProvisioned:
		return "ready"
	case landsAppFailed:
		return "being set up (we hit a problem and are on it)"
	case landsAppClosed:
		return "closed"
	}
	return state
}

// landsApplyView is the apply and applications pages' data, in landsView.Apply.
type landsApplyView struct {
	// State is what the apply page shows: closed (the Gate is not configured),
	// nobearer, unavailable, terms (accept first), github (connect first),
	// open (an application is already open), form.
	State  string
	GitHub *landsGitHubResult
	Input  landsApplyInput
	Open   *landsApplication
	// Applications is the caller's own, newest first.
	Applications []*landsApplication
	// Mail says whether the Gate mails, so the pages only promise mail when
	// it is sent.
	Mail bool
	// DefaultCredits is the grant when the operator names none.
	DefaultCredits string
	// ErrField is the form field the page's Error is about, if one: the
	// message shows at that field too, so it stays in view above a phone's
	// keyboard while the field is fixed (lands-mobile.md §3, gd-KiRStJd56I).
	ErrField string
}

func (s *Server) landsApplyBase(r *http.Request, title string) (landsView, bool) {
	v, ok := s.landsBase(r, title)
	if ok && s.landsGate != nil {
		v.Apply.Mail = s.landsGate.tpl != nil && mailer.Enabled(s.landsGate.mail)
		v.Apply.DefaultCredits = formatCredits(s.landsGate.defaultCredits)
	}
	return v, ok
}

// fillLandsApplyState decides what the apply page shows the caller; it
// returns the status to answer with.
func (s *Server) fillLandsApplyState(r *http.Request, v *landsView) int {
	if s.landsGate == nil || s.landsGitHub == nil {
		v.Apply.State = "closed"
		return http.StatusServiceUnavailable
	}
	s.fillLandsAcceptState(r.Context(), v)
	switch v.State {
	case landsStateNoBearer:
		v.Apply.State = "nobearer"
		return http.StatusForbidden
	case landsStateUnavailable:
		v.Apply.State = "unavailable"
		return http.StatusBadGateway
	case landsStateCurrent:
	default:
		v.Apply.State = "terms"
		return http.StatusOK
	}
	sub := oidcauth.SubjectFromContext(r.Context())
	if sub == "" || oidcauth.EmailFromContext(r.Context()) == "" {
		v.Apply.State = "nobearer"
		return http.StatusForbidden
	}
	ctx, cancel := s.landsGate.callCtx(r.Context())
	defer cancel()
	mine, err := s.landsGate.mine(ctx, sub)
	if err != nil {
		s.logger.Warn("lands apply: reading the caller's applications failed", "error", err)
		v.Apply.State = "unavailable"
		return http.StatusBadGateway
	}
	for _, a := range mine {
		if slices.Contains(landsOpenStates, a.State) {
			v.Apply.State, v.Apply.Open = "open", a
			return http.StatusOK
		}
	}
	gh, ok := s.landsGitHub.verifiedFor(r.Context(), r)
	if !ok {
		v.Apply.State = "github"
		return http.StatusOK
	}
	v.Apply.State, v.Apply.GitHub = "form", gh
	return http.StatusOK
}

func (s *Server) handleLandsApplyPage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsApplyBase(r, "Apply for a plot")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	status := s.fillLandsApplyState(r, &v)
	s.prefillFromGatehouse(r, &v)
	s.renderLands(w, status, "lands_apply", v)
}

// landsApplyUseDraft is the query that fills the form from the offered
// draft: the caller's own "use this draft".
const landsApplyUseDraft = "draft"

// prefillFromGatehouse offers the caller's newest draft with the gatehouse
// steward (lands_gatehouse_reactor.go), found by the caller's own verified
// GitHub account, quoting the message it came from. The form is filled from
// it only on ?draft=use, the caller's choice (gd-EaTO1lFPAG); their
// repository connection and Apply still file it.
func (s *Server) prefillFromGatehouse(r *http.Request, v *landsView) {
	if v.Apply.State != "form" && v.Apply.State != "github" {
		return
	}
	o := s.landsGatehouseTools.offerFor(r.Context(), oidcauth.GitHubIDFromContext(r.Context()), "apply")
	if o == nil {
		return
	}
	v.Offer = o
	if v.Apply.State == "form" && r.URL.Query().Get(landsApplyUseDraft) == "use" {
		v.Apply.Input, v.OfferUsed = o.Input, true
	}
}

func (s *Server) handleLandsApply(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsApplyBase(r, "Apply for a plot")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	status := s.fillLandsApplyState(r, &v)
	if v.Apply.State != "form" {
		if status == http.StatusOK {
			status = http.StatusConflict
		}
		s.renderLands(w, status, "lands_apply", v)
		return
	}
	in, msg := parseLandsApply(r.PostForm.Get)
	v.Apply.Input = in
	if msg != "" {
		v.Error, v.Apply.ErrField = msg, in.errField
		s.renderLands(w, http.StatusBadRequest, "lands_apply", v)
		return
	}
	ctx, cancel := s.landsGate.callCtx(r.Context())
	defer cancel()
	app, err := s.landsGate.submit(ctx, oidcauth.SubjectFromContext(r.Context()), oidcauth.EmailFromContext(r.Context()), v.Legal.Version, v.Apply.GitHub, in)
	var limit *errLandsLimit
	switch {
	case errors.As(err, &limit):
		v.Error, v.Apply.ErrField = limit.msg, limit.field
		s.renderLands(w, limit.status, "lands_apply", v)
		return
	case err != nil:
		s.logger.Warn("lands apply: filing the application failed", "error", err)
		v.Error = "Your application could not be filed. Nothing was saved; try again in a minute."
		s.renderLands(w, http.StatusBadGateway, "lands_apply", v)
		return
	}
	s.logger.Info("lands application submitted", "application", app.ID, "land", app.LandName, "repo", app.Repo, "decision", app.DecisionID)
	s.landsGate.sendMail(ctx, mailer.KindApplicationReceived, app, "")
	s.landsGatehouseTools.settle(r.Context(), oidcauth.GitHubIDFromContext(r.Context()), "apply", app.ID, "They applied: application "+app.ID+" is with the operator.")
	// The setup result has done its job: the next application connects again.
	http.SetCookie(w, &http.Cookie{Name: landsGitHubResultCookie, Value: "", Path: s.basePath + "/lands/", MaxAge: -1, HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.basePath+"/lands/applications?submitted=1", http.StatusSeeOther)
}

func (s *Server) handleLandsApplications(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsApplyBase(r, "Your applications")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	status := s.fillLandsApplications(r, &v)
	switch {
	case r.URL.Query().Get("submitted") == "1" && len(v.Apply.Applications) > 0:
		v.Notice = "Thank you. Your application is with the operator."
		if v.Apply.Mail {
			v.Notice += " We have emailed you a copy, and we will email you again when they answer."
		}
	case r.URL.Query().Get("withdrawn") == "1":
		v.Notice = "Your application is withdrawn."
	}
	s.renderLands(w, status, "lands_applications", v)
}

// fillLandsApplications reads the caller's own applications into v.
func (s *Server) fillLandsApplications(r *http.Request, v *landsView) int {
	if s.landsGate == nil {
		v.Apply.State = "closed"
		return http.StatusServiceUnavailable
	}
	sub := oidcauth.SubjectFromContext(r.Context())
	if sub == "" {
		v.Apply.State = "nobearer"
		return http.StatusForbidden
	}
	ctx, cancel := s.landsGate.callCtx(r.Context())
	defer cancel()
	mine, err := s.landsGate.mine(ctx, sub)
	if err != nil {
		s.logger.Warn("lands applications: read failed", "error", err)
		v.Apply.State = "unavailable"
		return http.StatusBadGateway
	}
	v.Apply.State, v.Apply.Applications = "list", mine
	return http.StatusOK
}

func (s *Server) handleLandsWithdraw(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsApplyBase(r, "Your applications")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if s.landsGate == nil {
		v.Apply.State = "closed"
		s.renderLands(w, http.StatusServiceUnavailable, "lands_applications", v)
		return
	}
	sub := oidcauth.SubjectFromContext(r.Context())
	ctx, cancel := s.landsGate.callCtx(r.Context())
	defer cancel()
	_, err := s.landsGate.withdraw(ctx, sub, r.PathValue("id"))
	var limit *errLandsLimit
	var status int
	switch {
	case err == nil:
		http.Redirect(w, r, s.basePath+"/lands/applications?withdrawn=1", http.StatusSeeOther)
		return
	case errors.Is(err, errLandsNotYours):
		status = http.StatusNotFound
		v.Error = "We could not find that application among yours."
	case errors.As(err, &limit):
		status = limit.status
		v.Error = limit.msg
	default:
		s.logger.Warn("lands withdraw failed", "error", err)
		status = http.StatusBadGateway
		v.Error = "Your application could not be withdrawn. Nothing changed; try again in a minute."
	}
	if st := s.fillLandsApplications(r, &v); st != http.StatusOK {
		status = st
	}
	s.renderLands(w, status, "lands_applications", v)
}

// landsGateEnabledFromEnv reports whether the deployment turned the Gate on
// (LANDS_APPLY_ENABLED). Off by default: the rollout needs the
// gardenio-machine-lands-gate role on the viewer's client first.
func landsGateEnabledFromEnv() bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("LANDS_APPLY_ENABLED")))
	return v
}
