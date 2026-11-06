package commons

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// unreadServer is commonsServer with read positions kept in the fixture and
// the scope's principal set to principal.
func unreadServer(t *testing.T, d *commonstest.Daemon, principal string) http.Handler {
	t.Helper()
	h := &Handler{
		Beads: d, Cursors: d, BasePath: "/base",
		Scope: func(r *http.Request) Scope {
			sc := adminScope
			if r.Header.Get("X-Test-As") == "claimlayer" {
				sc = claimlayerScope
			}
			sc.Principal = principal
			return sc
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := commonstest.AdminBearer
		if r.Header.Get("X-Test-As") == "claimlayer" {
			bearer = commonstest.ClaimlayerBearer
		}
		mux.ServeHTTP(w, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
	})
}

func getUnread(t *testing.T, h http.Handler, as string) unreadJSON {
	t.Helper()
	code, body := get(t, h, "/garden/c/unread", as)
	if code != http.StatusOK {
		t.Fatalf("GET /garden/c/unread as %q = %d: %s", as, code, body)
	}
	var u unreadJSON
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unread body: %v\n%s", err, body)
	}
	return u
}

func markRead(t *testing.T, h http.Handler, path, msg, as string) int {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"m": {msg}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if as != "" {
		r.Header.Set("X-Test-As", as)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// counts flattens an unread body to "land" and "land/channel" -> label.
func counts(u unreadJSON) map[string]string {
	out := map[string]string{"": u.Label}
	for _, l := range u.Lands {
		out[l.Land] = l.Label
		for _, ch := range l.Channels {
			out[l.Land+"/"+ch.Name] = ch.Label
		}
	}
	return out
}

func wantCounts(t *testing.T, u unreadJSON, want map[string]string) {
	t.Helper()
	got := counts(u)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("unread[%q] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestUnread_CountsTopLevelEntriesFromTheStart(t *testing.T) {
	h := unreadServer(t, commonstest.TwoLands(t0), "")
	// claimlayer #general: the wish head, the message and the card; not the
	// replies, the tombstone, the thread's task or the beads with no single
	// land. #crew: one message, two agents' cards and the death-storm roll-up;
	// not the crash and death notices in a card's thread. #decisions: the five
	// decision and permission cards. #escalations: the three owner escalation cards of five (the desk and operator ones fold, gd-bmOiteQi36).
	wantCounts(t, getUnread(t, h, ""), map[string]string{
		"": "17", "claimlayer": "15", "claimlayer/general": "3", "claimlayer/crew": "4", "claimlayer/decisions": "5", "claimlayer/escalations": "3",
		"gardenio": "2", "gardenio/general": "2",
	})
}

func TestUnread_ScopedToTheViewersLands(t *testing.T) {
	h := unreadServer(t, commonstest.TwoLands(t0), "")
	u := getUnread(t, h, "claimlayer")
	wantCounts(t, u, map[string]string{"": "15", "claimlayer": "15"})
	if len(u.Lands) != 1 {
		t.Fatalf("claimlayer sees lands %+v", u.Lands)
	}
	_, body := get(t, h, "/garden/c/unread", "claimlayer")
	mustNotContain(t, body, commonstest.Leaks...)

	// A position in another land is refused like a missing channel.
	if code := markRead(t, h, "/garden/c/gardenio/general/read", "gd-msg1", "claimlayer"); code != http.StatusNotFound {
		t.Fatalf("claimlayer marking gardenio read = %d, want 404", code)
	}
	if code := markRead(t, h, "/garden/c/claimlayer/general/read", "gd-msg1", "claimlayer"); code != http.StatusNotFound {
		t.Fatalf("claimlayer marking a gardenio message read = %d, want 404", code)
	}
}

func TestUnread_LeavesOutTheViewersOwnWords(t *testing.T) {
	h := unreadServer(t, commonstest.TwoLands(t0), "oidc:op@gardenio.example")
	wantCounts(t, getUnread(t, h, ""), map[string]string{"gardenio/general": "1"})
}

func TestMarkRead_MovesTheCursorForwardOnly(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := unreadServer(t, d, "")
	const general = "/garden/c/claimlayer/general/read"

	if code := markRead(t, h, general, "cl-msg1", "claimlayer"); code != http.StatusNoContent {
		t.Fatalf("mark cl-msg1 = %d", code)
	}
	if got := d.Cursors[commonstest.ClaimlayerBearer]["cl-chgen"]; got != "cl-msg1" {
		t.Fatalf("cursor = %q, want cl-msg1", got)
	}
	// Only the card is newer.
	wantCounts(t, getUnread(t, h, "claimlayer"), map[string]string{"claimlayer/general": "1", "claimlayer/crew": "4", "claimlayer": "13"})
	// Another viewer's positions are its own.
	wantCounts(t, getUnread(t, h, ""), map[string]string{"claimlayer/general": "3"})

	// An older entry (an old tab catching up) leaves the cursor where it is.
	if code := markRead(t, h, general, "cl-wish1", "claimlayer"); code != http.StatusNoContent {
		t.Fatalf("mark cl-wish1 = %d", code)
	}
	if got := d.Cursors[commonstest.ClaimlayerBearer]["cl-chgen"]; got != "cl-msg1" {
		t.Fatalf("cursor moved back to %q", got)
	}

	// Channel by id works like by name; the newest entry clears the badge.
	if code := markRead(t, h, "/garden/c/claimlayer/cl-chgen/read", "cl-card1", "claimlayer"); code != http.StatusNoContent {
		t.Fatalf("mark cl-card1 = %d", code)
	}
	wantCounts(t, getUnread(t, h, "claimlayer"), map[string]string{"claimlayer/general": "", "claimlayer": "12"})

	// A reply, an entry of another channel and a missing id are not positions.
	for _, m := range []string{"cl-rep1", "cl-crew1", "cl-nope"} {
		if code := markRead(t, h, general, m, "claimlayer"); code != http.StatusNotFound {
			t.Errorf("mark %s = %d, want 404", m, code)
		}
	}
	if code := markRead(t, h, general, "", "claimlayer"); code != http.StatusBadRequest {
		t.Errorf("mark with no m = %d, want 400", code)
	}
}

// A cursor that no longer names an entry of its channel counts from the start.
func TestUnread_StaleCursorCountsFromTheStart(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Cursors = map[string]map[string]string{commonstest.ClaimlayerBearer: {"cl-chgen": "cl-gone", "cl-chcrew": "cl-msg1"}}
	h := unreadServer(t, d, "")
	wantCounts(t, getUnread(t, h, "claimlayer"), map[string]string{"claimlayer/general": "3", "claimlayer/crew": "4"})
}

// Past the window the count is a floor: replies fill it without counting.
func TestUnread_WindowIsAFloor(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	for i := range unreadWindow {
		d.Put(&beadsapi.BeadDetail{ID: fmt.Sprintf("cl-r%03d", i), Type: "message", Status: "closed", Labels: cl,
			CreatedAt: t0.Add(time.Hour + time.Duration(i)*time.Second),
			Fields:    map[string]string{"channel_id": "cl-chcrew", "thread_id": "cl-crew1", "text": "reply"}})
	}
	d.Put(&beadsapi.BeadDetail{ID: "cl-late", Type: "message", Status: "closed", Labels: cl, CreatedAt: t0.Add(2 * time.Hour),
		Fields: map[string]string{"channel_id": "cl-chcrew", "text": "late"}})
	h := unreadServer(t, d, "")
	wantCounts(t, getUnread(t, h, "claimlayer"), map[string]string{"claimlayer/crew": "1+", "claimlayer": "12+", "": "12+"})
}

// Mine: a reply by someone else is the newest in a thread the viewer started
// or wrote in; the viewer's own reply answers it.
func TestUnread_MineWhenAThreadOfMineHasAReply(t *testing.T) {
	const ann = "oidc:ann@claimlayer.example"
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	say := func(id, thread, author string, at time.Duration) {
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Labels: cl, CreatedAt: t0.Add(at),
			Fields: map[string]string{"channel_id": "cl-chcrew", "thread_id": thread, "author": author, "text": id}})
	}
	mine := func(viewer string) bool { return getUnread(t, unreadServer(t, d, viewer), "claimlayer").Mine }

	// cl-rep1 answers a wish ann neither started nor wrote in.
	if mine(ann) {
		t.Fatal("mine with only another's thread answered")
	}
	say("cl-ann1", "", ann, time.Hour)
	if mine(ann) {
		t.Fatal("mine with no reply to ann's thread")
	}
	say("cl-ann1r1", "cl-ann1", "agent:claimlayer-crew-1", 2*time.Hour)
	if !mine(ann) {
		t.Fatal("not mine with a reply to ann's thread")
	}
	if mine("oidc:bob@claimlayer.example") || mine("") {
		t.Fatal("mine for a viewer who did not write in the thread")
	}
	say("cl-ann1r2", "cl-ann1", ann, 3*time.Hour)
	if mine(ann) {
		t.Fatal("mine after ann answered")
	}

	// A head (and ann's answer) said before the read position is read to
	// learn who started the thread.
	say("cl-mid", "", "agent:claimlayer-crew-2", 210*time.Minute)
	say("cl-ann1r3", "cl-ann1", "agent:claimlayer-crew-1", 4*time.Hour)
	d.Cursors = map[string]map[string]string{commonstest.ClaimlayerBearer: {"cl-chcrew": "cl-mid"}}
	if !mine(ann) {
		t.Fatal("not mine with ann's thread head before the read position")
	}
	// A reply before the read position is read.
	say("cl-late", "", "agent:claimlayer-crew-2", 5*time.Hour)
	d.Cursors = map[string]map[string]string{commonstest.ClaimlayerBearer: {"cl-chcrew": "cl-late"}}
	if mine(ann) {
		t.Fatal("mine with the reply before the read position")
	}
}

func TestMarkRead_NeedsCursors(t *testing.T) {
	h := &Handler{Beads: commonstest.TwoLands(t0), Scope: func(*http.Request) Scope { return adminScope }}
	mux := http.NewServeMux()
	h.Register(mux)
	if code := markRead(t, mux, "/garden/c/claimlayer/general/read", "cl-msg1", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("mark with no cursor store = %d, want 503", code)
	}
	// Counting still works, from the start.
	wantCounts(t, getUnread(t, mux, ""), map[string]string{"claimlayer/general": "3"})
}

func TestUnreadLabel(t *testing.T) {
	for u, want := range map[Unread]string{
		{}:                   "",
		{N: 3}:               "3",
		{N: 3, More: true}:   "3+",
		{N: 0, More: true}:   "",
		{N: 99}:              "99",
		{N: 150}:             "99+",
		{N: 150, More: true}: "99+",
	} {
		if got := u.Label(); got != want {
			t.Errorf("%+v.Label() = %q, want %q", u, got, want)
		}
	}
}

// TestMarkReadRefusesCrossOrigin: another site's page cannot move a reader's
// cursor (gd-OPACzpdlZU).
func TestMarkReadRefusesCrossOrigin(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := unreadServer(t, d, "")
	r := httptest.NewRequest("POST", "/garden/c/claimlayer/general/read", strings.NewReader(url.Values{"m": {"cl-msg1"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site mark read = %d, want 403", rec.Code)
	}
	if got := d.Cursors[commonstest.ClaimlayerBearer]["cl-chgen"]; got != "" {
		t.Fatalf("cursor moved to %q", got)
	}
}

// TestUnread_MachinesCountsNothing: #machines (single voice, steward
// amendment 2026-10-23 §2) carries no badge and adds none to its land's.
func TestUnread_MachinesCountsNothing(t *testing.T) {
	d := commonstest.TwoLands(t0)
	gd := []string{"project:gardenio"}
	d.Put(&beadsapi.BeadDetail{ID: "gd-chmach", Type: "channel", Title: "Machines", Status: "open", Labels: gd, CreatedAt: t0,
		Fields: map[string]string{"name": "machines", "channel_kind": "machines"}})
	for i, id := range []string{"gd-mcard1", "gd-mcard2"} {
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "message", Title: "Agent: gardenio-crew-" + id, Status: "closed", Labels: gd, CreatedAt: t0.Add(time.Duration(10+i) * time.Minute),
			Fields: map[string]string{"channel_id": "gd-chmach", "author": "agent:gardenio-crew-1", "text": "squawk " + id}})
	}
	u := getUnread(t, unreadServer(t, d, ""), "")
	wantCounts(t, u, map[string]string{"": "17", "gardenio": "2", "gardenio/general": "2", "gardenio/machines": ""})
}

func TestSortChannels_MachinesAfterThePeoplesChannels(t *testing.T) {
	chs := []Channel{{Name: "machines", Kind: kindMachines}, {Name: "design", Kind: "user"}, {Name: "escalations", Kind: "escalations"}, {Name: "general", Kind: "general"}}
	sortChannels(chs)
	var got []string
	for _, c := range chs {
		got = append(got, c.Name)
	}
	if strings.Join(got, ",") != "general,escalations,design,machines" {
		t.Errorf("order = %v, want general, escalations, design, machines", got)
	}
}

// TestUnread_ChannelLessMessageIsInNoChannel: a message bead with no
// channel_id (a Slack-mirror record such as a relayed agent comment, or a
// gardener turn post) is in no channel's timeline and adds to no unread
// count; only a reply with a thread_id takes its root's channel. So the
// channel-less records count against #general nowhere (gd-rar4hYRbGC).
func TestUnread_ChannelLessMessageIsInNoChannel(t *testing.T) {
	d := commonstest.TwoLands(t0)
	gd := []string{"project:gardenio"}
	d.Put(&beadsapi.BeadDetail{ID: "gd-mirror1", Type: "message", Title: ":memo: agent commented", Status: "closed", Labels: gd, CreatedAt: t0.Add(30 * time.Minute),
		Fields: map[string]string{"kind": "text", "author": "agent:gardenio-cicd-watchdog", "text": ":memo: agent commented on gd-x: gb stop wrap-up", "slack_channel": "C0CICD", "slack_ts": "1792800000.000100"}})
	wantCounts(t, getUnread(t, unreadServer(t, d, ""), ""), map[string]string{"": "17", "gardenio": "2", "gardenio/general": "2"})
	ctx := context.Background()
	b, err := d.GetBead(ctx, "gd-mirror1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := messageFromBead(b); ok {
		t.Fatal("messageFromBead read a channel-less bead as a message")
	}
}
