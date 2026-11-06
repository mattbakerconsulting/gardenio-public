package trustmetrics

import "testing"

func TestSplitProvenanceTrailers(t *testing.T) {
	desc := `Fix the auth token refresh race.

This narrows the lock to the refresh critical section.

Dark-Bead: gd-ABC123
Dark-MR: https://dark.example/group/repo/-/merge_requests/9
Provenance-Hash: deadbeef
Bead: gd-XYZ789
Operator: matthewbaker`

	prose, hasProv := splitProvenance(desc)
	if !hasProv {
		t.Error("expected provenance block detected")
	}
	// The prose should keep the rationale but drop every trailer line.
	if want := "Fix the auth token refresh race.\n\nThis narrows the lock to the refresh critical section."; prose != want {
		t.Errorf("prose = %q, want %q", prose, want)
	}
}

func TestSplitProvenanceFenced(t *testing.T) {
	desc := `Small terse change.

<!-- provenance:begin -->
peeled from dark gd-ABC, hash deadbeef
<!-- provenance:end -->`
	prose, hasProv := splitProvenance(desc)
	if !hasProv {
		t.Error("expected fenced provenance detected")
	}
	if prose != "Small terse change." {
		t.Errorf("prose = %q, want %q", prose, "Small terse change.")
	}
}

func TestSplitProvenanceNone(t *testing.T) {
	desc := "Just a plain description with no provenance."
	prose, hasProv := splitProvenance(desc)
	if hasProv {
		t.Error("did not expect provenance")
	}
	if prose != desc {
		t.Errorf("prose = %q, want unchanged", prose)
	}
}

func TestSplitProvenanceEmpty(t *testing.T) {
	prose, hasProv := splitProvenance("")
	if hasProv || prose != "" {
		t.Errorf("empty description: prose=%q hasProv=%v", prose, hasProv)
	}
}

func TestVerbosityRatioExcludesProvenance(t *testing.T) {
	// Two MRs with identical prose but one carries a large provenance block.
	// Their verbosity ratio must be equal — provenance is excluded.
	prose := "Terse rationale."
	withProv := prose + "\n\nDark-Bead: gd-ABC123\nProvenance-Hash: " + string(make([]byte, 0))
	p1, _ := splitProvenance(prose)
	p2, _ := splitProvenance(withProv)
	if len(p1) != len(p2) {
		t.Errorf("provenance not excluded from prose length: %d vs %d", len(p1), len(p2))
	}
}
