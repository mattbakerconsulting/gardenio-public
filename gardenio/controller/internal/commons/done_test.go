package commons

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// fakeReactor records the reactions the Commons sends the bridge and, like
// the bridge, closes the wish it names.
type fakeReactor struct {
	mu     sync.Mutex
	d      *commonstest.Daemon
	got    []ReactRequest
	bearer []string
	err    error
}

func (f *fakeReactor) React(ctx context.Context, req ReactRequest) (CardAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	f.bearer = append(f.bearer, beadsapi.UserBearerFromContext(ctx))
	if f.err != nil {
		return CardAnswer{}, f.err
	}
	if b, ok := f.d.Beads[req.Message]; ok {
		b.Status = "closed"
	}
	return CardAnswer{Notes: []string{"Marked *done*."}}, nil
}

const doneForm = `<form class="cm-done" method="post" action="/base/garden/c/t/cl-wish1/done" data-commons="act">`

// TestDone drives POST /garden/c/t/{thread}/done: who may, the same-origin
// check, what goes to the bridge, and the answer (the head as it now renders,
// with the bridge's words).
func TestDone(t *testing.T) {
	d := commonstest.TwoLands(t0)
	react := &fakeReactor{d: d}
	h := commonsServer(t, d, func(h *Handler) { h.React = react })

	for _, c := range []struct {
		name, path string
		hdr        []string
		code       int
		want       string
	}{
		{"anonymous", "/garden/c/t/cl-wish1/done", []string{"X-Test-Anon", "1"}, http.StatusUnauthorized, ""},
		{"cross-site", "/garden/c/t/cl-wish1/done", []string{"Sec-Fetch-Site", "same-site"}, http.StatusForbidden, "this page"},
		{"other origin", "/garden/c/t/cl-wish1/done", []string{"Origin", "https://evil.example"}, http.StatusForbidden, "this page"},
		{"guest", "/garden/c/t/cl-wish1/done", []string{"X-Test-Guest", "1"}, http.StatusForbidden, "does not let you"},
		{"another land's wish", "/garden/c/t/gd-wish1/done", nil, http.StatusNotFound, ""},
		{"not a wish", "/garden/c/t/cl-msg1/done", nil, http.StatusNotFound, ""},
		{"done", "/garden/c/t/cl-wish1/done", []string{"Sec-Fetch-Site", "same-origin"}, http.StatusOK, `id="m-cl-wish1"`},
		{"done again", "/garden/c/t/cl-wish1/done", nil, http.StatusConflict, "done already"},
	} {
		rec := postForm(t, h, c.path, "", c.hdr...)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %q, want %d containing %q", c.name, rec.Code, rec.Body.String(), c.code, c.want)
		}
		if c.name == "done" {
			body := rec.Body.String()
			if !strings.Contains(body, "cm-wish--closed") || strings.Contains(body, "cm-done") {
				t.Errorf("done: the answer is not the closed head without Done:\n%s", body)
			}
			if note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note")); note != "Marked done." {
				t.Errorf("done note %q", note)
			}
		}
	}
	if len(react.got) != 1 || react.got[0] != (ReactRequest{Kind: "react", Message: "cl-wish1", Reaction: "done"}) {
		t.Errorf("the bridge got %+v, want one done on cl-wish1", react.got)
	}
	for _, b := range react.bearer {
		if b != commonstest.ClaimlayerBearer {
			t.Errorf("the bridge call carried bearer %q, want the viewer's", b)
		}
	}
}

// TestDone_Refusals: the bridge's refusal reaches the viewer in its words, and
// with no bridge Done answers 501.
func TestDone_Refusals(t *testing.T) {
	d := commonstest.TwoLands(t0)
	react := &fakeReactor{d: d, err: &CardRefusal{Status: http.StatusConflict, Message: "This wish needs triage."}}
	h := commonsServer(t, d, func(h *Handler) { h.React = react })
	if rec := postForm(t, h, "/garden/c/t/cl-wish1/done", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "needs triage") {
		t.Errorf("triage: %d %q", rec.Code, rec.Body.String())
	}
	off := commonsServer(t, commonstest.TwoLands(t0))
	if rec := postForm(t, off, "/garden/c/t/cl-wish1/done", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("no bridge: %d %q", rec.Code, rec.Body.String())
	}
}

// TestDone_Offered: an open wish head offers Done in its thread and its
// refetch, to a viewer who may mark it done, and only when the Commons
// reaches the bridge.
func TestDone_Offered(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) { h.React = &fakeReactor{d: d} })
	for _, path := range []string{"/garden/c/claimlayer/general/t/cl-wish1", "/test/m/cl-wish1"} {
		if code, body := get(t, h, path, "claimlayer"); code != 200 || !strings.Contains(body, doneForm) {
			t.Errorf("GET %s = %d, want the Done form:\n%s", path, code, body)
		}
	}
	r := httptest.NewRequest("GET", "/garden/c/claimlayer/general/t/cl-wish1", nil)
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("X-Test-Guest", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if strings.Contains(rec.Body.String(), "cm-done") {
		t.Errorf("a guest is offered Done:\n%s", rec.Body.String())
	}
	if _, body := get(t, commonsServer(t, commonstest.TwoLands(t0)), "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer"); strings.Contains(body, "cm-done") {
		t.Errorf("Done offered with no bridge:\n%s", body)
	}

	// The thread with Done, pinned like the other fragments (TestFragmentGoldens).
	_, body := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	file := filepath.Join("testdata", "thread-done.golden.html")
	if *update {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if body != string(want) {
		t.Errorf("the thread with Done drifted from %s (run with -update if meant):\n%s", file, body)
	}
}
