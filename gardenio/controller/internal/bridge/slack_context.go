package bridge

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// slackContextMrkdwn renders a decision's or escalation's context, which is
// Markdown, for a Slack mrkdwn section. Code is where an author quotes text it
// does not vouch for (the Lands Gate fences every applicant value,
// gd-ZwnwonBxGS), and Slack reads code unlike Markdown: mrkdwn knows only the
// triple-backtick fence and the one-backtick span, and parses <…> as a control
// sequence (<!channel>, <url|label>) even inside code. So a fenced block of
// any fence becomes a ``` block and a code span of any delimiter a one-backtick
// span, each with & < > escaped and every backtick in it swapped for ˋ
// (U+02CB), and nothing quoted can close its code or act (gd-TyXgeI2OGT).
// Prose outside code is the author's own mrkdwn and passes through unchanged.
func slackContextMrkdwn(md string) string {
	lines := strings.Split(md, "\n")
	var out, prose []string
	flush := func() {
		if len(prose) > 0 {
			out = append(out, slackCodeSpans(strings.Join(prose, "\n")))
			prose = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		fence, ok := mdFenceOpen(lines[i])
		if !ok {
			prose = append(prose, lines[i])
			continue
		}
		// An unclosed fence runs to the end, as in CommonMark.
		end := i + 1
		for end < len(lines) && !mdFenceCloses(lines[end], fence) {
			end++
		}
		flush()
		out = append(out, "```", slackCode(strings.Join(lines[i+1:end], "\n")), "```")
		i = end
	}
	flush()
	return strings.Join(out, "\n")
}

// mdFenceOpen reports whether line opens a fenced code block, and its fence:
// three or more backticks or tildes after any indent, a backtick fence with no
// backtick in its info string. The indent is not limited to CommonMark's three
// spaces, so a block nested in a list item is code in Slack too.
func mdFenceOpen(line string) (string, bool) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return "", false
	}
	n := len(s) - len(strings.TrimLeft(s, s[:1]))
	if n < 3 || (s[0] == '`' && strings.Contains(s[n:], "`")) {
		return "", false
	}
	return s[:n], true
}

// mdFenceCloses reports whether line closes a block opened by fence: a run of
// the same character at least as long, and nothing after it but space.
func mdFenceCloses(line, fence string) bool {
	s := strings.TrimLeft(line, " \t")
	rest := strings.TrimLeft(s, fence[:1])
	return len(s)-len(rest) >= len(fence) && strings.TrimSpace(rest) == ""
}

// slackCodeSpans rewrites the Markdown code spans in prose as Slack code
// spans, by CommonMark's rule: a run of n backticks opens a span closed by the
// next run of exactly n, and a run with no such close is literal.
func slackCodeSpans(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if backslashes := len(s[:i]) - len(strings.TrimRight(s[:i], `\`)); backslashes%2 == 1 {
			b.WriteByte('`')
			i++
			continue
		}
		n := backtickRunAt(s, i)
		end := closingBacktickRun(s, i+n, n)
		if end < 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		code := strings.ReplaceAll(s[i+n:end], "\n", " ")
		if len(code) > 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
			code = code[1 : len(code)-1]
		}
		b.WriteString("`" + slackCode(code) + "`")
		i = end + n
	}
	return b.String()
}

func backtickRunAt(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// closingBacktickRun is the index of the first run of exactly n backticks in
// s at or after from, or -1.
func closingBacktickRun(s string, from, n int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		m := backtickRunAt(s, j)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

var slackCodeReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "ˋ")

// slackCode makes s literal inside Slack code: no control sequence, no
// backtick to close the code early.
func slackCode(s string) string { return slackCodeReplacer.Replace(s) }

// fitSlackContext cuts s, rendered by slackContextMrkdwn, to at most limit
// runes ending with tail. The cut falls on a line break in its last 300 runes
// when there is one, so it splits no code span or escape, and a ``` block it
// leaves open is closed: open, Slack shows the fence as text and the quoted
// text after it as live mrkdwn. Context that fits is returned unchanged.
func fitSlackContext(s string, limit int, tail string) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	const closer = "\n```"
	r := []rune(s)[:max(0, limit-utf8.RuneCountInString(tail)-len(closer))]
	cut := string(r)
	if nl := strings.LastIndexByte(cut, '\n'); nl >= 0 && utf8.RuneCountInString(cut[nl:]) <= 300 {
		cut = cut[:nl]
	}
	cut = strings.TrimRightFunc(cut, unicode.IsSpace)
	open := false
	for _, line := range strings.Split(cut, "\n") {
		if line == "```" {
			open = !open
		}
	}
	if open {
		cut += closer
	}
	return cut + tail
}
