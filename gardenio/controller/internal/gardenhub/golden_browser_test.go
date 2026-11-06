package gardenhub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview/decisiontest"
	"gardenio/controller/internal/golden"
)

// goldenNow is the instant every golden is rendered at: the farm's clock on the server and Date.now() in the browser, so
// "5m ago" reads the same on every run.
var goldenNow = time.Unix(baseUnix+10_000, 0)

// The arms cardWorld's plan P wears (heraldry step 4.4): the plan's, a picture at its plot gate; its first phase's and that
// phase's step's, which the World says in words only (no stake picture, Q16; tasks bear none, R6, and the farm passes a
// task's on without drawing it).
const (
	goldenPlanArms  = "🧭🔥📦"
	goldenPhaseArms = "sand . . 🧭 . ."
	goldenTaskArms  = "red . ⚙️ . 🔑 ."
)

// goldenSurfaces lists every surface testdata/golden_browser.js shoots, keyed by the file name's first word. A shot with
// no row here fails, so a new surface cannot land without choosing its tolerance (gardenio/tests/gardenhub-browser/GOLDENS.md).
var goldenSurfaces = map[string]golden.Tol{
	"opening": golden.Default, // the whole farm at the zoom-to-fit, the opening cursor's full label
	"menu":    golden.Default, // the camera's Menu open (3b R8): the bar and its list, framed together
	// a Become session (plan gd-0SUIdKXWV3 step 4): the banner above the opening farm
	"impersonated": golden.Default,
	// the cursor's signs at each depth (cursor-grid spec 6): the full label, child labels and glyphs on gardenio, plan A, a
	// phase bed of plan A and a step in it
	"cursorworld":   golden.Default,
	"world":         golden.Default, // the World level: the gardeners at work on gardenio as figures on its rim, and its "!1" (gd-GgKjBgsc0U)
	"cursorproject": golden.Default,
	"cursorplan":    golden.Default,
	"cursorphase":   golden.Default,
	"cursorharvest": golden.Default, // a harvest cell's full label: its lead words and its icon row (harvest spec 5)
	"porch":         golden.Default, // the Porch dock open on its agent list
	// the header purse (gd-H9LerJ0v3g): an admin's Fleet row ok, low, over, with no account budget and with the rollup down; a
	// scoped viewer's row per project
	"purseok":       golden.Default,
	"purselow":      golden.Default,
	"purseover":     golden.Default,
	"pursenobudget": golden.Default,
	"pursestale":    golden.Default,
	"pursescoped":   golden.Default,
	"search":        golden.Default, // the search panel on "edge": its field and three hits, closest in meaning first (Lands 5.7)
	"project":       golden.Default, // the project panel on gardenio's Decisions tab: its tabs, the line under the title, the asks with their answer box (Lands 5.11)
	"projectcost":   golden.Default, // the project panel's Cost tab on acme, a tenant land at the wall of its purse (Lands 5.11)
	"gardener":      golden.Default, // the gardener panel on guard the plan's Runs tab: its state, levers and runs (Lands 5.12)
	"settings":      golden.Default, // the settings panel: the viewer's spawn preferences and who they are (Lands 5.12)
	"costs":         golden.Default, // the Costs panel on the fleet: figures, range chips, cards with their frames loaded (plan gd-gh5J0vVbJP 2.1, 2.3)
	// the agent panels (plan gd-gh5J0vVbJP 4.2): a gardener loop's Charts tab, and one gardener's charts in the Costs panel
	"gardenercharts": golden.Default,
	"agentcharts":    golden.Default,
	"costsland":      golden.Default, // a tenant land's credits in the Costs panel: tiles, meter, burn to the wall, per agent (gd-gh5J0vVbJP 3.3)
	"land":           golden.Default, // where a tenant lands, /garden?land=: the camera on the land, its #general, the land header's purse (Lands 8.8)
	"landwall":       golden.Default, // the land header at the wall, the plaque only (Lands 8.8)
	"landcap":        golden.Default, // the land header with the steward resting at its run cap, the plaque only (gd-OvKMtyWtrp)
	"glass":          golden.Default, // the Glass, watch: a recorded session live in xterm, scaled to the panel (Lands 6.4)
	"glassoff":       golden.Default, // the Glass when its stream is unavailable: the cached screen and why (Lands 6.4)
	"transcript":     golden.Default, // one gardener's transcript: its calls newest first, a failure and an unfinished one (spec 11.4)
	"transcriptplot": golden.Default, // a plot's transcript: two gardeners' calls merged newest first, each run headed by its name (spec 11.5)
	"creds":          golden.Default, // the credential pool: each account's health, windows and line, read only (Lands 6.9)
	"glassask":       golden.Default, // the Glass of an agent at a permission prompt: the prompt, its options by label, the nudge box (Lands 6.5)
	"glassattach":    golden.Default, // the Glass attached from a click on the terminal: "attached as you", Ctrl-C typed asking first (Lands 6.6)
	"wall":           golden.Default, // the Wall: the land's live sessions as text-grid tiles in attention order (Lands 6.7)
	"wallcard":       golden.Default, // a gardener figure's card: its state chip and its screen's last six lines, live (Lands 6.7)
	"wallcardtap":    golden.Default, // a finger's tap on a gardener drawn with no link: its card, pinned, with Watch (GL-12)
	"bundle":         golden.Default, // the bundle panel: a bundle's documents read as one, each by its format; the contents rail on a desktop (Lands 5.10)
	"picpreview":     golden.Default, // a grade picture's hover preview beside it, over the bead view (Lands 5.15; desktop only, a phone has no hover)
	"picviewer":      golden.Default, // the picture viewer on picture 2 of 4: the picture, Previous, the counter, Open whole, Next, the filmstrip (Lands 5.15)
	"cockpit":        golden.Default, // the plan cockpit on plan A: where it stands, what it waits on, its first phase's steps (Lands 5.9)
	"cockpitstale":   golden.Default, // the same at its crew header, plan A's spend stale (gd-rtE07tg6Nh)
	"bead":           golden.Default, // the in-garden bead view of plan A, docked on a desktop, full width on a phone
	"beadconv":       golden.Default, // the fixture Slack wish's Conversation section, its reply composer at the foot (gd-tx2wMH48MV)
	"beadwish":       golden.Default, // the fixture Slack wish's panel as it opens: its newest line on a phone screen (gd-1jjjx7CMU3)
	"beadplan":       golden.Default,
	"beadrels":       golden.Default, // step gd-A2a as a writer at its relations: waits on, holds up, each row's ghost Remove (gd-r5dlqvrIau) // plan P's bead view at its card's sections: gardeners, operator, nine phases (build step 6)
	"plan":           golden.Default, // plan P's nine phases at the Plan level, the cursor's labelled, clipped around its plot
	"planout":        golden.Default, // the same at the bottom of the Plan level
	"armsgate":       golden.Default, // plan P's arms at its plot gate at device pixel ratio 2, its first stake and step bare (heraldry 4.4)

	// gardenio's Cost tab at its Charts: the range chips, the cards titled under the section, frames loaded (step 2.3b)
	"projectcostcharts": golden.Default,

	// Workbench panel tabs promoted from the commons-ux-acceptance skill's EXTRA rows once graded accept (gd-wSb1jRJnJm):
	// acme's Gardeners tab, whose list the daemon did not answer, says so with Try again (WB-15)
	"projectpartial":   golden.Default,
	"costsproject":     golden.Default, // the Costs panel on gardenio: its figures, range chips, cards with their frames loaded
	"gardenerabout":    golden.Default, // guard the plan's About tab: goal, wakes and stops, prompt, owner, spend
	"searchempty":      golden.Default, // a search that finds nothing: the field and what to try instead
	"settingsedit":     golden.Default, // the settings editor open: spawn defaults and identity as one form
	"queueinbox":       golden.Default, // the Queues panel's Inbox: a pinned row, why each row is there, its state in words
	"queueescalations": golden.Default, // the Queues panel's Escalations: whom each waits on
	"projectagents":    golden.Default, // gardenio's Agents tab
	"projectgardeners": golden.Default, // gardenio's Gardeners tab: its loops, one off, and New gardener
	"projectaccounts":  golden.Default, // gardenio's Accounts tab: the account and its spend
	"projectactivity":  golden.Default, // gardenio's Activity tab: what grows, what moved, what is blocked
	"mailbox":          golden.Default, // the dock's Mailbox tab with its open decision
	"cmdk":             golden.Default, // Cmd-K over the map: the jump holding "edge" and its suggestions

	// gardeners at their plots (gd-e4uXZs8coZ, homestead-agents-as-people.md section 8): plots A and B at the Plan level; plot A
	// at the Bead level; the Growing field's stands and the Barn's label; the legend's gardener rows
	"gardeners":     golden.Default,
	"gardenersbead": golden.Default,
	"gardenerarms":  golden.Default, // two gardeners on plan P at the Bead level, each with its line's arms as a badge over its head (heraldry 5.9)
	"porcharms":     golden.Default, // the Porch's rows for them: the run before "Tending:"
	"cellskeep":     golden.Default, // the gardenersbead framing after a second document files a step and drops one: every bead kept its cell (gd-xhpccozIcC)
	"barn":          golden.Default,
	"barnthread":    golden.Default, // thread agents at the barn (5b): the pink shirt, the doing pose, a bird over the Commons's barn, "In the Commons: ..."
	// tool calls as birds (gd-pLFocqmYyf): over plots A and B at the Plan level 0.7 s into their flights, a flock and a hawk; and
	// the same calls under reduced motion, one still bird over each figure's head; and those still birds at the Bead level, their
	// words beside them over the figures, off every face and "?" bubble and the cursor's ring (4n, gd-VHkZ3pCEqK)
	"birds":          golden.Default,
	"birdsstill":     golden.Default,
	"birdsbeadstill": golden.Default,
	// music's steward by its land's sign at the Bead level, its tip open: its memory's In flight line (Lands 9.8)
	"steward":    golden.Default,
	"pot":        golden.Default, // a staked plot's tip: its plan decision's card (9.11b), then its steps (26.14)
	"potbead":    golden.Default, // the staked plan's bead view: its Staked out section and the same card first, then its Steps
	"staked":     golden.Default, // the ready releases plan staked out with ten steps: its tip, Open the plan, the step count, the list
	"stakedbead": golden.Default, // its bead view: the staked out pill and card, Steps (10) with their bodies, the description
	"graveyard":  golden.Default, // the cursor on a done wish's headstone, its tip: outcome and reason, what its kin merged, its thread (3.2)
	// a goat's tip, its kin's line arms before the wish's title (heraldry 5.10): grazing by plan A, its gate in the pen, a pen goat with none
	"goatarms": golden.Default,
	"goatgate": golden.Default,
	"goatpen":  golden.Default,
	// the release truck at gardenio's loading bay (gd-yOOVahJMCh): loading, live under reduced motion, its tip
	"truckload": golden.Default,
	"truckgone": golden.Default,
	"truck":     golden.Default,
	// its release panel, opened by Enter (gd-rSz81Me1Pe), and a plot whose newest merged PR shipped, its stamp and sign line
	"release":     golden.Default,
	"shippedplot": golden.Default,
	"jumpwish":    golden.Default, // the phone's Go, "wish": the pen, gardenio's Graveyard, the goat and the stones in the jump list (3.3)
	"pen":         golden.Default, // the pen's row tapped: the cursor on gardenio's Pen zone in its farmstead, framed at 390 px (3.3, 28.2)
	// the Graveyard's row tapped: the cursor on gardenio's Graveyard band, its project framed on the whole phone screen (3.3)
	"graveyardjump": golden.Default,
	// a project's home from the phone's Go (28.6, 29.6, gd-cTKXpm8JH7): the empty list led by the cursor's project's home, then
	// that home's In, its zones on the phone's width, in gardenio and in the Commons
	"gohome":     golden.Default,
	"homein":     golden.Default,
	"commonsin":  golden.Default,
	"homeletter": golden.Default,
	"homegoat":   golden.Default,
	// the field stands from a phone (28.12, gd-a628FpJRO5): Go's empty list with the cursor's project's stands under its home;
	// coop's field crowded past its "+N"; a field figure's Glass as the phone's sheet, then its Go to its plot at full zoom
	"gostands":   golden.Default,
	"fieldcrowd": golden.Default,
	"fieldglass": golden.Default,
	"fieldplot":  golden.Default,
	"legend":     golden.Default,
	// the Plans panel sorted by spend (gd-BL2JQbsfyy): two moving figures and a settled one, then the dashes; and as the Forest
	"plans":       golden.Default,
	"plansforest": golden.Default,
	// the legend's doing poses (4c, gd-kxCviRUVA2), scrolled to the first one
	"legenddo": golden.Default,
	// the gardeners' transitions (gd-elQWxzYhdm, homestead-agents-as-people.md section 8), one keyframe each from a live farm
	// patch: the truck halfway to plot C; a burst's particles; the flight back with the scarecrow up; walking home with the
	// scroll; the tldr over the barn on arrival; the same patches under reduced motion (the end state only)
	"ride":     golden.Default,
	"burst":    golden.Default,
	"flyback":  golden.Default,
	"walkhome": golden.Default,
	"arrive":   golden.Default,
	"reduced":  golden.Default,
	"beacons":  golden.Default, // the edge arrows to what needs you off-screen, at the Bead level on plan P
	// the Commons v2 workbench (plan gd-lRaCxsiLvC step 4, desktop): an epic, a thread and a plan opened as roots
	"commons":       golden.Default,
	"commonsthread": golden.Default,
	"commonsplan":   golden.Default,
	"commonsjobs":   golden.Default, // the phone's node screen on its Jobs tab (Commons v2 step 7, gd-DhGFLKK0tj)
	// the steward as a workbench root (steward view S2, gd-LisW87zrnf, desktop): head, notes, plans, conversation, rails
	"commonssteward": golden.Default,
	// a wish opened as the Commons node (gd-0GnBi7SRfK)
	"commonswish": golden.Default,
	// and its document pushed on a phone, its conversation opened on its newest line (gd-1jjjx7CMU3)
	"commonswishdoc": golden.Default,

	// the typed faces (WB-1, gd-vFUZG6FnTT): each face fixture's bead view down to the end of its face (bead_face_test.go),
	// a decision and an escalation open and answered, and the generic fields table of a type with no face
	"facework":     golden.Default,
	"faceepic":     golden.Default,
	"faceplan":     golden.Default,
	"facedecision": golden.Default,
	// the acceptance decision's bead view: context, its evidence's screenshots, five options and Other (gd-DiU8SB6TAM)
	"facedecisiongate":   golden.Default,
	"facedecisiongateev": golden.Default, // scrolled to its screenshots and options
	// the Queues panel (WB-6, gd-HuJdsFabSb): the Decisions tab, the viewer's own, an answer box on each row
	"queues":               golden.Default,
	"facedecisionanswered": golden.Default,
	"faceescalation":       golden.Default,
	"faceescalationdone":   golden.Default,
	"faceagent":            golden.Default,
	"faceagentline":        golden.Default, // an agent's task's line arms (gd-ASLjQPkHJz)
	"facemessage":          golden.Default,
	"facebundle":           golden.Default,
	"facereport":           golden.Default,
	"facegardener":         golden.Default,
	"facemr":               golden.Default,
	"facerelease":          golden.Default, // the release face (gd-j3bQqfZSt7)
	"facegeneric":          golden.Default,
	// Change arms open on the epic as a writer: the row of three with their Removes, the register's look-alike, the library
	// (heraldry 3.2, 3.4, 5.5b)
	"facearms": golden.Default,

	// the splash (plan gd-EJGA3zDGCk steps 1 and 10) at the mid tier at noon, dusk, the full moon's midnight and dawn: the WebGL
	// scene drawn by SwiftShader, software GL, so looser than the rest. Runs on one agent pod match exactly (0 px); another
	// CPU's SwiftShader can round a shade or move an anti-aliased edge, so a channel may move 48 and 1200 pixels may differ,
	// about the horizon's row on a desktop. The smallest wrong scene seen, the box's side face unlit, moved 5304 px on a phone.
	// The low-sun and night frames are darker, so a rounding moves fewer pixels past 48; they take the noon frame's tolerance.
	"splash":      {Channel: 48, Pixels: 1200},
	"splashdusk":  {Channel: 48, Pixels: 1200},
	"splashnight": {Channel: 48, Pixels: 1200},
	"splashdawn":  {Channel: 48, Pixels: 1200},
	// noon zoomed 4x and turned (plan gd-I5QHVfGLbD): the same scene through a narrower field of view, so noon's tolerance
	"splashzoom": {Channel: 48, Pixels: 1200},
	// the same page where the browser gives no WebGL: the still covering the viewport and its one-line note
	"splashflat": golden.Default,
}

// The visual-regression gate for /garden (gd-D953GccBTY). Chromium under playwright-core shoots each surface of
// goldenSurfaces at a 390 px phone and a 1280 px desktop, light and dark, against cardWorld with a frozen clock, and each
// PNG is compared with testdata/golden/<same name> by golden.Check. A shot over its tolerance, a shot with no golden and a
// golden with no shot all fail; the actual, expected and diff PNGs land in golden.ArtifactsDir("garden"), which CI
// uploads. GARDENHUB_GOLDEN_UPDATE=1 rewrites the goldens that fail and removes stale ones instead:
// `make update-garden-goldens`.
//
// Runs only when GARDENHUB_GOLDENS=1 (both make targets set it) and only on Linux, where the goldens were rendered: the
// ordinary package run, and a laptop with the card walk's browser installed, skip it. GARDENHUB_BROWSER_REQUIRED=1 turns
// every skip into a failure, as for TestCardUnderChromium.
//
// The sweep runs in goldenShards shards at once (gd-EwwEfGfe6H), each against its own cardWorld, so it takes about a
// shard's share of the wall time. GARDENHUB_GOLDEN_SHARDS=1 is the old one-row-at-a-time sweep.
func TestGardenGoldens(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, mods, why := golden.Browser()
	if why != "" {
		skip("%s", why)
	}
	goldens, _ := filepath.Abs(filepath.Join("testdata", "golden"))
	script, _ := filepath.Abs(filepath.Join("testdata", "golden_browser.js"))

	bases := make([]string, goldenShards(t))
	for i := range bases {
		bases[i] = cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...).URL
	}
	shots := t.TempDir()
	cmd := exec.Command(node, script, strings.Join(bases, ","), mods, shots, filepath.Join(goldens, "fonts"), strconv.FormatInt(goldenNow.UnixMilli(), 10))
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("golden capture failed: %v\n%s", err, out)
	}

	// A golden is CI's render: say loudly when this machine's browser or fonts would draw it otherwise (gd-VJWqCUihlQ).
	update := os.Getenv("GARDENHUB_GOLDEN_UPDATE") == "1"
	capt := golden.ParseCapture(out)
	t.Logf("browser: Chromium %s, playwright-core %s", capt.Chromium, capt.Playwright)
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "golden time ") {
			t.Log(l) // the sweep's slowest surfaces, for sizing the Makefile's -timeout (gd-JWgIQDt4Yr)
		} else if strings.HasPrefix(l, "golden retry ") {
			t.Log(l) // a shot whose wait timed out once on a loaded runner and was shot again (gd-iXUhuhF1wO)
		}
	}
	pinned, err := golden.PinnedPlaywright(golden.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	if skew := golden.BrowserSkew(capt, pinned); skew != "" && update {
		t.Fatalf("not updating the goldens: %s. Install the pinned one (make update-garden-goldens), or take CI's renders (gardenio/scripts/garden-goldens-from-ci.sh)", skew)
	} else if skew != "" {
		t.Logf("WARNING: %s; a shot that differs below may differ for that reason alone", skew)
	}
	faces, err := golden.LoadPinned(filepath.Join(goldens, "fonts"))
	if err != nil {
		t.Fatal(err)
	}
	unpinned := faces.Unpinned(capt)
	for _, shot := range slices.Sorted(maps.Keys(unpinned)) {
		t.Logf("system font: %s %s; it may render differently on another machine", shot, unpinned[shot])
	}

	arts := golden.ArtifactsDir("garden")
	r, err := golden.Check(golden.Options{Shots: shots, Goldens: goldens, Artifacts: arts, Update: update, Surfaces: goldenSurfaces,
		Only: golden.OnlySet(os.Getenv("GARDENHUB_GOLDEN_ONLY")), Cells: golden.OnlySet(os.Getenv("GARDENHUB_GOLDEN_CELLS")), Unpinned: unpinned})
	for _, l := range r.Logs {
		t.Log(l)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c3Logs, c3Failed := checkC3(out, goldenC3Known)
	for _, l := range c3Logs {
		t.Log(l)
	}
	if len(c3Failed) > 0 {
		t.Errorf("%d phone shot(s) fail the Commons UX rubric's C3 probes (a control under 44x44 CSS px under touch, a box that scrolls sideways and is not built to, or a field under 16 px that iOS zooms into; probes.js says which). Fix the surface, or, for a bug filed and not fixed yet, give its <surface>-<device> a goldenC3Known row naming the bead.\n  %s",
			len(c3Failed), strings.Join(c3Failed, "\n  "))
	}
	if len(r.Failed) > 0 {
		t.Fatalf("%d golden screenshot(s) differ; actual/expected/diff PNGs are in %s. An intentional change? Commit no golden: the window bless PR (bless-goldens.yml) re-shoots them on main.\n  %s",
			len(r.Failed), arts, strings.Join(r.Failed, "\n  "))
	}
	t.Logf("%d shots match their goldens", r.Shots)
}

// c3Known is how many C3 probe hits a phone cell's shots may have while a filed bug stands (M4.1, gd-T3JPWCD9xM): controls
// under 44x44 under touch (Small), boxes scrolling sideways that are not built to (Sideways) and fields whose text is under
// 16 px, which iOS zooms into on focus (Zoom; M1.3 gd-xWWqgcmzXM), in both themes.
type c3Known struct {
	Bead                  string
	Small, Sideways, Zoom int
}

// goldenC3Known holds the C3 hits that are known bugs, keyed by <surface>-<device>. A shot with more hits than its row fails
// TestGardenGoldens, so a new one cannot land; a shot with fewer says to lower or delete its row. Every row names the bead
// whose fix deletes it.
var goldenC3Known = map[string]c3Known{}

// c3Hits is one shot's "golden c3" line from golden_browser.js: probes.js's smallTarget, sideways and zoomField hits, or why
// the probes could not run.
type c3Hits struct {
	Error                            string
	SmallTarget, Sideways, ZoomField struct {
		N     int
		Items []struct{ Sel, Text, Value string }
	}
}

// checkC3 holds each "golden c3 <shot> <json>" line of a capture against known: a shot whose probes did not run, or that has
// more small targets, sideways boxes or zoom fields than its <surface>-<device> row allows, is one line of failed, naming the hits.
func checkC3(out []byte, known map[string]c3Known) (logs, failed []string) {
	for _, l := range bytes.Split(out, []byte("\n")) {
		rest, ok := bytes.CutPrefix(l, []byte("golden c3 "))
		if !ok {
			continue
		}
		shot, js, _ := strings.Cut(string(rest), " ")
		var h c3Hits
		if err := json.Unmarshal([]byte(js), &h); err != nil {
			failed = append(failed, fmt.Sprintf("%s: unreadable probe line: %v", shot, err))
			continue
		}
		if h.Error != "" {
			failed = append(failed, fmt.Sprintf("%s: the probes did not run: %s (golden_roots_browser.js)", shot, h.Error))
			continue
		}
		key := strings.TrimSuffix(shot, ".png")
		if i := strings.LastIndex(key, "-"); i > 0 {
			key = key[:i] // <surface>-<device>, both themes
		}
		k := known[key]
		for _, p := range []struct {
			what  string
			n, ok int
			items []struct{ Sel, Text, Value string }
		}{{"control(s) under 44x44", h.SmallTarget.N, k.Small, h.SmallTarget.Items}, {"box(es) scrolling sideways", h.Sideways.N, k.Sideways, h.Sideways.Items},
			{"field(s) under 16 px", h.ZoomField.N, k.Zoom, h.ZoomField.Items}} {
			switch {
			case p.n > p.ok:
				var hits []string
				for _, it := range p.items {
					hits = append(hits, fmt.Sprintf("%s %q %s", it.Sel, it.Text, it.Value))
				}
				failed = append(failed, fmt.Sprintf("%s: %d %s, %d known: %s", shot, p.n, p.what, p.ok, strings.Join(hits, "; ")))
			case p.n < p.ok:
				logs = append(logs, fmt.Sprintf("C3 %s: %d %s, fewer than the %d goldenC3Known[%q] allows; lower it, or delete it once %s is fixed", shot, p.n, p.what, p.ok, key, k.Bead))
			case p.n > 0:
				logs = append(logs, fmt.Sprintf("C3 %s: %d known %s (%s)", shot, p.n, p.what, k.Bead))
			}
		}
	}
	return logs, failed
}

// goldenShards is how many shards TestGardenGoldens sweeps in: GARDENHUB_GOLDEN_SHARDS when set, else one per CPU this
// process may use (GOMAXPROCS, which follows a container's CPU limit) up to 4. Each shard is a Chromium page and a card
// world at a time; past a CPU each, they only wait on each other (the splash's SwiftShader most).
func goldenShards(t *testing.T) int {
	if v := os.Getenv("GARDENHUB_GOLDEN_SHARDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("GARDENHUB_GOLDEN_SHARDS=%q: want a whole number from 1", v)
		}
		return n
	}
	return min(4, runtime.GOMAXPROCS(0))
}

// goldenEvidence is the task the goldens' Mailbox decision names with --evidence: on the bead view's reader only
// (cardWorldRig), like the face fixtures, so no plot carries it.
const goldenEvidence = "gd-GateEvid01"

// goldenPorch is the Porch and Mailbox for the goldens: two agents and an open decision, timed against goldenNow.
func goldenPorch() []*beadsapi.BeadDetail {
	at := func(id, typ, title string, ago time.Duration, fields map[string]string) *beadsapi.BeadDetail {
		b := bead(id, typ, 0)
		b.Title, b.UpdatedAt, b.Fields = title, goldenNow.Add(-ago), fields
		return b
	}
	// the acceptance decision (decisiontest, gd-DiU8SB6TAM), its context past the fold (gardenmd.FoldAt, gd-OSI1uK35Wa) and
	// --evidence naming goldenEvidence, whose comment's two screenshots the row draws under the context
	d := at("d-1", "decision", decisiontest.Title, 5*time.Minute, decisiontest.Fields(goldenEvidence))
	d.Status, d.OperatedByDisplay = "open", "Flo" // the Mailbox says whom it is asked of
	return []*beadsapi.BeadDetail{
		at("a-1", "agent", "gardener-one", 2*time.Minute, map[string]string{"agent_state": "working"}),
		at("a-2", "agent", "gardener-two", 40*time.Minute, map[string]string{"agent_state": "idle"}),
		d,
	}
}

// checkC3 fails a phone shot with more C3 hits than its goldenC3Known row (none without one), a shot whose probes did not
// run, and says when a row allows more than its shots now have.
func TestCheckC3(t *testing.T) {
	out := []byte(strings.Join([]string{
		`shot land-phone-light.png`,
		`golden c3 land-phone-light.png {"smallTarget":{"n":0,"items":[]},"sideways":{"n":0,"items":[]}}`,
		`golden c3 pen-phoneland-light.png {"smallTarget":{"n":1,"items":[{"sel":"nav#cursor-bar > a.cb-p","text":"Pen","value":"30x44"}]},"sideways":{"n":0,"items":[]}}`,
		`golden c3 bead-phone-dark.png {"smallTarget":{"n":2,"items":[]},"sideways":{"n":1,"items":[]}}`,
		`golden c3 barn-phone-light.png {"smallTarget":{"n":0,"items":[]},"sideways":{"n":0,"items":[]}}`,
		`golden c3 menu-phone-light.png {"error":"root #menu-list not in the page"}`,
		`golden c3 cmdk-phone-dark.png {"smallTarget":{"n":0,"items":[]},"sideways":{"n":0,"items":[]},"zoomField":{"n":1,"items":[{"sel":"div.jump-row > input#jump-q","text":"Search","value":"14px"}]}}`,
	}, "\n"))
	known := map[string]c3Known{"bead-phone": {Bead: "gd-x", Small: 2, Sideways: 1}, "barn-phone": {Bead: "gd-y", Small: 1}}
	logs, failed := checkC3(out, known)
	want := []string{
		`pen-phoneland-light.png: 1 control(s) under 44x44, 0 known: nav#cursor-bar > a.cb-p "Pen" 30x44`,
		`menu-phone-light.png: the probes did not run: root #menu-list not in the page (golden_roots_browser.js)`,
		`cmdk-phone-dark.png: 1 field(s) under 16 px, 0 known: div.jump-row > input#jump-q "Search" 14px`,
	}
	if !slices.Equal(failed, want) {
		t.Errorf("failed:\n  %s\nwant:\n  %s", strings.Join(failed, "\n  "), strings.Join(want, "\n  "))
	}
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, `C3 bead-phone-dark.png: 2 known control(s) under 44x44 (gd-x)`) ||
		!strings.Contains(all, `C3 barn-phone-light.png: 0 control(s) under 44x44, fewer than the 1 goldenC3Known["barn-phone"] allows; lower it, or delete it once gd-y is fixed`) {
		t.Errorf("logs:\n%s", all)
	}
}
