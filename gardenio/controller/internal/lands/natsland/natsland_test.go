package natsland

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"gardenio/controller/internal/lands/landtoken"
)

var testKey = []byte("test-land-key")

// landServer runs a config-mode nats-server shaped like the chart's
// subjectIsolation + landCallout config: infra and agent bypass the callout,
// every other user goes to it. The callout serves lands in lands, each at
// epoch 0; a test changes c.Land to serve other epochs.
func landServer(t *testing.T, lands ...string) (url string, c *Callout) {
	t.Helper()
	issuer, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := issuer.PublicKey()
	opts := &natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		Users: []*natsserver.User{
			{Username: "infra", Password: "infra-pw"},
			{Username: "agent", Password: "agent-pw", Permissions: &natsserver.Permissions{
				Publish: &natsserver.SubjectPermission{Allow: []string{"coop.events.>"}},
			}},
		},
		AuthCallout: &natsserver.AuthCallout{Issuer: pub, AuthUsers: []string{"infra", "agent"}},
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	c = &Callout{
		Issuer: issuer,
		Key:    func() ([]byte, error) { return testKey, nil },
		Land:   func(l string) (uint64, bool) { return 0, slices.Contains(lands, l) },
	}
	infra := connect(t, ns.ClientURL(), "infra", "infra-pw")
	if _, err := c.Subscribe(infra); err != nil {
		t.Fatal(err)
	}
	// The subscription reaches the server before any land connects.
	if err := infra.Flush(); err != nil {
		t.Fatal(err)
	}
	return ns.ClientURL(), c
}

func connect(t *testing.T, url, user, pass string) *nats.Conn {
	t.Helper()
	nc, err := dial(url, user, pass, nil)
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func dial(url, user, pass string, errs chan<- error) (*nats.Conn, error) {
	opts := []nats.Option{nats.UserInfo(user, pass), nats.NoReconnect(), nats.Timeout(3 * time.Second)}
	if errs != nil {
		opts = append(opts, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	}
	return nats.Connect(url, opts...)
}

func landToken(t *testing.T, land string) string {
	t.Helper()
	return landTokenAt(t, land, 0)
}

func landTokenAt(t *testing.T, land string, epoch uint64) string {
	t.Helper()
	tok, err := landtoken.MintNATS(testKey, land, epoch)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A land's user publishes under its own prefix, which infra receives, and
// nowhere else: not under another land's, not on the fleet's coop.events
// subjects, and it may not subscribe at all.
func TestCallout_LandUserIsScopedToItsPrefix(t *testing.T) {
	url, _ := landServer(t, "acme", "beta")
	infra := connect(t, url, "infra", "infra-pw")
	got, err := infra.SubscribeSync(">")
	if err != nil {
		t.Fatal(err)
	}
	if err := infra.Flush(); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 8)
	acme, err := dial(url, UserName("acme"), landToken(t, "acme"), errs)
	if err != nil {
		t.Fatalf("land acme refused: %v", err)
	}
	defer acme.Close()

	own := EventPrefix("acme") + ".state"
	for _, subj := range []string{EventPrefix("beta") + ".state", "coop.events.state", "coop.events.messages.boss.assistant_text", "hooks.acme.PreToolUse", "coop.mux.session.x.input", own} {
		if err := acme.Publish(subj, []byte(subj)); err != nil {
			t.Fatal(err)
		}
	}
	if err := acme.Flush(); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for {
		msg, err := got.NextMsg(300 * time.Millisecond)
		if err != nil {
			break
		}
		if !strings.HasPrefix(msg.Subject, "$SYS.") && !strings.HasPrefix(msg.Subject, "_INBOX.") {
			seen = append(seen, msg.Subject)
		}
	}
	if len(seen) != 1 || seen[0] != own {
		t.Errorf("infra saw %v from land acme, want only %s", seen, own)
	}

	if _, err := acme.SubscribeSync("coop.events.>"); err != nil {
		t.Fatal(err)
	}
	_ = acme.Flush()
	select {
	case err := <-errs:
		if !errors.Is(err, nats.ErrPermissionViolation) && !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
			t.Errorf("land error = %v, want a permissions violation", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("land acme subscribed with no permissions violation")
	}
}

// The callout refuses a wrong token, a token for another land, a user that is
// not a land user, and a land that is no longer a tenant land.
func TestCallout_Refuses(t *testing.T) {
	url, _ := landServer(t, "acme", "beta")
	for name, tc := range map[string]struct{ user, pass string }{
		"no token":            {UserName("acme"), ""},
		"coopmux token":       {UserName("acme"), mustMint(t, landtoken.Mint, "acme")},
		"another land's":      {UserName("acme"), landToken(t, "beta")},
		"not a land user":     {"agent2", landToken(t, "acme")},
		"drained land":        {UserName("gone"), landToken(t, "gone")},
		"fleet user, no pass": {"agent", landToken(t, "acme")},
	} {
		t.Run(name, func(t *testing.T) {
			nc, err := dial(url, tc.user, tc.pass, nil)
			if err == nil {
				nc.Close()
				t.Fatalf("%s/%q connected", tc.user, tc.pass)
			}
			if !errors.Is(err, nats.ErrAuthorization) {
				t.Errorf("%s: err = %v, want an authorization violation", name, err)
			}
		})
	}
}

// Bumping one land's epoch refuses its old password at the next connect and
// leaves every other land's alone (gd-zo4W7Q8Yno).
func TestCallout_EpochRevokesOneLand(t *testing.T) {
	url, c := landServer(t)
	var mu sync.Mutex
	epochs := map[string]uint64{"acme": 0, "beta": 0}
	c.Land = func(l string) (uint64, bool) {
		mu.Lock()
		defer mu.Unlock()
		e, ok := epochs[l]
		return e, ok
	}
	old := landToken(t, "acme")
	connect(t, url, UserName("acme"), old)

	mu.Lock()
	epochs["acme"] = 1
	mu.Unlock()
	if nc, err := dial(url, UserName("acme"), old, nil); err == nil {
		nc.Close()
		t.Fatal("acme's epoch 0 password connected after the bump to 1")
	} else if !errors.Is(err, nats.ErrAuthorization) {
		t.Fatalf("acme's revoked password: err = %v, want an authorization violation", err)
	}
	connect(t, url, UserName("acme"), landTokenAt(t, "acme", 1))
	connect(t, url, UserName("beta"), landToken(t, "beta"))
	if nc, err := dial(url, UserName("acme"), landTokenAt(t, "acme", 2), nil); err == nil {
		nc.Close()
		t.Fatal("acme's epoch 2 password connected at epoch 1")
	}
}

// infra and agent keep their configured passwords and never reach the callout.
func TestCallout_FleetUsersBypass(t *testing.T) {
	url, c := landServer(t)
	c.Key = func() ([]byte, error) { return nil, errors.New("callout consulted") }
	connect(t, url, "infra", "infra-pw")
	connect(t, url, "agent", "agent-pw")
}

// A client looping bad connects writes one Info line per refusalLogEvery;
// the refusals in between go to Debug and are counted on the next Info.
func TestCallout_RefusalsAreRateLimited(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1_800_000_000, 0)
	c := &Callout{
		Key:    func() ([]byte, error) { return testKey, nil },
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    func() time.Time { return now },
	}
	refused := errors.New("bogus")
	for range 5 {
		c.logRefusal("bogus", refused)
		now = now.Add(time.Second)
	}
	now = now.Add(refusalLogEvery)
	c.logRefusal("bogus", refused)

	var infos []string
	debugs := 0
	for line := range strings.Lines(buf.String()) {
		switch {
		case strings.Contains(line, "level=INFO"):
			infos = append(infos, line)
		case strings.Contains(line, "level=DEBUG"):
			debugs++
		}
	}
	if len(infos) != 2 || debugs != 4 {
		t.Fatalf("got %d Info and %d Debug lines, want 2 and 4:\n%s", len(infos), debugs, buf.String())
	}
	if !strings.Contains(infos[0], "suppressed=0") || !strings.Contains(infos[1], "suppressed=4") {
		t.Errorf("Info lines = %q, want suppressed=0 then suppressed=4", infos)
	}
}

func mustMint(t *testing.T, mint func([]byte, string) (string, error), land string) string {
	t.Helper()
	tok, err := mint(testKey, land)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestLoadIssuer(t *testing.T) {
	dir := t.TempDir()
	acct, _ := nkeys.CreateAccount()
	seed, _ := acct.Seed()
	good := filepath.Join(dir, "seed")
	if err := os.WriteFile(good, append(seed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := LoadIssuer(good)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := acct.PublicKey()
	if got, _ := kp.PublicKey(); got != want {
		t.Errorf("LoadIssuer public key = %s, want %s", got, want)
	}
	user, _ := nkeys.CreateUser()
	useed, _ := user.Seed()
	bad := filepath.Join(dir, "user")
	if err := os.WriteFile(bad, useed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIssuer(bad); err == nil {
		t.Error("LoadIssuer accepted a user seed")
	}
}

func TestParseEpoch(t *testing.T) {
	for in, want := range map[string]uint64{"": 0, "0": 0, " 3 ": 3, "18446744073709551615": 1<<64 - 1} {
		if got, err := ParseEpoch(in); err != nil || got != want {
			t.Errorf("ParseEpoch(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "1.5", "x", "0x2", "18446744073709551616"} {
		if _, err := ParseEpoch(in); err == nil {
			t.Errorf("ParseEpoch(%q) accepted", in)
		}
	}
}

// actorLands places boss-acme in land acme, boss-beta in beta, and the fleet
// agent boss in the operator project gardenio.
func actorLands(actor string) (string, bool) {
	land, ok := map[string]string{"boss-acme": "acme", "boss-beta": "beta", "boss": "gardenio"}[actor]
	return land, ok
}

func TestParseLandMessage(t *testing.T) {
	m, err := parseLandMessage(EventPrefix("acme")+".messages.boss-acme.assistant_text", []byte("hi"), actorLands)
	if err != nil {
		t.Fatal(err)
	}
	if m.Land != "acme" || m.Actor != "boss-acme" || m.Kind != "assistant_text" || string(m.Data) != "hi" {
		t.Errorf("parseLandMessage = %+v", m)
	}
	for subject, want := range map[string]error{
		EventPrefix("acme") + ".messages.boss-beta.assistant_text": errForeignActor,
		EventPrefix("acme") + ".messages.boss.assistant_text":      errForeignActor,
		EventPrefix("acme") + ".messages.nobody.assistant_text":    errForeignActor,
		"coop.events.messages.boss.assistant_text":                 errNotLandMessage,
		EventPrefix("acme") + ".state":                             errNotLandMessage,
		EventPrefix("acme") + ".messages.boss-acme.x.y":            errNotLandMessage,
		EventPrefix("acme") + ".account.boss-acme.exhausted":       errNotLandMessage,
		"coop.events.land..messages.boss-acme.assistant_text":      errNotLandMessage,
	} {
		if _, err := parseLandMessage(subject, nil, actorLands); !errors.Is(err, want) {
			t.Errorf("parseLandMessage(%q) err = %v, want %v", subject, err, want)
		}
	}
}

// A land-acme publish naming a land-beta actor, or a fleet one, reaches a
// landMessagesSubject subscriber and is dropped by parseLandMessage; acme's own
// actor's is kept (gd-zo4W7Q8Yno).
func TestParseLandMessage_DropsAnotherLandsActor(t *testing.T) {
	url, _ := landServer(t, "acme", "beta")
	infra := connect(t, url, "infra", "infra-pw")
	sub, err := infra.SubscribeSync(landMessagesSubject)
	if err != nil {
		t.Fatal(err)
	}
	if err := infra.Flush(); err != nil {
		t.Fatal(err)
	}
	acme := connect(t, url, UserName("acme"), landToken(t, "acme"))
	for _, actor := range []string{"boss-beta", "boss", "boss-acme"} {
		if err := acme.Publish(EventPrefix("acme")+".messages."+actor+".assistant_text", []byte(actor)); err != nil {
			t.Fatal(err)
		}
	}
	if err := acme.Flush(); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for range 3 {
		msg, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("subscriber saw fewer than 3 events: %v", err)
		}
		if m, err := parseLandMessage(msg.Subject, msg.Data, actorLands); err == nil {
			kept = append(kept, m.Land+"/"+m.Actor)
		} else if !errors.Is(err, errForeignActor) {
			t.Errorf("parseLandMessage(%q) err = %v, want errForeignActor", msg.Subject, err)
		}
	}
	if len(kept) != 1 || kept[0] != "acme/boss-acme" {
		t.Errorf("kept %v, want only acme/boss-acme", kept)
	}
}

// landMessagesSubject matches every land's coop message events,
// EventPrefix(<land>).messages.<actor>.<kind>.
const landMessagesSubject = "coop.events.land.*.messages.*.*"

// landMessage is one coop message event a land's pod published.
type landMessage struct {
	// Land is the land whose user published it, from the subject: the server
	// lets only land-<Land> publish under EventPrefix(Land).
	Land string
	// Actor is the agent the subject names, which parseLandMessage has checked
	// belongs to Land.
	Actor string
	// Kind is the event type (assistant_text, thinking, tool_use, ...).
	Kind string
	// Data is the payload as the tenant wrote it, and untrusted. A consumer
	// keeps it as the land's content (a bead it writes goes in the land's
	// project, which kbeads taints untrusted-input, gd-Ms3cmrdW49) and never
	// acts on it as fleet input.
	Data []byte
}

// errNotLandMessage is returned by parseLandMessage for a subject that is not
// EventPrefix(<land>).messages.<actor>.<kind>, and errForeignActor for an
// actor that is not the land's.
var (
	errNotLandMessage = errors.New("not a land message subject")
	errForeignActor   = errors.New("actor is not the land's")
)

// parseLandMessage is how a consumer reads a land's message event
// (gd-zo4W7Q8Yno); it lives here until the first consumer moves it into the
// package with its first call. It takes the land from subject and refuses
// the event unless actorLand places the actor in that land, so a land cannot
// speak as another land's agent or a fleet one. actorLand reports the land (project)
// an agent belongs to; an agent it does not know is refused.
func parseLandMessage(subject string, data []byte, actorLand func(actor string) (land string, ok bool)) (landMessage, error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 7 || strings.Join(parts[:3], ".") != "coop.events.land" || parts[4] != "messages" {
		return landMessage{}, fmt.Errorf("%w: %q", errNotLandMessage, subject)
	}
	m := landMessage{Land: parts[3], Actor: parts[5], Kind: parts[6], Data: data}
	if m.Land == "" || m.Actor == "" || m.Kind == "" {
		return landMessage{}, fmt.Errorf("%w: %q", errNotLandMessage, subject)
	}
	if land, ok := actorLand(m.Actor); !ok || land != m.Land {
		return landMessage{}, fmt.Errorf("%w: %q under land %q", errForeignActor, m.Actor, m.Land)
	}
	return m, nil
}
