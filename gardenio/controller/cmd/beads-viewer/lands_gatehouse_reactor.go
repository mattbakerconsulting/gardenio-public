package main

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// The gatehouse reactor (task gd-K62coPtKkX, decision D4 gd-pbDt7wyQ3z
// apply-and-demo). The gatehouse's steward takes a newcomer's ask in
// conversation and files a type=gatehouse_request naming the message they
// wrote (gb lands gatehouse demo|apply). This reactor answers it, as the
// Gate's machine principal, and is the only thing that turns a request into a
// pass or a draft. It trusts nothing the steward wrote about who the person
// is: the steward reads free text from strangers, and carries the
// untrusted-input taint.
//
//   - The person is the GitHub account kbeads stamped on the message from the
//     verified token (author_github_id, author_github_login; kbeads
//     gatehouse_request.go), and the user bead of the message's verified
//     writer (created_by) for their terms. A message that is not in the
//     gatehouse, is older than gatehouseMessageMaxAge, or carries no stamp is
//     refused.
//   - demo: the demo door's own checks (lands_demo.go): the current terms
//     accepted, one pass per GitHub account, LANDS_DEMO_MAX_GUESTS open
//     passes; the request is then a draft. Nothing is filed: /lands/demo
//     offers the person the pass, quoting their message, and only their own
//     press files it (which marks the draft done). So a pass for a message
//     the steward was led to name joins nobody, mails nobody and takes no
//     place under the cap (gd-EaTO1lFPAG).
//   - apply: the form's own checks on what the steward took down
//     (parseLandsApply) and a land name nobody holds; the request is then a
//     draft. The person still proves they control the repository by
//     connecting it (lands_github.go), which no conversation can do: GET
//     /lands/apply quotes their message and offers the draft, filling the
//     form only on their "use this draft", and their own Apply files the
//     application and its decision through the unchanged Gate (the owner's
//     card is the same), which marks the draft done.
//
// kbeads holds the steward to one request per (message, kind) and a bound an
// hour (gatehouse_request.go admitGatehouseRequest), and a request to one
// answer (gatehouseAnswerOnceError): with more than one viewer replica, the
// reactor that answers second is refused and logs it (gd-GPvddfQY6y).
//
// It runs only while the gatehouse project carries land_gatehouse_tools=true,
// the switch the operator turns on after the 10.14 security review
// (gd-Ahv1F4wPUO). Off, requests stay pending and nothing is filed; kbeads
// refuses the steward's create anyway.

const (
	gatehouseRequestType = "gatehouse_request"
	// gatehouseToolsField is the gatehouse project's switch.
	gatehouseToolsField = "land_gatehouse_tools"
	// gatehouseReactorEvery is the sweep period. The steward's gb verb waits
	// for the answer, so it is short.
	gatehouseReactorEvery = 10 * time.Second
	// gatehouseMessageMaxAge is how old the person's message may be: the
	// steward acts on what someone just asked, not on anything they ever said.
	gatehouseMessageMaxAge = 24 * time.Hour
	// gatehouseDraftMaxAge is how long a draft is offered.
	gatehouseDraftMaxAge = 7 * 24 * time.Hour
	// gatehouseQuoteMax is how much of the person's message a draft quotes.
	gatehouseQuoteMax = 500
)

// Gatehouse request states (kbeads gatehouse_request.go).
const (
	gatehousePending = "pending"
	gatehouseDrafted = "drafted"
	gatehouseDone    = "done"
	gatehouseRefused = "refused"
)

// gatehouseReactor answers the gatehouse steward's requests. nil when the
// gatehouse is not configured or the viewer has no machine principal.
type gatehouseReactor struct {
	land    string
	daemon  landInvitesAPI
	machine beadsapi.TokenSource
	demo    *landsDemo // nil: no demo land
	gate    *landsGate // nil: applications closed
	terms   string     // the current terms version
	every   time.Duration
	now     func() time.Time
	logger  *slog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
}

func newGatehouseReactor(land string, daemon landInvitesAPI, machine beadsapi.TokenSource, demo *landsDemo, gate *landsGate, terms string, logger *slog.Logger) *gatehouseReactor {
	if land == "" || daemon == nil || machine == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &gatehouseReactor{land: land, daemon: daemon, machine: machine, demo: demo, gate: gate, terms: terms,
		every: gatehouseReactorEvery, now: time.Now, logger: logger}
}

// gatehouseReactorFor is the viewer's gatehouse reactor (main), nil without
// a gatehouse land or a machine principal. Without the GitHub App the form
// cannot take the repository, so the reactor drafts no applications.
func gatehouseReactorFor(srv *Server, daemon landInvitesAPI, logger *slog.Logger) *gatehouseReactor {
	if srv.landsGatehouse == "" || srv.machinePrincipal == nil {
		return nil
	}
	gate := srv.landsGate
	if srv.landsGitHub == nil {
		gate = nil
	}
	terms := ""
	if legal, err := currentLandsLegal(); err == nil {
		terms = legal.Version
	}
	return newGatehouseReactor(srv.landsGatehouse, daemon, srv.machinePrincipal, srv.landsDemo, gate, terms, logger)
}

// Start begins sweeping: once now, then every period. Safe on nil.
func (r *gatehouseReactor) Start() {
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
			r.sweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	r.logger.Info("lands gatehouse reactor started", "land", r.land, "every", r.every)
}

// Close stops the sweep and waits for the one in flight. Safe on nil.
func (r *gatehouseReactor) Close() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// callCtx is one call's context: the parent's cancellation and the machine
// principal, and none of a request's identity (landsGate.callCtx).
func (r *gatehouseReactor) callCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), landsGateCallTimeout)
	stop := context.AfterFunc(parent, cancel)
	return beadsapi.WithMachinePrincipal(ctx, r.machine), func() { stop(); cancel() }
}

// toolsOn reports whether the gatehouse project carries the switch.
func (r *gatehouseReactor) toolsOn(ctx context.Context) (bool, error) {
	proj, tenant, err := purse.Project(beadsapi.WithSystemRead(ctx), r.daemon, r.land)
	if err != nil {
		return false, err
	}
	return tenant && proj.Fields["land_gatehouse"] == "true" && proj.Fields[gatehouseToolsField] == "true", nil
}

// sweep answers every pending request once. It returns how many it answered.
func (r *gatehouseReactor) sweep(parent context.Context) int {
	ctx, cancel := r.callCtx(parent)
	defer cancel()
	on, err := r.toolsOn(ctx)
	if err != nil {
		r.logger.Warn("lands gatehouse reactor: reading the gatehouse failed", "land", r.land, "error", err)
		return 0
	}
	if !on {
		return 0
	}
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{gatehouseRequestType}, Labels: []string{"project:" + r.land},
		Fields: map[string]string{"state": gatehousePending}, Limit: landsGateListLimit,
	})
	if err != nil {
		r.logger.Warn("lands gatehouse reactor: listing requests failed", "error", err)
		return 0
	}
	n := 0
	for _, b := range res.Beads {
		if parent.Err() != nil {
			break
		}
		if b == nil || b.Type != gatehouseRequestType || b.Fields["state"] != gatehousePending || !slices.Contains(b.Labels, "project:"+r.land) {
			continue
		}
		out := r.answer(ctx, b)
		fields := map[string]any{"state": out.state, "outcome": out.outcome, "handled_at": r.now().UTC().Format(time.RFC3339)}
		if out.resultID != "" {
			fields["result_id"] = out.resultID
		}
		if out.githubID > 0 {
			fields["github_id"], fields["github_login"] = strconv.FormatInt(out.githubID, 10), out.login
		}
		if err := r.daemon.UpdateBeadFieldsTyped(ctx, b.ID, fields); err != nil {
			r.logger.Warn("lands gatehouse reactor: recording the answer failed", "request", b.ID, "error", err)
			continue
		}
		r.logger.Info("lands gatehouse request answered", "request", b.ID, "kind", b.Fields["kind"], "state", out.state, "result", out.resultID, "github_id", out.githubID)
		n++
	}
	return n
}

// gatehouseAnswer is what a request comes to.
type gatehouseAnswer struct {
	state, outcome, resultID string
	githubID                 int64
	login                    string
}

func refuse(why string) gatehouseAnswer {
	return gatehouseAnswer{state: gatehouseRefused, outcome: why}
}

// gatehousePerson is the person a request is for, as the daemon verified them.
type gatehousePerson struct {
	githubID int64
	login    string
	email    string
}

// answer works one request out. It never returns an error: what went wrong
// is the outcome the steward reads and passes on.
func (r *gatehouseReactor) answer(ctx context.Context, req *beadsapi.BeadDetail) gatehouseAnswer {
	who, why := r.person(ctx, req.Fields["message_id"])
	if why != "" {
		return refuse(why)
	}
	var out gatehouseAnswer
	switch req.Fields["kind"] {
	case "demo":
		out = r.demoDraft(ctx, who)
	case "apply":
		out = r.draft(ctx, req)
	default:
		return refuse("unknown request kind " + strconv.Quote(req.Fields["kind"]))
	}
	out.githubID, out.login = who.githubID, who.login
	return out
}

// person reads the message a request names and returns who wrote it, or why
// it will not do.
func (r *gatehouseReactor) person(ctx context.Context, messageID string) (gatehousePerson, string) {
	const notThisMessage = "I can only act on a message the person wrote in the gatehouse while signed in with GitHub, in the last day. Point them to /lands/apply or /lands/demo instead."
	if messageID == "" {
		return gatehousePerson{}, notThisMessage
	}
	m, err := r.daemon.GetBead(ctx, messageID)
	if err != nil || m == nil || m.Type != "message" || !slices.Contains(m.Labels, "project:"+r.land) {
		return gatehousePerson{}, notThisMessage
	}
	if m.CreatedAt.IsZero() || r.now().Sub(m.CreatedAt) > gatehouseMessageMaxAge {
		return gatehousePerson{}, notThisMessage
	}
	id, err := strconv.ParseInt(m.Fields["author_github_id"], 10, 64)
	login := m.Fields["author_github_login"]
	email := strings.TrimPrefix(m.CreatedBy, "oidc:")
	if err != nil || id <= 0 || !landsDemoLogin.MatchString(login) || !strings.Contains(email, "@") {
		return gatehousePerson{}, notThisMessage
	}
	who := gatehousePerson{githubID: id, login: login, email: email}
	accepted, err := r.termsOf(ctx, email)
	switch {
	case err != nil:
		r.logger.Warn("lands gatehouse reactor: reading the person's terms failed", "message", messageID, "error", err)
		return who, "I could not check their terms just now. Ask them to try /lands/demo or /lands/apply, or try again in a minute."
	case r.terms == "" || accepted != r.terms:
		return who, "They need to read and accept the current terms first, at /lands/accept. Then ask again."
	}
	return who, ""
}

// termsOf is the terms version the person with this email accepted, read
// from their user bead.
func (r *gatehouseReactor) termsOf(ctx context.Context, email string) (string, error) {
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"user"}, Fields: map[string]string{"email": email}, Limit: 5,
	})
	if err != nil {
		return "", err
	}
	for _, b := range res.Beads {
		if b != nil && b.Type == "user" && strings.EqualFold(b.Fields["email"], email) {
			return b.Fields["terms_version"], nil
		}
	}
	return "", nil
}

// demoDraft checks the person could take a guest pass at the demo door and
// leaves the request as a draft for their own ask there. It files nothing.
func (r *gatehouseReactor) demoDraft(ctx context.Context, who gatehousePerson) gatehouseAnswer {
	if r.demo == nil {
		return refuse("There is no demo land at the moment.")
	}
	switch open, err := r.demo.open(ctx); {
	case err != nil:
		r.logger.Warn("lands gatehouse reactor: reading the demo land failed", "error", err)
		return refuse("I could not reach the demo land just now. Ask them to try /lands/demo in a minute.")
	case !open:
		return refuse("There is no demo land at the moment.")
	}
	n, mine, err := r.demo.passes(ctx, who.githubID)
	switch {
	case err != nil:
		r.logger.Warn("lands gatehouse reactor: reading demo passes failed", "error", err)
		return refuse("I could not reach the demo land just now. Ask them to try /lands/demo in a minute.")
	case mine != nil:
		return gatehouseAnswer{state: gatehouseDone, resultID: mine.ID,
			outcome: "They already hold a guest pass to the demo land " + r.demo.land + ". /lands/demo shows them the gb login steps."}
	case n >= r.demo.maxGuests:
		return refuse("Every demo guest pass is taken. Ask them to try again later.")
	}
	return gatehouseAnswer{state: gatehouseDrafted, outcome: "Ready for them. Ask them to open /lands/demo and press Get a guest pass: nothing is filed until they do. " +
		"The pass to the demo land " + r.demo.land + " lasts " + landsDemoDuration(r.demo.guestTTL) + " from when it is ready."}
}

// draft checks an application request as the form would and leaves it as a
// draft for the person's own Apply.
func (r *gatehouseReactor) draft(ctx context.Context, req *beadsapi.BeadDetail) gatehouseAnswer {
	if r.gate == nil {
		return refuse("Applications are closed at the moment.")
	}
	in, msg := parseLandsApply(url.Values{
		"land_name": {req.Fields["land_name"]}, "purpose": {req.Fields["purpose"]}, "credits": {req.Fields["credits_requested_usd"]},
	}.Get)
	if msg != "" {
		return refuse(msg)
	}
	taken, err := r.gate.list(ctx, "land_name", in.LandName)
	if err != nil {
		r.logger.Warn("lands gatehouse reactor: reading applications failed", "error", err)
		return refuse("I could not check the land name just now. Try again in a minute.")
	}
	for _, a := range taken {
		if slices.Contains(landsClaimingStates, a.State) {
			return refuse("The land name " + in.LandName + " is taken. Ask them for another.")
		}
	}
	return gatehouseAnswer{state: gatehouseDrafted, outcome: "Drafted. Ask them to open /lands/apply, connect the GitHub repository the land will work on, " +
		"choose to use the draft, check the form and press Apply. The operator then decides."}
}

// draftFor is the newest draft of this kind for this GitHub account, or nil:
// what /lands/apply and /lands/demo offer.
func (r *gatehouseReactor) draftFor(ctx context.Context, githubID int64, kind string) *beadsapi.BeadDetail {
	if r == nil || githubID <= 0 {
		return nil
	}
	cctx, cancel := r.callCtx(ctx)
	defer cancel()
	id := strconv.FormatInt(githubID, 10)
	res, err := r.daemon.ListBeadsFiltered(cctx, beadsapi.ListBeadsQuery{
		Types: []string{gatehouseRequestType}, Labels: []string{"project:" + r.land},
		Fields: map[string]string{"state": gatehouseDrafted, "github_id": id, "kind": kind}, Limit: landsGateListLimit,
	})
	if err != nil {
		r.logger.Warn("lands gatehouse: reading drafts failed", "error", err)
		return nil
	}
	var newest *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Type != gatehouseRequestType || b.Fields["state"] != gatehouseDrafted || b.Fields["github_id"] != id ||
			b.Fields["kind"] != kind || !slices.Contains(b.Labels, "project:"+r.land) || r.now().Sub(b.CreatedAt) > gatehouseDraftMaxAge {
			continue
		}
		if newest == nil || b.CreatedAt.After(newest.CreatedAt) {
			newest = b
		}
	}
	return newest
}

// gatehouseOffer is a draft as the person's page shows it: the words of
// theirs it came from, and for an application what the steward took down.
type gatehouseOffer struct {
	Quote string
	Input landsApplyInput
}

// offerFor is the newest draft of this kind for this GitHub account with the
// message it came from, or nil. A draft whose message is gone or was not
// written by this account is not offered: the quote is how the person checks
// the draft is theirs.
func (r *gatehouseReactor) offerFor(ctx context.Context, githubID int64, kind string) *gatehouseOffer {
	d := r.draftFor(ctx, githubID, kind)
	if d == nil {
		return nil
	}
	cctx, cancel := r.callCtx(ctx)
	defer cancel()
	m, err := r.daemon.GetBead(cctx, d.Fields["message_id"])
	if err != nil || m == nil || m.Type != "message" || !slices.Contains(m.Labels, "project:"+r.land) ||
		m.Fields["author_github_id"] != strconv.FormatInt(githubID, 10) {
		return nil
	}
	quote := strings.TrimSpace(m.Fields["text"])
	if quote == "" {
		quote = strings.TrimSpace(m.Description)
	}
	if quote == "" {
		return nil
	}
	if rs := []rune(quote); len(rs) > gatehouseQuoteMax {
		quote = string(rs[:gatehouseQuoteMax]) + "…"
	}
	return &gatehouseOffer{Quote: quote,
		Input: landsApplyInput{LandName: d.Fields["land_name"], Purpose: d.Fields["purpose"], Credits: d.Fields["credits_requested_usd"]}}
}

// settle marks the account's drafts of this kind done once the person has
// acted on them.
func (r *gatehouseReactor) settle(ctx context.Context, githubID int64, kind, resultID, outcome string) {
	for range 10 {
		d := r.draftFor(ctx, githubID, kind)
		if d == nil {
			return
		}
		cctx, cancel := r.callCtx(ctx)
		err := r.daemon.UpdateBeadFieldsTyped(cctx, d.ID, map[string]any{
			"state": gatehouseDone, "result_id": resultID, "outcome": outcome,
		})
		cancel()
		if err != nil {
			r.logger.Warn("lands gatehouse: marking the draft done failed", "request", d.ID, "error", err)
			return
		}
	}
}
