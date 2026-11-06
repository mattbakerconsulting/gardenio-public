package main

// lint_trigger_prompts.go implements `gb lint trigger-prompts` — a drift-lint
// that flags live gardener trigger/schedule prompts teaching `gb` commands that
// no longer exist on the current `gb` surface.
//
// Why this exists (ka-0000003064 / design ka-0000007921):
//   A gardener trigger's instruction prompt is a `fields.prompt` snapshot
//   captured at trigger-creation time. It is NEVER re-rendered when the
//   canonical playbook template (gardener.go gardenerPlaybookPrompt) changes. So
//   when a `gb` command is renamed — e.g. the old `gb decision pingback
//   --thread` was replaced by the real `gb decision reflag <id> --reason` — the
//   frozen prompts keep teaching the OLD command, and every gardener they spawn
//   is handed an un-executable instruction. This already fired as CED bug
//   ka-0000007245 (fixed by a one-time backfill). Backfill does not prevent the
//   NEXT rename; this lint does, by cross-checking the frozen prompts against
//   the LIVE cobra command tree.
//
// The check is deliberately narrow:
//   - Only `gb`-command tokens are inspected (no kd, no shell).
//   - Only INSTRUCTION surfaces are scanned (fields.prompt, fields.terminal_goal,
//     and the authored instruction fields of the fields.gardener spec). Engine-
//     owned run history (progress_state / run_log / runs) is EXCLUDED — that is
//     append-only agent prose ("no pingback due") and is NOT an instruction, so
//     scanning it would manufacture false positives.
//   - The valid surface is derived by walking rootCmd, so it stays correct as
//     commands are added or removed — no hard-coded allow-list to rot.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandSurface is the set of valid `gb` subcommands and flags, derived by
// walking the live cobra command tree. It is the source of truth the lint
// cross-checks prompt tokens against.
type commandSurface struct {
	// subcommands maps a command path (space-joined, e.g. "decision reflag")
	// to true. Group commands ("decision") and leaves ("decision reflag") are
	// both present. The empty path "" is never stored.
	subcommands map[string]bool
	// groups maps a command path to true when that command has children (is a
	// group, e.g. "decision"), so we can tell "gb decision <sub>" apart from a
	// leaf like "gb stop <arg>".
	groups map[string]bool
	// flags maps a command path to its set of valid long-flag names (without
	// the leading "--"). Every command inherits the persistent root flags.
	flags map[string]map[string]bool
}

// buildCommandSurface walks a cobra command tree rooted at root and records
// every command path, which paths are groups, and the long flags valid at each
// path (local + inherited persistent). Paths are relative to `gb`, so the
// top-level "decision" command has path "decision" and its "reflag" child has
// path "decision reflag". cobra's auto-generated `help` / `completion` commands
// are skipped — they are not part of the authored surface we lint against.
func buildCommandSurface(root *cobra.Command) *commandSurface {
	s := &commandSurface{
		subcommands: map[string]bool{},
		groups:      map[string]bool{},
		flags:       map[string]map[string]bool{},
	}
	var walk func(cmd *cobra.Command, prefix []string)
	walk = func(cmd *cobra.Command, prefix []string) {
		path := strings.Join(prefix, " ")
		if path != "" {
			s.subcommands[path] = true
			// Collect long flags valid at this command: local + persistent +
			// inherited persistent. cmd.Flags() merges local and inherited once
			// the tree is assembled, but be explicit so the set is complete
			// regardless of when this runs.
			fset := map[string]bool{}
			collect := func(f *pflag.Flag) { fset[f.Name] = true }
			cmd.LocalFlags().VisitAll(collect)
			cmd.InheritedFlags().VisitAll(collect)
			cmd.PersistentFlags().VisitAll(collect)
			s.flags[path] = fset
		}
		children := cmd.Commands()
		if len(children) > 0 && path != "" {
			s.groups[path] = true
		}
		for _, c := range children {
			if c.Name() == "help" || c.Name() == "completion" {
				continue
			}
			walk(c, append(prefix, c.Name()))
		}
	}
	walk(root, nil)
	return s
}

// TriggerPromptFinding is one drift hit: a prompt on a live trigger/schedule
// bead teaches a `gb` subcommand or flag that is absent from the current
// surface.
type TriggerPromptFinding struct {
	ID      string `json:"id"`      // trigger/schedule bead id
	Title   string `json:"title"`   // bead title
	Field   string `json:"field"`   // which instruction surface (prompt / terminal_goal / gardener.<f>)
	Kind    string `json:"kind"`    // "subcommand" or "flag"
	Token   string `json:"token"`   // the offending token, e.g. "gb decision pingback" or "gb decision reflag --thread"
	Message string `json:"message"` // human-readable explanation
}

// gbAnchorRe locates the start of a `gb <...>` command region. It requires the
// `gb` to begin a token — preceded by start-of-string or a non-word,
// non-path-ish char (so a backtick-wrapped “ `gb …` “ and a sentence-leading
// ". gb …" match, but substrings like "ngb", "ka-gb", or "v1.gb" do not). The
// command region itself is extracted and tokenised separately by commandRegion,
// NOT captured here, so that argument placeholders like <id> stay visible to the
// classifier.
var gbAnchorRe = regexp.MustCompile(`(?:^|[^\w./-])gb `)

// argShapedRe matches a token that is unambiguously a COMMAND ARGUMENT rather
// than an English word: a flag (--foo), an angle/brace placeholder (<id>,
// {bead_id}), a bead id (gd-…), or a shell var ($X). This is the load-bearing
// prose/command discriminator: "gb decision reflag <id>" (real command, arg
// follows) is flagged, "gb decision or gb escalation" and "gb decision when
// human action is needed" (prose, an English word follows) are not.
var argShapedRe = regexp.MustCompile(`^(--|<|{|(?:gd|ka)-|\$)`)

// escalationMentionRe matches the word "escalation"/"escalations" anywhere in a
// surface, case-insensitively. It is the second half of the reflag type-mismatch
// signal: an instruction surface that teaches `gb decision reflag` AND also
// concerns escalations is teaching a command that is runtime-rejected on the
// escalation-typed beads it is pointed at (the type gate in runReflag). \b keeps
// it from matching the word buried inside a larger alphanumeric token (e.g.
// "descalationism"); hyphens are word boundaries, so "de-escalation" DOES match,
// which is fine — that still concerns escalations.
var escalationMentionRe = regexp.MustCompile(`(?i)\bescalations?\b`)

// lintTriggerPrompts scans the instruction surfaces of the given trigger/
// schedule beads and returns a finding for every `gb` subcommand/flag token
// that is absent from the current command surface. Findings are sorted by bead
// id then field then token for stable output.
func lintTriggerPrompts(beads []*beadsapi.BeadDetail, surface *commandSurface) []TriggerPromptFinding {
	var findings []TriggerPromptFinding
	for _, b := range beads {
		if b == nil {
			continue
		}
		for _, seg := range instructionSurfaces(b) {
			findings = append(findings, scanSurface(b, seg.field, seg.text, surface)...)
			findings = append(findings, scanReflagTypeMismatch(b, seg.field, seg.text, surface)...)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].ID != findings[j].ID {
			return findings[i].ID < findings[j].ID
		}
		if findings[i].Field != findings[j].Field {
			return findings[i].Field < findings[j].Field
		}
		return findings[i].Token < findings[j].Token
	})
	return findings
}

// surfaceSegment is one named instruction-text slice extracted from a bead.
type surfaceSegment struct {
	field string
	text  string
}

// instructionSurfaces extracts every INSTRUCTION-bearing text slice from a
// trigger/schedule bead, and NOTHING else. This is the load-bearing
// false-positive guard: we take the authored prompt fields but never the
// engine-owned run history (progress_state / run_log), which is append-only
// agent prose that legitimately mentions retired commands.
func instructionSurfaces(b *beadsapi.BeadDetail) []surfaceSegment {
	var segs []surfaceSegment
	if p := strings.TrimSpace(b.Fields["prompt"]); p != "" {
		segs = append(segs, surfaceSegment{field: "prompt", text: p})
	}
	if g := strings.TrimSpace(b.Fields["terminal_goal"]); g != "" {
		segs = append(segs, surfaceSegment{field: "terminal_goal", text: g})
	}
	// The gardener spec blob carries authored instruction fields (name,
	// terminal_goal, exit_condition) alongside engine-owned run state
	// (progress_state, run_log, runs_*). Decode into the authored-only subset
	// so we can never accidentally scan run history.
	if capBlob := strings.TrimSpace(b.Fields["gardener"]); capBlob != "" {
		var spec struct {
			Name          string `json:"name"`
			TerminalGoal  string `json:"terminal_goal"`
			ExitCondition string `json:"exit_condition"`
		}
		if err := json.Unmarshal([]byte(capBlob), &spec); err == nil {
			if spec.Name != "" {
				segs = append(segs, surfaceSegment{field: "gardener.name", text: spec.Name})
			}
			if spec.TerminalGoal != "" {
				segs = append(segs, surfaceSegment{field: "gardener.terminal_goal", text: spec.TerminalGoal})
			}
			if spec.ExitCondition != "" {
				segs = append(segs, surfaceSegment{field: "gardener.exit_condition", text: spec.ExitCondition})
			}
		}
	}
	return segs
}

// commandRegion returns the command region of text spanning [start, hardEnd),
// where start points just past "gb ". It stops early at the first newline,
// quote, or backtick, so quoted argument VALUES (which may legitimately contain
// gb-command examples or --flag-like text) never leak into the command's own
// token run. hardEnd is the start of the NEXT `gb …` region, which bounds the
// run so a flag belonging to a LATER `gb` command on the same line is never
// misattributed to this one (e.g. "gb yield … then gb stop --wrapup" must not
// read --wrapup as a flag of yield).
func commandRegion(text string, start, hardEnd int) string {
	if hardEnd > len(text) {
		hardEnd = len(text)
	}
	for i := start; i < hardEnd; i++ {
		switch text[i] {
		case '\n', '\'', '"', '`':
			return text[start:i]
		}
	}
	return text[start:hardEnd]
}

// scanSurface finds `gb`-command drift within a single instruction text slice.
func scanSurface(b *beadsapi.BeadDetail, field, text string, surface *commandSurface) []TriggerPromptFinding {
	var findings []TriggerPromptFinding
	seen := map[string]bool{} // dedup identical (kind,token) within one surface
	locs := gbAnchorRe.FindAllStringIndex(text, -1)
	for idx, loc := range locs {
		hardEnd := len(text)
		if idx+1 < len(locs) {
			// The next anchor match starts with its boundary char; bound this
			// region there so we never absorb the following `gb` command.
			hardEnd = locs[idx+1][0]
		}
		seg := commandRegion(text, loc[1], hardEnd)
		tokens := strings.Fields(seg)
		if len(tokens) == 0 {
			continue
		}
		f, ok := classifyGBTokens(tokens, surface)
		if !ok {
			continue
		}
		key := f.kind + "\x00" + f.token
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, TriggerPromptFinding{
			ID:      b.ID,
			Title:   b.Title,
			Field:   field,
			Kind:    f.kind,
			Token:   f.token,
			Message: f.message,
		})
	}
	return findings
}

// scanReflagTypeMismatch flags a DIFFERENT drift class than scanSurface: not a
// command that no longer EXISTS, but one that exists yet is TYPE-INCOMPATIBLE
// with the beads the surrounding instruction points it at. `gb decision reflag`
// and `gb escalation reflag` are both live subcommands (MR !5642), so the
// existence check passes both — but runReflag hard-rejects a decision reflag on
// a type=escalation bead ("<id> is not a decision bead (type=escalation)") and
// vice-versa. A surface that teaches `gb decision reflag <id>` while ALSO
// concerning escalations (e.g. "Escalations or decisions gating downstream
// steps … gb decision reflag <id>") is therefore handing gardeners an
// instruction that is guaranteed to fail on every escalation it is applied to.
//
// The signal, and the false-positive guards that keep it narrow:
//   - The surface must use `gb decision reflag` COMMAND-SHAPED (an arg follows
//     `reflag`), so prose like "raise a decision, then reflag it later" is not
//     a hit — same discriminator scanSurface uses.
//   - An escalation mention (escalationMentionRe) must sit WITHIN
//     reflagEscalationWindow characters of that command usage. This is the
//     load-bearing guard: the defect is "reflag pointed at escalations", where
//     the escalation is named as the reflag's target in the same clause
//     ("Escalations or decisions … : gb decision reflag <id>", measured ~100 chars
//     apart). The standard gardener template — a decision-only step 3 reflag plus
//     an escalation word ~1800+ chars away in unrelated `gb yield` prose — is
//     NOT the defect and must stay clean; a whole-surface mention test flags 3 of
//     4 live templates falsely, proximity flags only the real one.
//   - The surface must NOT also teach `gb escalation reflag`. A prompt that
//     freshens BY TYPE — `gb decision reflag` for decisions AND `gb escalation
//     reflag` for escalations — is the CORRECT fixed shape (the HEAD plangardener
//     template) and must stay clean; this guard is what exempts it.
//   - `gb escalation reflag` must actually exist on the live surface before we
//     suggest it, so the fix we recommend can never itself be stale.
//
// At most one finding per surface (the condition is a per-surface boolean).
func scanReflagTypeMismatch(b *beadsapi.BeadDetail, field, text string, surface *commandSurface) []TriggerPromptFinding {
	// Cheap gate first: no escalation mention, or no live `escalation reflag`
	// to recommend, means there is nothing to flag.
	if !escalationMentionRe.MatchString(text) || !surface.subcommands["escalation reflag"] {
		return nil
	}

	// Positions of every escalation-word mention, for the proximity test.
	escLocs := escalationMentionRe.FindAllStringIndex(text, -1)

	var hasEscalationReflag bool
	var decisionReflagNearEsc bool
	locs := gbAnchorRe.FindAllStringIndex(text, -1)
	for idx, loc := range locs {
		hardEnd := len(text)
		if idx+1 < len(locs) {
			hardEnd = locs[idx+1][0]
		}
		tokens := strings.Fields(commandRegion(text, loc[1], hardEnd))
		if present, cmdShaped := groupReflagUsage(tokens, "decision"); present && cmdShaped {
			// loc[1] points at the "decision" token — the reference point for
			// how close an escalation mention sits to this reflag usage.
			if escalationNear(escLocs, loc[1], reflagEscalationWindow) {
				decisionReflagNearEsc = true
			}
		}
		// The escalation-reflag guard is intentionally LENIENT (presence, not
		// command-shaped): any mention that the prompt already knows the
		// escalation verb means it is handling escalations by type, so we must
		// not flag it, even if that mention lacks a trailing argument.
		if present, _ := groupReflagUsage(tokens, "escalation"); present {
			hasEscalationReflag = true
		}
	}

	if decisionReflagNearEsc && !hasEscalationReflag {
		return []TriggerPromptFinding{{
			ID:    b.ID,
			Title: b.Title,
			Field: field,
			Kind:  "type-mismatch",
			Token: "gb decision reflag",
			Message: "`gb decision reflag` freshens only type=decision beads, but this surface also concerns escalations; " +
				"a decision reflag on a type=escalation bead is rejected at the type gate — teach `gb escalation reflag <id>` for escalations",
		}}
	}
	return nil
}

// reflagEscalationWindow is how close (in characters, either direction) an
// escalation mention must sit to a `gb decision reflag` usage for the two to be
// read as the same clause — "for stale escalations/decisions: gb decision
// reflag <id>". Live-corpus separation is stark: genuine defects mention the
// escalation ~100 chars from the reflag; the standard gardener template's
// unrelated escalation mention (in `gb yield` guidance) is 1800+ chars away. 400
// sits an order of magnitude below that floor while leaving generous margin for
// phrasing drift.
const reflagEscalationWindow = 400

// escalationNear reports whether any escalation-mention span in locs starts
// within window characters of ref (in either direction).
func escalationNear(locs [][]int, ref, window int) bool {
	for _, e := range locs {
		d := e[0] - ref
		if d < 0 {
			d = -d
		}
		if d <= window {
			return true
		}
	}
	return false
}

// groupReflagUsage reports whether a `gb …` token run (the fields following
// "gb ") is a `<group> reflag` usage, and whether it is command-shaped (an
// argument-shaped token follows `reflag`, e.g. `<id>`/`--reason`/`gd-…`). Bare
// trailing punctuation on either word is tolerated so "decision reflag." and
// "reflag," compare cleanly.
func groupReflagUsage(tokens []string, group string) (present, commandShaped bool) {
	if len(tokens) < 2 {
		return false, false
	}
	if stripPunct(tokens[0]) != group || stripPunct(tokens[1]) != "reflag" {
		return false, false
	}
	commandShaped = len(tokens) >= 3 && argShapedRe.MatchString(tokens[2])
	return true, commandShaped
}

// gbTokenFinding is the internal result of classifying one `gb …` token run.
type gbTokenFinding struct {
	kind    string
	token   string
	message string
}

// trailingPunct is stripped from the RIGHT of a bare command/subcommand word so
// "gb decision pingback," or "gb yield." tokenise cleanly. Flags and
// placeholders are handled separately and are not trimmed here.
const trailingPunct = ".,;:!?)]}"

// classifyGBTokens decides whether a run of tokens following `gb` teaches an
// invalid subcommand or flag. It returns (finding, true) only for a real drift
// hit; prose like "gb decision or gb escalation" returns ok=false.
//
// Rules (in order):
//  1. A leading flag ("gb --json …") is a root invocation; root flags are not
//     linted (too little signal, and prompts do not start with a bare flag), so
//     skip.
//  2. First token must be a known top-level command. If not, flag ONLY when the
//     next token is argument-shaped (a removed top-level command used as a real
//     command); otherwise skip as prose.
//  3. Descend through known groups while the next token is a valid child. When a
//     known GROUP is followed by a bare word that is NOT a valid child, that is
//     a subcommand-drift hit ONLY if the usage is command-shaped: the token
//     after the bare word is argument-shaped (--flag, <id>, {ph}, gd-…, $var).
//     A group followed by an English word ("decision or", "decision first.",
//     "decision when human action …") is prose and is skipped — a real removed
//     subcommand is virtually always written with an argument or flag after it.
//  4. Once the command path resolves to a real command, any --flag among the
//     remaining tokens that is invalid at that path is a flag-drift hit.
func classifyGBTokens(tokens []string, surface *commandSurface) (gbTokenFinding, bool) {
	first := stripPunct(tokens[0])
	if strings.HasPrefix(first, "--") {
		return gbTokenFinding{}, false // root-flag invocation; not linted
	}
	if first == "" {
		return gbTokenFinding{}, false
	}
	if !surface.subcommands[first] {
		if len(tokens) >= 2 && argShapedRe.MatchString(tokens[1]) {
			return gbTokenFinding{
				kind:    "subcommand",
				token:   "gb " + first,
				message: fmt.Sprintf("`gb %s` is not a current gb command", first),
			}, true
		}
		return gbTokenFinding{}, false
	}

	// Descend group depth. path is the deepest resolved command path.
	path := first
	i := 1
	for surface.groups[path] && i < len(tokens) {
		raw := tokens[i]
		if strings.HasPrefix(raw, "--") {
			break // flags begin; command path resolved
		}
		next := stripPunct(raw)
		if next == "" {
			break
		}
		candidate := path + " " + next
		if surface.subcommands[candidate] {
			path = candidate
			i++
			continue
		}
		// `next` is a bare word under a known GROUP but is not a valid child.
		// It is a drift hit ONLY when command-shaped: an argument-shaped token
		// (--flag, <id>, {ph}, gd-…, $var) follows it. This is exactly the
		// pingback signature "gb decision pingback <id> --thread". A bare word
		// followed by an English word ("decision or", "decision when human …")
		// or ending the region ("decision first.") is prose — skipped, because a
		// genuinely removed subcommand is essentially always written with an
		// argument or flag after it.
		commandShaped := i+1 < len(tokens) && argShapedRe.MatchString(tokens[i+1])
		if commandShaped {
			return gbTokenFinding{
				kind:    "subcommand",
				token:   "gb " + path + " " + next,
				message: fmt.Sprintf("`gb %s %s` is not a subcommand of `gb %s`", path, next, path),
			}, true
		}
		return gbTokenFinding{}, false
	}

	return classifyFlags(path, tokens[i:], surface)
}

// stripPunct trims trailing sentence punctuation from a bare word so tokens like
// "pingback," or "yield." compare cleanly against the command surface. It never
// touches a leading char, so flags (--foo) and placeholders (<id>) are safe to
// pass through unchanged elsewhere.
func stripPunct(tok string) string {
	return strings.TrimRight(tok, trailingPunct)
}

// classifyFlags checks the --flags in rest against the valid flag set for the
// resolved command path, returning the FIRST invalid flag as a finding. A path
// with no recorded flag set is skipped — flags are only judged against a known
// command.
func classifyFlags(path string, rest []string, surface *commandSurface) (gbTokenFinding, bool) {
	valid, ok := surface.flags[path]
	if !ok {
		return gbTokenFinding{}, false
	}
	for _, tok := range rest {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		name := strings.TrimRight(strings.TrimPrefix(tok, "--"), trailingPunct)
		// Strip an inline value: --reason=foo → reason.
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name == "" {
			continue
		}
		if !valid[name] {
			return gbTokenFinding{
				kind:    "flag",
				token:   "gb " + path + " --" + name,
				message: fmt.Sprintf("`--%s` is not a valid flag for `gb %s`", name, path),
			}, true
		}
	}
	return gbTokenFinding{}, false
}

// --- CLI wiring ---

var lintCmd = &cobra.Command{
	Use:     "lint",
	Short:   "Lint live gardenio config for drift",
	GroupID: "orchestration",
}

var lintTriggerPromptsCmd = &cobra.Command{
	Use:   "trigger-prompts",
	Short: "Flag live gardener trigger/schedule prompts teaching gb commands absent from the current gb surface",
	Long: `Scan every open type=trigger and type=schedule bead's INSTRUCTION prompt
surfaces (fields.prompt, fields.terminal_goal, and the authored fields of the
fields.gardener spec) for ` + "`gb`" + ` subcommand/flag tokens that no longer exist on
the current gb command tree.

Gardener trigger prompts are a snapshot captured at trigger-creation and never
re-rendered when the canonical playbook template changes. When a gb command is
renamed (e.g. the old ` + "`gb decision pingback --thread`" + ` → the real
` + "`gb decision reflag <id> --reason`" + `), the frozen prompts keep teaching the OLD
command and every gardener they spawn runs an un-executable instruction. This
lint catches that whole drift class by cross-checking the frozen prompts against
the LIVE cobra command tree, so it stays correct as commands are added/removed.

Alongside the existence check, a TYPE-COMPATIBILITY check flags a surface that
teaches ` + "`gb decision reflag <id>`" + ` while also concerning escalations without
teaching ` + "`gb escalation reflag`" + `. Both verbs exist, so the existence check
passes them — but runReflag hard-rejects a decision reflag on a type=escalation
bead, so such a prompt hands gardeners an instruction guaranteed to fail on every
escalation it is applied to. The fixed shape (freshen BY TYPE — decision reflag
for decisions, escalation reflag for escalations) stays clean.

Engine-owned run history (progress_state / run_log) is deliberately NOT scanned:
it is append-only agent prose and legitimately mentions retired commands.

Exits non-zero when any drift is found, so it can gate CI.

With --over-constraint, run the QUALITATIVE over-constraint checks over the same
instruction surfaces instead of the gb-command-drift check — rules-over-judgment
framing, directive-density spikes, and system-prompt/tool-description redundancy,
the exact three checks gb advice lint --over-constraint runs over advice beads
(the P4 context-audit guard, epic ka-0000006604). The P2 post-remediation
re-audit found the hand-authored gardener trigger prompt is now the single
hottest injected surface (3.89/1k), so this extends the guard that protects the
templated scaffolding to the surface that inherited the density habit. Those
findings are advisory (WARN); add --strict to exit non-zero when any is present.

For a deterministic, plateau-aware regression signal (the nightly over-constraint
watch, ka-0000000135), pass --baseline <file> to diff each directive-density
finding against a persisted per-surface floor: it is annotated NEW (not in the
baseline), CLIMBED (above its recorded floor — a regression that stays flagged
until re-remediated), or same. Pass --emit-baseline <file> to write the
ratcheting next baseline (the floor only ever moves DOWN, toward a
better-remediated state), so night-over-night drift that plateaus above a
remediation floor cannot hide. With --baseline, --strict gates on a real
regression (new/climbed) rather than the mere presence of a steady-state WARN.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		overConstraint, _ := cmd.Flags().GetBool("over-constraint")
		if overConstraint {
			strict, _ := cmd.Flags().GetBool("strict")
			baselinePath, _ := cmd.Flags().GetString("baseline")
			emitBaselinePath, _ := cmd.Flags().GetString("emit-baseline")
			return runTriggerPromptOverConstraint(cmd, strict, baselinePath, emitBaselinePath)
		}

		beads, err := listGardenerTriggerBeads(cmd)
		if err != nil {
			return err
		}
		surface := buildCommandSurface(rootCmd)
		findings := lintTriggerPrompts(beads, surface)

		if jsonOutput {
			printJSON(findings)
		} else {
			printTriggerPromptLint(findings, len(beads))
		}

		if len(findings) > 0 {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return fmt.Errorf("%d trigger-prompt drift finding(s)", len(findings))
		}
		return nil
	},
}

// runTriggerPromptOverConstraint executes the over-constraint lint mode over
// gardener trigger prompts: fetch the trigger beads + the injected config
// reference corpus, extract each bead's authored instruction surfaces, run the
// three over-constraint checks, print, and gate only with --strict (all
// findings are WARN, so the default is advisory).
//
// When baselinePath is set, each directive-density finding is classified
// new/climbed/same against the persisted per-surface floor (so the nightly watch
// has a deterministic, plateau-aware regression signal instead of eyeballing a
// night-over-night delta). When emitBaselinePath is set, the ratcheting next
// baseline (min of current vs prior, floor only moves down) is written there so
// the next run diffs against it. With a baseline loaded, --strict gates on a real
// regression (new or climbed) rather than the mere presence of any WARN.
func runTriggerPromptOverConstraint(cmd *cobra.Command, strict bool, baselinePath, emitBaselinePath string) error {
	beads, err := listGardenerTriggerBeads(cmd)
	if err != nil {
		return err
	}
	refCorpus := buildRefCorpus(cmd.Context(), daemon)
	surfaces := triggerPromptSurfaces(beads)
	findings := advice.LintTriggerPromptOverConstraint(surfaces, refCorpus, advice.OverConstraintConfig{})
	advice.SortOverConstraintFindings(findings)

	haveBaseline := baselinePath != ""
	findings, err = applyBaselineFiles(findings, baselinePath, emitBaselinePath)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(findings)
	} else {
		printTriggerPromptOverConstraint(findings, len(beads), refCorpus != "", haveBaseline)
	}

	// With a baseline, --strict gates on a real regression (new or climbed); a
	// steady-state WARN at/below its floor is deliberately NOT a failure — that
	// is the whole point of the floor. Without a baseline, --strict keeps its
	// original meaning: any WARN present fails.
	if strict {
		if haveBaseline {
			if advice.HasBaselineRegression(findings) {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return fmt.Errorf("%d over-constraint regression(s) vs baseline", countBaselineRegressions(findings))
			}
		} else if len(findings) > 0 {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return fmt.Errorf("%d over-constraint finding(s)", len(findings))
		}
	}
	return nil
}

// applyBaselineFiles is the file-I/O seam of the baseline flow, factored out so
// it is testable without a live daemon. When baselinePath is non-empty it loads
// the persisted floor (a missing file is treated as an empty floor — a first run
// has no prior baseline, so every finding reads as new, the same fail-open
// posture the redundancy check takes with no ref corpus) and diffs the findings
// against it. When emitBaselinePath is non-empty it writes the ratcheting next
// baseline (floor = min of current vs prior). It returns the (possibly
// diff-annotated) findings.
func applyBaselineFiles(findings []advice.OverConstraintFinding, baselinePath, emitBaselinePath string) ([]advice.OverConstraintFinding, error) {
	var baseline advice.OverConstraintBaseline
	if baselinePath != "" {
		data, err := os.ReadFile(baselinePath)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading baseline %q: %w", baselinePath, err)
		}
		baseline, err = advice.LoadOverConstraintBaseline(data)
		if err != nil {
			return nil, fmt.Errorf("parsing baseline %q: %w", baselinePath, err)
		}
		findings = advice.DiffOverConstraintBaseline(findings, baseline)
	}

	if emitBaselinePath != "" {
		next := advice.RatchetOverConstraintBaseline(findings, baseline)
		blob, err := advice.MarshalOverConstraintBaseline(next)
		if err != nil {
			return nil, fmt.Errorf("marshaling baseline: %w", err)
		}
		if err := os.WriteFile(emitBaselinePath, append(blob, '\n'), 0o644); err != nil {
			return nil, fmt.Errorf("writing baseline %q: %w", emitBaselinePath, err)
		}
	}
	return findings, nil
}

// countBaselineRegressions counts findings classified new or climbed against the
// baseline — the number reported in the --strict --baseline gate message.
func countBaselineRegressions(findings []advice.OverConstraintFinding) int {
	n := 0
	for _, f := range findings {
		if f.BaselineDelta == advice.BaselineNew || f.BaselineDelta == advice.BaselineClimbed {
			n++
		}
	}
	return n
}

// triggerPromptSurfaces adapts each trigger/schedule bead's authored
// instruction surfaces (the same INSTRUCTION-only extraction the drift lint
// uses — never engine-owned run history) into advice.TriggerPromptSurface units
// for the over-constraint checks.
func triggerPromptSurfaces(beads []*beadsapi.BeadDetail) []advice.TriggerPromptSurface {
	var out []advice.TriggerPromptSurface
	for _, b := range beads {
		if b == nil {
			continue
		}
		for _, seg := range instructionSurfaces(b) {
			out = append(out, advice.TriggerPromptSurface{
				ID: b.ID, Title: b.Title, Labels: b.Labels,
				Field: seg.field, Text: seg.text,
			})
		}
	}
	return out
}

// printTriggerPromptOverConstraint renders the over-constraint findings for
// gardener trigger prompts as a table, mirroring printOverConstraintLint. When a
// baseline diff was run (baselineUsed), a DELTA column reports each surface's
// classification against the persisted floor (NEW / +N vs floor / same), and the
// footer summarizes how many are regressions.
func printTriggerPromptOverConstraint(findings []advice.OverConstraintFinding, scanned int, refUsed, baselineUsed bool) {
	redundancyNote := ""
	if !refUsed {
		redundancyNote = " (redundancy check skipped — no config beads reachable)"
	}
	if len(findings) == 0 {
		fmt.Printf("No over-constraint smells in the prompt surfaces of %d open trigger/schedule bead(s)%s.\n", scanned, redundancyNote)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if baselineUsed {
		fmt.Fprintln(w, "ID\tFIELD\tSEVERITY\tCHECK\tLINE\tRULE\tDETAIL\tDELTA\tSNIPPET")
	} else {
		fmt.Fprintln(w, "ID\tFIELD\tSEVERITY\tCHECK\tLINE\tRULE\tDETAIL\tSNIPPET")
	}
	for _, f := range findings {
		snippet := f.Snippet
		if len(snippet) > 60 {
			snippet = snippet[:57] + "..."
		}
		if baselineUsed {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				f.ID, f.Field, f.Severity, strings.TrimPrefix(f.Check, "over-constraint:"),
				f.Line, f.Rule, overConstraintDetail(f), baselineDeltaLabel(f), snippet)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				f.ID, f.Field, f.Severity, strings.TrimPrefix(f.Check, "over-constraint:"),
				f.Line, f.Rule, overConstraintDetail(f), snippet)
		}
	}
	w.Flush()
	fmt.Printf("\n%d over-constraint finding(s) across the prompt surfaces of %d open trigger/schedule bead(s)%s\n",
		len(findings), scanned, redundancyNote)
	if baselineUsed {
		regressions := countBaselineRegressions(findings)
		if regressions == 0 {
			fmt.Println("(all at/below their baseline floor — no directive-density regression vs baseline)")
		} else {
			fmt.Printf("(%d regression(s) vs baseline — a surface NEW to the WARN set or CLIMBED above its recorded floor)\n", regressions)
		}
	}
	fmt.Println("(all WARN — smells to review, not delivery breakage; see the P4 context-audit epic ka-0000006604)")
}

// baselineDeltaLabel renders a directive-density finding's classification against
// the baseline for the DELTA column. Non-directive-density findings (which carry
// no baseline delta) render as a dash.
func baselineDeltaLabel(f advice.OverConstraintFinding) string {
	switch f.BaselineDelta {
	case advice.BaselineNew:
		return "NEW"
	case advice.BaselineClimbed:
		return fmt.Sprintf("CLIMBED (was %d)", f.BaselineFloor)
	case advice.BaselineSame:
		return fmt.Sprintf("same (floor %d)", f.BaselineFloor)
	default:
		return "-"
	}
}

// listGardenerTriggerBeads fetches all open type=trigger and type=schedule
// beads. It fails closed on a truncated page: a partial fetch could silently
// skip a drifted trigger and report a false all-clear, so we refuse rather than
// under-report.
func listGardenerTriggerBeads(cmd *cobra.Command) ([]*beadsapi.BeadDetail, error) {
	res, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"trigger", "schedule"},
		Statuses: []string{"open"},
		Limit:    1000,
	})
	if err != nil {
		return nil, fmt.Errorf("listing trigger/schedule beads: %w", err)
	}
	if res.Total > len(res.Beads) {
		return nil, fmt.Errorf("trigger/schedule listing truncated (%d of %d returned); rerun with a larger page or narrower filter", len(res.Beads), res.Total)
	}
	return res.Beads, nil
}

func printTriggerPromptLint(findings []TriggerPromptFinding, scanned int) {
	if len(findings) == 0 {
		fmt.Printf("No gb-command drift in %d open trigger/schedule prompt(s).\n", scanned)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tFIELD\tKIND\tTOKEN\tMESSAGE")
	for _, f := range findings {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", f.ID, f.Field, f.Kind, f.Token, f.Message)
	}
	w.Flush()
	fmt.Printf("\n%d drift finding(s) across %d open trigger/schedule prompt(s)\n", len(findings), scanned)
}

func init() {
	rootCmd.AddCommand(lintCmd)
	lintCmd.AddCommand(lintTriggerPromptsCmd)
	lintTriggerPromptsCmd.Flags().Bool("over-constraint", false, "run the qualitative over-constraint checks (rules-over-judgment framing, directive-density spikes, system-prompt redundancy) over the prompt surfaces instead of the gb-command-drift check")
	lintTriggerPromptsCmd.Flags().Bool("strict", false, "with --over-constraint, exit non-zero when any (WARN-severity) over-constraint finding is present — or, with --baseline, when any finding regressed (new/climbed) vs the baseline")
	lintTriggerPromptsCmd.Flags().String("baseline", "", "with --over-constraint, diff directive-density findings against a persisted per-surface floor (JSON file), annotating each new/climbed/same; a missing file reads as no prior floor")
	lintTriggerPromptsCmd.Flags().String("emit-baseline", "", "with --over-constraint, write the ratcheting next baseline (min of current vs prior floor) to this JSON file so the next run diffs against it")
}
