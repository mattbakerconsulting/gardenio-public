package buildinfo

import "testing"

func TestGetFallbacks(t *testing.T) {
	// With no ldflags injected in a test binary, Version falls back to "dev"
	// and Commit is either the VCS stamp or "unknown" — never empty.
	got := Get()
	if got.Version == "" {
		t.Error("Version should never be empty")
	}
	if got.Commit == "" {
		t.Error("Commit should never be empty")
	}
	if got.GoVersion == "" {
		t.Error("GoVersion should be populated from runtime.Version()")
	}
}

func TestCalverDays(t *testing.T) {
	tests := []struct {
		in   string
		ok   bool
		desc string
	}{
		{"2026.201.5", true, "full calver"},
		{"2026.201", true, "calver without patch"},
		{"v2026.201.5", true, "leading v tolerated"},
		{"2026.201.5-dirty", true, "dirty suffix stripped"},
		{"2026.201-rc1", true, "rc suffix stripped"},
		{"dev", false, "dev build not a calver"},
		{"", false, "empty not a calver"},
		{"abc123", false, "git sha not a calver"},
		{"1999.201.5", false, "year out of range"},
		{"2026.400.5", false, "day-of-year out of range"},
		{"2026.0.5", false, "day-of-year zero invalid"},
	}
	for _, tt := range tests {
		_, ok := calverDays(tt.in)
		if ok != tt.ok {
			t.Errorf("calverDays(%q) ok=%v, want %v (%s)", tt.in, ok, tt.ok, tt.desc)
		}
	}
}

func TestCalverDaysOrdering(t *testing.T) {
	older, _ := calverDays("2026.100.1")
	newer, _ := calverDays("2026.200.1")
	if newer <= older {
		t.Errorf("expected 2026.200 > 2026.100, got %d vs %d", newer, older)
	}
	// Year rollover: an early day next year must exceed a late day this year.
	lastYear, _ := calverDays("2026.360.1")
	nextYear, _ := calverDays("2027.005.1")
	if nextYear <= lastYear {
		t.Errorf("expected 2027.005 > 2026.360, got %d vs %d", nextYear, lastYear)
	}
}

func TestSkewCheck(t *testing.T) {
	tests := []struct {
		local, daemon string
		wantSkew      bool
		desc          string
	}{
		{"2026.201.5", "2026.201.9", false, "same day, different patch — no skew"},
		{"2026.201.5", "2026.210.1", false, "9 days behind — within threshold"},
		{"2026.201.5", "2026.201.5", false, "identical — no skew"},
		{"2026.150.1", "2026.201.1", true, "51 days behind — skewed"},
		{"2026.201.1", "2026.150.1", false, "local NEWER than daemon — never warn"},
		{"dev", "2026.201.1", false, "dev local — skip check"},
		{"2026.201.1", "unknown", false, "unknown daemon — skip check"},
		{"", "", false, "both empty — skip check"},
	}
	for _, tt := range tests {
		msg, skew := skewCheck(tt.local, tt.daemon)
		if skew != tt.wantSkew {
			t.Errorf("skewCheck(%q, %q) skew=%v, want %v (%s)", tt.local, tt.daemon, skew, tt.wantSkew, tt.desc)
		}
		if skew && msg == "" {
			t.Errorf("skewCheck(%q, %q) returned skew=true but empty message", tt.local, tt.daemon)
		}
		if !skew && msg != "" {
			t.Errorf("skewCheck(%q, %q) returned skew=false but non-empty message %q", tt.local, tt.daemon, msg)
		}
	}
}

func TestSeed(t *testing.T) {
	// Save + restore package state so the test is order-independent.
	origV, origC := Version, Commit
	defer func() { Version, Commit = origV, origC }()

	Version, Commit = "", ""
	Seed("2026.201.5", "abc123")
	if Version != "2026.201.5" || Commit != "abc123" {
		t.Errorf("Seed did not fill empty vars: version=%q commit=%q", Version, Commit)
	}
	// Seed must not clobber an already-set value.
	Seed("9999.001.1", "zzz")
	if Version != "2026.201.5" {
		t.Errorf("Seed clobbered a set Version: got %q", Version)
	}
}
