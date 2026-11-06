package bridge

// gitlab_mr_backlink_peelprov_test.go — E-PROV #21 (FW-6) GATE.
//
// A peel-on-demand MR lives on the UPSTREAM (real-monorepo) GitLab host. Its
// branch and title carry ONLY the upstream peel-request key, but its
// description MUST carry the R2 §7 provenance block anchoring back to the DARK
// beads/MRs that produced the slice (peel-trailer.sh emit_block):
//
//	Bead: PE-12345
//	Operator: matthew.baker@acme.example
//	# peel provenance (lab-internal)
//	Dark-Bead: gd-DarkAnchor1
//	Dark-MR: 42
//	Provenance-Hash: sha256:deadbeef...
//
// The reverse MR→bead reconciler treats any gd-XXXX it finds as the
// authoritative attachment key. Without the FW-6 guard it would lift
// gd-DarkAnchor1 out of the provenance block and set mr_url on the DARK bead
// pointing at the UPSTREAM peel MR — a cross-host backlink false-attach (a
// dark bead claiming to be produced by an upstream MR it never touched). The
// gate: a peel-shaped MR attaches ONLY to its (upstream) peel-request bead,
// NEVER to a dark anchor.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// peelMRDescription renders a realistic peel-MR body with the §7 dark
// provenance trailer block, parameterized by the dark anchor key so the test
// can assert that exact key is never used as an attachment target.
func peelMRDescription(darkBead string) string {
	return strings.Join([]string{
		"Peel-on-demand slice extracted from dark integration via method A.",
		"",
		"This Draft MR carries ONLY the requested slice atop pristine upstream main.",
		"Provenance (durable anchors, never SHAs):",
		"",
		"Bead: PE-12345",
		"Operator: matthew.baker@acme.example",
		"# peel provenance (lab-internal — dark beads/MRs live on gitlab-dark)",
		"Dark-Bead: " + darkBead,
		"Dark-MR: 42",
		"Provenance-Hash: sha256:deadbeefcafef00dba5eba11deadbeefcafef00dba5eba11deadbeefcafef00d",
	}, "\n")
}

// TestBacklink_PeelMR_NeverAttachesDarkAnchor is the FW-6 GATE. The upstream
// bridge's reconciler sees a peel MR whose ONLY explicit gd-ref is the dark
// anchor buried in the provenance block; the legitimate attachment key is the
// upstream JIRA peel-request key on branch/title. It must link the peel-request
// bead and must NOT set mr_url on the dark anchor.
func TestBacklink_PeelMR_NeverAttachesDarkAnchor(t *testing.T) {
	const darkBead = "gd-DarkAnchor1"
	const upstreamPeelMR = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/777"

	d := newFakeBacklinkDaemon()
	// The dark anchor exists as a real actionable bead — so if the reconciler
	// DID extract it, GetBead would succeed and it would be (wrongly) linked.
	d.beads[darkBead] = actionableBead(darkBead, "task", time.Now())
	// The legitimate upstream peel-request bead, found via its jira label.
	peelReq := actionableBead("gd-peelreq", "task", time.Now(), "jira:PE-12345")
	d.byLabel["jira:PE-12345"] = []*beadsapi.BeadDetail{peelReq}

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{
			WebURL:       upstreamPeelMR,
			SourceBranch: "peel/PE-12345-ecrf-slice",
			Title:        "peel: PE-12345",
			Description:  peelMRDescription(darkBead),
		}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if _, attached := d.updates[darkBead]; attached {
		t.Fatalf("FW-6 GATE VIOLATED: peel MR cross-host false-attached to dark anchor %s (updates=%v)", darkBead, d.updates)
	}
	if got := d.updates["gd-peelreq"]["mr_url"]; got != upstreamPeelMR {
		t.Fatalf("expected peel-request bead gd-peelreq linked to %s, got %q (updates=%v)", upstreamPeelMR, got, d.updates)
	}
	if s.linked != 1 {
		t.Fatalf("expected exactly one link (the peel-request bead), got %+v", s)
	}
}

// TestBacklink_PeelMR_NoUpstreamKey_RefusesDarkAnchor covers the harder case:
// a peel MR whose branch/title carry NO upstream key AND whose description's
// ONLY bead-identifying token is the dark anchor in the §7 provenance block
// (no upstream `Bead:` trailer). With the dark trailers stripped there is no
// recoverable key, so the reconciler must skip the MR as no-key — NOT fall
// through to the dark anchor.
func TestBacklink_PeelMR_NoUpstreamKey_RefusesDarkAnchor(t *testing.T) {
	const darkBead = "gd-DarkAnchor2"
	const upstreamPeelMR = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/778"

	// Description carries ONLY the lab-internal dark provenance block — the
	// dark anchor is the sole gd-ref anywhere on this MR.
	darkOnlyDesc := strings.Join([]string{
		"Peel-on-demand slice (no upstream tracking key).",
		"",
		"# peel provenance (lab-internal)",
		"Dark-Bead: " + darkBead,
		"Dark-MR: 43",
		"Provenance-Hash: sha256:abc123",
	}, "\n")

	d := newFakeBacklinkDaemon()
	d.beads[darkBead] = actionableBead(darkBead, "task", time.Now())

	gl := &fakeMRLister{byState: map[string][]GitLabMR{
		"opened": {{
			WebURL:       upstreamPeelMR,
			SourceBranch: "peel/ecrf-slice",  // no key
			Title:        "peel: ecrf slice", // no key
			Description:  darkOnlyDesc,
		}},
	}}
	r := newReconciler(gl, d)
	s := r.scan(context.Background())

	if len(d.updates) != 0 {
		t.Fatalf("FW-6 GATE VIOLATED: expected zero links (dark anchor must not leak), got updates=%v", d.updates)
	}
	if s.noKey != 1 {
		t.Fatalf("expected noKey=1 once the dark provenance block is stripped, got %+v", s)
	}
}

// TestStripDarkProvenanceAnchors verifies the strip removes exactly the §7
// dark trailers and nothing else — the upstream-visible Bead:/Operator: lines
// and any non-provenance gd-ref in the prose survive.
func TestStripDarkProvenanceAnchors(t *testing.T) {
	in := peelMRDescription("gd-DarkAnchor1")
	out := stripDarkProvenanceAnchors(in)

	for _, banned := range []string{"Dark-Bead:", "Dark-MR:", "Provenance-Hash:", "gd-DarkAnchor1"} {
		if strings.Contains(out, banned) {
			t.Fatalf("strip left %q in output:\n%s", banned, out)
		}
	}
	// Upstream-visible fleet trailers and prose must be preserved.
	for _, keep := range []string{"Bead: PE-12345", "Operator: matthew.baker@acme.example", "Peel-on-demand slice"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("strip dropped expected line %q from output:\n%s", keep, out)
		}
	}

	// A legitimate gd-ref outside the provenance block (e.g. an upstream task
	// referenced in prose) must survive the strip.
	withProse := "Implements gd-Upstream9 per spec.\n\nDark-Bead: gd-DarkAnchor1\nProvenance-Hash: sha256:abc"
	got := stripDarkProvenanceAnchors(withProse)
	if !strings.Contains(got, "gd-Upstream9") {
		t.Fatalf("strip wrongly removed prose gd-ref:\n%s", got)
	}
	if strings.Contains(got, "gd-DarkAnchor1") || strings.Contains(got, "Provenance-Hash:") {
		t.Fatalf("strip failed to remove dark trailers:\n%s", got)
	}
}
