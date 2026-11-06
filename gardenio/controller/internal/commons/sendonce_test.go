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

// heldWriter holds each CreateBead until gate gives way, and says on in when
// one is waiting there.
type heldWriter struct {
	Writer
	in, gate chan struct{}
}

func (w heldWriter) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	w.in <- struct{}{}
	<-w.gate
	return w.Writer.CreateBead(ctx, req)
}

func postNonce(h http.Handler, path, text, nonce string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"text": {text}, "nonce": {nonce}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-As", "claimlayer")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A Retry of a send that was only slow (gd-JpYV1FWtnZ): the second attempt with
// the same nonce waits for the first and answers its message, so one bead is
// written, and it keeps the nonce for the stream's copy of the row. A later
// repeat answers the same message; another poster's same nonce, and a new
// nonce, write their own.
func TestReplyWithANonceWritesOnce(t *testing.T) {
	d := commonstest.TwoLands(t0)
	in, gate := make(chan struct{}, 8), make(chan struct{})
	h := commonsServer(t, d, func(h *Handler) { h.Writes = heldWriter{Writer: d, in: in, gate: gate} })
	const path, nonce = "/garden/c/t/cl-msg1/reply", "n0nce-retry-1"

	first, again := make(chan *httptest.ResponseRecorder), make(chan *httptest.ResponseRecorder)
	go func() { first <- postNonce(h, path, "on my way", nonce) }()
	<-in // the first attempt is writing
	go func() { again <- postNonce(h, path, "on my way", nonce) }()
	select {
	case <-in:
		t.Fatal("the Retry wrote while the first attempt was still writing")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	a, b := <-first, <-again
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("attempts = %d %s / %d %s", a.Code, a.Body, b.Code, b.Body)
	}
	if len(d.Created) != 1 {
		t.Fatalf("%d beads written, want 1", len(d.Created))
	}
	_, f := lastCreated(t, d)
	if f[fieldNonce] != nonce {
		t.Errorf("fields = %v, want nonce %q", f, nonce)
	}
	mustContain(t, a.Body.String(), `id="m-cm-new1"`, `data-nonce="`+nonce+`"`)
	mustContain(t, b.Body.String(), `id="m-cm-new1"`, `data-nonce="`+nonce+`"`)

	if rec := postNonce(h, path, "on my way", nonce); rec.Code != http.StatusCreated || len(d.Created) != 1 {
		t.Errorf("a later repeat = %d, %d beads", rec.Code, len(d.Created))
	}
	postNonce(h, path, "on my way", nonce, "X-Test-Self", "oidc:ann@claimlayer.example")
	postNonce(h, path, "on my way", "n0nce-retry-2")
	if len(d.Created) != 3 {
		t.Errorf("%d beads written, want 3: another poster's and a new nonce write their own", len(d.Created))
	}
}

// An attempt that wrote nothing frees its nonce: the Retry writes the message.
func TestNonceOfAFailedSendIsFree(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.FailWrites = &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "down"}
	h := commonsServer(t, d)
	const path, nonce = "/garden/c/t/cl-msg1/reply", "n0nce-failed-1"
	if rec := postNonce(h, path, "again", nonce); rec.Code == http.StatusCreated {
		t.Fatalf("a failed write = %d", rec.Code)
	}
	d.FailWrites = nil
	if rec := postNonce(h, path, "again", nonce); rec.Code != http.StatusCreated || len(d.Created) != 1 {
		t.Errorf("the Retry = %d %s, %d beads", rec.Code, rec.Body, len(d.Created))
	}
}

func TestSendKey(t *testing.T) {
	for _, c := range []struct{ who, nonce, want string }{
		{"oidc:bo@x", "abcd1234", "oidc:bo@x\x00abcd1234"},
		{"", "abcd1234", ""},
		{"oidc:bo@x", "", ""},
		{"oidc:bo@x", "short", ""},
		{"oidc:bo@x", "bad nonce!", ""},
		{"oidc:bo@x", strings.Repeat("a", 65), ""},
	} {
		if got := sendKey(c.who, c.nonce); got != c.want {
			t.Errorf("sendKey(%q, %q) = %q, want %q", c.who, c.nonce, got, c.want)
		}
	}
}
