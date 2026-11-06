package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

var t0 = time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)

const (
	applicantSub = "0f6b1a8e-1111-4222-8333-444455556666"
	otherSub     = "0f6b1a8e-7777-4888-8999-aaaabbbbcccc"
)

// errCrash is every call after the simulated process death.
var errCrash = errors.New("crashed")

// world is the fakes' shared state: the daemon, LiteLLM and Keycloak. Every
// write counts, and crashAt kills the "process" at that write: the write is
// not applied and every later call, read or write, fails until revive.
type world struct {
	t      *testing.T
	beads  []*beadsapi.BeadDetail
	idem   map[string]string
	nextID int

	writes  int
	crashAt int
	dead    bool
	ops     []string // every applied write, in order
	fail    map[string]error

	keys   map[string]*fakeKey
	groups map[string]map[string]bool // "project-acme", "project-acme/owners", "lands-tenants" → users
	marked map[string]bool            // land groups the provisioner made
	// landScopes are the lands whose land-<land> client scope exists.
	landScopes map[string]bool
	ns         map[string]int // land → max_live_agents its namespace was applied with
	branch     string
	mail       *mailer.Recorder
	comment    []string

	users  []keycloakadmin.User // the realm's users, for FindUserByGitHubID and FindUserByEmail
	lookup func(id int64) (keycloakadmin.User, error)
	// emailLookup, when set, answers FindUserByEmail instead of users.
	emailLookup func(email string) (keycloakadmin.User, error)

	// stewardAge is how long before t0 a steward the fake binds was made;
	// StewardSettle by default, so a pass need not wait for trigger-engine.
	stewardAge time.Duration
	// upgraded are the open stewards Stewards.Upgrade was handed, land:id.
	upgraded []string
}

type fakeKey struct {
	alias  string
	budget float64
	meta   map[string]string
}

func newWorld(t *testing.T) *world {
	w := &world{
		t: t, idem: map[string]string{}, keys: map[string]*fakeKey{}, fail: map[string]error{},
		groups: map[string]map[string]bool{keycloakadmin.TenantsGroup: {}, keycloakadmin.InvitedGroup: {}}, marked: map[string]bool{}, landScopes: map[string]bool{}, ns: map[string]int{},
		branch: "trunk", mail: mailer.NewRecorder(), stewardAge: StewardSettle,
	}
	w.add("account", "fleet-template", "open", nil, map[string]string{
		"provider": "anthropic", "credential": "secret:fleet-upstream", "base_url": "http://litellm:4000",
		"model": "claude-opus-5-5", "litellm_key": "sk-fleet-template",
	})
	w.add("credit_rate", "Credit rate v1", "closed", nil, map[string]string{"version": "1", "llm_multiplier": "1.5"})
	return w
}

func (w *world) add(typ, title, status string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	w.nextID++
	if status == "" {
		status = "open"
	}
	if fields == nil {
		fields = map[string]string{}
	}
	b := &beadsapi.BeadDetail{
		ID: fmt.Sprintf("gd-%04d", w.nextID), Type: typ, Title: title, Status: status,
		Labels: labels, Fields: fields, CreatedAt: t0.Add(time.Duration(w.nextID) * time.Second),
	}
	w.beads = append(w.beads, b)
	return b
}

// application adds an approved plot application for land acme.
func (w *world) application(state string) *beadsapi.BeadDetail {
	return w.add(ApplicationType, "Plot application: acme", "open", []string{IntakeLabel}, map[string]string{
		"state": state, "land_name": "acme", "repo": "octo/acme-site", "applicant_sub": applicantSub,
		"applicant_email": "octo@example.com", "github_login": "octo", "github_id": "42",
		"github_installation_id": "9001", "credits_granted_usd": "50", "model": "claude-sonnet-5-5",
		"decision_id": "gd-dec1", "decided_by": "operator@example.com",
	})
}

func (w *world) read(op string) error {
	if w.dead {
		return errCrash
	}
	return w.fail[op]
}

func (w *world) write(op string) error {
	if w.dead {
		return errCrash
	}
	if err := w.fail[op]; err != nil {
		return err
	}
	w.writes++
	if w.crashAt > 0 && w.writes == w.crashAt {
		w.dead = true
		return errCrash
	}
	w.ops = append(w.ops, op)
	return nil
}

func (w *world) revive() { w.dead, w.crashAt = false, 0 }

func (w *world) bead(id string) *beadsapi.BeadDetail {
	for _, b := range w.beads {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (w *world) byType(typ string) []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, b := range w.beads {
		if b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

func (w *world) one(typ, title string) *beadsapi.BeadDetail {
	w.t.Helper()
	var found []*beadsapi.BeadDetail
	for _, b := range w.byType(typ) {
		if b.Title == title {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("%d %s beads titled %q, want 1", len(found), typ, title)
	}
	return found[0]
}

// stringify turns typed fields into the strings the daemon returns.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// ---- Daemon ----

type fakeDaemon struct{ w *world }

func (d fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if err := d.w.read("list"); err != nil {
		return nil, err
	}
	var match []*beadsapi.BeadDetail
	for _, b := range d.w.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if ok {
			match = append(match, b)
		}
	}
	if q.Offset >= len(match) {
		match = nil
	} else {
		match = match[q.Offset:]
	}
	return &beadsapi.ListBeadsResult{Beads: match, Total: len(match)}, nil
}

func (d fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if err := d.w.read("get"); err != nil {
		return nil, err
	}
	if b := d.w.bead(id); b != nil {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: 404}
}

func (d fakeDaemon) CreateBeadDetail(_ context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	if id, ok := d.w.idem[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		if err := d.w.read("create:" + req.Type); err != nil {
			return nil, err
		}
		return d.w.bead(id), nil
	}
	if err := d.w.write("create:" + req.Type); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	_ = json.Unmarshal(req.Fields, &raw)
	// Like kbeads (channel_binding_authz.go): the provisioner is no
	// gardenio-admin, so any Slack binding field on a channel create is a 403.
	if req.Type == "channel" {
		for _, k := range []string{"mirror", "slack_channel_id"} {
			if _, ok := raw[k]; ok {
				return nil, fmt.Errorf("HTTP 403: insufficient permissions: create on type channel setting %s requires gardenio-admin", k)
			}
		}
	}
	fields := map[string]string{}
	for k, v := range raw {
		fields[k] = stringify(v)
	}
	b := d.w.add(req.Type, req.Title, req.Status, req.Labels, fields)
	b.CreatedBy = req.CreatedBy
	if req.IdempotencyKey != "" {
		d.w.idem[req.IdempotencyKey] = b.ID
	}
	return b, nil
}

func (d fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	b := d.w.bead(id)
	op := "update"
	if b != nil {
		op += ":" + b.Type
		for _, k := range []string{"state", "provision_step", "close_step"} {
			if v, ok := fields[k]; ok {
				op += ":" + k + "=" + stringify(v)
			}
		}
	}
	if err := d.w.write(op); err != nil {
		return err
	}
	if b == nil {
		return &beadsapi.APIError{StatusCode: 404}
	}
	for k, v := range fields {
		b.Fields[k] = stringify(v)
	}
	return nil
}

func (d fakeDaemon) CloseBead(_ context.Context, id string, _ map[string]string) error {
	b := d.w.bead(id)
	if err := d.w.write("close:" + b.Type); err != nil {
		return err
	}
	b.Status = "closed"
	return nil
}

func (d fakeDaemon) AddComment(_ context.Context, id, _, text string) error {
	if err := d.w.write("comment"); err != nil {
		return err
	}
	d.w.comment = append(d.w.comment, id+": "+text)
	return nil
}

// ---- LiteLLM ----

type fakeKeys struct{ w *world }

func (k fakeKeys) Ensure(_ context.Context, key, alias string, budget float64, meta map[string]string) error {
	if _, ok := k.w.keys[key]; ok {
		return k.w.read("key:ensure")
	}
	if err := k.w.write("key:generate"); err != nil {
		return err
	}
	k.w.keys[key] = &fakeKey{alias: alias, budget: budget, meta: meta}
	return nil
}

func (k fakeKeys) SetBudget(_ context.Context, key string, usd float64) error {
	if err := k.w.write("key:budget"); err != nil {
		return err
	}
	fk, ok := k.w.keys[key]
	if !ok {
		return errKeyNotFound
	}
	fk.budget = usd
	return nil
}

func (k fakeKeys) Delete(_ context.Context, key string) error {
	if err := k.w.write("key:delete"); err != nil {
		return err
	}
	delete(k.w.keys, key)
	return nil
}

// ---- GitHub ----

type fakeRepos struct{ w *world }

func (r fakeRepos) DefaultBranch(_ context.Context, inst int64, repo string) (string, error) {
	if err := r.w.read("github"); err != nil {
		return "", err
	}
	if inst != 9001 || repo != "octo/acme-site" {
		return "", fmt.Errorf("unexpected repo %d %s", inst, repo)
	}
	return r.w.branch, nil
}

// ---- Kubernetes ----

type fakeNamespaces struct{ w *world }

// Apply is idempotent, as landns.Applier's is: applying a land already at
// maxLive writes nothing.
func (n fakeNamespaces) Apply(_ context.Context, land string, maxLive int) (string, error) {
	if cur, ok := n.w.ns[land]; ok && cur == maxLive {
		return "land-" + land, n.w.read("ns:apply")
	}
	if err := n.w.write("ns:apply"); err != nil {
		return "", err
	}
	n.w.ns[land] = maxLive
	return "land-" + land, nil
}

func (n fakeNamespaces) Delete(_ context.Context, land string) error {
	if err := n.w.write("ns:delete"); err != nil {
		return err
	}
	delete(n.w.ns, land)
	return nil
}

// ---- Steward ----

// fakeStewards binds a land's steward as a trigger bead in the world and
// wakes it with an idempotent post, both through the world's write counter.
type fakeStewards struct{ w *world }

func (s fakeStewards) Bind(_ context.Context, land, operator string) (*beadsapi.BeadDetail, error) {
	for _, b := range s.w.byType("trigger") {
		if slices.Contains(b.Labels, "gardener-land:"+land) && b.Status != "closed" {
			return b, s.w.read("steward:find")
		}
	}
	if err := s.w.write("steward:bind"); err != nil {
		return nil, err
	}
	b := s.w.add("trigger", "Steward ("+land+")", "open", []string{"gardener-land:" + land, "project:" + land},
		map[string]string{"operator": operator})
	b.CreatedAt = t0.Add(-s.w.stewardAge)
	return b, nil
}

func (s fakeStewards) Wake(_ context.Context, land, key string) error {
	if _, ok := s.w.idem[key]; ok {
		return s.w.read("steward:wake")
	}
	if err := s.w.write("steward:wake"); err != nil {
		return err
	}
	b := s.w.add("message", "Land "+land+" is open", "closed", []string{"project:" + land}, map[string]string{"origin": "land_opened"})
	s.w.idem[key] = b.ID
	return nil
}

// Greet posts the person-joined notice once per key, as steward.ProvisionHook
// does; name and role ride on the post for the tests to read.
func (s fakeStewards) Greet(_ context.Context, land, name, role, key string) error {
	if _, ok := s.w.idem[key]; ok {
		return s.w.read("steward:greet")
	}
	if err := s.w.write("steward:greet"); err != nil {
		return err
	}
	b := s.w.add("message", name+" joined "+land, "closed", []string{"project:" + land},
		map[string]string{"origin": "person_joined", "name": name, "role": role})
	s.w.idem[key] = b.ID
	return nil
}

// Upgrade records the open steward it was handed. The real one writes only a
// steward bound before a wake was added, which the fake's never are, so it
// is no write here.
func (s fakeStewards) Upgrade(_ context.Context, land string, trig *beadsapi.BeadDetail) error {
	if err := s.w.read("steward:upgrade"); err != nil {
		return err
	}
	s.w.upgraded = append(s.w.upgraded, land+":"+trig.ID)
	return nil
}

// greetings are the person-joined posts in land, oldest first.
func (w *world) greetings(land string) []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, b := range w.byType("message") {
		if b.Fields["origin"] == "person_joined" && slices.Contains(b.Labels, "project:"+land) {
			out = append(out, b)
		}
	}
	return out
}

// Retire disarms and closes the land's open steward, one write each, as
// steward.ProvisionHook does.
func (s fakeStewards) Retire(_ context.Context, land, why string) error {
	for _, b := range s.w.byType("trigger") {
		if b.Status != "open" || !slices.Contains(b.Labels, "gardener-land:"+land) {
			continue
		}
		if err := s.w.write("steward:disarm"); err != nil {
			return err
		}
		b.Fields["enabled"] = "false"
		if err := s.w.write("steward:close"); err != nil {
			return err
		}
		b.Status = "closed"
		s.w.comment = append(s.w.comment, b.ID+": "+why)
		return nil
	}
	return s.w.read("steward:find")
}

func (w *world) provisioner() *Provisioner {
	n := 0
	return &Provisioner{
		Daemon: fakeDaemon{w}, Keys: fakeKeys{w}, Groups: fakeGroups{w}, Repos: fakeRepos{w}, Namespaces: fakeNamespaces{w},
		Stewards:   fakeStewards{w},
		LandScopes: fakeLandScopes{w},
		Mailer:     w.mail, Templates: mailer.NewTemplates(mailer.Config{BaseURL: "https://gardenio.example"}),
		AccountTemplate: "fleet-template",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:             func() time.Time { return t0 },
		// Each provisioner (each "process") mints its own key values, as
		// a restarted controller would.
		NewKey: func() string { n++; return fmt.Sprintf("sk-lands-test-%p-%d", &n, n) },
	}
}

// snapshot is what a land amounts to, without ids, timestamps or key values,
// so a run that crashed and resumed can be compared with a clean one.
func (w *world) snapshot() string {
	var lines []string
	for _, b := range w.beads {
		var keys []string
		for k := range b.Fields {
			switch k {
			case "litellm_key", "land_default_channel", "land_opened_at", "land_closed_at",
				"provision_failed_at", "close_requested_at":
				keys = append(keys, k+"=<set>")
				continue
			}
			keys = append(keys, k+"="+b.Fields[k])
		}
		sort.Strings(keys)
		lines = append(lines, fmt.Sprintf("%s %q %s %v %s", b.Type, b.Title, b.Status, b.Labels, strings.Join(keys, " ")))
	}
	sort.Strings(lines)
	var keys []string
	for _, k := range w.keys {
		keys = append(keys, fmt.Sprintf("key %s budget=%.2f", k.alias, k.budget))
	}
	sort.Strings(keys)
	var groups []string
	for name, members := range w.groups {
		var ids []string
		for id := range members {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		groups = append(groups, fmt.Sprintf("group %s %v", name, ids))
	}
	sort.Strings(groups)
	for land, maxLive := range w.ns {
		groups = append(groups, fmt.Sprintf("namespace land-%s max_live_agents=%d", land, maxLive))
	}
	return strings.Join(append(append(lines, keys...), groups...), "\n")
}

func appOf(t *testing.T, w *world, id string) *Application {
	t.Helper()
	app, ok := ApplicationFromBead(w.bead(id))
	if !ok {
		t.Fatalf("%s is not an application", id)
	}
	return app
}

func pass(t *testing.T, p *Provisioner) {
	t.Helper()
	if err := p.Pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

// ---- provisioning ----

func TestProvision_ApprovedApplicationBecomesALand(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	p := w.provisioner()
	pass(t, p)

	app := appOf(t, w, appBead.ID)
	if app.State != StateProvisioned || app.Step != StepMail || app.Attempt != 1 || app.Error != "" {
		t.Fatalf("application = %+v", app)
	}
	if appBead.Fields["project"] != "acme" {
		t.Errorf("application project = %q", appBead.Fields["project"])
	}

	proj := w.one("project", "acme")
	want := map[string]string{
		"prefix": "acme", "git_url": "https://github.com/octo/acme-site.git", "default_branch": "trunk",
		"repos":     `[{"branch":"trunk","role":"primary","url":"https://github.com/octo/acme-site.git"}]`,
		"land_kind": "tenant", "land_owner_sub": applicantSub, "land_application_id": appBead.ID,
		"land_slack_mirror": "off", "github_app": "lands", "github_installation_id": "9001",
		"max_live_agents": "3", "triggers_enabled": "true", "schedules_enabled": "false",
		"model": "claude-sonnet-5-5", "account": "lands-acme", "land_opened_at": t0.Format(time.RFC3339),
		"namespace": "land-acme", "runtime_class": "",
	}
	for k, v := range want {
		if proj.Fields[k] != v {
			t.Errorf("project %s = %q, want %q", k, proj.Fields[k], v)
		}
	}
	if !slices.Equal(proj.Labels, []string{"project:acme"}) {
		t.Errorf("project labels = %v", proj.Labels)
	}

	acct := w.one("account", "lands-acme")
	// The template's credential is copied: the land's agents authenticate
	// upstream with it (owner ruling gd-NXTrUXD1Sq).
	if acct.Fields["provider"] != "anthropic" || acct.Fields["credential"] != "secret:fleet-upstream" ||
		acct.Fields["base_url"] != "http://litellm:4000" || acct.Fields["model"] != "claude-sonnet-5-5" {
		t.Errorf("account fields = %v", acct.Fields)
	}
	key := acct.Fields["litellm_key"]
	if key == "" || key == "sk-fleet-template" {
		t.Fatalf("account key = %q: want the land's own key", key)
	}
	fk := w.keys[key]
	if fk == nil || fk.alias != "lands-acme" || fk.budget != 33.33 || fk.meta["plot_application"] != appBead.ID {
		t.Fatalf("LiteLLM key = %+v, want alias lands-acme, max_budget 33.33 (50 credits / 1.5)", fk)
	}

	grants := w.byType("credit_grant")
	if len(grants) != 1 {
		t.Fatalf("%d grants", len(grants))
	}
	g := grants[0]
	if g.Status != "closed" || g.Fields["amount_credits"] != "50" || g.Fields["rate_version"] != "1" ||
		g.Fields["decision_id"] != "gd-dec1" || g.Fields["granted_by"] != "operator@example.com" ||
		!slices.Equal(g.Labels, []string{"project:acme"}) {
		t.Errorf("grant = %+v", g)
	}

	if !w.groups["project-acme/owners"][applicantSub] || !w.groups[keycloakadmin.TenantsGroup][applicantSub] {
		t.Errorf("applicant not owner and tenant: %v", w.groups)
	}
	if len(w.groups["project-acme"]) != 0 {
		t.Errorf("project-acme direct members = %v; the owner joins through owners", w.groups["project-acme"])
	}

	var names []string
	for _, ch := range w.byType("channel") {
		names = append(names, ch.Title+"/"+ch.Fields["channel_kind"]+"/"+ch.Fields["mirror"])
		if !slices.Equal(ch.Labels, []string{"project:acme"}) {
			t.Errorf("%s labels = %v", ch.Title, ch.Labels)
		}
		if ch.Title == "#general" && proj.Fields["land_default_channel"] != ch.ID {
			t.Errorf("land_default_channel = %q, want %s", proj.Fields["land_default_channel"], ch.ID)
		}
	}
	sort.Strings(names)
	if want := "#crew/crew/ #decisions/decisions/ #escalations/escalations/ #general/general/ #machines/machines/ #support/user/"; strings.Join(names, " ") != want {
		t.Errorf("channels = %v", names)
	}

	if w.ns["acme"] != DefaultMaxLiveAgents {
		t.Errorf("namespaces = %v, want land-acme applied with max_live_agents %d", w.ns, DefaultMaxLiveAgents)
	}

	if sent := w.mail.SentKind(mailer.KindLandReady); len(sent) != 1 || sent[0].To[0] != "octo@example.com" {
		t.Fatalf("land ready mail = %+v", sent)
	}
	if len(w.byType("escalation")) != 0 {
		t.Errorf("escalations filed on a clean run")
	}

	// A provisioned land is left alone.
	before := len(w.ops)
	pass(t, p)
	if len(w.ops) != before {
		t.Errorf("a second pass wrote %v", w.ops[before:])
	}
}

// §3.3 step 6: the land's steward is bound once, with the approver as its
// operator, and woken once, before the land is mailed as ready.
func TestProvision_BindsAndWakesTheSteward(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	p := w.provisioner()
	pass(t, p)

	trigs := w.byType("trigger")
	if len(trigs) != 1 || trigs[0].Fields["operator"] != "oidc:operator@example.com" {
		t.Fatalf("steward triggers = %+v, want one with the approver as operator", trigs)
	}
	wakes := w.byType("message")
	if len(wakes) != 1 || w.idem["lands-provision:"+appBead.ID+":steward-wake"] != wakes[0].ID {
		t.Fatalf("wake posts = %+v", wakes)
	}
	wake, mail := slices.Index(w.ops, "steward:wake"), slices.Index(w.ops, "update:plot_application:state=provisioned:provision_step=mail")
	if wake < 0 || mail < 0 || wake > mail {
		t.Fatalf("ops %v: want the steward woken before the land is provisioned", w.ops)
	}

	// A provisioned land is never bound or woken again.
	before := len(w.ops)
	pass(t, p)
	if len(w.ops) != before {
		t.Errorf("a second pass wrote %v", w.ops[before:])
	}
}

// A steward bound on this pass is woken on a later one, once trigger-engine
// has had time to load it: a wake posted at once could match no loaded
// trigger and be dropped.
func TestProvision_StewardWakeWaitsForTheEngine(t *testing.T) {
	w := newWorld(t)
	w.stewardAge = 0
	appBead := w.application(StateApproved)
	p := w.provisioner()
	pass(t, p)

	app := appOf(t, w, appBead.ID)
	if app.State != StateProvisioning || app.Step != StepNamespace || app.Error != "" {
		t.Fatalf("application = %+v, want provisioning, waiting after the namespace step", app)
	}
	if len(w.byType("trigger")) != 1 || len(w.byType("message")) != 0 || len(w.mail.SentKind(mailer.KindLandReady)) != 0 {
		t.Fatalf("want the steward bound, not yet woken, no mail: ops %v", w.ops)
	}
	if len(w.byType("escalation")) != 0 {
		t.Fatal("waiting for the engine is not a failure")
	}

	// Too soon still waits.
	p.Now = func() time.Time { return t0.Add(StewardSettle - time.Second) }
	pass(t, p)
	if len(w.byType("message")) != 0 {
		t.Fatal("woke the steward before it settled")
	}

	p.Now = func() time.Time { return t0.Add(StewardSettle) }
	pass(t, p)
	app = appOf(t, w, appBead.ID)
	if app.State != StateProvisioned || app.Attempt != 1 {
		t.Fatalf("application = %+v", app)
	}
	if len(w.byType("trigger")) != 1 || len(w.byType("message")) != 1 {
		t.Fatalf("want one steward woken once: ops %v", w.ops)
	}
}

func TestProvision_NoStewardHookFails(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	p := w.provisioner()
	p.Stewards = nil
	pass(t, p)
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || app.Step != StepNamespace || !strings.HasPrefix(app.Error, "steward: ") {
		t.Fatalf("application = %+v", app)
	}
	if len(w.mail.SentKind(mailer.KindLandReady)) != 0 {
		t.Fatal("a land without its steward was mailed as ready")
	}
}

func TestProvision_StepsRunInOrder(t *testing.T) {
	w := newWorld(t)
	w.application(StateApproved)
	pass(t, w.provisioner())
	var steps []string
	for _, op := range w.ops {
		if _, step, ok := strings.Cut(op, "provision_step="); ok {
			steps = append(steps, step)
		}
	}
	if !slices.Equal(steps, Steps) {
		t.Fatalf("recorded steps %v, want %v", steps, Steps)
	}
}

// A crash at any write, which covers a crash between each pair of steps
// (before and after provision_step is recorded) and inside each step, is
// finished by the next pass, and the land is the one a clean run makes.
func TestProvision_CrashAtEveryWriteResumes(t *testing.T) {
	clean := newWorld(t)
	clean.application(StateApproved)
	pass(t, clean.provisioner())
	want, total := clean.snapshot(), clean.writes

	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("write%02d", n), func(t *testing.T) {
			w := newWorld(t)
			w.application(StateApproved)
			w.crashAt = n
			_ = w.provisioner().Pass(context.Background())
			if !w.dead {
				t.Fatalf("no crash at write %d", n)
			}
			crashedOn := w.ops
			w.revive()
			p := w.provisioner()
			pass(t, p)
			pass(t, p)
			if got := w.snapshot(); got != want {
				t.Fatalf("after a crash at write %d (after %v):\n%s\nwant:\n%s", n, crashedOn, got, want)
			}
			if mails := len(w.mail.SentKind(mailer.KindLandReady)); mails < 1 || mails > 2 {
				t.Fatalf("%d land ready mails", mails)
			}
		})
	}
}

func TestProvision_FailedStepEscalatesAndRetryResumes(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	delete(w.groups, keycloakadmin.TenantsGroup) // the chart group is not rolled out yet
	p := w.provisioner()
	pass(t, p)

	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || app.Step != StepPurse || !strings.HasPrefix(app.Error, "keycloak: ") {
		t.Fatalf("application = %+v", app)
	}
	escs := w.byType("escalation")
	if len(escs) != 1 {
		t.Fatalf("%d escalations", len(escs))
	}
	esc := escs[0]
	if esc.Fields["requesting_task"] != appBead.ID || esc.Fields["operation_type"] != "lands_provision" ||
		!slices.Equal(esc.Labels, []string{IntakeLabel}) || appBead.Fields["provision_escalation_id"] != esc.ID ||
		!strings.Contains(esc.Fields["context"], "gb lands provision --retry "+appBead.ID) {
		t.Fatalf("escalation = %+v", esc)
	}

	// Failed stays failed: no pass retries on its own.
	before := len(w.ops)
	pass(t, p)
	if len(w.ops) != before {
		t.Fatalf("a pass touched a failed application: %v", w.ops[before:])
	}

	w.groups[keycloakadmin.TenantsGroup] = map[string]bool{}
	if err := Retry(context.Background(), fakeDaemon{w}, app); err != nil {
		t.Fatal(err)
	}
	if err := Retry(context.Background(), fakeDaemon{w}, appOf(t, w, appBead.ID)); err == nil {
		t.Fatal("retrying a provisioning application must be refused")
	}
	before = len(w.ops)
	pass(t, p)
	app = appOf(t, w, appBead.ID)
	if app.State != StateProvisioned || app.Attempt != 2 || app.Error != "" {
		t.Fatalf("after retry: %+v", app)
	}
	for _, op := range w.ops[before:] {
		if op == "create:project" || op == "key:generate" || op == "create:credit_grant" {
			t.Errorf("the retry redid %s; it resumes after provision_step", op)
		}
	}
	if !w.groups[keycloakadmin.TenantsGroup][applicantSub] {
		t.Error("applicant not a tenant after retry")
	}
}

// TestProvision_RefusedStepRecordFails: when kbeads refuses the provision_step
// record itself (HTTP 400, as it did for "namespace" on gd-AQ9iCiKxML), the
// next pass would refuse it again, so the attempt fails and escalates instead
// of re-running the step every pass with no error showing anywhere. A
// transient refusal still just waits for the next pass.
func TestProvision_RefusedStepRecordFails(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	op := "update:plot_application:provision_step=" + StepNamespace
	w.fail[op] = &beadsapi.APIError{StatusCode: 503, Message: "unavailable"}
	p := w.provisioner()
	pass(t, p)
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioning || app.Step != StepChannels || len(w.byType("escalation")) != 0 {
		t.Fatalf("control: a transient refusal must wait for the next pass, got %+v", app)
	}

	w.fail[op] = &beadsapi.APIError{StatusCode: 400, Message: "invalid fields: validation failed: provision_step: must be one of [project purse keycloak channels mail]"}
	pass(t, p)
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || app.Step != StepChannels || !strings.HasPrefix(app.Error, StepNamespace+": ") ||
		!strings.Contains(app.Error, "provision_step") {
		t.Fatalf("application = %+v", app)
	}
	if escs := w.byType("escalation"); len(escs) != 1 {
		t.Fatalf("%d escalations, want 1", len(escs))
	}
}

func TestProvision_NoCreditRateFailsClosed(t *testing.T) {
	w := newWorld(t)
	for _, b := range w.byType("credit_rate") {
		b.Type = "deleted"
	}
	appBead := w.application(StateApproved)
	pass(t, w.provisioner())
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || !strings.Contains(app.Error, "no credit rate") {
		t.Fatalf("application = %+v", app)
	}
	if len(w.keys) != 0 || len(w.byType("credit_grant")) != 0 || len(w.byType("account")) != 1 {
		t.Fatalf("a purse was made without a rate: keys %v", w.keys)
	}
}

func TestProvision_NameTakenByAnotherProjectFails(t *testing.T) {
	w := newWorld(t)
	w.add("project", "acme", "open", nil, map[string]string{"land_application_id": "gd-other", "git_url": "https://example.com/x.git"})
	appBead := w.application(StateApproved)
	pass(t, w.provisioner())
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || app.Step != "" || !strings.HasPrefix(app.Error, "project: ") {
		t.Fatalf("application = %+v", app)
	}
	// The only groups are the chart's two, lands-tenants and invited.
	if len(w.byType("project")) != 1 || len(w.keys) != 0 || len(w.groups) != 2 {
		t.Fatal("the provisioner built on another land's name")
	}
}

// TestProvision_KeycloakGuardsFailClosed guards gd-G9ZfREbe8z: a Keycloak
// refusal (an unmarked or reserved land group) fails the application closed,
// joins no one to lands-tenants and escalates.
func TestProvision_KeycloakGuardsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *world)
		want  error
	}{
		{"unmarked group", func(w *world) { w.groups["project-acme"] = map[string]bool{} }, keycloakadmin.ErrNotLandGroup},
		{"reserved", func(w *world) { w.fail["kc:group"] = keycloakadmin.ErrReservedLand }, keycloakadmin.ErrReservedLand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			appBead := w.application(StateApproved)
			pass(t, w.provisioner())
			app := appOf(t, w, appBead.ID)
			if app.State != StateFailed || !strings.HasPrefix(app.Error, "keycloak: ") || !strings.Contains(app.Error, tc.want.Error()) {
				t.Fatalf("application = %+v, want a keycloak failure carrying %v", app, tc.want)
			}
			if w.groups[keycloakadmin.TenantsGroup][applicantSub] {
				t.Fatal("applicant joined lands-tenants despite the refusal")
			}
			if len(w.byType("escalation")) != 1 {
				t.Fatal("no escalation")
			}
		})
	}
}

func TestProvision_InvalidApplicationFailsBeforeAnyWrite(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	appBead.Fields["repo"] = "not-a-repo"
	p := w.provisioner()
	p.ReservedLands = nil
	pass(t, p)
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || !strings.Contains(app.Error, "owner/name") {
		t.Fatalf("application = %+v", app)
	}
	if len(w.byType("project")) != 0 {
		t.Fatal("a project was made for an invalid application")
	}

	w2 := newWorld(t)
	b2 := w2.application(StateApproved)
	p2 := w2.provisioner()
	p2.ReservedLands = []string{"acme"}
	pass(t, p2)
	if app := appOf(t, w2, b2.ID); app.State != StateFailed || !strings.Contains(app.Error, "reserved") {
		t.Fatalf("reserved land: %+v", app)
	}
}

func TestProvision_ZeroGrantWritesNoLedgerEntry(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	appBead.Fields["credits_granted_usd"] = "0"
	pass(t, w.provisioner())
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioned {
		t.Fatalf("application = %+v", app)
	}
	if len(w.byType("credit_grant")) != 0 {
		t.Fatal("a zero grant was written")
	}
	key := w.one("account", "lands-acme").Fields["litellm_key"]
	if w.keys[key].budget != 0 {
		t.Fatalf("budget = %v, want 0", w.keys[key].budget)
	}
}

func TestPass_SkipsOtherStates(t *testing.T) {
	w := newWorld(t)
	for _, s := range []string{"submitted", "denied", "withdrawn", StateFailed, StateClosed} {
		w.application(s)
	}
	pass(t, w.provisioner())
	if len(w.ops) != 0 {
		t.Fatalf("pass wrote %v", w.ops)
	}
}

// A land provisioned before per-land audiences (gd-eAfptUBh9Z) gets its
// land-<land> client scope on the first pass after a start, and only that;
// a later pass by the same process does not ask Keycloak again, and a
// failure is retried on the next pass without touching the application.
func TestPass_BackfillsLandScopeOfProvisionedLand(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateProvisioned)
	w.fail["kc:scope"] = errors.New("keycloak down")
	p := w.provisioner()
	pass(t, p)
	if w.landScopes["acme"] || len(w.ops) != 0 {
		t.Fatalf("failed backfill: scopes %v, ops %v", w.landScopes, w.ops)
	}
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioned {
		t.Fatalf("a failed backfill changed the application: %+v", app)
	}
	delete(w.fail, "kc:scope")
	pass(t, p)
	if !w.landScopes["acme"] || !slices.Equal(w.ops, []string{"kc:scope"}) {
		t.Fatalf("backfill: scopes %v, ops %v, want [kc:scope]", w.landScopes, w.ops)
	}
	w.fail["kc:scope-ensure"] = errors.New("must not be asked again")
	pass(t, p)
	if !slices.Equal(w.ops, []string{"kc:scope"}) {
		t.Fatalf("second pass wrote %v", w.ops)
	}
}

// A new land gets its client scope in the Keycloak step.
func TestProvision_CreatesLandScope(t *testing.T) {
	w := newWorld(t)
	w.application(StateApproved)
	pass(t, w.provisioner())
	if !w.landScopes["acme"] {
		t.Fatal("provisioned land has no land-acme client scope")
	}
}

// The land scope never fails a land (PR #862 review: the provisioner's
// Keycloak credential holds only query-users and query-groups, and the scope
// is a KeycloakClientScope resource now). A land whose scope cannot be made,
// because the resource write is refused or there is no writer at all, still
// opens; the scope arrives by backfill once the write succeeds.
func TestProvision_LandScopeFailureDoesNotFailTheLand(t *testing.T) {
	for name, setup := range map[string]func(*world, *Provisioner){
		"write refused": func(w *world, _ *Provisioner) {
			w.fail["kc:scope"] = errors.New(`keycloakclientscopes.v1.edp.epam.com is forbidden`)
		},
		"no writer": func(_ *world, p *Provisioner) { p.LandScopes = nil },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			appBead := w.application(StateApproved)
			p := w.provisioner()
			setup(w, p)
			pass(t, p)
			if app := appOf(t, w, appBead.ID); app.State != StateProvisioned {
				t.Fatalf("state = %s, want %s", app.State, StateProvisioned)
			}
			if w.landScopes["acme"] {
				t.Fatal("scope made despite the setup")
			}
			delete(w.fail, "kc:scope")
			p.LandScopes = fakeLandScopes{w}
			pass(t, p)
			if !w.landScopes["acme"] {
				t.Fatal("backfill did not make the scope once it could")
			}
		})
	}
}

// ---- closing ----

// provisioned returns a world holding the provisioned land acme with a
// running agent, a member who holds no other land and one who does.
func provisioned(t *testing.T) (*world, *beadsapi.BeadDetail) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	pass(t, w.provisioner())
	if appBead.Fields["state"] != StateProvisioned {
		t.Fatalf("setup: %v", appBead.Fields)
	}
	w.groups["project-acme"][otherSub] = true
	w.groups[keycloakadmin.TenantsGroup][otherSub] = true
	w.groups["project-beta"] = map[string]bool{otherSub: true}
	w.add("agent", "acme-crew-1", "open", []string{"project:acme"}, map[string]string{"agent": "acme-crew-1", "agent_state": "working", "project": "acme"})
	w.add("agent", "acme-crew-2", "open", []string{"project:acme"}, map[string]string{"agent": "acme-crew-2", "agent_state": "done", "project": "acme"})
	w.add("agent", "gd-crew", "open", []string{"project:gardenio"}, map[string]string{"agent": "gd-crew", "agent_state": "working", "project": "gardenio"})
	w.ops = nil
	return w, appBead
}

func requestClose(t *testing.T, w *world, id string) {
	t.Helper()
	if err := RequestClose(context.Background(), fakeDaemon{w}, appOf(t, w, id), "operator@example.com", "abuse report", t0); err != nil {
		t.Fatal(err)
	}
}

func TestClose_TakesTheLandApartInReverse(t *testing.T) {
	w, appBead := provisioned(t)
	key := w.one("account", "lands-acme").Fields["litellm_key"]
	requestClose(t, w, appBead.ID)
	w.ops = nil
	pass(t, w.provisioner())

	app := appOf(t, w, appBead.ID)
	if app.State != StateClosed || app.CloseStep != CloseMail || app.Error != "" {
		t.Fatalf("application = %+v", app)
	}
	proj := w.one("project", "acme")
	if proj.Status != "closed" || proj.Fields["land_frozen"] != "true" || proj.Fields["land_closed_at"] == "" {
		t.Errorf("project = %s %v", proj.Status, proj.Fields)
	}
	acct := w.one("account", "lands-acme")
	if acct.Status != "closed" || acct.Fields["litellm_key"] != "" || w.keys[key] != nil {
		t.Errorf("account %s key %q, LiteLLM still has it: %v", acct.Status, acct.Fields["litellm_key"], w.keys[key] != nil)
	}
	for _, a := range w.byType("agent") {
		want := map[string]string{"acme-crew-1": "stopping", "acme-crew-2": "done", "gd-crew": "working"}[a.Title]
		if a.Fields["agent_state"] != want {
			t.Errorf("%s agent_state = %q, want %q", a.Title, a.Fields["agent_state"], want)
		}
	}
	if len(w.groups["project-acme"]) != 0 || len(w.groups["project-acme/owners"]) != 0 {
		t.Errorf("acme groups not emptied: %v", w.groups)
	}
	if w.groups[keycloakadmin.TenantsGroup][applicantSub] {
		t.Error("the owner holds no land now but is still a tenant")
	}
	if !w.groups[keycloakadmin.TenantsGroup][otherSub] || !w.groups["project-beta"][otherSub] {
		t.Error("a member who holds another land lost it, or lost lands-tenants")
	}
	if _, ok := w.ns["acme"]; ok {
		t.Error("the land namespace was not deleted")
	}
	if trig := w.one("trigger", "Steward (acme)"); trig.Status != "closed" || trig.Fields["enabled"] != "false" {
		t.Errorf("the steward is %s enabled=%q, want retired: its digest cron would keep firing", trig.Status, trig.Fields["enabled"])
	}
	if sent := w.mail.SentKind(mailer.KindLandClosed); len(sent) != 1 || !strings.Contains(sent[0].Text, "abuse report") {
		t.Fatalf("closed mail = %+v", sent)
	}

	// The order: freeze, steward, agents, members, key, namespace, project, mail.
	var order []string
	for _, op := range w.ops {
		var step string
		switch {
		case strings.HasPrefix(op, "update:project") && len(order) == 0:
			step = CloseFreeze
		case strings.HasPrefix(op, "steward:"):
			step = CloseSteward
		case op == "update:agent":
			step = CloseAgents
		case op == "kc:remove":
			step = CloseMembers
		case op == "key:delete":
			step = CloseKey
		case op == "ns:delete":
			step = CloseNamespace
		case op == "close:project":
			step = CloseProject
		case strings.Contains(op, "state=closed"):
			step = CloseMail
		}
		if step != "" && (len(order) == 0 || order[len(order)-1] != step) {
			order = append(order, step)
		}
	}
	if !slices.Equal(order, CloseSteps) {
		t.Fatalf("close order %v, want %v (ops %v)", order, CloseSteps, w.ops)
	}

	before := len(w.ops)
	pass(t, w.provisioner())
	if len(w.ops) != before {
		t.Fatalf("a closed land was touched again: %v", w.ops[before:])
	}
}

func TestClose_CrashAtEveryWriteResumes(t *testing.T) {
	clean, cleanApp := provisioned(t)
	requestClose(t, clean, cleanApp.ID)
	start := clean.writes
	pass(t, clean.provisioner())
	want, total := clean.snapshot(), clean.writes-start

	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("write%02d", n), func(t *testing.T) {
			w, appBead := provisioned(t)
			requestClose(t, w, appBead.ID)
			w.crashAt = w.writes + n
			_ = w.provisioner().Pass(context.Background())
			if !w.dead {
				t.Fatalf("no crash at write %d", n)
			}
			w.revive()
			pass(t, w.provisioner())
			pass(t, w.provisioner())
			if got := w.snapshot(); got != want {
				t.Fatalf("after a crash at close write %d:\n%s\nwant:\n%s", n, got, want)
			}
		})
	}
}

func TestClose_NeverTouchesAnotherApplicationsProject(t *testing.T) {
	w := newWorld(t)
	w.add("project", "acme", "open", nil, map[string]string{"land_application_id": "gd-other"})
	appBead := w.application(StateFailed)
	requestClose(t, w, appBead.ID)
	w.ops = nil
	pass(t, w.provisioner())
	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || !strings.HasPrefix(app.Error, "close freeze: ") {
		t.Fatalf("application = %+v", app)
	}
	if p := w.one("project", "acme"); p.Status != "open" || p.Fields["land_frozen"] != "" {
		t.Fatalf("another application's project was touched: %v", p.Fields)
	}
	escs := w.byType("escalation")
	if len(escs) != 1 || escs[0].Fields["operation_type"] != "lands_close" {
		t.Fatalf("escalations = %+v", escs)
	}
	// A failed close waits for the operator to ask again.
	before := len(w.ops)
	pass(t, w.provisioner())
	if len(w.ops) != before {
		t.Fatalf("a failed close retried on its own: %v", w.ops[before:])
	}
}

// A land whose Keycloak step is recorded but whose group the lookup cannot
// find must not close as if it never reached Keycloak: under hidden list
// calls (gd-7peNzGjJ3C) that would leave every member in place.
func TestClose_MissingGroupAfterKeycloakStepFails(t *testing.T) {
	w, appBead := provisioned(t)
	delete(w.groups, "project-acme")
	requestClose(t, w, appBead.ID)
	pass(t, w.provisioner())
	app := appOf(t, w, appBead.ID)
	if app.State != StateProvisioned || app.CloseStep != CloseAgents || !strings.HasPrefix(app.Error, "close members: ") ||
		!strings.Contains(app.Error, "group not found") {
		t.Fatalf("application = %+v", app)
	}
	if escs := w.byType("escalation"); len(escs) != 1 || escs[0].Fields["operation_type"] != "lands_close" {
		t.Fatalf("escalations = %+v", escs)
	}
}

// A controller with no steward hook must not close a land past its steward
// step as if it had none: the steward's digest would keep firing.
func TestClose_NoStewardHook(t *testing.T) {
	w, appBead := provisioned(t)
	requestClose(t, w, appBead.ID)
	p := w.provisioner()
	p.Stewards = nil
	pass(t, p)
	app := appOf(t, w, appBead.ID)
	if app.State != StateProvisioned || app.CloseStep != CloseFreeze || !strings.HasPrefix(app.Error, "close steward: ") ||
		!strings.Contains(app.Error, "no steward hook") {
		t.Fatalf("application = %+v", app)
	}
	if trig := w.one("trigger", "Steward (acme)"); trig.Status != "open" {
		t.Fatalf("steward = %s", trig.Status)
	}

	// A land that never reached the steward step closes without one.
	w = newWorld(t)
	appBead = w.application(StateApproved)
	w.fail["ns:apply"] = errors.New("quota")
	pass(t, w.provisioner())
	if app := appOf(t, w, appBead.ID); app.State != StateFailed || app.Step != StepChannels {
		t.Fatalf("setup: application = %+v", app)
	}
	requestClose(t, w, appBead.ID)
	p = w.provisioner()
	p.Stewards = nil
	pass(t, p)
	if app := appOf(t, w, appBead.ID); app.State != StateClosed || app.Error != "" {
		t.Fatalf("application = %+v", app)
	}
}

func TestClose_ApprovedButNeverProvisioned(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	requestClose(t, w, appBead.ID)
	pass(t, w.provisioner())
	if app := appOf(t, w, appBead.ID); app.State != StateClosed {
		t.Fatalf("application = %+v", app)
	}
	if len(w.byType("project")) != 0 || len(w.mail.Sent()) != 0 {
		t.Fatal("closing an approval provisioned or mailed something")
	}
}

func TestClose_WaitsForAProvisioningApplication(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateProvisioning)
	appBead.Fields["close_requested_at"] = t0.Format(time.RFC3339)
	pass(t, w.provisioner())
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioning || app.CloseStep != "" {
		t.Fatalf("application = %+v", app)
	}
	if len(w.ops) != 0 {
		t.Fatalf("pass wrote %v", w.ops)
	}
}

// ---- requests ----

func TestRequestClose_Refusals(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for state, ok := range map[string]bool{
		StateApproved: true, StateProvisioned: true, StateFailed: true,
		StateProvisioning: false, StateClosed: false, "submitted": false, "denied": false,
	} {
		b := w.application(state)
		err := RequestClose(ctx, fakeDaemon{w}, appOf(t, w, b.ID), "op@example.com", "", t0)
		if (err == nil) != ok {
			t.Errorf("state %s: err %v, want ok=%v", state, err, ok)
		}
	}
	b := w.application(StateProvisioned)
	if err := RequestClose(ctx, fakeDaemon{w}, appOf(t, w, b.ID), " ", "", t0); err == nil {
		t.Error("a close with no operator must be refused")
	}
}

func TestFind_ByIDOrLandName(t *testing.T) {
	w := newWorld(t)
	w.application("denied")
	old := w.application(StateClosed)
	newest := w.application(StateProvisioned)
	ctx := context.Background()
	if app, err := Find(ctx, fakeDaemon{w}, old.ID); err != nil || app.ID != old.ID {
		t.Fatalf("by id: %+v, %v", app, err)
	}
	if app, err := Find(ctx, fakeDaemon{w}, "acme"); err != nil || app.ID != newest.ID {
		t.Fatalf("by land: %+v, %v", app, err)
	}
	if _, err := Find(ctx, fakeDaemon{w}, "nope"); !errors.Is(err, ErrNoApplication) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := Find(ctx, fakeDaemon{w}, w.one("account", "fleet-template").ID); !errors.Is(err, ErrNoApplication) {
		t.Fatalf("another type: %v", err)
	}
}

// ---- the land namespace (step 10.2) ----

func TestProvision_NamespacesOffFailsBeforeTheLandOpens(t *testing.T) {
	w := newWorld(t)
	appBead := w.application(StateApproved)
	p := w.provisioner()
	p.Namespaces = nil
	pass(t, p)

	app := appOf(t, w, appBead.ID)
	if app.State != StateFailed || app.Step != StepChannels || !strings.HasPrefix(app.Error, "namespace: ") {
		t.Fatalf("application = %+v", app)
	}
	proj := w.one("project", "acme")
	if proj.Fields["land_opened_at"] != "" || proj.Fields["namespace"] != "" {
		t.Errorf("a land with no namespace opened: %v", proj.Fields)
	}
	if len(w.mail.SentKind(mailer.KindLandReady)) != 0 {
		t.Error("land ready mail sent for a land with no namespace")
	}
	if len(w.byType("escalation")) != 1 {
		t.Errorf("%d escalations, want 1", len(w.byType("escalation")))
	}

	// With namespaces on, the retry resumes at the namespace step.
	if err := Retry(context.Background(), fakeDaemon{w}, app); err != nil {
		t.Fatal(err)
	}
	pass(t, w.provisioner())
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioned {
		t.Fatalf("after retry: %+v", app)
	}
	if proj := w.one("project", "acme"); proj.Fields["namespace"] != "land-acme" || proj.Fields["runtime_class"] != "" {
		t.Errorf("project placement = %q %q", proj.Fields["namespace"], proj.Fields["runtime_class"])
	}
}

func TestProvision_NamespaceQuotaFollowsMaxLiveAgents(t *testing.T) {
	w := newWorld(t)
	w.application(StateApproved)
	p := w.provisioner()
	// Stop after the project exists, raise its cap, then finish.
	w.fail["ns:apply"] = errors.New("not yet")
	pass(t, p)
	proj := w.one("project", "acme")
	proj.Fields["max_live_agents"] = "5"
	delete(w.fail, "ns:apply")
	app := appOf(t, w, w.one(ApplicationType, "Plot application: acme").ID)
	if err := Retry(context.Background(), fakeDaemon{w}, app); err != nil {
		t.Fatal(err)
	}
	pass(t, p)
	if w.ns["acme"] != 5 {
		t.Errorf("namespace applied with max_live_agents %d, want 5", w.ns["acme"])
	}
}

// A provisioned land's namespace is applied again on the first pass after a
// controller start and then every NamespaceReconcile, so a raised
// max_live_agents or a hand edit to the fence reaches it (gd-pXik57wZA9); a
// failed reconcile never touches the application.
func TestPass_ReconcilesProvisionedLandNamespace(t *testing.T) {
	w, appBead := provisioned(t)
	now := t0
	p := w.provisioner()
	p.Now = func() time.Time { return now }

	// A new process re-applies a namespace that drifted.
	w.ns["acme"] = 99
	pass(t, p)
	if w.ns["acme"] != DefaultMaxLiveAgents {
		t.Fatalf("after a start: namespace max_live_agents %d, want %d", w.ns["acme"], DefaultMaxLiveAgents)
	}
	// The same process leaves it until NamespaceReconcile has passed.
	w.one("project", "acme").Fields["max_live_agents"] = "5"
	now = now.Add(NamespaceReconcile - time.Minute)
	pass(t, p)
	if w.ns["acme"] != DefaultMaxLiveAgents {
		t.Fatalf("re-applied within NamespaceReconcile: %d", w.ns["acme"])
	}
	now = now.Add(time.Minute)
	pass(t, p)
	if w.ns["acme"] != 5 {
		t.Fatalf("after NamespaceReconcile: namespace max_live_agents %d, want 5", w.ns["acme"])
	}

	// A failure is retried on the next pass and leaves the application be.
	w.ns["acme"] = 99
	w.fail["ns:apply"] = errors.New("apiserver down")
	p = w.provisioner()
	pass(t, p)
	if app := appOf(t, w, appBead.ID); app.State != StateProvisioned || app.Error != "" {
		t.Fatalf("a failed reconcile changed the application: %+v", app)
	}
	delete(w.fail, "ns:apply")
	pass(t, p)
	if w.ns["acme"] != 5 {
		t.Fatalf("reconcile not retried: namespace max_live_agents %d, want 5", w.ns["acme"])
	}
}

// The namespace step stamps the project's runtime_class from the
// provisioner's RuntimeClass (LANDS_RUNTIME_CLASS): empty by default, runsc
// when gVisor is selected. A project already stamped with the setting is not
// rewritten, and one stamped under the other setting is brought to it once.
func TestProvision_NamespaceStepStampsRuntimeClass(t *testing.T) {
	updates := func(w *world) int {
		n := 0
		for _, op := range w.ops {
			if op == "update:project" {
				n++
			}
		}
		return n
	}
	for _, c := range []struct{ from, to string }{{"", ""}, {"runsc", "runsc"}, {"runsc", ""}, {"", "runsc"}} {
		t.Run(c.from+"->"+c.to, func(t *testing.T) {
			w := newWorld(t)
			appBead := w.application(StateApproved)
			p := w.provisioner()
			p.RuntimeClass = c.from
			pass(t, p)
			if got := w.one("project", "acme").Fields["runtime_class"]; got != c.from {
				t.Fatalf("provisioned with runtime_class %q, want %q", got, c.from)
			}
			app := appOf(t, w, appBead.ID)
			p.RuntimeClass = c.to
			for range 2 {
				w.ops = nil
				if err := p.stepNamespace(context.Background(), app); err != nil {
					t.Fatal(err)
				}
				if got := w.one("project", "acme").Fields["runtime_class"]; got != c.to {
					t.Fatalf("runtime_class = %q, want %q", got, c.to)
				}
			}
			// The second run found the project at the setting and wrote nothing.
			if n := updates(w); n != 0 {
				t.Errorf("namespace step rewrote a project already at runtime_class %q (%d writes)", c.to, n)
			}
		})
	}
}

func TestClose_NamespacesOff(t *testing.T) {
	// A land that has a namespace cannot be closed by a controller that
	// cannot delete it.
	w, appBead := provisioned(t)
	requestClose(t, w, appBead.ID)
	p := w.provisioner()
	p.Namespaces = nil
	pass(t, p)
	app := appOf(t, w, appBead.ID)
	if app.State == StateClosed || app.CloseStep != CloseKey || !strings.Contains(app.Error, "close namespace") {
		t.Fatalf("application = %+v", app)
	}
	if _, ok := w.ns["acme"]; !ok {
		t.Error("namespace gone")
	}

	// An approval that never reached the namespace step closes without one.
	w = newWorld(t)
	appBead = w.application(StateApproved)
	requestClose(t, w, appBead.ID)
	p = w.provisioner()
	p.Namespaces = nil
	pass(t, p)
	if app := appOf(t, w, appBead.ID); app.State != StateClosed {
		t.Fatalf("application = %+v", app)
	}
}
