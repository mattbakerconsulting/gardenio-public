package main

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailer"
)

// The apply reactor (Track B spec §2.5, step 7.7): it applies each resolved
// approval decision to its plot application and mails the applicant.
//
// It sweeps rather than listens. Every landsApplyReactorEvery it lists the
// applications still submitted and reads each one's decision; a decision that
// is closed with an answer moves the application, and one closed without a
// usable answer is filed again. A sweep needs no backfill after a restart or
// a dropped stream, and the set it reads is small: §2.4 caps submissions at
// the daily limit, and an application leaves the set as soon as it is
// answered. The operator answers in minutes or days, so a half-minute lag is
// not felt.
//
// What the answer does:
//
//   - approve: state approved, decided_by and decided_at from the answer,
//     credits_granted_usd from "credits=<n>" in the reply or the default, and
//     model from "model=opus" or Sonnet 5.5 (OQ3 gd-Se3rF0T5pu). Phase 8's
//     provisioner picks it up; until then approved is a resting state.
//   - deny: state denied, the reply as deny_reason.
//   - ask: the reply as question and asked_at; the application stays
//     submitted and a fresh decision is filed, so the operator can still
//     approve or deny once the applicant answers by email.
//
// Whose answer counts (gd-ekhj3MJjEa). Every registered machine service
// account, every OIDC human with project scope and the Slack bridge relaying
// whoever clicked may resolve a decision, and an approval grants up to
// landsApplyCreditsMax. So the reactor first checks the decision is the one
// the application names and still asks what the Gate filed
// (verifyLandsDecision against the application's decision_digest; created_by
// is never read, since X-Actor forges it), then acts only on an answer the
// daemon's resolve stamped
// (kbeads resolver_subject / resolver_admin, which no client can write) as
// given by LANDS_APPROVER or by a human session holding gardenio-admin. Any
// other answer is logged and the application gets a fresh decision whose
// context says why; responded_by alone is never trusted, since a relaying
// caller shapes it.

// landsApplyReactorEvery is the sweep period.
const landsApplyReactorEvery = 30 * time.Second

// LandsApplyReactor runs the sweep. A nil *LandsApplyReactor is safe: Start and
// Close are no-ops, so a viewer without the Gate runs without it.
type LandsApplyReactor struct {
	gate   *landsGate
	every  time.Duration
	logger *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

// NewLandsApplyReactor returns nil when the Gate is not configured.
func NewLandsApplyReactor(g *landsGate, logger *slog.Logger) *LandsApplyReactor {
	if g == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LandsApplyReactor{gate: g, every: landsApplyReactorEvery, logger: logger}
}

// Start begins sweeping: once now, then every period.
func (r *LandsApplyReactor) Start() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.every)
		defer t.Stop()
		for {
			r.gate.sweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	r.logger.Info("lands apply reactor started", "every", r.every)
}

// Close stops the sweep and waits for the one in flight.
func (r *LandsApplyReactor) Close() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// sweep reacts to every submitted application once. It returns how many
// applications it moved or re-filed, for tests.
func (g *landsGate) sweep(parent context.Context) int {
	ctx, cancel := g.callCtx(parent)
	defer cancel()
	submitted, err := g.list(ctx, "state", landsAppSubmitted)
	if err != nil {
		g.logger.Warn("lands apply reactor: listing submitted applications failed", "error", err)
		return 0
	}
	moved := 0
	for _, app := range submitted {
		if parent.Err() != nil {
			break
		}
		if g.react(ctx, app) {
			moved++
		}
	}
	return moved
}

// landsAnswer is a resolved decision's answer.
type landsAnswer struct {
	choice string // approve | deny | ask | "" (unusable)
	text   string
	by     string // the daemon-stamped resolver_subject, not responded_by
	at     string
}

// landsAnswerChoices are the decision's option ids.
var landsAnswerChoices = map[string]bool{"approve": true, "deny": true, "ask": true}

// readLandsAnswer reads the answer off a closed decision. A free-text reply
// with no option counts when its first word names one ("approve credits=50").
func readLandsAnswer(f map[string]string) landsAnswer {
	a := landsAnswer{text: strings.TrimSpace(f["response_text"]), by: strings.TrimSpace(f["resolver_subject"]), at: f["responded_at"]}
	chosen := strings.ToLower(strings.TrimSpace(f["chosen"]))
	if landsAnswerChoices[chosen] {
		a.choice = chosen
		return a
	}
	if chosen != "" {
		return a // dismissed, _expired, or an option this Gate never offered
	}
	word, rest, _ := strings.Cut(a.text, " ")
	word = strings.ToLower(strings.TrimRight(word, ":,.!"))
	if landsAnswerChoices[word] {
		a.choice, a.text = word, strings.TrimSpace(rest)
	}
	return a
}

var (
	// "credits=500", "credits: 500" and "credits 500", or "500 credits" / "$500 credits".
	landsCreditsRe      = regexp.MustCompile(`(?i)\bcredits\s*[=:]?\s*\$?(\d+(?:\.\d+)?)`)
	landsCreditsAfterRe = regexp.MustCompile(`(?i)\$?(\d+(?:\.\d+)?)\s*credits\b`)
	// "model=opus", "model: opus" and "model opus".
	landsModelRe = regexp.MustCompile(`(?i)\bmodel\s*[=:]?\s*(claude-opus-5-5|claude-sonnet-5-5|opus|sonnet)\b`)
)

// approvalTerms reads credits=<n> and model=opus|sonnet from an approval's
// reply, written either that way or as "500 credits" / "model opus", falling
// back to the default grant and Sonnet 5.5.
func (g *landsGate) approvalTerms(text string) (credits float64, model string) {
	credits, model = g.defaultCredits, landsModelSonnet
	m := landsCreditsRe.FindStringSubmatch(text)
	if m == nil {
		m = landsCreditsAfterRe.FindStringSubmatch(text)
	}
	if m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			credits = min(f, landsApplyCreditsMax)
		}
	}
	if m := landsModelRe.FindStringSubmatch(text); m != nil && strings.Contains(strings.ToLower(m[1]), "opus") {
		model = landsModelOpus
	}
	return credits, model
}

// landsPrincipal normalises a principal for comparison: an OIDC session's
// subject is the bare email, while LANDS_APPROVER and a relayed operator read
// "oidc:<email>".
func landsPrincipal(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "oidc:")
}

// answerRefused says why the reactor must not act on a's decision, or "" when
// the daemon recorded it as answered by LANDS_APPROVER or a gardenio-admin.
func (g *landsGate) answerRefused(f map[string]string, a landsAnswer) string {
	switch {
	case a.by == "":
		return "the daemon recorded no resolver: the answer was not given through a decision resolve"
	case f["resolver_admin"] == "true":
		return ""
	case g.approver != "" && landsPrincipal(a.by) == landsPrincipal(g.approver):
		return ""
	}
	return a.by + " is neither " + landsApproverEnv + " nor a gardenio-admin"
}

// react applies app's decision if it is resolved. It reports whether the
// application moved or got a fresh decision.
func (g *landsGate) react(ctx context.Context, app *landsApplication) bool {
	if app.State != landsAppSubmitted {
		return false
	}
	if app.DecisionID == "" {
		return g.refile(ctx, app, "", "no decision yet", "")
	}
	d, err := g.intake.GetBead(ctx, app.DecisionID)
	if err != nil {
		g.logger.Warn("lands apply reactor: reading the decision failed", "application", app.ID, "decision", app.DecisionID, "error", err)
		return false
	}
	if why := verifyLandsDecision(app, d, app.DecisionDigest); why != "" {
		g.logger.Warn("lands apply reactor: refused the application's decision", "application", app.ID, "decision", d.ID, "why", why)
		if d.Status == "open" {
			// Answering it would do nothing; take it off the operator's list.
			if err := g.intake.CancelDecision(ctx, d.ID, "the Lands Gate refused it: "+why, "lands-gate"); err != nil {
				g.logger.Warn("lands apply reactor: cancelling a refused decision failed", "application", app.ID, "decision", d.ID, "error", err)
			}
		}
		return g.refile(ctx, app, d.ID, "refused decision: "+why,
			"The Gate refused this application's previous decision "+d.ID+" ("+why+"), and no answer on it counts. Answer this decision instead.")
	}
	if d.Status != "closed" {
		return false
	}
	a := readLandsAnswer(d.Fields)
	if a.choice != "" {
		if why := g.answerRefused(d.Fields, a); why != "" {
			g.logger.Warn("lands apply reactor: refused an answer", "application", app.ID, "decision", d.ID, "chosen", a.choice, "why", why)
			return g.refile(ctx, app, d.ID, "answer refused: "+why,
				"The answer on "+d.ID+" ("+a.choice+") was not acted on: "+why+". Only "+orDash(g.approver)+" or a gardenio-admin can answer.")
		}
	}
	decidedAt := a.at
	if _, err := time.Parse(time.RFC3339, decidedAt); err != nil {
		decidedAt = g.now().UTC().Format(time.RFC3339)
	}
	switch a.choice {
	case "approve":
		credits, model := g.approvalTerms(a.text)
		if !g.update(ctx, app, map[string]any{
			"state": landsAppApproved, "decided_by": a.by, "decided_at": decidedAt,
			"credits_granted_usd": credits, "model": model,
		}) {
			return false
		}
		app.State, app.CreditsGranted, app.Model = landsAppApproved, credits, model
		g.logger.Info("lands application approved", "application", app.ID, "land", app.LandName, "credits", credits, "model", model, "by", a.by)
		g.sendMail(ctx, mailer.KindApplicationApproved, app, "")
		return true
	case "deny":
		if !g.update(ctx, app, map[string]any{
			"state": landsAppDenied, "decided_by": a.by, "decided_at": decidedAt, "deny_reason": a.text,
		}) {
			return false
		}
		app.State, app.DenyReason = landsAppDenied, a.text
		g.logger.Info("lands application denied", "application", app.ID, "land", app.LandName, "by", a.by)
		g.sendMail(ctx, mailer.KindApplicationDenied, app, a.text)
		return true
	case "ask":
		if a.text == "" {
			// Nothing to send the applicant: ask again rather than mail an empty question.
			return g.refile(ctx, app, d.ID, "ask without a question", "")
		}
		if !g.update(ctx, app, map[string]any{"question": a.text, "asked_at": decidedAt}) {
			return false
		}
		app.Question, app.AskedAt = a.text, decidedAt
		g.logger.Info("lands application: the operator asked a question", "application", app.ID, "land", app.LandName, "by", a.by)
		g.sendMail(ctx, mailer.KindApplicationQuestion, app, a.text)
		return g.refile(ctx, app, d.ID, "asked", "")
	}
	note := ""
	if strings.TrimSpace(d.Fields["chosen"]) == "" && a.text != "" {
		// A reply with no option was not acted on; say so, or the operator
		// sees their answer vanish and a fresh decision appear.
		note = "The reply on " + d.ID + " (\"" + a.text + "\") picked no option, so nothing was done. Choose Approve, Deny or Ask; a reply without an option only counts when it starts with one of those words."
	}
	return g.refile(ctx, app, d.ID, "closed without approve, deny or ask", note)
}

func (g *landsGate) update(ctx context.Context, app *landsApplication, fields map[string]any) bool {
	if err := g.intake.UpdateBeadFieldsTyped(ctx, app.ID, fields); err != nil {
		g.logger.Warn("lands apply reactor: updating the application failed", "application", app.ID, "error", err)
		return false
	}
	return true
}

// refile files a fresh decision for app, replacing after. note heads the
// operator's context when the old answer was refused.
func (g *landsGate) refile(ctx context.Context, app *landsApplication, after, why, note string) bool {
	id, err := g.fileDecision(ctx, app, after, note)
	if err != nil {
		g.logger.Warn("lands apply reactor: filing a decision failed", "application", app.ID, "after", after, "why", why, "error", err)
		return false
	}
	g.logger.Info("lands apply reactor: filed a decision", "application", app.ID, "decision", id, "after", after, "why", why)
	return true
}

// Compile-time check: the daemon client is what main.go hands the Gate.
var _ landsIntakeAPI = (*beadsapi.Client)(nil)
