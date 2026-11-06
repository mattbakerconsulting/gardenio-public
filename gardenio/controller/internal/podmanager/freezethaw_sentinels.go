package podmanager

// freezethaw_sentinels.go — decoding the node scripts' stdout sentinels and the snapshot bundle and sub-container encodings.
// Split out of freezethaw.go (gd-Wl6LuCnvlH); see freezethaw.go for the design overview.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// --- sentinel parsing helpers ----------------------------------------------

// bundleSpecNames are the two spec files captured per container subdir (the
// sandbox root and every sub-container). Keeping the list in one place keeps the
// freeze emit, the Go completeness cross-check, and the thaw re-materialize in
// sync. rootfs (the large overlay) is NOT captured — thaw rebuilds it from
// .overlay-opts (ka-0000000655 fix C).
var bundleSpecNames = []string{"config.json", ".overlay-opts"}

// decodeSpecFiles decodes the repeated FT_SPEC sentinel lines into a map keyed
// by bundle-relative path (fix B, ka-0000000301; N-container generalization
// ka-0000003561). Each line is "<bundle-rel-path>|<base64-bytes>". It fails
// closed: every emitted spec file must decode to non-empty bytes, mirroring the
// FT_BUNDLE_OK guard. The per-container COMPLETENESS check (sandbox + each
// sub-container has both config.json and .overlay-opts) is the caller's job via
// assertBundleComplete, once it knows the sub-container roster.
func decodeSpecFiles(specLines []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(specLines))
	for _, line := range specLines {
		path, b64, ok := strings.Cut(line, "|")
		if !ok || path == "" {
			return nil, fmt.Errorf("malformed FT_SPEC line %q (want <path>|<base64>)", line)
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("spec file %s base64 decode: %w", path, err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("spec file %s decoded empty", path)
		}
		out[path] = b
	}
	if len(out) == 0 {
		return nil, errors.New("no FT_SPEC spec files emitted by freeze")
	}
	return out, nil
}

// assertBundleComplete fails closed when the captured spec-file set is missing
// any container's config.json or .overlay-opts (ka-0000003561). The expected
// subdirs are the sandbox root plus every captured sub-container name; a snapshot
// missing one container's spec is structurally un-thawable (thaw would
// FetchSpec-ENOENT on that bundle), so the caller refuses to persist it.
func assertBundleComplete(specs map[string][]byte, subs []SnapshotContainer) error {
	subdirs := make([]string, 0, len(subs)+1)
	subdirs = append(subdirs, "sandbox")
	for _, s := range subs {
		subdirs = append(subdirs, s.Name)
	}
	for _, dir := range subdirs {
		for _, f := range bundleSpecNames {
			path := dir + "/" + f
			if len(specs[path]) == 0 {
				return fmt.Errorf("captured bundle missing %s (container %q)", path, dir)
			}
		}
	}
	return nil
}

// decodeSubContainers parses the repeated FT_SUBC roster lines into the
// SnapshotContainer list (ka-0000003561). Each line is "<container-name>|<cid>".
// It fails closed on a malformed or empty-named entry. The list is the agent
// plus any always-on native sidecars; the sandbox root is not included.
func decodeSubContainers(subcLines []string) ([]SnapshotContainer, error) {
	out := make([]SnapshotContainer, 0, len(subcLines))
	for _, line := range subcLines {
		name, cid, ok := strings.Cut(line, "|")
		if !ok || name == "" || cid == "" {
			return nil, fmt.Errorf("malformed FT_SUBC line %q (want <name>|<cid>)", line)
		}
		out = append(out, SnapshotContainer{Name: name, CID: cid})
	}
	return out, nil
}

// encodeThawSpecFiles turns the snapshot's captured spec files into the
// base64-carried thaw-script entries (fix B, ka-0000000301), sorted by path so
// the re-materialize step is deterministic.
func encodeThawSpecFiles(specs map[string][]byte) []thawSpecFile {
	if len(specs) == 0 {
		return nil
	}
	paths := make([]string, 0, len(specs))
	for p := range specs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]thawSpecFile, 0, len(paths))
	for _, p := range paths {
		out = append(out, thawSpecFile{
			Path:   p,
			Base64: base64.StdEncoding.EncodeToString(specs[p]),
		})
	}
	return out
}

// encodeThawSubContainers turns the snapshot's sub-container roster into the
// thaw-script entries the barrier-restore script iterates (ka-0000003561),
// preserving capture order (agent first, then sidecars).
func encodeThawSubContainers(subs []SnapshotContainer) []thawSubContainer {
	if len(subs) == 0 {
		return nil
	}
	out := make([]thawSubContainer, 0, len(subs))
	for _, s := range subs {
		out = append(out, thawSubContainer{Name: s.Name, ACID: s.CID})
	}
	return out
}

// overlayRestoreFlag renders the "--overlay2=<medium>" flag the thaw script
// appends to every `runsc restore` from a snapshot's self-described overlay medium
// (overlay-dir-medium, ka-0000007239). Empty/default-self medium ⇒ "" so the
// restore lines stay byte-identical to the pre-fix script (zero blast radius on
// self-medium nodes). The medium is probed off the node's own runsc boot args, so
// it is controller-trusted; still, defensively REJECT any value with shell-unsafe
// characters (whitespace, quotes, $, ;, backtick, etc.) — a malformed medium yields
// "" (default behaviour) rather than risk injecting into the node script. A valid
// medium is the runsc --overlay2 grammar: mount(root|all) ':' backing-spec, e.g.
// "root:dir=/var/lib/gardenio/overlay" or "root:self".
func overlayRestoreFlag(medium string) string {
	medium = strings.TrimSpace(medium)
	if medium == "" {
		return ""
	}
	// self IS the default — no flag needed, and emitting one is harmless but noisy;
	// keep the script byte-identical to today for the common case.
	if medium == "root:self" || medium == "self" {
		return ""
	}
	for _, r := range medium {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ':' || r == '=' || r == '/' || r == '.' || r == '-' || r == '_' || r == ',':
		default:
			// shell-unsafe or unexpected char — fall back to default (no flag).
			return ""
		}
	}
	return "--overlay2=" + medium
}

// parseSentinels extracts KEY=value lines emitted by the node scripts. Only
// lines of the form FT_*=… are collected; everything else (progress logs) is
// ignored. The last value for a key wins (repeated keys like FT_SPEC/FT_SUBC are
// instead collected in order by parseSentinelList).
func parseSentinels(stdout string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "FT_") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		out[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}
	return out
}

// parseSentinelList collects ALL values for a repeated sentinel key, in emission
// order (ka-0000003561). Used for FT_SPEC (per-container spec files) and FT_SUBC
// (the sub-container roster), where the count scales with ContainerCount and the
// last-wins collapse of parseSentinels would discard all but the final line. The
// match is exact on the key (so FT_SPEC does not swallow a future FT_SPECIAL).
func parseSentinelList(stdout, key string) []string {
	var out []string
	prefix := key + "="
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, prefix); ok {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// classifyCopyLeg resolves the payload-completeness verdict for one warm-copy
// leg (/work or /tmp) from its cp rc and the node's FT_*_CP_REASON classification
// (ka-0000006933). It returns (failed, symlinkSkip):
//   - failed=true      => a GENUINE truncating copy failure (the incomplete-payload
//     signal that WARNs + drives the {reason=work_copy|tmp_copy}
//     metric). The affected mount cold-starts at thaw.
//   - symlinkSkip=true => the rc was tripped ONLY by a benign dangling symlink cp
//     skipped; the payload is COMPLETE (the de-noised case,
//     drives the {leg} symlink-skip metric).
//
// At most one is true; both false means a clean leg (rc==0 / reason==clean, or —
// older node — no rc at all).
//
// Verdict table (rcKnown, rc, reason) — errs toward "failed", never a false-clean:
//   - rc known & rc == 0                    => clean            (false,false)
//   - reason == "symlink"                   => benign skip      (false,true)  [de-noise]
//   - reason == "copyfail"                  => genuine failure  (true,false)
//   - reason == "clean"                     => clean            (false,false)
//   - reason absent, rc known & rc != 0     => OLDER node script: fall back to the
//     pre-fix rc rule (rc!=0 => incomplete). Safe-but-noisy until the reason-
//     emitting node ships; never a false-clean. (true,false)
//   - reason present but UNRECOGNIZED (SIGKILL-truncated/partial) on an rc!=0 leg
//     => genuine failure (true,false). M6: a corrupt reason must never
//     downgrade a lost freeze to complete.
//   - rc absent (older-still / leg not attempted) => clean       (false,false)
func classifyCopyLeg(rc int, rcKnown bool, reason string) (failed, symlinkSkip bool) {
	if rcKnown && rc == 0 {
		return false, false
	}
	switch reason {
	case payloadReasonClean:
		return false, false
	case payloadReasonSymlink:
		return false, true
	case payloadReasonCopyFail:
		return true, false
	case "":
		// No reason from the node. Fall back to the pre-fix rc-based rule: a known
		// non-zero rc is incomplete (today's safe-but-noisy behaviour), a missing/zero
		// rc is clean. This is the older-node/new-Go back-compat path (M8).
		return rcKnown && rc != 0, false
	default:
		// A present-but-unrecognized reason. If the rc says the copy was non-clean
		// (or unknown), treat it as a genuine failure — a truncated/corrupt reason
		// must never be read as the benign "symlink" verdict (M6). Only a known rc==0
		// (handled above) can clear a leg.
		return true, false
	}
}

func intField(fields map[string]string, key string) (int, bool) {
	v, ok := fields[key]
	if !ok || v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sumSuffixedSentinels sums a pair of PER-CONTAINER counter sentinels emitted as
// "<key>_<dstn>=<n>" (e.g. FT_RELOC_REFLINK_HITS_agent, ..._sandbox) across every
// container suffix, returning (sumA, sumB, known). known is true iff at least one
// "<keyA>_*" sentinel with a parseable integer value was present — so a self-medium
// freeze or an older node script (which emit neither) is reported as unknown rather
// than a misleading 0/0. A present-but-unparseable value is skipped (best-effort,
// diagnostic-only); keyA presence alone flips known so a genuine all-fallbacks run
// (hits=0, fallbacks>0) is never mistaken for "not emitted".
func sumSuffixedSentinels(fields map[string]string, keyA, keyB string) (int, int, bool) {
	prefixA, prefixB := keyA+"_", keyB+"_"
	var a, b int
	var known bool
	for k, v := range fields {
		if suffix, ok := strings.CutPrefix(k, prefixA); ok && suffix != "" {
			known = true
			if n, err := strconv.Atoi(v); err == nil {
				a += n
			}
		} else if suffix, ok := strings.CutPrefix(k, prefixB); ok && suffix != "" {
			if n, err := strconv.Atoi(v); err == nil {
				b += n
			}
		}
	}
	return a, b, known
}

// int64Field is the wide-int variant of intField for byte-sized sentinels
// (FT_PAGES_BYTES is a ~3.2 GiB integer that fits in an int on 64-bit but not
// on 32-bit — parseSentinels emits a raw digit string, so use strconv.ParseInt).
func int64Field(fields map[string]string, key string) (int64, bool) {
	v, ok := fields[key]
	if !ok || v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
