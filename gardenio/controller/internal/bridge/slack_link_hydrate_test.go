package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const testViewer = "https://viewer.example"

// hydrateFixture is the real plan-gardener watchdog squawk from the design
// thread (gd-8Gxghn6hkt) and the beads it names.
var hydrateFixture = map[string]*beadsapi.BeadDetail{
	"gd-Oc3GVm9gc6": {ID: "gd-Oc3GVm9gc6", Type: "epic", Labels: []string{"plan"}, Status: "in_progress", Priority: 1,
		Title: "Attribute agent slowness to specific tool calls and commands"},
	"gd-Lh590iWd8Y": {ID: "gd-Lh590iWd8Y", Type: "decision", Status: "open", Priority: 2,
		Title: "Plan-gardener gd-Oc3GVm9gc6 hits its 8-run budget this cycle — extend watchdog coverage?"},
	"gd-lGvUZtjpRQ": {ID: "gd-lGvUZtjpRQ", Type: "task", Status: "in_progress", Assignee: "gardenio-p0-forward-durationms-ud9",
		Title: "P0: forward duration_ms + tool_use_id through gb hook relay"},
	"gd-GdEpjNcXXh": {ID: "gd-GdEpjNcXXh", Type: "task", Status: "open",
		Title: "P5: surface tool/command latency — Grafana panels + gb tools top"},
	"gd-YcmtujOTrC": {ID: "gd-YcmtujOTrC", Type: "task", Status: "blocked",
		Title: "P4: home for the slow-call ledger + per-session slowest-commands view"},
	"gd-6LYU2D5VUQ": {ID: "gd-6LYU2D5VUQ", Type: "escalation", Status: "open", Priority: 2,
		Title: "Merge PR #209: P4 per-session slowest tool calls (gd-YcmtujOTrC)"},
	"gd-DDG9qRDcmq": {ID: "gd-DDG9qRDcmq", Type: "agent", Status: "closed", Title: "gardenio-p3-measure-the-u4n"},
}

const hydrateFixtureText = "Plan-gardener gd-Oc3GVm9gc6 watchdog (see decision gd-Lh590iWd8Y): " +
	"P0 (gd-lGvUZtjpRQ) has a live agent. P5 (gd-GdEpjNcXXh) left open. " +
	"P4 gd-YcmtujOTrC parked on gd-6LYU2D5VUQ."

func newTestHydrator(beads map[string]*beadsapi.BeadDetail, th Theme) (*linkHydrator, *int) {
	calls := 0
	h := newLinkHydrator(testViewer, nil)
	h.fetch = func(_ context.Context, ids []string) map[string]*beadsapi.BeadDetail {
		calls++
		out := make(map[string]*beadsapi.BeadDetail)
		for _, id := range ids {
			if b, ok := beads[id]; ok {
				out[id] = b
			}
		}
		return out
	}
	h.theme = func(context.Context, string) Theme { return th }
	return h, &calls
}

func formVals(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestHydrateForm_InlineGarden(t *testing.T) {
	h, calls := newTestHydrator(hydrateFixture, Garden)
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	if !h.hydrateForm(context.Background(), vals) {
		t.Fatal("expected hydration to change the form")
	}
	got := vals.Get("text")

	wantInline := []string{
		"🗺️ <" + testViewer + "/gd-Oc3GVm9gc6|Attribute agent slowness to specific…>",
		"🪧 <" + testViewer + "/gd-Lh590iWd8Y|extend watchdog coverage?>",
		"P0 (🌱 <" + testViewer + "/gd-lGvUZtjpRQ|forward duration_ms + tool_use_id…>) has a live agent",
		"🌱 <" + testViewer + "/gd-GdEpjNcXXh|surface tool/command latency>",
		"🚨 <" + testViewer + "/gd-6LYU2D5VUQ|Merge PR #209>",
	}
	for _, w := range wantInline {
		if !strings.Contains(got, w) {
			t.Errorf("inline text missing %q\n got: %s", w, got)
		}
	}
	// No bare id survives outside a link URL: strip every <url|label> span
	// and look for ids in what is left.
	if ids := scanBareBeadIDsAll(got); len(ids) != 0 {
		t.Errorf("bare ids survived hydration: %v\n%s", ids, got)
	}
	// One batch round-trip for the whole message.
	if *calls != 1 {
		t.Errorf("fetch calls = %d, want 1", *calls)
	}
}

func TestHydrateForm_FooterGardenOrderAndShape(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")

	i := strings.Index(got, "\n> ")
	if i < 0 {
		t.Fatalf("no quoted footer in text:\n%s", got)
	}
	footer := got[i+1:]
	wantLines := []string{
		"> 🪧 🫘 <" + testViewer + "/gd-Lh590iWd8Y|extend watchdog coverage?> · P2",
		"> 🚨 🫘 <" + testViewer + "/gd-6LYU2D5VUQ|Merge PR #209> · awaiting operator",
		"> 🗺️ 🍃 <" + testViewer + "/gd-Oc3GVm9gc6|Attribute agent slowness to specific…> · P1",
		"> 🌱 🥀 <" + testViewer + "/gd-YcmtujOTrC|home for the slow-call ledger…>",
		"> 🌱 🍃 <" + testViewer + "/gd-lGvUZtjpRQ|forward duration_ms + tool_use_id…> · @gardenio-p0-forward-durationms-ud9",
		"> 🌱 🫘 <" + testViewer + "/gd-GdEpjNcXXh|surface tool/command latency> · unassigned",
	}
	if footer != strings.Join(wantLines, "\n") {
		t.Errorf("footer mismatch\n got:\n%s\nwant:\n%s", footer, strings.Join(wantLines, "\n"))
	}
	// Footer carries no status words and no ids outside links.
	for _, banned := range []string{"in progress", "open ·", "blocked", "`gd-"} {
		if strings.Contains(footer, banned) {
			t.Errorf("footer contains %q, which the design forbids:\n%s", banned, footer)
		}
	}
}

// TestHydrateForm_ShortTitleColumnLabelsLinks pins that an authored
// short_title (the column, gd-V5nGseRS5f) is the label both inline and in the
// References footer, and that the bead id appears only as the link target.
func TestHydrateForm_ShortTitleColumnLabelsLinks(t *testing.T) {
	beads := map[string]*beadsapi.BeadDetail{}
	for id, b := range hydrateFixture {
		c := *b
		beads[id] = &c
	}
	beads["gd-lGvUZtjpRQ"].ShortTitle = "relay hook durations"
	h, _ := newTestHydrator(beads, Garden)
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	if !h.hydrateForm(context.Background(), vals) {
		t.Fatal("expected hydration to change the form")
	}
	got := vals.Get("text")

	link := "<" + testViewer + "/gd-lGvUZtjpRQ|relay hook durations>"
	if !strings.Contains(got, "P0 (🌱 "+link+") has a live agent") {
		t.Errorf("inline text missing short-title link %q\n got: %s", link, got)
	}
	i := strings.Index(got, "\n> ")
	if i < 0 {
		t.Fatalf("no quoted footer in text:\n%s", got)
	}
	if want := "> 🌱 🍃 " + link + " · @gardenio-p0-forward-durationms-ud9"; !strings.Contains(got[i+1:], want) {
		t.Errorf("footer missing %q\n got:\n%s", want, got[i+1:])
	}
	if ids := scanBareBeadIDsAll(got); len(ids) != 0 {
		t.Errorf("bare ids survived hydration: %v\n%s", ids, got)
	}
	if n := strings.Count(got, "gd-lGvUZtjpRQ"); n != strings.Count(got, testViewer+"/gd-lGvUZtjpRQ|") {
		t.Errorf("gd-lGvUZtjpRQ appears outside a link target:\n%s", got)
	}
}

func TestHydrateForm_NoFooterBelowThreshold(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	vals := formVals("channel", "C1", "text", "Spawned gd-DDG9qRDcmq on gd-GdEpjNcXXh with context.")
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")
	if strings.Contains(got, "\n>") {
		t.Errorf("two beads must not draw a footer:\n%s", got)
	}
	if !strings.Contains(got, "🤖 <"+testViewer+"/gd-DDG9qRDcmq|@gardenio-p3-measure-the-u4n>") {
		t.Errorf("agent bead should render as its handle:\n%s", got)
	}
}

func TestHydrateForm_FooterCapsLines(t *testing.T) {
	beads := make(map[string]*beadsapi.BeadDetail)
	var ids []string
	for i := 0; i < 9; i++ {
		id := "gd-cap" + string(rune('A'+i))
		beads[id] = &beadsapi.BeadDetail{ID: id, Type: "task", Status: "open", Title: "task " + string(rune('A'+i))}
		ids = append(ids, id)
	}
	h, _ := newTestHydrator(beads, Garden)
	vals := formVals("channel", "C1", "text", "closed "+strings.Join(ids, ", "))
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")
	if n := strings.Count(got, "\n> "); n != hydrateFooterMax+1 {
		t.Errorf("footer lines = %d, want %d lines + '+N more'\n%s", n, hydrateFooterMax, got)
	}
	if !strings.HasSuffix(got, "> +3 more") {
		t.Errorf("footer should end with '+3 more':\n%s", got)
	}
}

func TestHydrateForm_Business(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Business)
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")

	// Inline: tags only for plan/decision/escalation; tasks get a bare link.
	for _, w := range []string{
		"[PLAN] <" + testViewer + "/gd-Oc3GVm9gc6|",
		"[DECISION] <" + testViewer + "/gd-Lh590iWd8Y|",
		"[ESCALATION] <" + testViewer + "/gd-6LYU2D5VUQ|",
		"P0 (<" + testViewer + "/gd-lGvUZtjpRQ|",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("Business inline missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "[TASK] <") && !strings.Contains(got, "> [TASK]") {
		t.Errorf("Business must not tag tasks inline:\n%s", got)
	}
	// Footer: bracket tags + geometric status glyphs, no Garden emoji.
	if !strings.Contains(got, "> [DECISION] ○ <") || !strings.Contains(got, "> [TASK] ■ <") {
		t.Errorf("Business footer shape wrong:\n%s", got)
	}
	for _, g := range Garden.BeadType {
		if strings.Contains(got, g) {
			t.Errorf("Business output leaked Garden glyph %q:\n%s", g, got)
		}
	}
}

func TestHydrateForm_BlocksGetContextFooter(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"` + hydrateFixtureText + `"}}]`
	vals := formVals("channel", "C1", "blocks", blocks)
	if !h.hydrateForm(context.Background(), vals) {
		t.Fatal("expected change")
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(vals.Get("blocks")), &arr); err != nil {
		t.Fatalf("blocks not JSON: %v\n%s", err, vals.Get("blocks"))
	}
	if len(arr) != 2 {
		t.Fatalf("want section + context, got %d blocks", len(arr))
	}
	sec := arr[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(sec, "🪧 <"+testViewer+"/gd-Lh590iWd8Y|extend watchdog coverage?>") {
		t.Errorf("section not hydrated: %s", sec)
	}
	if arr[1]["type"] != "context" {
		t.Fatalf("appended block type = %v, want context", arr[1]["type"])
	}
	el := arr[1]["elements"].([]any)[0].(map[string]any)
	footer := el["text"].(string)
	if strings.HasPrefix(footer, "> ") || strings.Contains(footer, "\n> ") || strings.Count(footer, "\n") != 5 {
		t.Errorf("context footer should be 6 unquoted lines, got:\n%s", footer)
	}
	// No HTML escaping of the link syntax.
	if strings.Contains(vals.Get("blocks"), `\u003c`) {
		t.Error("blocks JSON HTML-escaped the link angle brackets")
	}
}

func TestHydrateForm_AttachmentsInlineOnlyNoFooter(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	atts := `[{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"` + hydrateFixtureText + `"}}]}]`
	vals := formVals("channel", "C1", "attachments", atts)
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("attachments")
	if !strings.Contains(got, "🚨 <"+testViewer+"/gd-6LYU2D5VUQ|Merge PR #209>") {
		t.Errorf("attachment mrkdwn not hydrated:\n%s", got)
	}
	if strings.Contains(got, `"context"`) || vals.Get("text") != "" || vals.Get("blocks") != "" {
		t.Errorf("attachments-only message must not grow a footer:\ntext=%q blocks=%q", vals.Get("text"), vals.Get("blocks"))
	}
}

func TestHydrateForm_LeavesExistingLinksAndUnknownIDs(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	text := "already <" + testViewer + "/gd-Oc3GVm9gc6|linked> and " + testViewer + "/gd-Lh590iWd8Y and unknown gd-nope plus gd-GdEpjNcXXh"
	vals := formVals("channel", "C1", "text", text)
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")
	if strings.Count(got, "gd-Oc3GVm9gc6") != 1 || strings.Count(got, "gd-Lh590iWd8Y") != 1 {
		t.Errorf("ids inside existing links/URLs must be untouched:\n%s", got)
	}
	if !strings.Contains(got, "unknown gd-nope plus") {
		t.Errorf("unknown id must be left for the regex rules:\n%s", got)
	}
	if !strings.Contains(got, "🌱 <"+testViewer+"/gd-GdEpjNcXXh|surface tool/command latency>") {
		t.Errorf("resolvable id must hydrate:\n%s", got)
	}
}

func TestHydrateForm_FetchFailureFallsBack(t *testing.T) {
	h := newLinkHydrator(testViewer, nil)
	h.fetch = func(context.Context, []string) map[string]*beadsapi.BeadDetail { return nil }
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	if h.hydrateForm(context.Background(), vals) {
		t.Fatal("a failed fetch must leave the form untouched")
	}
	if vals.Get("text") != hydrateFixtureText {
		t.Fatalf("text changed on failed fetch: %s", vals.Get("text"))
	}
}

func TestHydrateForm_NotReadyIsNoop(t *testing.T) {
	var nilH *linkHydrator
	if nilH.ready() {
		t.Fatal("nil hydrator must not be ready")
	}
	vals := formVals("text", hydrateFixtureText)
	if nilH.hydrateForm(context.Background(), vals) {
		t.Fatal("nil hydrator must be a no-op")
	}
	noFetch := newLinkHydrator(testViewer, nil)
	if noFetch.ready() {
		t.Fatal("hydrator without fetch must not be ready")
	}
	noViewer := newLinkHydrator("", nil)
	noViewer.fetch = func(context.Context, []string) map[string]*beadsapi.BeadDetail { return nil }
	if noViewer.ready() {
		t.Fatal("hydrator without viewer URL must not be ready")
	}
}

func TestHydrateResolve_CachesHitsAndMisses(t *testing.T) {
	h, calls := newTestHydrator(hydrateFixture, Garden)
	now := time.Date(2026, 10, 11, 16, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	ids := []string{"gd-Oc3GVm9gc6", "gd-nope"}
	if got := h.resolve(context.Background(), ids); len(got) != 1 {
		t.Fatalf("first resolve: got %d beads, want 1", len(got))
	}
	h.resolve(context.Background(), ids)
	if *calls != 1 {
		t.Fatalf("second resolve within TTL hit the daemon: calls=%d", *calls)
	}
	now = now.Add(hydrateCacheTTL + time.Second)
	h.resolve(context.Background(), ids)
	if *calls != 2 {
		t.Fatalf("resolve after TTL should refetch: calls=%d", *calls)
	}
}

func TestRewriteFormBodyHydrated_RunsBeforeRules(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	rules := buildLinkRules(LinkifyConfig{BeadsViewerURL: testViewer})
	body := []byte(url.Values{"channel": {"C1"}, "text": {"see gd-GdEpjNcXXh and gd-unknown"}}.Encode())
	out, changed := rewriteFormBodyHydrated(context.Background(), body, rules, h)
	if !changed {
		t.Fatal("expected change")
	}
	vals, err := url.ParseQuery(out)
	if err != nil {
		t.Fatal(err)
	}
	got := vals.Get("text")
	want := "see 🌱 <" + testViewer + "/gd-GdEpjNcXXh|surface tool/command latency> and <" + testViewer + "/gd-unknown|gd-unknown>"
	if got != want {
		t.Errorf("hydration + rules\n got %q\nwant %q", got, want)
	}

	// Without a hydrator the legacy behaviour is byte-identical.
	legacy, _ := rewriteFormBody(body, rules)
	noH, _ := rewriteFormBodyHydrated(context.Background(), body, rules, nil)
	if legacy != noH {
		t.Errorf("nil hydrator must match rewriteFormBody:\n%s\n%s", legacy, noH)
	}
}

func TestHydrateTypeGlyph_FallbacksAndPlan(t *testing.T) {
	plan := &beadsapi.BeadDetail{Type: "epic", Labels: []string{"plan"}}
	if g := hydrateTypeGlyph(Garden, plan); g != "🗺️" {
		t.Errorf("Garden plan glyph = %q", g)
	}
	if g := hydrateTypeGlyph(Business, plan); g != "[PLAN]" {
		t.Errorf("Business plan glyph = %q", g)
	}
	bundle := &beadsapi.BeadDetail{Type: "bundle"}
	if g := hydrateTypeGlyph(Garden, bundle); g != "🪺" {
		t.Errorf("Garden unthemed fallback = %q", g)
	}
	if g := hydrateTypeGlyph(Business, bundle); g != "[BUNDLE]" {
		t.Errorf("Business unthemed fallback = %q", g)
	}
}

func TestHydrateResolve_CacheStoresSlimBeads(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-lGvUZtjpRQ": {ID: "gd-lGvUZtjpRQ", Type: "task", Status: "open", Priority: 2, Assignee: "a",
			Title: "P0: forward duration_ms through gb hook relay", Description: big, Notes: big,
			Fields: map[string]string{"big": big}},
	}
	h, _ := newTestHydrator(beads, Garden)
	got := h.resolve(context.Background(), []string{"gd-lGvUZtjpRQ"})["gd-lGvUZtjpRQ"]
	if got == nil {
		t.Fatal("bead not resolved")
	}
	if got.Description != "" || got.Notes != "" || got.Title != "" || got.Fields != nil {
		t.Fatalf("cache kept more than render fields: %+v", got)
	}
	if want := beadDisplayName(beads["gd-lGvUZtjpRQ"]); beadDisplayName(got) != want {
		t.Fatalf("display name changed through slimming: %q vs %q", beadDisplayName(got), want)
	}
	if got.Type != "task" || got.Status != "open" || got.Priority != 2 || got.Assignee != "a" {
		t.Fatalf("render fields lost: %+v", got)
	}
}

func TestSlimBead_KeepsPlanAndAgentRendering(t *testing.T) {
	plan := fixtureBead("gd-Oc3GVm9gc6")
	if !beadIsPlan(slimBead(plan)) {
		t.Error("plan label lost")
	}
	agent := fixtureBead("gd-DDG9qRDcmq")
	if got, want := beadDisplayName(slimBead(agent)), beadDisplayName(agent); got != want {
		t.Errorf("agent name %q != %q", got, want)
	}
	long := &beadsapi.BeadDetail{ID: "gd-x", Type: "task", Title: strings.Repeat("word ", 30)}
	if got, want := beadDisplayName(slimBead(long)), beadDisplayName(long); got != want {
		t.Errorf("truncated name not stable: %q vs %q", got, want)
	}
}

func fixtureBead(id string) *beadsapi.BeadDetail { return hydrateFixture[id] }

func TestHydrateResolve_SweepsExpiredAndCapsSize(t *testing.T) {
	h, _ := newTestHydrator(nil, Garden)
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.fetch = func(_ context.Context, ids []string) map[string]*beadsapi.BeadDetail {
		out := make(map[string]*beadsapi.BeadDetail, len(ids))
		for _, id := range ids {
			out[id] = &beadsapi.BeadDetail{ID: id, Type: "task", Title: "t " + id}
		}
		return out
	}

	h.resolve(context.Background(), []string{"gd-old1", "gd-old2"})
	now = now.Add(hydrateCacheTTL + time.Second)
	h.resolve(context.Background(), []string{"gd-new1"})
	h.mu.Lock()
	n := len(h.cache)
	_, stale := h.cache["gd-old1"]
	h.mu.Unlock()
	if stale || n != 1 {
		t.Fatalf("expired entries not swept: len=%d stale=%v", n, stale)
	}

	// Many distinct live ids: the map must stay at the cap.
	for i := 0; i < hydrateCacheMax/hydrateMaxIDs+5; i++ {
		ids := make([]string, hydrateMaxIDs)
		for j := range ids {
			ids[j] = fmt.Sprintf("gd-b%d-%d", i, j)
		}
		now = now.Add(time.Millisecond) // later insertions expire later
		h.resolve(context.Background(), ids)
	}
	h.mu.Lock()
	n = len(h.cache)
	h.mu.Unlock()
	if n > hydrateCacheMax+hydrateMaxIDs {
		t.Fatalf("cache grew past cap: %d entries", n)
	}
}

func TestHydrateForm_BlockKitLimits(t *testing.T) {
	// Context elements cap at 2000, section text at 3000. Pad so the hydrated
	// text lands between the two limits: it fits a section but not a context.
	pad := strings.Repeat("z", 1990)
	text := pad + " gd-lGvUZtjpRQ"
	h, _ := newTestHydrator(hydrateFixture, Garden)
	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"` + text + `"}},` +
		`{"type":"context","elements":[{"type":"mrkdwn","text":"` + text + `"}]},` +
		`{"type":"section","fields":[{"type":"mrkdwn","text":"` + text + `"}]}]`
	vals := formVals("channel", "C1", "blocks", blocks)
	if !h.hydrateForm(context.Background(), vals) {
		t.Fatal("expected the section text to hydrate")
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(vals.Get("blocks")), &arr); err != nil {
		t.Fatal(err)
	}
	if got := arr[0]["text"].(map[string]any)["text"].(string); !strings.Contains(got, testViewer+"/gd-lGvUZtjpRQ|") {
		t.Errorf("section text (3000 limit) should have hydrated: %.60s…", got[1980:])
	}
	if got := arr[1]["elements"].([]any)[0].(map[string]any)["text"].(string); got != text {
		t.Errorf("context element (2000 limit) must be left as-is")
	}
	if got := arr[2]["fields"].([]any)[0].(map[string]any)["text"].(string); got != text {
		t.Errorf("section field (2000 limit) must be left as-is")
	}
}

func TestHydrateForm_FooterSkippedAtBlockCap(t *testing.T) {
	h, _ := newTestHydrator(hydrateFixture, Garden)
	mk := func(n int) string {
		var parts []string
		parts = append(parts, `{"type":"section","text":{"type":"mrkdwn","text":"`+hydrateFixtureText+`"}}`)
		for i := 1; i < n; i++ {
			parts = append(parts, `{"type":"divider"}`)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for _, tc := range []struct {
		blocks int
		want   int
	}{{49, 50}, {50, 50}} {
		vals := formVals("channel", "C1", "blocks", mk(tc.blocks))
		h.hydrateForm(context.Background(), vals)
		var arr []any
		if err := json.Unmarshal([]byte(vals.Get("blocks")), &arr); err != nil {
			t.Fatal(err)
		}
		if len(arr) != tc.want {
			t.Errorf("%d blocks in: got %d out, want %d", tc.blocks, len(arr), tc.want)
		}
	}
	// 50 in: inline labels still apply, only the footer is skipped.
	vals := formVals("channel", "C1", "blocks", mk(50))
	h.hydrateForm(context.Background(), vals)
	if !strings.Contains(vals.Get("blocks"), testViewer+"/gd-lGvUZtjpRQ|") {
		t.Error("inline hydration should still run at the block cap")
	}
}
