package bypass

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// commitRecordDelim separates commit records in the `git log --format`
// output. Two NULs flank a marker that cannot appear inside a commit
// message (`%H`, `%an`, `%ae` cannot produce NUL bytes; `%B` is opaque
// text that git itself sanitizes).
//
// We build it from git's `%x00` directive rather than embedding raw NUL
// bytes in the Go string, because exec(2) rejects argv entries that
// contain NULs. Git interprets the directive at format-render time, so
// the actual stdout bytes contain NULs as intended.
const commitRecordDelim = "%x00%x00END_OF_COMMIT%x00%x00"

// commitRecordDelimRendered is what the delim looks like in git's
// stdout — the same shape with NULs materialized.
const commitRecordDelimRendered = "\x00\x00END_OF_COMMIT\x00\x00"

// ParseFromCommitRange parses Coverage-Bypass trailers from the commits
// in fromSHA..toSHA (exclusive..inclusive) inside repoPath. It reuses
// the W2b-5 Parse logic — only the source of commits differs.
//
// fromSHA is the previous green-main commit (the baseline bead's
// last_updated_commit_sha); toSHA is $CI_COMMIT_SHA (the current
// main-push commit). The returned *Bypass is the same shape the
// patch-cov gate (W2b) receives, so the ratchet job (W2c-1) and the
// MR-level gate see a single struct.
//
// Spec: docs/research/coverage-phase2c-design.md §8.1–§8.2.
//
// Empty-range semantics:
//   - toSHA == "" → error (caller misuse; ratchet job must always know
//     $CI_COMMIT_SHA).
//   - fromSHA == "" → (nil, nil). This is the "initial seed" case: no
//     baseline bead yet, so there's no defined commit range. The
//     ratchet job logs and seeds the baseline rather than attempting
//     a bypass.
//   - fromSHA == toSHA → (nil, nil). Zero new commits, zero trailers.
//
// Error semantics match Parse: any non-nil error MUST be treated as
// "no bypass granted" by the caller, which then files a synthetic
// `tooling-broken` follow-up bead per §3.5 / §8.3.
//
// Follow-up bead obligations enumerated in §8.3 are deferred to the
// caller (W2c-1). This function lands the parser surface only.
func ParseFromCommitRange(repoPath, fromSHA, toSHA string) (*Bypass, error) {
	if toSHA == "" {
		return nil, fmt.Errorf("coverage-bypass: ParseFromCommitRange: toSHA is empty")
	}
	if fromSHA == "" || fromSHA == toSHA {
		return nil, nil
	}

	rangeArg := fromSHA + ".." + toSHA
	// --reverse so the slice is in commit order (oldest first); Parse
	// applies last-write-wins on slice order, matching git's natural
	// merge/squash-concat semantics.
	//
	// --format uses NUL field separators inside a record and a unique
	// record delimiter between records, because commit bodies are
	// multi-line free text and would defeat any newline-based split.
	cmd := exec.Command("git", "-C", repoPath, "log", "--reverse",
		"--format=%H%x00%an <%ae>%x00%B"+commitRecordDelim, rangeArg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("coverage-bypass: git log %s in %s: %w (stderr: %s)",
			rangeArg, repoPath, err, strings.TrimSpace(stderr.String()))
	}

	text := string(out)
	if strings.TrimSpace(text) == "" {
		// Range resolved to zero commits (e.g. toSHA is an ancestor of
		// fromSHA — fast-forward in the wrong direction). Treat as
		// "no trailer found"; the caller already passed a syntactically
		// valid range, so this is not a tooling error.
		return nil, nil
	}

	var commits []Commit
	for chunk := range strings.SplitSeq(text, commitRecordDelimRendered) {
		// `git log` prefixes each record after the first with a newline.
		chunk = strings.TrimLeft(chunk, "\n")
		if chunk == "" {
			continue
		}
		parts := strings.SplitN(chunk, "\x00", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("coverage-bypass: malformed git log record in range %s (got %d fields)", rangeArg, len(parts))
		}
		commits = append(commits, Commit{
			SHA:     parts[0],
			Author:  parts[1],
			Message: parts[2],
		})
	}

	return Parse(commits)
}
