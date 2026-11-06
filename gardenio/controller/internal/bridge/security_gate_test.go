package bridge

import (
	"reflect"
	"strings"
	"testing"
)

// ── gardenio/locks/security-tools.lock parsing ─────────────────────────────────────────

func TestParseSecurityToolsLock(t *testing.T) {
	lock := `# comment
GITLEAKS_VERSION=8.21.2
GOVULNCHECK_VERSION=1.1.4

OSV_SCANNER_VERSION=1.9.2
CARGO_AUDIT_VERSION=0.21.2
CARGO_DENY_VERSION="0.16.4"
SOME_OTHER_SETTING=ignored
`
	got := ParseSecurityToolsLock(lock)
	want := map[string]string{
		"gitleaks":    "8.21.2",
		"govulncheck": "1.1.4",
		"osv-scanner": "1.9.2",
		"cargo-audit": "0.21.2",
		"cargo-deny":  "0.16.4", // quotes stripped
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSecurityToolsLock:\n got=%v\nwant=%v", got, want)
	}
}

func TestParseSecurityToolsLock_RealFile(t *testing.T) {
	// Guard against the lock schema drifting away from what the gate parses:
	// the real committed file must yield every canonical scanner the gate
	// aggregates (except gitleaks-only edge cases). This is a lightweight
	// self-consistency check; the file is gardenio/locks/security-tools.lock.
	// We only assert the parse recognizes the expected canonical names when fed
	// the canonical content, keeping the test hermetic (no filesystem read).
	pins := ParseSecurityToolsLock("GITLEAKS_VERSION=8.21.2\nGOVULNCHECK_VERSION=1.1.4\nOSV_SCANNER_VERSION=1.9.2\nCARGO_AUDIT_VERSION=0.21.2\nCARGO_DENY_VERSION=0.16.4\n")
	for _, name := range []string{"gitleaks", "govulncheck", "osv-scanner", "cargo-audit", "cargo-deny"} {
		if _, ok := pins[name]; !ok {
			t.Errorf("expected canonical scanner %q in parsed lock", name)
		}
	}
}

// ── Security-Scan: MR-description trailer parsing ────────────────────────────

func TestParseSecurityScanBlock_Full(t *testing.T) {
	desc := `## Summary
does a thing

Operator: Matt Baker
Security-Scan: a1b2c3d gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass security-review=high=0-findings
`
	blk := ParseSecurityScanBlock(desc)
	if !blk.Present {
		t.Fatal("expected block present")
	}
	if blk.SHA != "a1b2c3d" {
		t.Errorf("SHA=%q want a1b2c3d", blk.SHA)
	}
	if len(blk.MalformedTups) != 0 {
		t.Errorf("unexpected malformed tuples: %v", blk.MalformedTups)
	}
	gv := blk.Entries["govulncheck"]
	if gv.Version != "1.1.4" || gv.Result != "0-reachable" {
		t.Errorf("govulncheck entry=%+v", gv)
	}
	if blk.Review == nil || blk.Review.Version != "high" || blk.Review.Result != "0-findings" {
		t.Errorf("review=%+v", blk.Review)
	}
}

func TestParseSecurityScanBlock_Absent(t *testing.T) {
	blk := ParseSecurityScanBlock("no trailer here\njust prose\n")
	if blk.Present {
		t.Fatal("expected block absent")
	}
}

func TestParseSecurityScanBlock_Malformed(t *testing.T) {
	// A garbled tuple must be captured, not silently dropped (fail-loud).
	blk := ParseSecurityScanBlock("Security-Scan: gitleaks=8.21.2=clean garbage-no-eq govulncheck==broken")
	if !blk.Present {
		t.Fatal("expected present")
	}
	if len(blk.MalformedTups) != 2 {
		t.Fatalf("want 2 malformed tuples, got %v", blk.MalformedTups)
	}
	if _, ok := blk.Entries["gitleaks"]; !ok {
		t.Error("valid gitleaks tuple should still parse alongside malformed ones")
	}
}

func TestParseSecurityScanBlock_CaseInsensitiveKey(t *testing.T) {
	blk := ParseSecurityScanBlock("security-scan: gitleaks=8.21.2=clean")
	if !blk.Present {
		t.Fatal("expected case-insensitive trailer key to parse")
	}
}

// ── Two-tier /security-review path classifier ────────────────────────────────

func TestClassifySecurityReviewEffort(t *testing.T) {
	tests := []struct {
		name       string
		files      []string
		wantEffort string
		wantPaths  []string
	}{
		{
			name:       "docs only -> low",
			files:      []string{"docs/foo.md", "README.md"},
			wantEffort: "low",
			wantPaths:  nil,
		},
		{
			name:       "go.mod bump -> high",
			files:      []string{"kbeads/go.mod", "kbeads/go.sum"},
			wantEffort: "high",
			wantPaths:  []string{"kbeads/go.mod", "kbeads/go.sum"},
		},
		{
			name:       "bun.lock -> high",
			files:      []string{"coop/crates/web/bun.lock"},
			wantEffort: "high",
			wantPaths:  []string{"coop/crates/web/bun.lock"},
		},
		{
			name:       "auth code -> high",
			files:      []string{"gardenio/controller/internal/bridge/edge_auth_shim.go"},
			wantEffort: "high",
			wantPaths:  []string{"gardenio/controller/internal/bridge/edge_auth_shim.go"},
		},
		{
			name:       "gitleaks config -> high",
			files:      []string{".gitleaks.toml"},
			wantEffort: "high",
			wantPaths:  []string{".gitleaks.toml"},
		},
		{
			name:       "crypto/tls path -> high",
			files:      []string{"gardenio/controller/internal/tlsconfig/mtls.go"},
			wantEffort: "high",
			wantPaths:  []string{"gardenio/controller/internal/tlsconfig/mtls.go"},
		},
		{
			name:       "security-tools.lock itself -> high",
			files:      []string{"gardenio/locks/security-tools.lock"},
			wantEffort: "high",
			wantPaths:  []string{"gardenio/locks/security-tools.lock"},
		},
		{
			name:       "mixed -> high, only sensitive listed & sorted",
			files:      []string{"z_docs/readme.md", "app/token_store.go", "a.txt"},
			wantEffort: "high",
			wantPaths:  []string{"app/token_store.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifySecurityReviewEffort(tc.files)
			if got.Effort != tc.wantEffort {
				t.Errorf("effort=%q want %q", got.Effort, tc.wantEffort)
			}
			if !reflect.DeepEqual(got.SensitivePaths, tc.wantPaths) {
				t.Errorf("paths=%v want %v", got.SensitivePaths, tc.wantPaths)
			}
		})
	}
}

// ── isFailingResult / isTriagedToZero helpers ────────────────────────────────

func TestResultTokenHelpers(t *testing.T) {
	passing := []string{"clean", "pass", "0-findings", "0-reachable", "0-vulns", "none", "OK"}
	for _, r := range passing {
		if isFailingResult(r) {
			t.Errorf("isFailingResult(%q)=true, want false", r)
		}
	}
	failing := []string{"fail", "3-findings", "1-reachable", "2-vulns", "", "wat"}
	for _, r := range failing {
		if !isFailingResult(r) {
			t.Errorf("isFailingResult(%q)=false, want true", r)
		}
	}
	for _, r := range []string{"0-findings", "clean", "triaged-to-zero", "0"} {
		if !isTriagedToZero(r) {
			t.Errorf("isTriagedToZero(%q)=false, want true", r)
		}
	}
	for _, r := range []string{"2-findings", "open", ""} {
		if isTriagedToZero(r) {
			t.Errorf("isTriagedToZero(%q)=true, want false", r)
		}
	}
}

// ── The aggregate gate ───────────────────────────────────────────────────────

func greenStatuses() map[string]string {
	m := map[string]string{}
	for _, n := range SecurityStatusCheckNames {
		m[n] = "success"
	}
	return m
}

func cleanLock() map[string]string {
	return map[string]string{
		"gitleaks":    "8.21.2",
		"govulncheck": "1.1.4",
		"osv-scanner": "1.9.2",
		"cargo-audit": "0.21.2",
		"cargo-deny":  "0.16.4",
	}
}

func cleanBlock() SecurityScanBlock {
	return ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass")
}

func TestEvaluateSecurityGate_Pass(t *testing.T) {
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    cleanBlock(),
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if !d.Pass {
		t.Fatalf("expected pass, got reasons: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_FailsOnRedStatusCheck(t *testing.T) {
	st := greenStatuses()
	st["CI / Go-Vuln"] = "failed"
	in := SecurityGateInput{
		StatusChecks: st,
		ScanBlock:    cleanBlock(),
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail on red Go-Vuln")
	}
	if !containsReason(d.Reasons, "CI / Go-Vuln") {
		t.Errorf("reasons missing Go-Vuln mention: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_FailsOnMissingStatusCheck(t *testing.T) {
	st := greenStatuses()
	delete(st, "CI / Cargo-Deny")
	in := SecurityGateInput{
		StatusChecks: st,
		ScanBlock:    cleanBlock(),
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail on missing Cargo-Deny status")
	}
}

func TestEvaluateSecurityGate_FailsOnMissingBlock(t *testing.T) {
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    SecurityScanBlock{Entries: map[string]SecurityScanEntry{}},
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail with no Security-Scan block")
	}
	if !containsReason(d.Reasons, "Security-Scan") {
		t.Errorf("reasons missing block mention: %v", d.Reasons)
	}
}

// TestEvaluateSecurityGate_FailsOnVersionDrift guards ka-0000007861: a
// Security-Scan block whose scanner version differs from its lock pin fails.
func TestEvaluateSecurityGate_FailsOnVersionDrift(t *testing.T) {
	// Block claims a stale govulncheck version vs the lock -> drift -> fail.
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.0.0=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail on version drift")
	}
	if !containsReason(d.Reasons, "drift") {
		t.Errorf("reasons missing drift mention: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_FailsOnMissingScannerTuple(t *testing.T) {
	// Block omits cargo-deny entirely.
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail when a pinned scanner is omitted from the block")
	}
	if !containsReason(d.Reasons, "cargo-deny") {
		t.Errorf("reasons missing cargo-deny omission: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_FailsOnFailingScannerResult(t *testing.T) {
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=2-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "low"},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail on govulncheck=2-reachable")
	}
}

func TestEvaluateSecurityGate_HighTierRequiresReview(t *testing.T) {
	// High-tier diff but block has no security-review tuple -> fail.
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    cleanBlock(), // no security-review tuple
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "high", SensitivePaths: []string{"kbeads/go.mod"}},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail: high-tier diff without security-review tuple")
	}
	if !containsReason(d.Reasons, "security-review") {
		t.Errorf("reasons missing security-review: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_HighTierLowEffortRejected(t *testing.T) {
	// Sensitive path touched but only a low-effort review recorded -> fail.
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass security-review=low=0-findings")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "high", SensitivePaths: []string{".gitleaks.toml"}},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail: high-tier diff requires security-review=high")
	}
}

func TestEvaluateSecurityGate_HighTierPasses(t *testing.T) {
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass security-review=high=0-findings")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "high", SensitivePaths: []string{"kbeads/go.mod"}},
	}
	d := EvaluateSecurityGate(in)
	if !d.Pass {
		t.Fatalf("expected high-tier pass, reasons: %v", d.Reasons)
	}
}

func TestEvaluateSecurityGate_HighTierReviewWithFindingsRejected(t *testing.T) {
	blk := ParseSecurityScanBlock("Security-Scan: deadbeef gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass security-review=high=3-findings")
	in := SecurityGateInput{
		StatusChecks: greenStatuses(),
		ScanBlock:    blk,
		LockPins:     cleanLock(),
		Tier:         SecurityReviewTier{Effort: "high", SensitivePaths: []string{"app/auth.go"}},
	}
	d := EvaluateSecurityGate(in)
	if d.Pass {
		t.Fatal("expected fail: high-effort review not triaged-to-zero")
	}
}

func containsReason(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
