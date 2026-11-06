package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// stubPersonRoles is a PersonRoleSource backed by a map, recording who was asked.
type stubPersonRoles struct {
	mu    sync.Mutex
	roles map[string][]string
	err   error
	asked []string
}

func (s *stubPersonRoles) GardenioRoles(_ context.Context, email string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, email)
	return s.roles[email], s.err
}

const (
	ownerSlackID = "U0C37URBQVD"
	ownerEmail   = "matthewbaker1110@gmail.com"
)

func personBot(t *testing.T, src PersonRoleSource) (*Bot, *mockDaemon, *ephemeralCapture) {
	t.Helper()
	daemon := newMockDaemon()
	daemon.seedUser("Owner", map[string]string{"slack_id": ownerSlackID, "email": "Matthewbaker1110@Gmail.com"})
	daemon.seedUser("NoRoles", map[string]string{"slack_id": "UNOROLES", "email": "norole@example.com"})
	daemon.seedUser("NoEmail", map[string]string{"slack_id": "UNOEMAIL"})
	daemon.mu.Lock()
	daemon.beads["dec-p1"] = &beadsapi.BeadDetail{ID: "dec-p1", Type: "decision", Status: "open"}
	daemon.mu.Unlock()
	ec := newEphemeralCapture(t)
	bot := newTestBot(daemon, ec.srv)
	if src != nil {
		bot.SetPersonRoleSource(src)
	}
	return bot, daemon, ec
}

func dismissCallback(slackUser string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: slackUser, Name: "someone"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{{ActionID: "dismiss_decision", Value: "dec-p1"}},
		},
	}
}

// The owner's Slack id acts as the owner, but the Keycloak admin role is capped
// at gardenio-operator (decision gd-8PT2Qr2gEo): Slack never signs admin.
func TestHandleInteraction_ActsAsMappedPersonCappedAtOperator(t *testing.T) {
	src := &stubPersonRoles{roles: map[string][]string{ownerEmail: {"gardenio-admin"}}}
	bot, daemon, _ := personBot(t, src)

	bot.handleInteraction(context.Background(), dismissCallback(ownerSlackID))

	daemon.mu.Lock()
	ctx, closes := daemon.lastCloseBeadCtx, len(daemon.closed)
	daemon.mu.Unlock()
	if closes != 1 || ctx == nil {
		t.Fatalf("expected the owner's dismissal to reach the daemon, closes=%d", closes)
	}
	id, ok := beadsapi.EdgeAuthIdentityFromContext(ctx)
	if !ok {
		t.Fatal("no v2 edge-auth identity on the daemon ctx")
	}
	if id.Operator != "oidc:"+ownerEmail || id.AuthMode != "slack" || strings.Join(id.Roles, ",") != "gardenio-operator" {
		t.Errorf("identity = %+v, want oidc:%s / slack / [gardenio-operator]", id, ownerEmail)
	}
	if got := beadsapi.EdgeAuthOperatorFromContext(ctx); got != "" {
		t.Errorf("legacy v1 slack:U… envelope must not be set alongside the person identity, got %q", got)
	}
	if got := beadsapi.OperatorFromContext(ctx); got != "oidc:"+ownerEmail {
		t.Errorf("attribution operator = %q, want the canonical person oidc:%s", got, ownerEmail)
	}
	if len(src.asked) != 1 || src.asked[0] != ownerEmail {
		t.Errorf("roles asked for %v, want the lowercased bead email %q", src.asked, ownerEmail)
	}
}

// An unmapped Slack user, a user with no role, and a Keycloak outage are all
// refused VISIBLY and never reach the daemon.
func TestHandleInteraction_RefusesUnmappedOrRolelessVisibly(t *testing.T) {
	cases := []struct {
		name      string
		slackUser string
		src       *stubPersonRoles
		wantText  string
	}{
		{"unknown slack id", "USTRANGER", &stubPersonRoles{}, "isn't linked"},
		{"user bead without email", "UNOEMAIL", &stubPersonRoles{}, "isn't linked"},
		{"mapped but no gardenio role", "UNOROLES", &stubPersonRoles{}, "don't hold a Gardenio role"},
		{"keycloak down fails closed", ownerSlackID, &stubPersonRoles{err: errors.New("boom")}, "couldn't verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot, daemon, ec := personBot(t, tc.src)
			bot.handleInteraction(context.Background(), dismissCallback(tc.slackUser))

			daemon.mu.Lock()
			closes := len(daemon.closed)
			daemon.mu.Unlock()
			if closes != 0 {
				t.Fatalf("refused user still wrote to the daemon (%d closes)", closes)
			}
			msgs := ec.all()
			if len(msgs) != 1 || msgs[0].user != tc.slackUser || !strings.Contains(msgs[0].text, tc.wantText) {
				t.Fatalf("ephemeral = %+v, want one message to %s containing %q", msgs, tc.slackUser, tc.wantText)
			}
		})
	}
}

// Without a PersonRoleSource nothing changes: the legacy slack:U… envelope.
func TestHandleInteraction_NoPersonRoleSourceKeepsLegacyEnvelope(t *testing.T) {
	bot, daemon, _ := personBot(t, nil)
	bot.handleInteraction(context.Background(), dismissCallback("UANYONE"))

	daemon.mu.Lock()
	ctx := daemon.lastCloseBeadCtx
	daemon.mu.Unlock()
	if ctx == nil {
		t.Fatal("close did not reach the daemon")
	}
	if _, ok := beadsapi.EdgeAuthIdentityFromContext(ctx); ok {
		t.Error("v2 identity set without a PersonRoleSource")
	}
	if got := beadsapi.EdgeAuthOperatorFromContext(ctx); got != "slack:UANYONE" {
		t.Errorf("legacy envelope operator = %q, want slack:UANYONE", got)
	}
}

// End to end through the real beadsapi client: the daemon sees a v2 envelope
// for the person, and the same Slack action by an unmapped user sends nothing.
func TestHandleInteraction_WireEnvelopeReachesDaemon(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method != http.MethodGet {
			seen = append(seen, r.Header.Clone())
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"dec-p1","type":"decision","status":"open"}`))
	}))
	defer daemonSrv.Close()

	real, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	real.SetEdgeAuthHMACKey([]byte("shared-secret"))

	src := &stubPersonRoles{roles: map[string][]string{ownerEmail: {"gardenio-admin"}}}
	bot, mock, _ := personBot(t, src)
	bot.daemon = real
	// The user directory reads user beads from the daemon; keep it on the mock.
	bot.userDir = NewUserDirectory(mock, slog.Default())

	bot.handleInteraction(context.Background(), dismissCallback(ownerSlackID))
	bot.handleInteraction(context.Background(), dismissCallback("USTRANGER"))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("daemon saw %d writes, want exactly the owner's one", len(seen))
	}
	h := seen[0]
	if h.Get("X-Operator") != "oidc:"+ownerEmail || h.Get("X-Edge-Auth-Version") != "2" ||
		h.Get("X-Edge-Auth-Roles") != "gardenio-operator" || h.Get("X-Edge-Auth-Mode") != "slack" ||
		h.Get("X-Edge-Auth-Sig-V2") == "" {
		t.Errorf("owner write envelope = %v", h)
	}
}

func TestCapSlackRoles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roles []string
		want  string
	}{
		{"admin only", []string{"gardenio-admin"}, "gardenio-operator"},
		{"admin and viewer", []string{"gardenio-admin", "gardenio-viewer"}, "gardenio-operator"},
		{"operator", []string{"gardenio-operator"}, "gardenio-operator"},
		{"viewer stays viewer", []string{"gardenio-viewer"}, "gardenio-viewer"},
		{"non-canonical dropped", []string{"offline_access"}, ""},
		{"none", nil, ""},
	} {
		if got := strings.Join(capSlackRoles(tc.roles), ","); got != tc.want {
			t.Errorf("%s: capSlackRoles(%v) = %q, want %q", tc.name, tc.roles, got, tc.want)
		}
	}
}

// --- KeycloakRoleChecker.GardenioRoles ---------------------------------------

type fakeKCRoles struct {
	srv        *httptest.Server
	users      []map[string]any
	composite  map[string][]string
	directOnly map[string][]string
	userCalls  atomic.Int64
	status     int
}

func newFakeKCRoles() *fakeKCRoles {
	f := &fakeKCRoles{composite: map[string][]string{}, directOnly: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/gardenio/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
	})
	mux.HandleFunc("/admin/realms/gardenio/users", func(w http.ResponseWriter, r *http.Request) {
		f.userCalls.Add(1)
		if f.status != 0 {
			http.Error(w, "nope", f.status)
			return
		}
		email := r.URL.Query().Get("email")
		var out []map[string]any
		for _, u := range f.users {
			if strings.EqualFold(u["email"].(string), email) {
				out = append(out, u)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/admin/realms/gardenio/users/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/realms/gardenio/users/"), "/")
		src := f.directOnly
		if strings.HasSuffix(r.URL.Path, "/realm/composite") {
			src = f.composite
		}
		out := []map[string]string{}
		for _, n := range src[parts[0]] {
			out = append(out, map[string]string{"name": n})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeKCRoles) checker() *KeycloakRoleChecker {
	return NewKeycloakRoleChecker(KeycloakConfig{BaseURL: f.srv.URL, Realm: "gardenio", ClientID: "c", ClientSecret: "s"}, slog.Default())
}

func TestGardenioRoles_EffectiveCanonicalSortedAndCached(t *testing.T) {
	f := newFakeKCRoles()
	defer f.srv.Close()
	f.users = []map[string]any{{"id": "kc-owner", "email": ownerEmail, "enabled": true}}
	// The owner gets gardenio-admin through the gardenio-admins group, so it is
	// only in the composite listing; non-canonical roles must be dropped.
	f.composite["kc-owner"] = []string{"offline_access", "gardenio-viewer", "gardenio-admin", "gardenio-mention", "gardenio-admin"}
	f.directOnly["kc-owner"] = []string{"offline_access"}
	k := f.checker()

	got, err := k.GardenioRoles(context.Background(), " Matthewbaker1110@gmail.com ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gardenio-admin,gardenio-viewer" {
		t.Fatalf("roles = %v, want [gardenio-admin gardenio-viewer]", got)
	}
	if _, err := k.GardenioRoles(context.Background(), ownerEmail); err != nil {
		t.Fatal(err)
	}
	if n := f.userCalls.Load(); n != 1 {
		t.Errorf("keycloak user lookups = %d, want 1 (second call cached)", n)
	}
}

func TestGardenioRoles_UnknownOrDisabledUserHasNoRoles(t *testing.T) {
	f := newFakeKCRoles()
	defer f.srv.Close()
	f.users = []map[string]any{{"id": "kc-off", "email": "off@example.com", "enabled": false}}
	f.composite["kc-off"] = []string{"gardenio-admin"}
	k := f.checker()

	for _, email := range []string{"off@example.com", "ghost@example.com"} {
		got, err := k.GardenioRoles(context.Background(), email)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: roles=%v err=%v, want none", email, got, err)
		}
	}
}

func TestGardenioRoles_KeycloakErrorIsAnErrorNotEmpty(t *testing.T) {
	f := newFakeKCRoles()
	defer f.srv.Close()
	f.status = http.StatusInternalServerError
	k := f.checker()

	if got, err := k.GardenioRoles(context.Background(), ownerEmail); err == nil {
		t.Fatalf("roles=%v err=nil, want an error so callers fail closed", got)
	}
	// And the failure is not cached as "no roles".
	f.status = 0
	f.users = []map[string]any{{"id": "kc-owner", "email": ownerEmail, "enabled": true}}
	f.composite["kc-owner"] = []string{"gardenio-admin"}
	if got, err := k.GardenioRoles(context.Background(), ownerEmail); err != nil || len(got) != 1 {
		t.Fatalf("after recovery roles=%v err=%v, want [gardenio-admin]", got, err)
	}
}

// Two open user beads carrying one slack_id make "who is this" depend on daemon
// list order, so the user is refused and nothing is asked of Keycloak or written.
func TestHandleInteraction_AmbiguousSlackIDRefuses(t *testing.T) {
	src := &stubPersonRoles{roles: map[string][]string{ownerEmail: {"gardenio-admin"}, "other@example.com": {"gardenio-admin"}}}
	bot, daemon, ec := personBot(t, src)
	daemon.seedUser("Impostor", map[string]string{"slack_id": ownerSlackID, "email": "other@example.com"})

	bot.handleInteraction(context.Background(), dismissCallback(ownerSlackID))

	daemon.mu.Lock()
	closes := len(daemon.closed)
	daemon.mu.Unlock()
	if closes != 0 {
		t.Fatalf("ambiguous slack_id still wrote to the daemon (%d closes)", closes)
	}
	if len(src.asked) != 0 {
		t.Errorf("roles were looked up for an ambiguous slack_id: %v", src.asked)
	}
	msgs := ec.all()
	if len(msgs) != 1 || msgs[0].user != ownerSlackID || !strings.Contains(msgs[0].text, "more than one") {
		t.Fatalf("ephemeral = %+v, want one refusal to %s mentioning more than one person", msgs, ownerSlackID)
	}
}

// With a PersonRoleSource set, a decision-thread reply by an unmapped user, an
// ambiguous one, or while Keycloak is down gets the ephemeral refusal in the
// thread and never reaches the daemon. A mapped user's reply still resolves.
func TestHandleThreadReply_PersonRefusalNeverReachesDaemon(t *testing.T) {
	cases := []struct {
		name      string
		slackUser string
		src       *stubPersonRoles
		wantText  string
	}{
		{"unmapped user", "USTRANGER", &stubPersonRoles{}, "isn't linked"},
		{"mapped but no gardenio role", "UNOROLES", &stubPersonRoles{}, "don't hold a Gardenio role"},
		{"keycloak down fails closed", ownerSlackID, &stubPersonRoles{err: errors.New("boom")}, "couldn't verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot, daemon, ec := personBot(t, tc.src)
			sm, err := NewStateManager(t.TempDir() + "/state.json")
			if err != nil {
				t.Fatalf("NewStateManager: %v", err)
			}
			bot.state = sm
			seedOpenDecisionWithOwner(t, daemon, bot, "dec-1", "agent-x", "UOWNER", "C123", "1700.0001")

			bot.resolveDecisionByReply(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
				User: tc.slackUser, Channel: "C123", ThreadTimeStamp: "1700.0001", Text: "blue",
			}, ""))

			daemon.mu.Lock()
			gets, closes := daemon.getCalls, len(daemon.closed)
			daemon.mu.Unlock()
			if gets != 0 || closes != 0 {
				t.Fatalf("refused thread reply reached the daemon: gets=%d closes=%d", gets, closes)
			}
			msgs := ec.all()
			if len(msgs) != 1 || msgs[0].user != tc.slackUser || !strings.Contains(msgs[0].text, tc.wantText) {
				t.Fatalf("ephemeral = %+v, want one message to %s containing %q", msgs, tc.slackUser, tc.wantText)
			}
		})
	}
}
