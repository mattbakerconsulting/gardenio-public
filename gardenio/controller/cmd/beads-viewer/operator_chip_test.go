package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestResolveOperatorChip_Human(t *testing.T) {
	chip := resolveOperatorChip("oidc:matt@example.com", "Matt", "", "", "agent-foo", "/b", "")
	if chip.Kind != "human" {
		t.Fatalf("Kind = %q, want human", chip.Kind)
	}
	if chip.Display != "Matt" {
		t.Errorf("Display = %q, want Matt", chip.Display)
	}
	if chip.RawID != "oidc:matt@example.com" {
		t.Errorf("RawID = %q", chip.RawID)
	}
	if !strings.Contains(chip.FilterHref, "operated_by=oidc%3Amatt%40example.com") {
		t.Errorf("FilterHref = %q, missing escaped operated_by", chip.FilterHref)
	}
	if chip.IsViewerSelf {
		t.Error("IsViewerSelf = true, want false (no viewer email)")
	}
}

func TestResolveOperatorChip_HumanFallbackDisplay(t *testing.T) {
	chip := resolveOperatorChip("slack:U123", "", "", "", "agent", "/b", "")
	if chip.Display != "slack:U123" {
		t.Errorf("Display = %q, want slack:U123 fallback", chip.Display)
	}
}

// The daemon stamps operated_by_display="gardenio" (the fleet bot) on beads a
// human actually drove via a Slack thread. A human operated_by carrying that
// bot-name sentinel must be re-attributed to the human from the resolved
// email, not rendered as "gardenio" (ka-0000003240 — the /plans creator-chip
// bug where every card read "gardenio"/GA).
func TestResolveOperatorChip_BotDisplaySentinelPrefersEmail(t *testing.T) {
	chip := resolveOperatorChip("slack:U0OPERATOR1", "gardenio", "Matthew.Baker@acme.example", "", "Matthew Baker", "/b", "")
	if chip.Kind != "human" {
		t.Fatalf("Kind = %q, want human", chip.Kind)
	}
	if chip.Display != "Matthew Baker" {
		t.Errorf("Display = %q, want Matthew Baker (re-attributed from email)", chip.Display)
	}
	// The raw human id and email survive for the raw chip / mailto link.
	if chip.RawID != "slack:U0OPERATOR1" {
		t.Errorf("RawID = %q, want slack id preserved", chip.RawID)
	}
	if chip.Email != "Matthew.Baker@acme.example" {
		t.Errorf("Email = %q, want email preserved", chip.Email)
	}
}

// Case-insensitive on the sentinel, and the OIDC human shape triggers the same
// re-attribution.
func TestResolveOperatorChip_BotDisplaySentinelCaseInsensitiveOIDC(t *testing.T) {
	chip := resolveOperatorChip("oidc:alice.smith@example.com", "Gardenio", "alice.smith@example.com", "", "some-agent", "/b", "")
	if chip.Display != "Alice Smith" {
		t.Errorf("Display = %q, want Alice Smith", chip.Display)
	}
}

// No email but a slug is present: re-attribute from the slug rather than the
// bot name.
func TestResolveOperatorChip_BotDisplaySentinelFallsBackToSlug(t *testing.T) {
	chip := resolveOperatorChip("slack:U0OPERATOR1", "gardenio", "", "matthew-baker", "agent", "/b", "")
	if chip.Display != "Matthew Baker" {
		t.Errorf("Display = %q, want Matthew Baker (from slug)", chip.Display)
	}
}

// Neither email nor slug: fall through to the raw id (never render the bot
// sentinel as the human's name).
func TestResolveOperatorChip_BotDisplaySentinelNoHumanSignalFallsToRaw(t *testing.T) {
	chip := resolveOperatorChip("slack:U0OPERATOR1", "gardenio", "", "", "agent", "/b", "")
	if chip.Display != "slack:U0OPERATOR1" {
		t.Errorf("Display = %q, want raw id fallback (not the bot sentinel)", chip.Display)
	}
}

// A fleet:/system: sentinel is classified as a machine chip BEFORE the
// bot-display re-attribution (ka-0000003240) can run — ka-0000003465. The
// daemon-stamped display="gardenio" is dropped in favor of the sentinel's own
// source suffix so it never renders as a bolded human; the machine guard owns
// this raw-id case now.
func TestResolveOperatorChip_FleetSentinelClassifiedMachine(t *testing.T) {
	chip := resolveOperatorChip("fleet:slack-bridge", "gardenio", "noreply@kbeads.local", "", "agent", "/b", "")
	if chip.Kind != "machine" {
		t.Fatalf("Kind = %q, want machine for fleet: sentinel", chip.Kind)
	}
	if chip.Display != "slack-bridge" {
		t.Errorf("Display = %q, want slack-bridge (source suffix), never the bot display", chip.Display)
	}
	if chip.Email != "" {
		t.Errorf("Email = %q, want empty — a machine chip carries no human email", chip.Email)
	}
	if chip.FilterHref != "" {
		t.Errorf("FilterHref = %q, want empty — no human filter link for a sentinel", chip.FilterHref)
	}
	if chip.IsViewerSelf {
		t.Error("IsViewerSelf = true, want false for a machine sentinel")
	}
}

// A genuine human name that is NOT the bot sentinel is left exactly as the
// daemon resolved it — no email-derived clobbering.
func TestResolveOperatorChip_RealHumanDisplayUntouched(t *testing.T) {
	chip := resolveOperatorChip("slack:U0OPERATOR1", "Matt Baker", "Matthew.Baker@acme.example", "", "agent", "/b", "")
	if chip.Display != "Matt Baker" {
		t.Errorf("Display = %q, want the resolved human display left intact", chip.Display)
	}
}

func TestHumanLabelFromEmail(t *testing.T) {
	cases := map[string]string{
		"Matthew.Baker@acme.example": "Matthew Baker",
		"alice_smith@example.com":   "Alice Smith",
		"mike@example.com":          "Mike",
		"a.b.c@x.io":                "A B C",
		"jane+tag@example.com":      "Jane",
		"":                          "",
		"@example.com":              "",
	}
	for in, want := range cases {
		if got := humanLabelFromEmail(in); got != want {
			t.Errorf("humanLabelFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveOperatorChip_Scheduler(t *testing.T) {
	chip := resolveOperatorChip("", "", "", "", "sched-cycle-watchdog", "/b", "")
	if chip.Kind != "scheduler" {
		t.Fatalf("Kind = %q, want scheduler", chip.Kind)
	}
}

// ka-0000003465: a fleet:/system: sentinel operated_by must classify as a
// machine chip — never a bolded human with a filter link and self-detection.
func TestResolveOperatorChip_MachineSentinels(t *testing.T) {
	cases := []struct {
		name        string
		operatedBy  string
		wantDisplay string
	}{
		{"fleet_slack_bridge", "fleet:slack-bridge", "slack-bridge"},
		{"fleet_gitlab_bridge", "fleet:gitlab-bridge", "gitlab-bridge"},
		{"system_machine", "system:machine", "machine"},
		{"system_scheduler", "system:scheduler", "scheduler"},
		{"system_audit_log", "system:audit-log", "audit-log"},
		// A bare prefix with no suffix falls back to the full id so the
		// "(automated: <source>)" label never renders blank.
		{"bare_fleet", "fleet:", "fleet:"},
		{"bare_system", "system:", "system:"},
		// Leading/trailing whitespace is trimmed before classification.
		{"whitespace", "  fleet:slack-bridge  ", "slack-bridge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Pass a viewer email that could never legitimately match a sentinel,
			// and a bot display, to prove neither leaks through.
			chip := resolveOperatorChip(c.operatedBy, "gardenio", "matt@example.com", "matt-baker", "agent-foo", "/b", "matt@example.com")
			if chip.Kind != "machine" {
				t.Fatalf("Kind = %q, want machine", chip.Kind)
			}
			if chip.Display != c.wantDisplay {
				t.Errorf("Display = %q, want %q", chip.Display, c.wantDisplay)
			}
			if chip.RawID != strings.TrimSpace(c.operatedBy) {
				t.Errorf("RawID = %q, want trimmed raw id %q", chip.RawID, strings.TrimSpace(c.operatedBy))
			}
			if chip.FilterHref != "" {
				t.Errorf("FilterHref = %q, want empty (no human filter link)", chip.FilterHref)
			}
			if chip.IsViewerSelf {
				t.Error("IsViewerSelf = true, want false for a machine sentinel")
			}
			if chip.Email != "" {
				t.Errorf("Email = %q, want empty", chip.Email)
			}
			if chip.Slug != "" {
				t.Errorf("Slug = %q, want empty (no /u/{slug} link for a sentinel)", chip.Slug)
			}
		})
	}
}

// A human id that merely CONTAINS "fleet:"/"system:" mid-string (not as a
// prefix) is still a human — the guard is prefix-anchored, matching the
// daemon's isHumanOperator which keys on the id shape.
func TestResolveOperatorChip_NonPrefixSentinelStaysHuman(t *testing.T) {
	chip := resolveOperatorChip("oidc:fleet.system@example.com", "Fleet System", "", "", "agent", "/b", "")
	if chip.Kind != "human" {
		t.Errorf("Kind = %q, want human (sentinel words only mid-string)", chip.Kind)
	}
}

func TestMachineOperatorSource(t *testing.T) {
	cases := []struct {
		in         string
		wantSource string
		wantOK     bool
	}{
		{"fleet:slack-bridge", "slack-bridge", true},
		{"system:machine", "machine", true},
		{"fleet:", "fleet:", true},
		{"system:", "system:", true},
		{"  system:audit-log  ", "audit-log", true},
		{"slack:U123", "", false},
		{"oidc:matt@example.com", "", false},
		{"agent-foo", "", false},
		{"", "", false},
		// Only a leading prefix counts.
		{"oidc:system:x@example.com", "", false},
	}
	for _, c := range cases {
		src, ok := machineOperatorSource(c.in)
		if ok != c.wantOK || src != c.wantSource {
			t.Errorf("machineOperatorSource(%q) = (%q,%v), want (%q,%v)", c.in, src, ok, c.wantSource, c.wantOK)
		}
	}
}

// ka-0000003067: when operated_by is empty but a CreatedBy agent identity is
// present, fall back to an "agent" chip so the Operator column always carries
// attribution signal instead of an em-dash.
func TestResolveOperatorChip_AgentFallback(t *testing.T) {
	chip := resolveOperatorChip("", "", "", "", "agent-foo", "/b", "")
	if chip.Kind != "agent" {
		t.Fatalf("Kind = %q, want agent", chip.Kind)
	}
	if chip.Display != "agent-foo" {
		t.Errorf("Display = %q, want agent-foo", chip.Display)
	}
	if chip.RawID != "agent-foo" {
		t.Errorf("RawID = %q, want agent-foo", chip.RawID)
	}
	if !strings.Contains(chip.FilterHref, "operated_by=agent-foo") {
		t.Errorf("FilterHref = %q, missing operated_by filter", chip.FilterHref)
	}
}

// True-empty: no operated_by and no created_by — render the em-dash sentinel.
func TestResolveOperatorChip_Empty(t *testing.T) {
	chip := resolveOperatorChip("", "", "", "", "", "/b", "")
	if chip.Kind != "empty" {
		t.Fatalf("Kind = %q, want empty", chip.Kind)
	}
}

func TestResolveOperatorChip_ViewerSelfOIDC(t *testing.T) {
	chip := resolveOperatorChip("oidc:me@example.com", "Me", "", "", "agent-foo", "/b", "me@example.com")
	if !chip.IsViewerSelf {
		t.Error("IsViewerSelf = false, want true for OIDC viewer match")
	}
}

func TestResolveOperatorChip_ViewerSelfPlainEmail(t *testing.T) {
	chip := resolveOperatorChip("me@example.com", "Me", "", "", "agent-foo", "/b", "me@example.com")
	if !chip.IsViewerSelf {
		t.Error("IsViewerSelf = false, want true for plain-email match")
	}
}

func TestResolveCommentOperatorChip_SelfSuppression(t *testing.T) {
	chip := resolveCommentOperatorChip("alice", "", "", "", "alice", "alice", "/b", "")
	if chip.Kind != "self" {
		t.Errorf("Kind = %q, want self (operator equals author)", chip.Kind)
	}
	if html := renderOperatorChip(chip); string(html) != "" {
		t.Errorf("renderOperatorChip(self) = %q, want empty", html)
	}
}

func TestResolveCommentOperatorChip_EmptyAgentSuppressed(t *testing.T) {
	chip := resolveCommentOperatorChip("", "", "", "", "agent-foo", "agent-foo", "/b", "")
	if chip.Kind != "suppressed" {
		t.Errorf("Kind = %q, want suppressed", chip.Kind)
	}
	if html := renderOperatorChip(chip); string(html) != "" {
		t.Errorf("renderOperatorChip(suppressed) = %q, want empty", html)
	}
}

func TestResolveCommentOperatorChip_SchedulerMidThread(t *testing.T) {
	chip := resolveCommentOperatorChip("", "", "", "", "sched-foo", "sched-foo", "/b", "")
	if chip.Kind != "scheduler" {
		t.Errorf("Kind = %q, want scheduler mid-thread", chip.Kind)
	}
}

func TestResolveCommentOperatorChip_HumanOverridesAgentAuthor(t *testing.T) {
	chip := resolveCommentOperatorChip("oidc:matt@x", "Matt", "", "", "agent-foo", "agent-foo", "/b", "")
	if chip.Kind != "human" {
		t.Errorf("Kind = %q, want human (operator differs from author)", chip.Kind)
	}
	if chip.Display != "Matt" {
		t.Errorf("Display = %q, want Matt", chip.Display)
	}
}

func TestResolveOperatedByParam_Empty(t *testing.T) {
	got, isMe := resolveOperatedByParam("", "viewer@x")
	if got != "" || isMe {
		t.Errorf("got %q,%v, want \"\",false", got, isMe)
	}
}

func TestResolveOperatedByParam_MeWithViewer(t *testing.T) {
	// `me` resolves to the canonical operator id (oidc:<lowercased-email>), not
	// the bare email — the daemon alias filter keys on "kind:subject", and a
	// bare/mixed-case email never matched (ka-0000002505).
	got, isMe := resolveOperatedByParam("me", "viewer@x")
	if got != "oidc:viewer@x" || !isMe {
		t.Errorf("got %q,%v, want oidc:viewer@x,true", got, isMe)
	}
}

// The X-Auth-Request-Email header Keycloak sets is mixed-case; `me` must fold
// it to the lowercase canonical id so it matches the fleet-stamped
// oidc:<lowercased-email> (ka-0000002505, the "my plans shows nothing" bug).
func TestResolveOperatedByParam_MeLowercasesMixedCaseEmail(t *testing.T) {
	got, isMe := resolveOperatedByParam("me", "Matthew.Baker@acme.example")
	if got != "oidc:matthew.baker@acme.example" || !isMe {
		t.Errorf("got %q,%v, want oidc:matthew.baker@acme.example,true", got, isMe)
	}
}

func TestResolveOperatedByParam_MeAnonymous(t *testing.T) {
	got, isMe := resolveOperatedByParam("me", "")
	if got != "" || !isMe {
		t.Errorf("got %q,%v, want \"\",true", got, isMe)
	}
}

func TestResolveOperatedByParam_Null(t *testing.T) {
	got, isMe := resolveOperatedByParam("null", "viewer@x")
	if got != "" || isMe {
		t.Errorf("got %q,%v, want \"\",false (caller handles null)", got, isMe)
	}
}

func TestResolveOperatedByParam_Verbatim(t *testing.T) {
	got, isMe := resolveOperatedByParam("slack:U123", "viewer@x")
	if got != "slack:U123" || isMe {
		t.Errorf("got %q,%v, want slack:U123,false", got, isMe)
	}
}

func TestIsSchedulerAgent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"sched-cycle-watchdog", true},
		{"sched-", true},
		{"  sched-foo  ", true},
		{"agent-foo", false},
		{"", false},
		{"scheduler", false},
	}
	for _, c := range cases {
		if got := isSchedulerAgent(c.in); got != c.want {
			t.Errorf("isSchedulerAgent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// canonicalOIDC folds a viewer email into the canonical
// "oidc:<lowercased-email>" operator id the fleet stamps on writes, leaving
// case-sensitive slack: ids untouched (ka-0000002505).
func TestCanonicalOIDC(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"matthew.baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"Matthew.Baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"  Matthew.Baker@acme.example  ", "oidc:matthew.baker@acme.example"},
		{"oidc:Matthew.Baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"oidc:matthew.baker@acme.example", "oidc:matthew.baker@acme.example"},
		// Slack ids are case-sensitive (^U[A-Z0-9]+$) — preserved verbatim.
		{"slack:U0OPERATOR1", "slack:U0OPERATOR1"},
	}
	for _, c := range cases {
		if got := canonicalOIDC(c.in); got != c.want {
			t.Errorf("canonicalOIDC(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOperatorIDsMatchViewer(t *testing.T) {
	cases := []struct {
		op, viewer string
		want       bool
	}{
		{"matt@x", "matt@x", true},
		{"oidc:matt@x", "matt@x", true},
		{"MATT@X", "matt@x", true},
		{"slack:U123", "matt@x", false},
		{"", "matt@x", false},
		{"matt@x", "", false},
	}
	for _, c := range cases {
		if got := operatorIDsMatchViewer(c.op, c.viewer); got != c.want {
			t.Errorf("operatorIDsMatchViewer(%q,%q) = %v, want %v", c.op, c.viewer, got, c.want)
		}
	}
}

func TestRenderOperatorChip_HumanIncludesRawAndSelf(t *testing.T) {
	chip := operatorChip{
		Kind:         "human",
		Display:      "Matt",
		RawID:        "oidc:matt@x",
		FilterHref:   "/b/search?operated_by=oidc%3Amatt%40x",
		IsViewerSelf: true,
	}
	html := string(renderOperatorChip(chip))
	for _, want := range []string{
		`class="operator-chip"`,
		`data-operated-by="oidc:matt@x"`,
		`<strong>Matt</strong>`,
		`<code class="operator-chip-raw">oidc:matt@x</code>`,
		`(self)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("renderOperatorChip missing %q in:\n%s", want, html)
		}
	}
}

// TestResolveOperatorChip_PropagatesSlug pins that the slug parameter
// surfaces on the resulting chip and feeds /u/{slug} rendering. M2 §6
// (ka-0000007787).
func TestResolveOperatorChip_PropagatesSlug(t *testing.T) {
	chip := resolveOperatorChip("oidc:matt@x", "Matt", "", "matt-baker", "agent", "/b", "")
	if chip.Slug != "matt-baker" {
		t.Errorf("Slug = %q, want matt-baker", chip.Slug)
	}
}

// TestRenderOperatorChip_PrefersSlugHref pins that when a slug is set,
// the chip's display label is wrapped in <a href="/u/{slug}"> instead of
// the legacy /search?operated_by=… filter href. M2 §6 (ka-0000007787).
func TestRenderOperatorChip_PrefersSlugHref(t *testing.T) {
	chip := operatorChip{
		Kind:       "human",
		Display:    "Matt",
		RawID:      "oidc:matt@x",
		FilterHref: "/b/search?operated_by=oidc%3Amatt%40x",
		Slug:       "matt-baker",
	}
	html := string(renderOperatorChip(chip))
	if !strings.Contains(html, `href="/u/matt-baker"`) {
		t.Errorf("expected /u/{slug} href, got:\n%s", html)
	}
	if strings.Contains(html, `href="/b/search?operated_by=`) {
		t.Errorf("legacy filter href should be suppressed when slug is set:\n%s", html)
	}
	if !strings.Contains(html, `data-slug="matt-baker"`) {
		t.Errorf("expected data-slug attr, got:\n%s", html)
	}
}

// TestRenderOperatorChipCompact_PrefersSlugHref pins that the compact
// (initials-only) chip also uses /u/{slug} when present. M2 §6.
func TestRenderOperatorChipCompact_PrefersSlugHref(t *testing.T) {
	chip := operatorChip{
		Kind:       "human",
		Display:    "Matt Baker",
		RawID:      "oidc:matt@x",
		FilterHref: "/b/search?operated_by=oidc%3Amatt%40x",
		Slug:       "matt-baker",
	}
	html := string(renderOperatorChipCompact(chip))
	if !strings.Contains(html, `href="/u/matt-baker"`) {
		t.Errorf("expected /u/{slug} href in compact chip, got:\n%s", html)
	}
	if strings.Contains(html, `href="/b/search?operated_by=`) {
		t.Errorf("legacy filter href should be suppressed in compact chip when slug is set:\n%s", html)
	}
	if !strings.Contains(html, `is-slug-link`) {
		t.Errorf("expected is-slug-link class, got:\n%s", html)
	}
}

func TestRenderOperatorChip_Scheduler(t *testing.T) {
	html := string(renderOperatorChip(operatorChip{Kind: "scheduler"}))
	if !strings.Contains(html, "(scheduler)") {
		t.Errorf("scheduler chip missing literal: %s", html)
	}
}

func TestRenderOperatorChip_Empty(t *testing.T) {
	html := string(renderOperatorChip(operatorChip{Kind: "empty"}))
	if !strings.Contains(html, "&mdash;") {
		t.Errorf("empty chip missing em-dash: %s", html)
	}
}

func TestRenderOperatorChip_Agent(t *testing.T) {
	chip := operatorChip{
		Kind:       "agent",
		Display:    "agent-foo",
		RawID:      "agent-foo",
		FilterHref: "/b/search?operated_by=agent-foo",
	}
	html := string(renderOperatorChip(chip))
	for _, want := range []string{
		`class="operator-chip operator-chip-agent"`,
		`data-operated-by="agent-foo"`,
		`href="/b/search?operated_by=agent-foo"`,
		`agent-foo</a>`,
		`(agent)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("renderOperatorChip(agent) missing %q in:\n%s", want, html)
		}
	}
}

func TestRenderOperatorChip_Machine(t *testing.T) {
	chip := operatorChip{
		Kind:    "machine",
		Display: "slack-bridge",
		RawID:   "fleet:slack-bridge",
	}
	html := string(renderOperatorChip(chip))
	for _, want := range []string{
		`class="operator-chip operator-chip-machine"`,
		`data-operated-by="fleet:slack-bridge"`,
		`(automated: slack-bridge)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("renderOperatorChip(machine) missing %q in:\n%s", want, html)
		}
	}
	// A machine chip must never leak the human affordances.
	for _, forbidden := range []string{"<strong>", "operator-chip-link", "(self)", "/u/"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("renderOperatorChip(machine) must not contain %q in:\n%s", forbidden, html)
		}
	}
}

// The machine chip HTML-escapes both the source and the raw id so a crafted
// sentinel value can never inject markup.
func TestRenderOperatorChip_MachineEscapes(t *testing.T) {
	chip := operatorChip{
		Kind:    "machine",
		Display: `x"><b>`,
		RawID:   `fleet:x"><b>`,
	}
	html := string(renderOperatorChip(chip))
	if strings.Contains(html, "<b>") {
		t.Errorf("machine chip did not escape markup:\n%s", html)
	}
}

// The compact (initials-only) chip renders a distinct is-machine badge for a
// fleet:/system: sentinel — not a fake-human monogram, not a blank cell.
func TestRenderOperatorChipCompact_Machine(t *testing.T) {
	chip := operatorChip{
		Kind:    "machine",
		Display: "slack-bridge",
		RawID:   "fleet:slack-bridge",
	}
	html := string(renderOperatorChipCompact(chip))
	if html == "" {
		t.Fatal("compact machine chip rendered empty; want a badge")
	}
	for _, want := range []string{
		`operator-chip-compact`,
		`is-machine`,
		`data-operated-by="fleet:slack-bridge"`,
		`>SB<`, // monogram of the source suffix
	} {
		if !strings.Contains(html, want) {
			t.Errorf("compact machine chip missing %q in:\n%s", want, html)
		}
	}
	// No link and no viewer-self skin for a sentinel.
	for _, forbidden := range []string{"is-viewer-self", "href="} {
		if strings.Contains(html, forbidden) {
			t.Errorf("compact machine chip must not contain %q in:\n%s", forbidden, html)
		}
	}
}

// The bead-detail "Operated by" banner suppresses the machine sentinel — only
// resolved humans get the royal-purple banner (bead_show.go returns "" for any
// non-human Kind, which now includes machine).
func TestOperatedByBanner_MachineSuppressed(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		OperatedBy: "fleet:slack-bridge",
		CreatedBy:  "agent-foo",
	}
	if html := string(operatedByBanner(bead, "/b", "")); html != "" {
		t.Errorf("operatedByBanner(machine) = %q, want empty", html)
	}
}

// resolveCommentOperatorChip routes a non-empty sentinel operated_by through
// resolveOperatorChip, so a mid-thread machine comment renders the muted
// automated chip rather than a fake human (ka-0000003465).
func TestResolveCommentOperatorChip_MachineSentinel(t *testing.T) {
	chip := resolveCommentOperatorChip("fleet:slack-bridge", "gardenio", "", "", "some-agent", "some-agent", "/b", "")
	if chip.Kind != "machine" {
		t.Fatalf("Kind = %q, want machine", chip.Kind)
	}
	if chip.Display != "slack-bridge" {
		t.Errorf("Display = %q, want slack-bridge", chip.Display)
	}
	html := string(renderOperatorChip(chip))
	if !strings.Contains(html, "(automated: slack-bridge)") {
		t.Errorf("comment machine chip missing automated label:\n%s", html)
	}
}

// TestOperatorHeaderChipHelper_RendersAllFiveStates is the render-shape test
// proving the template helper emits a non-empty chip span with the expected
// data-state for each of the five chip states the spec defines (R2 §4.2).
func TestOperatorHeaderChipHelper_RendersAllFiveStates(t *testing.T) {
	cases := []struct {
		name        string
		urlHasKey   bool
		resolved    string
		isUnowned   bool
		display     string
		viewerEmail string
		wantState   string
		wantText    string
	}{
		{"mine_default", false, "oidc:me@x", false, "", "me@x", "mine", "Showing your work"},
		{"all", true, "", false, "", "me@x", "all", "Showing all work"},
		{"unowned", true, "", true, "", "me@x", "unowned", "Showing unowned work"},
		{"other", true, "alice@x", false, "Alice", "me@x", "other", "Alice"},
		{"anonymous", false, "", false, "", "", "anonymous", "sign in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html := string(operatorHeaderChipHelper(c.urlHasKey, c.resolved, c.isUnowned, c.display, c.viewerEmail, "/active", ""))
			if !strings.Contains(html, `class="operator-header-chip"`) {
				t.Errorf("missing chip span in: %s", html)
			}
			if !strings.Contains(html, `data-state="`+c.wantState+`"`) {
				t.Errorf("missing data-state=%q in: %s", c.wantState, html)
			}
			if !strings.Contains(html, c.wantText) {
				t.Errorf("missing %q in: %s", c.wantText, html)
			}
		})
	}
}

// TestRenderPlanHeaderChip_StatesAndCopy pins the /plans-scoped header chip
// (Step 2, ka-0000005414): it reuses the shared five-state view-model but speaks
// plan-count-aware, discovery-not-reset copy. Each case asserts the state span,
// the plan-count-aware headline where meaningful, and the discovery framing of
// the widen-out link.
func TestRenderPlanHeaderChip_StatesAndCopy(t *testing.T) {
	cases := []struct {
		name        string
		urlHasKey   bool
		resolved    string
		isUnowned   bool
		display     string
		viewerEmail string
		count       int
		wantState   string
		wantSubs    []string // substrings that MUST appear
		notSubs     []string // substrings that must NOT appear
	}{
		{
			name: "mine_default_pluralized", urlHasKey: false, resolved: "oidc:me@x",
			viewerEmail: "me@x", count: 3, wantState: "mine",
			wantSubs: []string{"Showing your 3 plans", "(auto)", "everyone&#39;s plans"},
			// The default scope must read as discovery, never a bare "all" reset.
			notSubs: []string{">all<", "(default)"},
		},
		{
			name: "mine_explicit_no_auto_pill_singular", urlHasKey: true, resolved: "oidc:me@x",
			viewerEmail: "me@x", count: 1, wantState: "mine",
			wantSubs: []string{"Showing your 1 plan", "everyone&#39;s plans"},
			// ?operated_by=me is explicit-mine — no cold-load (auto) pill, and the
			// singular noun must not render "1 plans".
			notSubs: []string{"(auto)", "1 plans"},
		},
		{
			name: "all_reads_as_everyones", urlHasKey: true, resolved: "",
			viewerEmail: "me@x", count: 12, wantState: "all",
			// The headline apostrophe is written raw (valid HTML text, mirroring
			// renderOperatorHeaderChip); only link labels route through the
			// escaping link() helper.
			wantSubs: []string{"Showing everyone's plans", "just your plans"},
			// A fleet-wide scope shows no plan count (it would read as a firehose
			// size, not a scope).
			notSubs: []string{"12 plans"},
		},
		{
			name: "other_person_possessive_count", urlHasKey: true, resolved: "alice@x",
			display: "Alice", viewerEmail: "me@x", count: 2, wantState: "other",
			wantSubs: []string{"<strong>Alice</strong>", "2 plans", "everyone&#39;s plans"},
		},
		{
			name: "anonymous_offers_signin", urlHasKey: false, resolved: "",
			viewerEmail: "", count: 5, wantState: "anonymous",
			wantSubs: []string{"Showing everyone's plans", "sign in", "just your plans"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html := string(renderPlanHeaderChip(c.urlHasKey, c.resolved, c.isUnowned, c.display, c.viewerEmail, "/plans", "", c.count))
			if !strings.Contains(html, `data-state="`+c.wantState+`"`) {
				t.Fatalf("missing data-state=%q in:\n%s", c.wantState, html)
			}
			for _, want := range c.wantSubs {
				if !strings.Contains(html, want) {
					t.Errorf("missing %q in:\n%s", want, html)
				}
			}
			for _, forbidden := range c.notSubs {
				if strings.Contains(html, forbidden) {
					t.Errorf("unexpected %q in:\n%s", forbidden, html)
				}
			}
		})
	}
}

// TestRenderPlanHeaderChip_EmptyWhenNoBasePath guards that the chip degrades to
// nothing (not a bare span) when it can't build links — same contract as the
// shared renderer, so the template can embed it unconditionally.
func TestRenderPlanHeaderChip_EmptyWhenNoBasePath(t *testing.T) {
	if html := string(renderPlanHeaderChip(false, "oidc:me@x", false, "", "me@x", "", "", 3)); html != "" {
		t.Errorf("renderPlanHeaderChip with empty basePath = %q, want empty", html)
	}
}

// TestRenderPlanHeaderChip_PreservesSiblingFilters pins that the widen-out
// ("everyone's plans") link carries the encoded sibling-filter fragment so
// switching the user scope never silently drops the active status/project/group
// filters — the whole "switching one filter never resets the rest" contract the
// plans control strip depends on.
func TestRenderPlanHeaderChip_PreservesSiblingFilters(t *testing.T) {
	html := string(renderPlanHeaderChip(false, "oidc:me@x", false, "", "me@x", "/plans", "status=closed&project=gardenio", 4))
	if !strings.Contains(html, "status=closed&amp;project=gardenio&amp;operated_by=") {
		t.Errorf("widen-out link dropped sibling filters in:\n%s", html)
	}
}

// TestOperatorChipEmailHasDirectWidthClamp guards ka-0000008025: the
// .operator-chip-email anchor must carry its own width constraint, not rely
// solely on the .operator-chip overflow:hidden ellipsis clamp. As a flex
// item the anchor defaults to min-width:auto and won't shrink below its
// intrinsic content width (~216px), so at a 375px mobile viewport it extends
// past the edge and is only saved by the parent's overflow clamp. If a
// future change removes/overrides that parent clamp, mobile re-breaks. This
// test fails loud if the direct clamp is dropped.
func TestOperatorChipEmailHasDirectWidthClamp(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	css := string(raw)

	// Locate the .operator-chip-email rule and assert it carries the
	// shrink-enabling + clamping declarations. min-width:0 is the load-bearing
	// one — without it the flex item ignores max-width:100% and overflows.
	const sel = ".operator-chip-email {"
	idx := strings.Index(css, sel)
	if idx < 0 {
		t.Fatalf("no %q rule in layout.html — the email anchor would rely solely on the parent .operator-chip clamp (ka-0000008025)", sel)
	}
	end := strings.Index(css[idx:], "}")
	if end < 0 {
		t.Fatalf("malformed %q rule (no closing brace)", sel)
	}
	rule := css[idx : idx+end]
	for _, decl := range []string{"min-width: 0", "max-width: 100%", "overflow: hidden", "text-overflow: ellipsis"} {
		if !strings.Contains(rule, decl) {
			t.Errorf("operator-chip-email rule missing %q (ka-0000008025); rule = %q", decl, rule)
		}
	}
}
