package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Phase 3 task 3-5 (ka-0000001087) — polish tests for the /threads surface:
// markdown parity, the optimistic-reconcile de-dup key, accessible-composer
// markup, the dark-skin contrast alias, and the empty/loading/error chrome.

// --- (e) optimistic-reconcile de-dup key -----------------------------------

// TestThreadMsgKey_GoldenVectors pins threadMsgKey's output against fixed
// vectors. These same (author, text) -> key pairs are produced byte-for-byte by
// msgKey() in templates/static/thread_composer.js (verified by running the JS
// function over the identical inputs); if either side drifts, the optimistic
// bubble would stop matching its server echo and the de-dup would silently
// break (bubble lingers or duplicates). The vectors below are the JS output.
func TestThreadMsgKey_GoldenVectors(t *testing.T) {
	cases := []struct {
		author, text, want string
	}{
		{"matt@example.com", "hello world", "b7a4fbbe"},
		{"", "x", "59770925"},
		{"a", "  spaced  ", "aa5dfe9a"},
		{"u", "list: `code` and [x](y)", "ba50fec8"},
		// NEL / BOM edge vectors (ka-0000006057 gap 1). threadKeyTrim and the JS
		// keyTrim() both strip the UNION of the Go and JS whitespace sets, so a
		// NEL- or BOM-wrapped input keys IDENTICALLY to its bare form. These want
		// values are the JS msgKey() output over the same inputs (node-verified),
		// so they also pin the Go↔JS byte-identity for the two chars where plain
		// strings.TrimSpace and String.prototype.trim() disagree.
		{"a", "\u0085hi\u0085", "4ea635d7"},                // leading/trailing NEL (== "a","hi")
		{"a", "hi", "4ea635d7"},                            // bare form: same key
		{"\uFEFFbob\uFEFF", "\uFEFFyes\uFEFF", "e13359c7"}, // leading/trailing BOM (== "bob","yes")
		{"bob", "yes", "e13359c7"},                         // bare form: same key
	}
	for _, c := range cases {
		if got := threadMsgKey(c.author, c.text); got != c.want {
			t.Errorf("threadMsgKey(%q,%q) = %q, want %q (must match thread_composer.js msgKey)", c.author, c.text, got, c.want)
		}
	}
}

// TestThreadMsgKey_TrimsAndSeparates proves the key trims surrounding whitespace
// on BOTH halves (so a leading-space draft matches its trimmed server echo) and
// that the author/text boundary is real (swapping author<->text yields a
// different key, i.e. the NUL separator prevents "ab"+"c" colliding with
// "a"+"bc").
func TestThreadMsgKey_TrimsAndSeparates(t *testing.T) {
	if threadMsgKey("alice", "hi there") != threadMsgKey("  alice ", "\thi there\n") {
		t.Errorf("threadMsgKey must be whitespace-insensitive on author and text")
	}
	if threadMsgKey("ab", "c") == threadMsgKey("a", "bc") {
		t.Errorf("threadMsgKey author/text boundary must be unambiguous (NUL separator)")
	}
}

// TestConversation_CarriesDedupKey proves the curation layer stamps a non-empty
// content key on conversation bubbles (comments + composer messages) and leaves
// lifecycle markers keyless — so only real messages participate in the
// optimistic reconcile.
func TestConversation_CarriesDedupKey(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head,
		[]threadMemberRow{{ID: head, IsHead: true}, {ID: "gd-msg", IsMessage: true}},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),         // opened marker -> no key
			tEntry(20, threadEntryComment, head, "wish", "bob", "a reply", 1),        // comment -> key
			tEntry(30, threadEntryMessage, "gd-msg", "msg", "matt@x", "composed", 0), // composer msg -> key
		})
	convo := v.Conversation()
	if len(convo) != 3 {
		t.Fatalf("Conversation len = %d, want 3", len(convo))
	}
	if convo[0].Key != "" {
		t.Errorf("lifecycle marker must carry no de-dup key, got %q", convo[0].Key)
	}
	if convo[1].Key == "" || convo[1].Key != threadMsgKey("bob", "a reply") {
		t.Errorf("comment bubble key = %q, want %q", convo[1].Key, threadMsgKey("bob", "a reply"))
	}
	if convo[2].Key == "" || convo[2].Key != threadMsgKey("matt@x", "composed") {
		t.Errorf("composer message key = %q, want %q", convo[2].Key, threadMsgKey("matt@x", "composed"))
	}
}

// TestThreadDetail_RendersDedupKeyAttr proves the rendered detail page stamps
// data-msg-key onto each conversation bubble, keyed exactly as the composer JS
// computes it — the hook the optimistic reconcile matches against.
func TestThreadDetail_RendersDedupKeyAttr(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "wish", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedAt: now.Add(-time.Hour),
		},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish": {{ID: 1, Author: "bob", Text: "a reply", CreatedAt: now.Add(-30 * time.Minute)}},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	want := `data-msg-key="` + threadMsgKey("bob", "a reply") + `"`
	if !strings.Contains(body, want) {
		t.Errorf("rendered bubble missing %s", want)
	}
}

// --- (b) markdown parity ----------------------------------------------------

// TestThreadDetail_MarkdownParity proves a conversation message body renders
// through the SAME goldmark pipeline bead comments use: a code span becomes
// <code>, a link becomes <a>, and a list becomes <li> — identical to the bead
// comment view. The bubble also wears the .comment-body class so it inherits the
// per-skin code/pre treatment.
func TestThreadDetail_MarkdownParity(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "wish", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedAt: now.Add(-time.Hour),
		},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish": {{
			ID: 1, Author: "bob",
			Text:      "run `make test` then see [docs](https://example.com)\n\n- one\n- two",
			CreatedAt: now.Add(-30 * time.Minute),
		}},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	for _, want := range []string{
		"<code>make test</code>",                // code span
		`<a href="https://example.com"`,         // link
		">one</li>",                             // list item (<li> now carries a data-block-id, T1)
		`class="thread2-msg-body comment-body"`, // parity styling hook
	} {
		if !strings.Contains(body, want) {
			t.Errorf("markdown parity: rendered body missing %q", want)
		}
	}
	// The raw markdown source must NOT survive as literal text.
	if strings.Contains(body, "`make test`") {
		t.Errorf("code span left as raw backticks — markdown did not render")
	}
}

// --- (c) accessible composer ------------------------------------------------

// TestThreadComposer_AccessibleMarkup proves the composer renders a programmatic
// label, a focusable textarea described by the keyboard-shortcut hint, and an
// assertive error region — the a11y-baseline shape (labelled control + announced
// errors). The conversation log is an ARIA live region.
func TestThreadComposer_AccessibleMarkup(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "wish", Status: "open",
			Labels: []string{threadHeadLabel},
			Fields: map[string]string{
				threadSlackChannelField: "C0AHE7C0E1G",
				threadSlackTSField:      "1782600000.000100",
			},
			CreatedAt: now.Add(-time.Hour),
		},
	}
	mux := mkServerFromFake(t, f)
	// Authenticate as operator so CanCompose flips true (dev-bypass leaves
	// viewerCanOperate true; the email is the only missing piece).
	body, code := getBodyWithRoles(t, mux, "/threads/gd-wish", "matt@example.com", nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	for _, want := range []string{
		`<label class="thread2-composer-label" for="thread-composer-text"`, // programmatic label
		`id="thread-composer-text"`,                                        // matching control id
		`aria-describedby="thread-composer-hint"`,                          // hint association
		`id="thread-composer-hint"`,                                        // the hint element
		`role="alert"`,                                                     // error region
		`role="log"`,                                                       // conversation live region
		`aria-live="polite"`,                                               // log politeness
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accessible composer: rendered page missing %q", want)
		}
	}
	// The redundant aria-label that previously overrode the visible label must
	// be gone (the <label for> is now the accessible name).
	if strings.Contains(body, `aria-label="Message this thread"`) {
		t.Errorf("textarea still carries the redundant aria-label that overrode the visible <label>")
	}
}

// --- (a)/(e) dark-skin contrast alias --------------------------------------

// TestThreadTemplates_DarkSkinAliased proves the thread surfaces alias
// data-skin="dark" onto the dark-terminal token overrides via the
// [data-skin^="dark"] selector, so the muted greys stay legible under the
// previously-untreated "dark" skin (the filed contrast defect). No bare
// html[data-skin="dark-terminal"] selectors should remain on the thread pages.
func TestThreadTemplates_DarkSkinAliased(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {ID: "gd-wish", Title: "wish", Status: "open", Labels: []string{threadHeadLabel}, CreatedAt: now},
	}
	mux := mkServerFromFake(t, f)
	for _, path := range []string{"/threads", "/threads/gd-wish"} {
		body, code := getBody(t, mux, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
		if !strings.Contains(body, `[data-skin^="dark"]`) {
			t.Errorf("%s: expected the [data-skin^=\"dark\"] alias selector (dark contrast fix)", path)
		}
		if strings.Contains(body, `[data-skin="dark-terminal"]`) {
			t.Errorf("%s: a bare [data-skin=\"dark-terminal\"] selector survived — dark skin would not be aliased", path)
		}
	}
}

// --- (a) loading / chrome ---------------------------------------------------

// TestThreadTimeline_LoadingAffordance proves the polled timeline region carries
// the aria-busy toggles and an (aria-hidden) "Updating…" indicator so a slow
// ?since= refresh shows activity rather than a frozen pane.
func TestThreadTimeline_LoadingAffordance(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {ID: "gd-wish", Title: "wish", Status: "open", Labels: []string{threadHeadLabel}, CreatedAt: now},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	for _, want := range []string{
		`hx-on:htmx:before-request="this.setAttribute('aria-busy','true')"`,
		`class="thread2-refreshing"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("loading affordance: missing %q", want)
		}
	}
}
