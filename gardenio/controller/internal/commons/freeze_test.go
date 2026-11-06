package commons

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// switchFake is a LandSwitch over a map: claimlayer is a tenant land,
// gardenio the operator's.
type switchFake struct {
	frozen map[string]bool
	sets   []string
}

func (s *switchFake) wire(h *Handler) {
	h.Lands = LandSwitch{
		State: func(_ context.Context, land string) (bool, bool) {
			return land == "claimlayer", s.frozen[land]
		},
		Set: func(r *http.Request, land string, frozen bool, reason string) error {
			s.frozen[land] = frozen
			s.sets = append(s.sets, land+":"+map[bool]string{true: "freeze", false: "thaw"}[frozen]+":"+reason)
			return nil
		},
	}
}

func frozenServer(t *testing.T, frozen bool) (http.Handler, *commonstest.Daemon, *switchFake) {
	t.Helper()
	d := commonstest.TwoLands(t0)
	sw := &switchFake{frozen: map[string]bool{"claimlayer": frozen}}
	return commonsServer(t, d, sw.wire), d, sw
}

// A frozen land refuses its members' says, replies and joins with the banner's
// words, and writes nothing (step 10.5: freeze refuses writes).
func TestFrozenLandRefusesMemberWrites(t *testing.T) {
	h, d, _ := frozenServer(t, true)
	made := len(d.Created)
	for _, path := range []string{"/garden/c/claimlayer/general/say", "/garden/c/t/cl-msg1/reply", "/garden/c/claimlayer/build/join"} {
		code, body := post(t, h, path, "claimlayer", "hi")
		if code != http.StatusForbidden || !strings.Contains(body, "This land is frozen") {
			t.Errorf("%s = %d %q, want 403 with the frozen banner", path, code, body)
		}
	}
	if len(d.Created) != made || len(d.Updated) != 0 {
		t.Errorf("a frozen land took a write: %d created (want %d), %d updated", len(d.Created), made, len(d.Updated))
	}
}

func TestFrozenLandShowsMembersABannerForTheComposer(t *testing.T) {
	h, d, _ := frozenServer(t, true)
	_, body := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, body, `<p class="cm-frozen" role="status">`+FrozenNote+`</p>`)
	mustNotContain(t, body, `data-commons="compose"`)
	// A user channel's Join gives way to the banner too.
	withBuildThread(d)
	_, body = get(t, h, "/garden/c/claimlayer/build/t/cl-bld1", "claimlayer")
	mustContain(t, body, `<p class="cm-frozen" role="status">`+FrozenNote+`</p>`)
	mustNotContain(t, body, `data-commons="join"`)
}

// withBuildThread adds a thread to claimlayer's #build, a user channel only
// ann is in.
func withBuildThread(d *commonstest.Daemon) {
	d.Put(&beadsapi.BeadDetail{ID: "cl-bld1", Type: "message", Title: "build", Status: "closed", Labels: []string{"project:claimlayer"}, CreatedAt: t0.Add(time.Minute),
		Fields: map[string]string{"channel_id": "cl-chbuild", "author": "oidc:ann@claimlayer.example", "text": "build is green"}})
}

// withSupport adds claimlayer's #support, a user channel nobody has joined, and
// a request in it.
func withSupport(d *commonstest.Daemon) {
	cl := []string{"project:claimlayer"}
	d.Put(&beadsapi.BeadDetail{ID: "cl-chsup", Type: "channel", Title: "Support", Status: "open", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"name": "support", "channel_kind": "user"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-sup1", Type: "message", Title: "help", Status: "closed", Labels: cl, CreatedAt: t0.Add(time.Minute),
		Fields: map[string]string{"channel_id": "cl-chsup", "author": "oidc:ann@claimlayer.example", "text": "help"}})
}

// A frozen land's #support stays open to its members (Terms §9.1,
// gd-mJw9pxK2wu): the composer under a banner saying so, and their says and
// replies taken, though nobody can join it during the freeze. A role that may
// not post gets the frozen banner, and the operator a banner of their own.
func TestFrozenLandKeepsSupportOpen(t *testing.T) {
	h, d, _ := frozenServer(t, true)
	withSupport(d)
	d.Frozen = map[string]bool{"claimlayer": true}
	_, body := get(t, h, "/garden/c/claimlayer/support/t/cl-sup1", "claimlayer")
	mustContain(t, body, `<p class="cm-frozen" role="status">`+FrozenSupportNote+`</p>`, `data-commons="compose"`)
	mustNotContain(t, body, FrozenNote, `data-commons="join"`)
	for _, path := range []string{"/garden/c/claimlayer/support/say", "/garden/c/t/cl-sup1/reply"} {
		if code, body := post(t, h, path, "claimlayer", "we are frozen, why?"); code != http.StatusCreated {
			t.Errorf("member %s in a frozen land = %d %s, want 201", path, code, body)
		}
		if req, f := lastCreated(t, d); f["channel_id"] != "cl-chsup" || f["author"] != "oidc:bo@claimlayer.example" || req.Labels[0] != "project:claimlayer" {
			t.Errorf("%s wrote %+v %v, want bo's message in #support", path, req, f)
		}
	}
	made := len(d.Created)
	if code, body := post(t, h, "/garden/c/claimlayer/support/join", "claimlayer", ""); code != http.StatusForbidden || !strings.Contains(body, "This land is frozen") {
		t.Errorf("join #support in a frozen land = %d %q, want 403 with the frozen banner", code, body)
	}
	if code, body := post(t, h, "/garden/c/claimlayer/support/say", "claimlayer", "hi", "X-Test-Guest", "1"); code != http.StatusForbidden || !strings.Contains(body, "You may not post in #support") {
		t.Errorf("a role that may not post, in a frozen #support = %d %q, want 403", code, body)
	}
	if len(d.Created) != made || len(d.Updated) != 0 {
		t.Errorf("a refused write was made: %d created (want %d), %d updated", len(d.Created), made, len(d.Updated))
	}
	r := httptest.NewRequest("GET", "/garden/c/claimlayer/support/t/cl-sup1", nil)
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("X-Test-Guest", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	body = rec.Body.String()
	mustContain(t, body, `<p class="cm-frozen" role="status">`+FrozenNote+`</p>`)
	mustNotContain(t, body, `data-commons="compose"`)

	_, body = get(t, h, "/garden/c/claimlayer/support/t/cl-sup1", "")
	mustContain(t, body, FrozenSupportOperatorNote, `data-commons="compose"`)
	mustNotContain(t, body, FrozenOperatorNote)
}

// Thawed, #support is a user channel like any other: join it to post.
func TestThawedSupportIsMembersOnly(t *testing.T) {
	h, d, _ := frozenServer(t, false)
	withSupport(d)
	_, body := get(t, h, "/garden/c/claimlayer/support/t/cl-sup1", "claimlayer")
	mustContain(t, body, `data-commons="join"`)
	mustNotContain(t, body, `data-commons="compose"`, "cm-frozen")
	if code, body := post(t, h, "/garden/c/claimlayer/support/say", "claimlayer", "hi"); code != http.StatusForbidden || !strings.Contains(body, "Join #support") {
		t.Errorf("non-member says in a thawed #support = %d %q, want 403 naming Join", code, body)
	}
}

// The operator still posts in a frozen land, under a banner saying so, and
// may thaw it.
func TestFrozenLandLetsTheOperatorPostAndThaw(t *testing.T) {
	h, _, sw := frozenServer(t, true)
	const thread = "/garden/c/claimlayer/general/t/cl-wish1"
	_, body := get(t, h, thread, "")
	mustContain(t, body, FrozenOperatorNote, `data-commons="compose"`)
	if code, b := post(t, h, "/garden/c/claimlayer/general/say", "", "frozen for an AUP review"); code != http.StatusCreated {
		t.Fatalf("operator say in a frozen land = %d %s", code, b)
	}
	code, body := post(t, h, "/garden/c/claimlayer/general/thaw", "", "")
	if code != http.StatusNoContent || body != "" || len(sw.sets) != 1 || sw.sets[0] != "claimlayer:thaw:" {
		t.Fatalf("thaw = %d (sets %v) %q, want 204", code, sw.sets, body)
	}
	_, body = get(t, h, thread, "")
	mustContain(t, body, `data-commons="compose"`)
	mustNotContain(t, body, FrozenOperatorNote)
	// A member of the thawed land posts with no banner.
	_, body = get(t, h, thread, "claimlayer")
	mustContain(t, body, `data-commons="compose"`)
	mustNotContain(t, body, "cm-frozen")
}

func freeze(t *testing.T, h http.Handler, path, as, reason string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"reason": {reason}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if as != "" {
		r.Header.Set("X-Test-As", as)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func TestFreezeRoute(t *testing.T) {
	h, _, sw := frozenServer(t, false)
	if code, _ := freeze(t, h, "/garden/c/claimlayer/general/freeze", "claimlayer", "x"); code != http.StatusForbidden {
		t.Errorf("a member freezing = %d, want 403", code)
	}
	if code, _ := freeze(t, h, "/garden/c/claimlayer/general/freeze", "", "  "); code != http.StatusBadRequest {
		t.Errorf("no reason = %d, want 400", code)
	}
	if code, _ := freeze(t, h, "/garden/c/claimlayer/general/freeze", "", strings.Repeat("x", 501)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a 501-byte reason = %d, want 413", code)
	}
	if code, _ := freeze(t, h, "/garden/c/gardenio/general/freeze", "", "x"); code != http.StatusConflict {
		t.Errorf("freezing an operator land = %d, want 409", code)
	}
	if len(sw.sets) != 0 {
		t.Fatalf("a refused freeze was set: %v", sw.sets)
	}
	code, body := freeze(t, h, "/garden/c/claimlayer/general/freeze", "", "spam in #general")
	if code != http.StatusNoContent || body != "" || len(sw.sets) != 1 || sw.sets[0] != "claimlayer:freeze:spam in #general" {
		t.Fatalf("freeze = %d (sets %v) %q, want 204", code, sw.sets, body)
	}
	_, body = get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "")
	mustContain(t, body, FrozenOperatorNote)
}

// Without a LandSwitch nothing is frozen or freezes.
func TestNoLandSwitchFreezesNothing(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	_, body := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "")
	mustNotContain(t, body, "cm-frozen")
	if code, _ := freeze(t, h, "/garden/c/claimlayer/general/freeze", "", "x"); code != http.StatusForbidden {
		t.Errorf("freeze with no switch = %d, want 403", code)
	}
}

// The kill switch takes a freeze or thaw only from the Commons' own page.
func TestFreezeRefusesCrossOrigin(t *testing.T) {
	h, _, sw := frozenServer(t, false)
	send := func(path string, hdr map[string]string) int {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"reason": {"spam"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for name, hdr := range map[string]map[string]string{
		"cross-site":            {"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		"same-site":             {"Sec-Fetch-Site": "same-site", "Origin": "https://other.example.com"},
		"foreign origin":        {"Origin": "https://evil.example"},
		"opaque origin":         {"Origin": "null"},
		"origin on another fwd": {"Origin": "https://evil.example", "X-Forwarded-Host": "example.com"},
	} {
		for _, path := range []string{"/garden/c/claimlayer/general/freeze", "/garden/c/claimlayer/general/thaw"} {
			if code := send(path, hdr); code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", name, path, code)
			}
		}
	}
	if len(sw.sets) != 0 {
		t.Fatalf("a cross-origin request threw the switch: %v", sw.sets)
	}
	// Controls: the page's own fetch, and a browser that only sends Origin
	// naming this host (directly or through the edge).
	for name, hdr := range map[string]map[string]string{
		"same-origin":    {"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example"},
		"origin is host": {"Origin": "http://example.com"},
		"origin via fwd": {"Origin": "https://garden.example", "X-Forwarded-Host": "garden.example"},
	} {
		if code := send("/garden/c/claimlayer/general/thaw", hdr); code != http.StatusNoContent {
			t.Errorf("%s thaw = %d, want 204", name, code)
		}
	}
	if code := send("/garden/c/claimlayer/general/freeze", map[string]string{"Sec-Fetch-Site": "same-origin"}); code != http.StatusNoContent || len(sw.sets) == 0 {
		t.Errorf("same-origin freeze = %d (sets %v), want 204", code, sw.sets)
	}
}
