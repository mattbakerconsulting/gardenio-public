package buildinfo

import "testing"

func TestGetFallbacks(t *testing.T) {
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
		in string
		ok bool
	}{
		{"2026.201.5", true},
		{"2026.201", true},
		{"v2026.201.5", true},
		{"2026.201.5-dirty", true},
		{"dev", false},
		{"", false},
		{"abc123", false},
		{"1999.201.5", false},
		{"2026.400.5", false},
		{"2026.0.5", false},
	}
	for _, tt := range tests {
		if _, ok := calverDays(tt.in); ok != tt.ok {
			t.Errorf("calverDays(%q) ok=%v, want %v", tt.in, ok, tt.ok)
		}
	}
}

func TestSkewCheck(t *testing.T) {
	tests := []struct {
		local, daemon string
		wantSkew      bool
	}{
		{"2026.201.5", "2026.201.9", false},
		{"2026.201.5", "2026.210.1", false},
		{"2026.150.1", "2026.201.1", true},
		{"2026.201.1", "2026.150.1", false},
		{"dev", "2026.201.1", false},
		{"2026.201.1", "unknown", false},
	}
	for _, tt := range tests {
		msg, skew := skewCheck(tt.local, tt.daemon)
		if skew != tt.wantSkew {
			t.Errorf("skewCheck(%q, %q) skew=%v, want %v", tt.local, tt.daemon, skew, tt.wantSkew)
		}
		if skew != (msg != "") {
			t.Errorf("skewCheck(%q, %q) message/flag mismatch: skew=%v msg=%q", tt.local, tt.daemon, skew, msg)
		}
	}
}

func TestSeed(t *testing.T) {
	origV, origC := Version, Commit
	defer func() { Version, Commit = origV, origC }()

	Version, Commit = "", ""
	Seed("2026.201.5", "abc123")
	if Version != "2026.201.5" || Commit != "abc123" {
		t.Errorf("Seed did not fill empty vars: version=%q commit=%q", Version, Commit)
	}
	Seed("9999.001.1", "zzz")
	if Version != "2026.201.5" {
		t.Errorf("Seed clobbered a set Version: got %q", Version)
	}
}
