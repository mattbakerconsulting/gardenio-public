package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestReleaseImagesFromTag: --images-from-tag reads images.json as the tag
// committed it, so an image added after the tag is not in its refs.
func TestReleaseImagesFromTag(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".github", "images.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write(`[{"image":"kbeads"},{"image":"controller"}]`)
	git("add", ".")
	git("commit", "-qm", "one")
	git("tag", "2026.307.1")
	write(`[{"image":"kbeads"},{"image":"controller"},{"image":"later"}]`)
	git("commit", "-qam", "two")

	defer func(f string, reg string, fromTag bool, repo string) {
		releaseBuiltImagesFile, releaseBuiltRegistry, releaseBuiltFromTag, releaseRecordRepoDir = f, reg, fromTag, repo
	}(releaseBuiltImagesFile, releaseBuiltRegistry, releaseBuiltFromTag, releaseRecordRepoDir)
	releaseBuiltImagesFile, releaseBuiltRegistry, releaseBuiltFromTag, releaseRecordRepoDir = ".github/images.json", "ecr.example/gardenio/", true, dir

	got, err := releaseImages("2026.307.1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ecr.example/gardenio/kbeads:2026.307.1", "ecr.example/gardenio/controller:2026.307.1"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want refs %v", got, want)
	}
	for i, w := range want {
		if got[i].Ref != w {
			t.Errorf("image %d: ref %q, want %q", i, got[i].Ref, w)
		}
	}

	if _, err := releaseImages("2026.999.1"); err == nil {
		t.Error("a tag that does not exist: want an error, got none")
	}
}
