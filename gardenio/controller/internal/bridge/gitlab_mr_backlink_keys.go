package bridge

import (
	"regexp"
	"sort"
	"strings"
)

// backlinkKDRefRe matches explicit bead references like "ka-AbCdEf12". It is
// case-sensitive on the gd- prefix and requires at least 6 id chars so it does
// not match stray tokens. Mirrors summarizeBeadIDPattern but slightly looser on
// length to catch shorter ids that still appear in branches.
var backlinkKDRefRe = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]{6,}\b`)

// darkProvenanceTrailerRe matches the lab-internal provenance trailer lines a
// peel-on-demand MR description carries (peel-trailer.sh emit_block, R2 §7):
// the dark anchors and the provenance hash. Those lines reference DARK
// beads/MRs that live on a DIFFERENT GitLab host than the peel MR itself, so
// the reverse reconciler must NOT treat a ka-XXXX inside them as an attachment
// key — extracting it would set mr_url on a DARK bead pointing at an UPSTREAM
// peel MR, the cross-host backlink false-attach E-PROV #21 (FW-6) gates
// against. The upstream-visible peel-request key is carried separately on the
// branch, the title, and the plain `Bead:`/JIRA trailers, so dropping these
// lines never loses the legitimate attachment target. Matched per-line,
// case-insensitive on the trailer token, tolerant of leading whitespace.
var darkProvenanceTrailerRe = regexp.MustCompile(`(?im)^[ \t]*(?:Dark-Bead|Dark-MR|Provenance-Hash)[ \t]*:.*$`)

// stripDarkProvenanceAnchors removes the §7 dark-provenance trailer lines from
// an MR description before bead-key extraction, so a peel MR's dark anchors are
// never mistaken for its (upstream) attachment key. See darkProvenanceTrailerRe
// for why this is the load-bearing cross-host guard.
func stripDarkProvenanceAnchors(description string) string {
	return darkProvenanceTrailerRe.ReplaceAllString(description, "")
}

// mrBeadKeys are the bead-identifying keys recovered from an MR, partitioned by
// the SOURCE they came from. Source precedence is load-bearing (bug
// ka-0000002276): a kd ref in the branch or title identifies the MR's SUBJECT —
// the bead the MR fixes — while a kd ref that appears only in the description is
// a cross-reference to some OTHER bead (a "related:"/"parent:" line, a PROOF
// packet, a follow-up list). Flattening both into one alphabetically-sorted
// list and picking the first (the pre-fix bug) let a description cross-ref that
// merely sorted earlier win over the real subject, false-attaching mr_url onto
// a bead the MR never touched.
//
// JIRA keys are not tiered: they resolve via the jira:<key> label, and the
// existing multi-distinct-key refusal already keeps that path safe.
type mrBeadKeys struct {
	subjectKD []string // distinct kd refs from branch+title, stable order
	bodyKD    []string // distinct kd refs from description only (subject-excluded)
	jira      []string // distinct JIRA keys from all sources, sorted
}

// empty reports whether no recoverable key was found in any source/tier.
func (k mrBeadKeys) empty() bool {
	return len(k.subjectKD) == 0 && len(k.bodyKD) == 0 && len(k.jira) == 0
}

// all returns every recovered key in a flat, deterministic order (subject kd
// refs, then body kd refs, then JIRA keys) for logging and unlinked-MR filing.
// It is NOT used for resolution — resolution honors the tier boundary.
func (k mrBeadKeys) all() []string {
	out := make([]string, 0, len(k.subjectKD)+len(k.bodyKD)+len(k.jira))
	out = append(out, k.subjectKD...)
	out = append(out, k.bodyKD...)
	out = append(out, k.jira...)
	return out
}

// distinctKDRefs returns the de-duplicated ka-XXXX refs in s, sorted for
// within-source determinism. Sorting here is safe: it only orders refs that
// share a tier (where >1 actionable ref is refused as ambiguous anyway), so it
// never crosses the branch/title-vs-description precedence boundary that the
// flat sort used to destroy.
func distinctKDRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range backlinkKDRefRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// extractMRBeadKeys recovers the bead-identifying keys from an MR's source
// branch, title, and description, partitioned by source so the reverse
// reconciler can honor precedence (see mrBeadKeys). kd refs from the branch and
// title form the SUBJECT tier; kd refs found only in the description form the
// BODY tier (any ref already claimed by the subject tier is dropped from the
// body tier). JIRA-style ticket keys (PE-XXXX / FSM-XXXX / DEVOPS-XXXX …) are
// collected across all sources, de-duplicated, and sorted; ticketKeyRe (defined
// in card_subject.go) is reused for those.
//
// Callers strip dark-provenance trailers from the description before passing it
// (see stripDarkProvenanceAnchors) so a peel MR's dark anchors never leak into
// the body tier.
func extractMRBeadKeys(branch, title, description string) mrBeadKeys {
	subject := distinctKDRefs(branch + "\n" + title)
	inSubject := map[string]bool{}
	for _, k := range subject {
		inSubject[k] = true
	}

	var body []string
	for _, k := range distinctKDRefs(description) {
		if !inSubject[k] {
			body = append(body, k)
		}
	}

	// JIRA keys across all sources. kd refs never match ticketKeyRe (lowercase
	// prefix, non-numeric tail), but guard anyway by skipping anything already
	// captured as a kd ref, mirroring the original defensive check.
	joined := strings.Join([]string{branch, title, description}, "\n")
	var jira []string
	seenJira := map[string]bool{}
	for _, m := range ticketKeyRe.FindAllString(joined, -1) {
		if inSubject[m] || seenJira[m] {
			continue
		}
		seenJira[m] = true
		jira = append(jira, m)
	}
	sort.Strings(jira)

	return mrBeadKeys{subjectKD: subject, bodyKD: body, jira: jira}
}
