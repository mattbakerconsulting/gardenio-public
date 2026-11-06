package bridge

import (
	"bufio"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// security_gate.go implements the deterministic security bar for a dark MR
// (bundle ka-0000005053 §2.3; task ka-0000007861). It is the aggregator that
// `gb mr security-gate` wraps and the CLI two-tier `/security-review` classifier
// (`gb mr security-tier`) calls — every decision here is a PURE function with no
// GitLab/daemon I/O, so the whole surface is table-testable in
// security_gate_test.go, exactly like EvaluateTicketResolvedCloseGate /
// EvaluateDispatchGate in this package.
//
// # What the gate proves (§2.3 "Verifiable gate")
//
//	gb mr security-gate <iid> is green IFF:
//	  - the named security status checks are green:
//	      CI / Gitleaks, CI / Go-Vuln, CI / Cargo-Audit, CI / Cargo-Deny, CI / OSV-Scan
//	  - the MR description carries a machine-parseable `Security-Scan:` block, and
//	  - every scanner version recorded in that block MATCHES the pin in
//	    gardenio/locks/security-tools.lock (drift = fail — an MR cannot claim a scan it ran
//	    against a stale/unpinned scanner), and
//	  - if the MR touches a sensitive path glob, the block records a high-effort
//	    /security-review with findings triaged-to-zero.
//
// The reachability-diff itself (NEW reachable crit/high vs the merge-base
// baseline) is computed in CI by scripts/security/govulncheck-diff.py and
// surfaced here only as the CI / Go-Vuln status-check verdict — this module does
// not re-run scanners, it AGGREGATES their recorded results the way the
// coverage-patch gate aggregates patch-cov-result.json.

// ── Named security status checks ─────────────────────────────────────────────

// SecurityStatusCheckNames are the externally posted GitLab commit-status names
// the security gate requires green. They match the status-checks blocks of the
// RWX ci.yml archived at docs/archive/rwx/; nothing posts them since RWX was
// deleted on 2026-09-16. CI / Gitleaks predates this work; the other four are added by
// task ka-0000007861.
var SecurityStatusCheckNames = []string{
	"CI / Gitleaks",
	"CI / Go-Vuln",
	"CI / Cargo-Audit",
	"CI / Cargo-Deny",
	"CI / OSV-Scan",
}

// ── gardenio/locks/security-tools.lock parsing ─────────────────────────────────────────

// securityLockLineRe matches a `KEY_VERSION=value` line in security-tools.lock
// (shell-sourceable KEY=VALUE, comments/blank lines ignored). We only harvest
// keys ending in `_VERSION` so an unrelated future variable never becomes a
// phantom scanner pin.
var securityLockLineRe = regexp.MustCompile(`^([A-Za-z0-9_]+)_VERSION=(.+)$`)

// ParseSecurityToolsLock parses gardenio/locks/security-tools.lock into a map of
// canonical scanner name -> pinned version. The canonical name is the lowercased
// lock key with `_` collapsed to `-` (GITLEAKS -> gitleaks, OSV_SCANNER ->
// osv-scanner, CARGO_AUDIT -> cargo-audit) so it matches the scanner token used
// in the Security-Scan: MR block.
func ParseSecurityToolsLock(content string) map[string]string {
	pins := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := securityLockLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := canonScannerName(m[1])
		ver := strings.TrimSpace(m[2])
		// Strip optional surrounding quotes (shell-style).
		ver = strings.Trim(ver, `"'`)
		if name != "" && ver != "" {
			pins[name] = ver
		}
	}
	return pins
}

// canonScannerName normalizes a scanner token to the canonical form used across
// the lock file and the Security-Scan block: lowercase, `_`->`-`, trimmed.
func canonScannerName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

// ── Security-Scan: MR-description trailer parsing ────────────────────────────

// SecurityScanEntry is one scanner's recorded result in a Security-Scan: block.
type SecurityScanEntry struct {
	Scanner string // canonical name: gitleaks, govulncheck, osv-scanner, cargo-audit, cargo-deny
	Version string // recorded scanner version
	Result  string // recorded result token: clean | 0-reachable | pass | <n>-findings | fail | ...
}

// SecurityScanBlock is the parsed Security-Scan: MR-description trailer.
//
// Schema (single logical line; whitespace-separated scanner=version=result
// tuples after an optional leading commit SHA):
//
//	Security-Scan: <sha> gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable \
//	    osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass \
//	    security-review=high=0-findings
//
// The optional `security-review=<effort>=<result>` tuple records the two-tier
// /security-review outcome (effort=low|high, result triaged-to-zero => 0-findings).
type SecurityScanBlock struct {
	Present bool
	SHA     string
	Entries map[string]SecurityScanEntry // keyed by canonical scanner name
	// Review is the security-review tuple if present (effort + result).
	Review        *SecurityScanEntry
	MalformedTups []string // tuples that did not parse as scanner=version=result
}

// securityScanLineRe captures the payload after the `Security-Scan:` trailer key
// at the start of a line (case-insensitive, tolerant of leading whitespace),
// mirroring darkProvenanceTrailerRe's tolerance.
var securityScanLineRe = regexp.MustCompile(`(?im)^[ \t]*Security-Scan[ \t]*:[ \t]*(.+?)[ \t]*$`)

// shaLikeRe matches a bare git SHA (7-64 hex) used as the optional leading token.
var shaLikeRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// ParseSecurityScanBlock extracts the Security-Scan: block from an MR
// description. Returns Present=false when no such trailer line exists. The
// parse is lenient about ordering and the leading SHA token; each remaining
// token must be scanner=version=result or it is recorded in MalformedTups so
// the gate can reject a garbled block loudly rather than silently pass.
func ParseSecurityScanBlock(description string) SecurityScanBlock {
	blk := SecurityScanBlock{Entries: map[string]SecurityScanEntry{}}
	m := securityScanLineRe.FindStringSubmatch(description)
	if m == nil {
		return blk
	}
	blk.Present = true
	for f := range strings.FieldsSeq(m[1]) {
		// Optional leading SHA (only if we haven't seen the SHA yet and it
		// isn't a scanner tuple).
		if !strings.Contains(f, "=") {
			if blk.SHA == "" && shaLikeRe.MatchString(f) {
				blk.SHA = f
				continue
			}
			blk.MalformedTups = append(blk.MalformedTups, f)
			continue
		}
		parts := strings.SplitN(f, "=", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			blk.MalformedTups = append(blk.MalformedTups, f)
			continue
		}
		name := canonScannerName(parts[0])
		entry := SecurityScanEntry{Scanner: name, Version: parts[1], Result: strings.ToLower(parts[2])}
		if name == "security-review" {
			e := entry
			blk.Review = &e
			continue
		}
		blk.Entries[name] = entry
	}
	return blk
}

// ── Two-tier /security-review path classifier ────────────────────────────────

// SensitivePathGlobs are the path patterns that escalate /security-review from
// effort=low (every dark MR diff) to effort=high (bundle ka-0000005053 §2.3):
// auth/token/credential/crypto/deserialization code, the gitleaks config,
// securityContext templates, and any dependency manifest/lockfile. Matching is
// substring/glob on the repo-relative path, case-insensitive.
var SensitivePathGlobs = []string{
	// dependency manifests + lockfiles (a bumped dep is the top vuln vector)
	"go.mod", "go.sum",
	"Cargo.toml", "Cargo.lock",
	"bun.lock", "bun.lockb", "package.json", "package-lock.json", "yarn.lock",
	// secret-scan config + security tooling pins
	".gitleaks.toml",
	"gardenio/locks/security-tools.lock",
	// auth / token / credential / secret / crypto / deserialization surfaces
	"*auth*", "*token*", "*credential*", "*creds*", "*secret*", "*password*",
	"*crypto*", "*cipher*", "*tls*", "*oauth*", "*jwt*", "*keycloak*",
	"*deserialize*", "*unmarshal*", "*decode*",
	// k8s securityContext / RBAC surfaces
	"*securitycontext*", "*rbac*", "*serviceaccount*",
}

// pathMatchesSensitive reports whether a repo-relative path matches any sensitive
// glob. A glob with no `*`/`/` matches on the BASENAME (so "go.mod" matches
// "kbeads/go.mod"); a glob containing `*` is matched with path.Match against
// both the full path and the basename; a glob containing `/` is matched against
// the full path only.
func pathMatchesSensitive(p string) bool {
	lp := strings.ToLower(strings.TrimSpace(p))
	if lp == "" {
		return false
	}
	base := path.Base(lp)
	for _, g := range SensitivePathGlobs {
		lg := strings.ToLower(g)
		switch {
		case strings.Contains(lg, "/"):
			if ok, _ := path.Match(lg, lp); ok {
				return true
			}
			if strings.Contains(lp, lg) {
				return true
			}
		case strings.Contains(lg, "*"):
			if ok, _ := path.Match(lg, base); ok {
				return true
			}
			if ok, _ := path.Match(lg, lp); ok {
				return true
			}
		default:
			// literal filename -> basename match (go.mod anywhere in the tree)
			if base == lg {
				return true
			}
		}
	}
	return false
}

// SecurityReviewTier is the classified /security-review effort for an MR's diff.
type SecurityReviewTier struct {
	Effort         string   // "low" (default) or "high" (sensitive path touched)
	SensitivePaths []string // the changed files that triggered the high tier (sorted)
}

// ClassifySecurityReviewEffort implements the two-tier path-glob policy: low on
// every MR, high when any changed file matches a sensitive glob.
func ClassifySecurityReviewEffort(changedFiles []string) SecurityReviewTier {
	tier := SecurityReviewTier{Effort: "low"}
	seen := map[string]bool{}
	for _, f := range changedFiles {
		if pathMatchesSensitive(f) && !seen[f] {
			seen[f] = true
			tier.SensitivePaths = append(tier.SensitivePaths, f)
		}
	}
	if len(tier.SensitivePaths) > 0 {
		tier.Effort = "high"
		sort.Strings(tier.SensitivePaths)
	}
	return tier
}

// ── The aggregate security gate ──────────────────────────────────────────────

// SecurityGateInput is the fully-resolved, I/O-free input to the gate. The CLI
// (gb mr security-gate) gathers these from GitLab + the lock file; the pure
// evaluation below never touches the network.
type SecurityGateInput struct {
	// StatusChecks maps a named status check to its latest verdict (success,
	// failed, running, ...). Keyed by the exact status name; missing = not run.
	StatusChecks map[string]string
	// ScanBlock is the parsed Security-Scan: trailer from the MR description.
	ScanBlock SecurityScanBlock
	// LockPins is the parsed gardenio/locks/security-tools.lock (canonical name -> version).
	LockPins map[string]string
	// Tier is the classified /security-review effort for this MR's diff.
	Tier SecurityReviewTier
	// RequiredScanners are the canonical scanner names whose version MUST appear
	// in the Security-Scan block and match the lock. Defaults (when nil) to the
	// lock's own keys — every pinned scanner must be recorded.
	RequiredScanners []string
}

// SecurityGateDecision is the gate's verdict.
type SecurityGateDecision struct {
	Pass    bool
	Reasons []string // one human-readable line per failing (or noteworthy) condition
}

// EvaluateSecurityGate is the pure aggregate decision (§2.3 "Verifiable gate").
// Pass IFF: every named security status check is green AND the Security-Scan
// block is present and well-formed AND every required scanner's recorded version
// matches the lock AND (if the diff is high-tier) the block records a high-effort
// /security-review triaged-to-zero.
func EvaluateSecurityGate(in SecurityGateInput) SecurityGateDecision {
	var reasons []string

	// 1. Named status checks must be green.
	for _, name := range SecurityStatusCheckNames {
		v, ok := in.StatusChecks[name]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("status check %q not found (did the pipeline run it?)", name))
			continue
		}
		if strings.ToLower(v) != "success" {
			reasons = append(reasons, fmt.Sprintf("status check %q is %q, not success", name, v))
		}
	}

	// 2. Security-Scan block must be present + well-formed.
	blk := in.ScanBlock
	if !blk.Present {
		reasons = append(reasons, "MR description has no machine-parseable `Security-Scan:` block")
	} else if len(blk.MalformedTups) > 0 {
		reasons = append(reasons, fmt.Sprintf("Security-Scan block has malformed tuple(s): %s", strings.Join(blk.MalformedTups, " ")))
	}

	// 3. Recorded scanner versions must match the lock pins.
	required := in.RequiredScanners
	if required == nil {
		required = make([]string, 0, len(in.LockPins))
		for name := range in.LockPins {
			required = append(required, name)
		}
		sort.Strings(required)
	}
	if blk.Present {
		for _, name := range required {
			pin, pinned := in.LockPins[name]
			entry, recorded := blk.Entries[name]
			if !recorded {
				reasons = append(reasons, fmt.Sprintf("Security-Scan block omits scanner %q (pinned at %s in security-tools.lock)", name, pin))
				continue
			}
			if pinned && entry.Version != pin {
				reasons = append(reasons, fmt.Sprintf("scanner %q version drift: block=%s lock=%s", name, entry.Version, pin))
			}
			if isFailingResult(entry.Result) {
				reasons = append(reasons, fmt.Sprintf("scanner %q recorded a failing result: %s", name, entry.Result))
			}
		}
	}

	// 4. High-tier diffs require a high-effort /security-review triaged-to-zero.
	if in.Tier.Effort == "high" {
		switch {
		case blk.Review == nil:
			reasons = append(reasons, fmt.Sprintf("MR touches %d sensitive path(s) but Security-Scan block records no `security-review=` tuple", len(in.Tier.SensitivePaths)))
		case strings.ToLower(blk.Review.Version) != "high":
			// Version field of the review tuple carries the EFFORT.
			reasons = append(reasons, fmt.Sprintf("MR touches sensitive paths -> requires security-review=high, block records security-review=%s", blk.Review.Version))
		case !isTriagedToZero(blk.Review.Result):
			reasons = append(reasons, fmt.Sprintf("high-effort security-review not triaged-to-zero: result=%s", blk.Review.Result))
		}
	}

	return SecurityGateDecision{Pass: len(reasons) == 0, Reasons: reasons}
}

// isFailingResult reports whether a recorded scanner result token denotes a
// failure. Clean/pass/zero-finding tokens pass; anything else (fail, <n>-findings
// with n>0, <n>-reachable with n>0) fails. Unknown tokens are treated as failing
// (fail-loud — a garbled result never silently passes).
func isFailingResult(result string) bool {
	r := strings.ToLower(strings.TrimSpace(result))
	switch r {
	case "clean", "pass", "passed", "ok", "0-findings", "0-reachable", "none", "green":
		return false
	}
	// "0-<anything>" is a zero count -> pass (0-vulns, 0-advisories, ...).
	if strings.HasPrefix(r, "0-") {
		return false
	}
	return true
}

// isTriagedToZero reports whether a /security-review result denotes zero
// outstanding findings.
func isTriagedToZero(result string) bool {
	r := strings.ToLower(strings.TrimSpace(result))
	switch r {
	case "0-findings", "clean", "triaged", "triaged-to-zero", "pass", "none", "0":
		return true
	}
	return strings.HasPrefix(r, "0-")
}
