package captioner

// scrub.go — what Haiku is sent, and what of its reply may be shown (spec §5.3, §8). Both run through the same
// rules: the relay's redaction patterns, URLs cut to their host, paths cut to a repo-relative form or a basename,
// opaque tokens and email addresses removed. The command is then stripped of a leading `cd <dir> &&` and of scratch
// variable assignments, and cut to maxCommandBytes.

import (
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"gardenio/controller/internal/toollatency"
)

const (
	maxCommandBytes = 400 // of the scrubbed command Haiku is sent
	MaxCaptionRunes = 80  // of a caption after scrubbing (spec §5.1)
)

var (
	urlRe   = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://([^/\s'"` + "`" + `<>()]+)[^\s'"` + "`" + `<>()]*`)
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// A path token: ~/ or / at the start or after a character that cannot be part of a word or a path.
	pathRe   = regexp.MustCompile(`(^|[\s'"` + "`" + `=:,;(\[{<>|&])((?:~/|/)[^\s'"` + "`" + `;|&<>(){}\[\],$]*)`)
	opaqueRe = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{32,}`)
	spaceRe  = regexp.MustCompile(`\s+`)

	// A leading `cd <dir> &&` (or `;`), and a leading `NAME=value` that ends in a separator (a scratch variable):
	// neither says what the command is for.
	leadCdRe     = regexp.MustCompile(`^\s*cd\s+(?:"[^"]*"|'[^']*'|\S+)\s*(?:&&|;)\s*`)
	leadAssignRe = regexp.MustCompile(`^\s*(?:export\s+)?[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|\S*)\s*(?:&&|;|\n)\s*`)
)

// fsRoots are first segments that make a path a file path even when it is short enough to read as a URL route.
var fsRoots = map[string]bool{
	"home": true, "tmp": true, "proc": true, "etc": true, "var": true, "usr": true, "root": true, "opt": true,
	"srv": true, "mnt": true, "dev": true, "sys": true, "run": true, "workspace": true, "Users": true, "private": true,
}

// PrepareCommand is the command as Haiku is sent it: scrubbed, without a leading cd or scratch assignments, on one
// line, at most maxCommandBytes. Empty when nothing is left.
func PrepareCommand(cmd string) string {
	for {
		if loc := leadCdRe.FindStringIndex(cmd); loc != nil {
			cmd = cmd[loc[1]:]
			continue
		}
		if loc := leadAssignRe.FindStringIndex(cmd); loc != nil {
			cmd = cmd[loc[1]:]
			continue
		}
		break
	}
	cmd = scrubText(cmd, false)
	cmd = strings.TrimSpace(spaceRe.ReplaceAllString(dropControl(cmd), " "))
	return cutBytes(cmd, maxCommandBytes)
}

// Scrub is the default scrubber for Haiku's reply (the hub may pass its own, spec §5.3): the same rules, with a
// repo path cut to its last two segments, whitespace collapsed and surrounding quotes dropped. It does not cut to
// MaxCaptionRunes: a reply over it is dropped, not shortened.
func Scrub(s string) string {
	s = scrubText(s, true)
	s = strings.TrimSpace(spaceRe.ReplaceAllString(dropControl(s), " "))
	return strings.TrimSpace(strings.Trim(s, "\"'`“”‘’"))
}

// scrubText applies §5.3's rules 1-4 in order. short cuts a repo-relative path to its last two segments.
func scrubText(s string, short bool) string {
	s = toollatency.RedactText(s)
	s = urlRe.ReplaceAllString(s, "$1")
	s = emailRe.ReplaceAllString(s, "")
	s = pathRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := pathRe.FindStringSubmatch(m)
		return sub[1] + reducePath(sub[2], short)
	})
	return opaqueRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, "/") && !mixedCase(m) {
			return m // a repo-relative path such as gardenio/controller/internal/gardenhub, not a token
		}
		return "…"
	})
}

// mixedCase reports an upper-case letter, a lower-case one and a digit in s: what a base64 token has and a path
// rarely does.
func mixedCase(s string) bool {
	var up, low, dig bool
	for _, r := range s {
		up = up || unicode.IsUpper(r)
		low = low || unicode.IsLower(r)
		dig = dig || unicode.IsDigit(r)
	}
	return up && low && dig
}

// reducePath is a path as a caption may show it: the part under a repo checkout (…/work/<rest>), the basename of
// any other file path, or a short URL route (at most two segments, no dot in the last, not under a file system
// root) as it is.
func reducePath(p string, short bool) string {
	if i := strings.Index(p, "/work/"); i >= 0 {
		rest := strings.Trim(p[i+len("/work/"):], "/")
		if short {
			if segs := strings.Split(rest, "/"); len(segs) > 2 {
				rest = strings.Join(segs[len(segs)-2:], "/")
			}
		}
		if rest != "" {
			return rest
		}
	}
	segs := strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
	if !strings.HasPrefix(p, "~") && len(segs) <= 2 && (len(segs) == 0 || !strings.Contains(segs[len(segs)-1], ".")) &&
		(len(segs) == 0 || !fsRoots[segs[0]]) {
		return p
	}
	b := path.Base(strings.TrimRight(p, "/"))
	if b == "~" || b == "/" || b == "." {
		return ""
	}
	return b
}

func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// cutBytes cuts s to at most n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
