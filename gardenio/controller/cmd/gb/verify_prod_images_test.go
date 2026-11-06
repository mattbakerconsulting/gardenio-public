package main

import (
	"os"
	"strings"
	"testing"
)

const prodRegistry = "registry.gitlab.com/acme/platform/gardenio"

// TestClassifyImageRef exercises the pure cover-image classifier against both
// the repo-suffix signal and the tag-scheme signal, plus the clean references
// that MUST NOT trip it.
func TestClassifyImageRef(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		isCover bool
	}{
		// --- cover by repository -cover suffix ---
		{"go-cover-main-tag", prodRegistry + "/controller-cover:main-abc1234", true},
		{"go-cover-rc-tag", prodRegistry + "/kbeads-cover:2026.213.1-rc", true},
		{"cover-digest", prodRegistry + "/coopmux-cover@sha256:" + strings.Repeat("a", 64), true},
		{"cover-untagged", prodRegistry + "/agent-cover", true},
		// --- cover by tag token, non-cover repo (defensive / hand-edited) ---
		{"cover-tag-on-plain-repo", prodRegistry + "/controller:cover-main", true},
		{"cover-token-suffix-tag", prodRegistry + "/controller:main-cover", true},
		{"bare-cover-tag", prodRegistry + "/controller:cover", true},
		// --- clean references (must NOT trip) ---
		{"plain-main-sha", prodRegistry + "/controller:main-abc1234", false},
		{"plain-calver", prodRegistry + "/controller:2026.213.1", false},
		{"plain-latest", prodRegistry + "/kbeads:latest", false},
		{"plain-digest", prodRegistry + "/controller@sha256:" + strings.Repeat("b", 64), false},
		{"third-party-pinned", "postgres:pg17", false},
		{"discovery-substring-not-cover", prodRegistry + "/controller:discovery-1", false},
		{"recover-substring-not-cover", prodRegistry + "/controller:recover", false},
		{"empty", "", false},
		{"repo-contains-cover-midword", prodRegistry + "/coverage-patch:main-abc1234", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := classifyImageRef(tt.image)
			gotCover := reason != ""
			if gotCover != tt.isCover {
				t.Fatalf("classifyImageRef(%q) = cover:%v (reason %q), want cover:%v",
					tt.image, gotCover, reason, tt.isCover)
			}
		})
	}
}

// TestImageTag checks tag extraction across tagged, untagged, digest, and
// registry-port shapes so classifyImageRef never mistakes a host:port for a
// tag.
func TestImageTag(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{prodRegistry + "/controller:main-abc1234", "main-abc1234"},
		{prodRegistry + "/controller", ""},
		{prodRegistry + "/controller@sha256:" + strings.Repeat("a", 64), ""},
		{"registry:5000/repo:v1", "v1"},
		{"registry:5000/repo", ""},
		{"bare", ""},
	}
	for _, tt := range tests {
		if got := imageTag(tt.image); got != tt.want {
			t.Errorf("imageTag(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

// TestScanValuesForCoverImages_Clean confirms a representative clean prod
// deploy-values snippet (image.repository + third-party pinned tags) produces
// NO offenders — the common case must never false-positive.
func TestScanValuesForCoverImages_Clean(t *testing.T) {
	values := `
global:
  coverage:
    enabled: false
beads:
  image:
    repository: kbeads
    pullPolicy: IfNotPresent
controller:
  image:
    repository: controller
coopmux:
  image:
    repository: coopmux
postgres:
  image:
    tag: "pg17"
grafana:
  image:
    tag: "v1.84.8"
`
	offenders := scanValuesForCoverImages("clean.yaml", []byte(values))
	if len(offenders) != 0 {
		t.Fatalf("clean values produced %d offender(s): %+v", len(offenders), offenders)
	}
}

// TestScanValuesForCoverImages_CoverRepo catches a repository whose name
// carries the -cover suffix, split across image.repository + image.tag.
func TestScanValuesForCoverImages_CoverRepo(t *testing.T) {
	values := `
controller:
  image:
    repository: controller-cover
    tag: "main-abc1234"
`
	offenders := scanValuesForCoverImages("bad.yaml", []byte(values))
	if len(offenders) == 0 {
		t.Fatalf("expected an offender for a -cover repository, got none")
	}
	found := false
	for _, o := range offenders {
		if strings.Contains(o.Image, "controller-cover") {
			found = true
			if o.Surface != "file" {
				t.Errorf("offender surface = %q, want file", o.Surface)
			}
		}
	}
	if !found {
		t.Fatalf("no offender referenced controller-cover: %+v", offenders)
	}
}

// TestScanValuesForCoverImages_CoverTag catches a full image reference on one
// line whose repo carries -cover.
func TestScanValuesForCoverImages_InlineImage(t *testing.T) {
	values := `
controller:
  image: ` + prodRegistry + `/controller-cover:2026.213.1-rc
`
	offenders := scanValuesForCoverImages("inline.yaml", []byte(values))
	if len(offenders) != 1 {
		t.Fatalf("expected exactly 1 offender, got %d: %+v", len(offenders), offenders)
	}
}

// TestScanValuesForCoverImages_BareCoverTag catches a cover TAG even when the
// repository is a plain (non-cover) name — the defensive tag-scheme signal.
func TestScanValuesForCoverImages_BareCoverTag(t *testing.T) {
	values := `
controller:
  image:
    repository: controller
    tag: "cover-main"
`
	offenders := scanValuesForCoverImages("tag.yaml", []byte(values))
	if len(offenders) == 0 {
		t.Fatalf("expected an offender for a cover tag on a plain repo, got none")
	}
}

// TestSplitYAMLKeyVal exercises key/value parsing including list markers and
// non-key lines.
func TestSplitYAMLKeyVal(t *testing.T) {
	cases := []struct {
		line, key, val string
	}{
		{"  image: controller-cover:tag", "image", "controller-cover:tag"},
		{"  - name: registry-pull-secret", "name", "registry-pull-secret"},
		{"    tag: \"pg17\"", "tag", "\"pg17\""},
		{"no colon here", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		k, v := splitYAMLKeyVal(c.line)
		if k != c.key || v != c.val {
			t.Errorf("splitYAMLKeyVal(%q) = (%q,%q), want (%q,%q)", c.line, k, v, c.key, c.val)
		}
	}
}

// TestStripYAMLComment ensures trailing comments are dropped so a commented
// example image reference never false-positives.
func TestStripYAMLComment(t *testing.T) {
	in := "  repository: controller  # not controller-cover, just a note"
	got := stripYAMLComment(in)
	if strings.Contains(got, "controller-cover") {
		t.Fatalf("comment not stripped: %q", got)
	}
	// And the commented reference must not be classified as an offender.
	offenders := scanValuesForCoverImages("comment.yaml", []byte(in+"\n"))
	if len(offenders) != 0 {
		t.Fatalf("commented reference produced offenders: %+v", offenders)
	}
}

// TestResolveProdValuesPath_Explicit verifies an explicit --values path is
// honored and a missing one errors.
func TestResolveProdValuesPath_Explicit(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/vals.yaml"
	if err := os.WriteFile(p, []byte("controller:\n  image:\n    repository: controller\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveProdValuesPath(p)
	if err != nil {
		t.Fatalf("resolveProdValuesPath(%q) errored: %v", p, err)
	}
	if got != p {
		t.Errorf("resolveProdValuesPath(%q) = %q, want %q", p, got, p)
	}

	if _, err := resolveProdValuesPath(dir + "/does-not-exist.yaml"); err == nil {
		t.Errorf("expected error for a missing explicit --values path")
	}
}

// TestSortOffenders confirms deterministic ordering (surface, source, image).
func TestSortOffenders(t *testing.T) {
	offenders := []coverOffender{
		{Surface: "file", Source: "b.yaml:2", Image: "x"},
		{Surface: "cluster", Source: "pod-z", Image: "y"},
		{Surface: "file", Source: "a.yaml:9", Image: "z"},
	}
	sortOffenders(offenders)
	if offenders[0].Surface != "cluster" {
		t.Errorf("expected cluster first, got %q", offenders[0].Surface)
	}
	if offenders[1].Source != "a.yaml:9" || offenders[2].Source != "b.yaml:2" {
		t.Errorf("file offenders not source-ordered: %+v", offenders[1:])
	}
}
