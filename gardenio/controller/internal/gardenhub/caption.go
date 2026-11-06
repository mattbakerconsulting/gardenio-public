package gardenhub

// caption.go — a bird's caption: what a call is for, in a line of words (step 2 of plan gd-jMl4oF2cJO, task
// gd-gCHKhIROvB; spec gardenio/docs/superpowers/specs/2026-10-28-bird-captions-design.md sections 5 and 7). It is made
// once per call here in the hub, without a model, from the few tool_input fields that say what a call is for: the
// Bash description, a file's path, a search pattern, a URL's host, a subagent's description. Every caption is
// scrubbed (scrubCaption) before it is kept: no secret, no path outside the repo, no URL beyond its host, no opaque
// token, no email. Nothing else of tool_input is kept. Shapes: gardenio/docs/homestead-farm-read-model.md, "Activity".

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gardenio/controller/internal/toollatency"
)

const (
	captionMax     = 80  // runes of a caption after scrubbing (the transcript's)
	sayMax         = 280 // runes of what a gardener said on its transcript row (a say row, gd-HHGjxt59Mr): the message is the point, so more than a caption
	wireCaptionMax = 80  // runes of a caption on the wire: cap on activity, doing and gardeners[].doing (spec 7.3)
	captionPattern = 40  // runes of a Grep/Glob pattern or a skill name in a caption
	captionQuery   = 50  // runes of a WebSearch query in a caption
	captionPathTip = 2   // segments a repo path is shortened to when the whole would overflow
)

// CaptionSettings are the captions' switch and the bounds a deploy may set (spec 8 and 14), from beads-viewer's
// environment (CaptionSettingsFromEnv; Helm beadsViewer.captions.* and beadsViewer.transcript.rows). The Haiku
// fallback's own switch and bounds are captioner.Config.
type CaptionSettings struct {
	Off            bool // FARM_CAPTIONS=off: no caption is made, sent, kept or asked of Haiku, which is the wire before captions
	WireCap        int  // FARM_CAPTION_WIRE_CAP: runes of cap on the wire, 1 to wireCaptionMax (the transcript keeps captionMax)
	TranscriptRows int  // FARM_TRANSCRIPT_ROWS: rows a gardener's transcript ring keeps, 1 to maxTranscriptRows
}

// DefaultCaptionSettings is captions on, with the spec's bounds.
func DefaultCaptionSettings() CaptionSettings {
	return CaptionSettings{WireCap: wireCaptionMax, TranscriptRows: transcriptRows}
}

// CaptionSettingsFromEnv is DefaultCaptionSettings overridden by the environment. A value that does not parse, or is
// out of range, keeps the default.
func CaptionSettingsFromEnv(getenv func(string) string) CaptionSettings {
	c := DefaultCaptionSettings()
	switch strings.ToLower(strings.TrimSpace(getenv("FARM_CAPTIONS"))) {
	case "off", "false", "0", "no":
		c.Off = true
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv("FARM_CAPTION_WIRE_CAP"))); err == nil && n >= 1 && n <= wireCaptionMax {
		c.WireCap = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv("FARM_TRANSCRIPT_ROWS"))); err == nil && n >= 1 && n <= maxTranscriptRows {
		c.TranscriptRows = n
	}
	return c
}

// The caption's sources, as the metric label src (spec 5.2).
const (
	CapSrcDescription = "description"
	CapSrcPath        = "path"
	CapSrcPattern     = "pattern"
	CapSrcHost        = "host"
	CapSrcAgent       = "agent"
	CapSrcBuiltin     = "builtin"
	CapSrcMCP         = "mcp"
	CapSrcNone        = "none"
)

// toolFields are the tool_input fields a caption may be made from; Command is read only to classify a Bash command.
// The rest of tool_input is never decoded.
type toolFields struct {
	Command      string `json:"command"`
	Description  string `json:"description"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Pattern      string `json:"pattern"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Skill        string `json:"skill"`
}

// geminiTools names a Gemini CLI tool by the Claude tool its caption is made like.
var geminiTools = map[string]string{
	"run_shell_command": "Bash", "read_file": "Read", "write_file": "Write", "replace": "Edit",
	"glob": "Glob", "search_file_content": "Grep", "web_fetch": "WebFetch", "google_web_search": "WebSearch",
}

// builtinCaptions is the fixed phrase of a tool whose input says nothing a caption needs (spec 5.2 row 6).
var builtinCaptions = map[string]string{
	"ToolSearch":       "Load tools",
	"TaskStop":         "Stop a background task",
	"TaskCreate":       "Add a task",
	"TaskUpdate":       "Update a task",
	"TaskList":         "List tasks",
	"TaskGet":          "Read a task",
	"TaskOutput":       "Read a background task's output",
	"ScheduleWakeup":   "Schedule a wake-up",
	"Monitor":          "Watch a background command",
	"SendMessage":      "Message an agent",
	"TodoWrite":        "Update the to-do list",
	"KillShell":        "Stop a background shell",
	"BashOutput":       "Read a background shell's output",
	"LSP":              "Ask the language server",
	"ReportFindings":   "Report review findings",
	"SendFeedback":     "Draft feedback",
	"Artifact":         "Work on an artifact",
	"ArtifactData":     "Read or write an artifact's data",
	"ArtifactComments": "Read an artifact's comments",
	"Workflow":         "Run a workflow",
	"ListAgents":       "List agents",
	"CronCreate":       "Schedule a task",
	"CronDelete":       "Cancel a scheduled task",
	"CronList":         "List scheduled tasks",
	"PushNotification": "Send a notification",
	"RemoteTrigger":    "Trigger a remote agent",
	"EnterPlanMode":    "Enter plan mode",
	"ExitPlanMode":     "Leave plan mode",
	"AskUserQuestion":  "Ask the user a question",
	"EnterWorktree":    "Enter a worktree",
	"ExitWorktree":     "Leave a worktree",
	"EndConversation":  "End the conversation",
}

// captionTools are the tool names captionOf reads by name: the tool label of gardenio_farm_uncaptioned_total keeps
// these and calls any other mcp or other (captionToolLabel), so the label stays bounded.
var captionTools = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range []string{"Bash", "Monitor", "Read", "NotebookRead", "Edit", "MultiEdit", "NotebookEdit", "Write",
		"Grep", "Glob", "WebFetch", "WebSearch", "Agent", "Task", "Skill"} {
		m[t] = true
	}
	for t := range builtinCaptions {
		m[t] = true
	}
	for t := range geminiTools {
		m[t] = true
	}
	return m
}()

// captionToolLabel is tool as the tool label of an uncaptioned call (gd-Ty2eNoZipX): its name when captionOf knows
// it, mcp for an MCP tool, else other.
func captionToolLabel(tool string) string {
	switch {
	case captionTools[tool]:
		return tool
	case strings.HasPrefix(tool, "mcp__"):
		return "mcp"
	}
	return "other"
}

// captionOf is the caption of a call to tool with input in, and its source: deterministic, never a model. It is
// empty (src none) when nothing in the input says what the call is for, and for a Bash call with no description,
// which the Haiku fallback (step 3) may caption later. Always scrubbed and at most captionMax runes.
func captionOf(tool string, in toolFields) (caption, src string) {
	t := tool
	if g, ok := geminiTools[tool]; ok {
		t = g
	}
	switch t {
	case "Bash", "Monitor":
		if c := scrubCaption(in.Description); c != "" {
			return c, CapSrcDescription
		}
	case "Read", "NotebookRead", "Edit", "MultiEdit", "NotebookEdit", "Write":
		p := in.FilePath
		if p == "" {
			p = in.NotebookPath
		}
		if p = strings.TrimSpace(p); p != "" {
			verb := "Read"
			switch t {
			case "Edit", "MultiEdit", "NotebookEdit":
				verb = "Edit"
			case "Write":
				verb = "Write"
			}
			if c := scrubCaption(verb + " " + p); c != "" {
				return c, CapSrcPath
			}
		}
	case "Grep":
		if p := clipWords(oneLine(in.Pattern), captionPattern); p != "" {
			return scrubCaption(`Search for "` + p + `"`), CapSrcPattern
		}
	case "Glob":
		if p := clipWords(oneLine(in.Pattern), captionPattern); p != "" {
			return scrubCaption("Find files " + p), CapSrcPattern
		}
	case "WebFetch":
		if u, err := url.Parse(strings.TrimSpace(in.URL)); err == nil && u.Hostname() != "" {
			return scrubCaption("Fetch " + u.Hostname()), CapSrcHost
		}
	case "WebSearch":
		if q := clipWords(oneLine(in.Query), captionQuery); q != "" {
			return scrubCaption("Search the web: " + q), CapSrcHost
		}
	case "Agent", "Task":
		if c := scrubCaption(in.Description); c != "" {
			return c, CapSrcAgent
		}
	case "Skill":
		if s := clipWords(oneLine(in.Skill), captionPattern); s != "" {
			return scrubCaption("Use skill " + s), CapSrcBuiltin
		}
		return "Use a skill", CapSrcBuiltin
	}
	if p, ok := builtinCaptions[t]; ok {
		return p, CapSrcBuiltin
	}
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		if server, name, ok := strings.Cut(rest, "__"); ok && words(server) != "" && words(name) != "" {
			return scrubCaption(words(server) + ": " + words(name)), CapSrcMCP
		}
	}
	return "", CapSrcNone
}

// words is an MCP server or tool name in words: kept to toolWord's alphabet, with _ and - read as spaces.
func words(s string) string {
	return strings.Join(strings.FieldsFunc(toolWord(s, ""), func(r rune) bool { return r == '_' || r == '-' }), " ")
}

var (
	captionURLRe   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'\x60<>]+`)
	captionEmailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// An absolute or ~/ path: a / or ~/ at the start of a word, or after = : ( [ or a quote.
	captionPathRe   = regexp.MustCompile(`(^|[\s=:(\[{"'\x60])(~?/[^\s"'\x60)\]},;]*)`)
	captionOpaqueRe = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{32,}`)
	captionBeadRe   = regexp.MustCompile(`\b(?:[a-z]{2}-[A-Za-z0-9]{10}|ka-\d{10})\b`)
)

// scrubCaption makes s fit to show (spec 5.1 and 5.3): one line of words, toollatency.RedactText, a URL cut to its
// host, emails removed, a path in the repo checkout shown relative to it and any other path by its basename, an
// opaque token of 32 or more characters replaced by …, a pair of quotes or backticks round the whole dropped, and the whole cut at
// a word boundary to captionMax runes. Bead ids are kept: maskScoped takes them out for a scoped viewer.
func scrubCaption(s string) string { return scrubText(s, captionMax) }

// scrubText is scrubCaption cut to max runes instead: what a gardener said keeps sayMax (ObserveSay).
func scrubText(s string, max int) string {
	s = toollatency.RedactText(oneLine(s))
	s = captionURLRe.ReplaceAllStringFunc(s, func(m string) string {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
		return "…"
	})
	s = captionEmailRe.ReplaceAllString(s, "")
	out := scrubPaths(s, false)
	if utf8.RuneCountInString(out) > max {
		out = scrubPaths(s, true)
	}
	out = captionOpaqueRe.ReplaceAllStringFunc(out, func(m string) string {
		if pathLike(m) {
			return m
		}
		return "…"
	})
	out = oneLine(out)
	for len(out) >= 2 && out[0] == out[len(out)-1] && strings.IndexByte("\"'`", out[0]) >= 0 {
		out = strings.TrimSpace(out[1 : len(out)-1]) // a quoted caption: the quotes are not words
	}
	return clipWords(out, max)
}

// ScrubCaption is scrubCaption, for the captioner's WithScrub: Haiku's reply passes the same scrubber (spec 8).
func ScrubCaption(s string) string { return scrubCaption(s) }

// scrubPaths rewrites each absolute or ~/ path in s: one in a repo checkout (a "work" or "workspace" directory) as
// the path inside it, short to its last captionPathTip segments; any other as its basename. A URL route (at most two
// segments, no dot in the last) is not a file path and is kept.
func scrubPaths(s string, short bool) string {
	return captionPathRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := captionPathRe.FindStringSubmatch(m)
		lead, p := sub[1], sub[2]
		return lead + scrubPath(p, short)
	})
}

func scrubPath(p string, short bool) string {
	trail := ""
	for strings.HasSuffix(p, ".") || strings.HasSuffix(p, ":") || strings.HasSuffix(p, "!") || strings.HasSuffix(p, "?") {
		trail, p = p[len(p)-1:]+trail, p[:len(p)-1]
	}
	segs := strings.FieldsFunc(strings.TrimPrefix(p, "~"), func(r rune) bool { return r == '/' })
	if len(segs) == 0 {
		return p + trail
	}
	if !strings.HasPrefix(p, "~") && len(segs) <= 2 && !strings.Contains(segs[len(segs)-1], ".") {
		return p + trail // a route such as /w/ or /api/slack
	}
	clean := strings.Split(strings.TrimPrefix(path.Clean("/"+strings.Join(segs, "/")), "/"), "/")
	for i, seg := range clean {
		if (seg == "work" || seg == "workspace") && i+1 < len(clean) && !strings.HasPrefix(p, "~") {
			rest := clean[i+1:]
			if short && len(rest) > captionPathTip {
				rest = rest[len(rest)-captionPathTip:]
			}
			return strings.Join(rest, "/") + trail
		}
	}
	return clean[len(clean)-1] + trail
}

// pathLike says a long run of the opaque-token alphabet is a repo path (words between slashes), not a token: two
// or more segments, each under 32 and none mixing upper case, lower case and digits, and no + or =.
func pathLike(m string) bool {
	if strings.ContainsAny(m, "+=") {
		return false
	}
	segs := strings.Split(strings.Trim(m, "/"), "/")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		if seg == "" || len(seg) >= 32 {
			return false
		}
		var up, low, dig bool
		for _, r := range seg {
			up, low, dig = up || unicode.IsUpper(r), low || unicode.IsLower(r), dig || unicode.IsDigit(r)
		}
		if up && low && dig && len(seg) >= 8 {
			return false
		}
	}
	return true
}

// oneLine is s with control characters dropped and whitespace collapsed to single spaces.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// clipWords cuts s to max runes at a word boundary, ending in … when it cut.
func clipWords(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)[:max-1]
	cut := string(r)
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

// scopedMasks are what maskScoped takes out of a caption for a scoped viewer, in order, beside bead ids. The §5.3
// scrubber keeps a URL's host and any word that is not a path or a token, which is fine for an unrestricted viewer
// but not for a project member: a dollar amount may be fleet spend, which the scoped cut never sends (farm_cut.go),
// and a cluster host, an address or an AWS account says how the fleet is built (gd-Kvh3fdmFg0).
var scopedMasks = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?:US)?\$\s?\d[\d,]*(?:\.\d+)?(?:\s?[kKmMbB]\b)?|\b\d[\d,]*(?:\.\d+)?\s?(?:USD|usd)\b`), "an amount"},
	{regexp.MustCompile(`\barn:aws[a-z-]*:[^\s,;)\]}"'\x60]*`), "an AWS resource"},
	{regexp.MustCompile(`\b[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.amazonaws\.com(?::\d+)?`), "an AWS host"},
	{regexp.MustCompile(`\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:svc(?:\.cluster\.local)?|cluster\.local|internal|local)\b(?::\d+)?`), "a host"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b(?::\d+)?`), "an address"},
	{regexp.MustCompile(`\b\d{12}\b`), "an account"},
}

// maskScoped is a caption for a scoped viewer (spec 12): bead ids out, since the id may name another project's bead,
// and the scopedMasks out.
func maskScoped(s string) string {
	if s == "" {
		return s
	}
	s = captionBeadRe.ReplaceAllString(s, "a bead")
	for _, m := range scopedMasks {
		s = m.re.ReplaceAllString(s, m.with)
	}
	return s
}
