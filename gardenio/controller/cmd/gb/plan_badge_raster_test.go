package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/badge"
)

func TestBadgeKey_ContentAddressed(t *testing.T) {
	k := badgeKey("coop/sessions", "ka-0000007031", "tier0-128.png", "f9961060ed9507169c3c0ad5f64c64de")
	want := "coop/sessions/images/badges/ka-0000007031/f9961060-tier0-128.png"
	if k != want {
		t.Errorf("badgeKey = %q, want %q", k, want)
	}
}

func TestBadgeKey_TrimsPrefixSlash(t *testing.T) {
	k := badgeKey("coop/sessions/", "gd-x", "tier2-24.png", "abcdef0123456789")
	if strings.Contains(k, "//") {
		t.Errorf("badgeKey has a double slash: %q", k)
	}
	if !strings.HasPrefix(k, "coop/sessions/images/badges/gd-x/abcdef01-") {
		t.Errorf("badgeKey = %q, unexpected layout", k)
	}
}

// The same badge bytes must always map to the same key (idempotent upload);
// different content -> different key (no silent overwrite of a distinct badge).
func TestBadgeKey_Idempotency(t *testing.T) {
	a := badgeKey("p", "gd-1", "tier0-128.png", "aaaaaaaabbbb")
	b := badgeKey("p", "gd-1", "tier0-128.png", "aaaaaaaabbbb")
	if a != b {
		t.Errorf("same content produced different keys: %q vs %q", a, b)
	}
	c := badgeKey("p", "gd-1", "tier0-128.png", "ccccccccdddd")
	if a == c {
		t.Errorf("different content produced the same key: %q", a)
	}
}

func TestEmitPlanBadgePNG_WriteToFile(t *testing.T) {
	resetBadgeFlags()
	dir := t.TempDir()
	out := filepath.Join(dir, "badge.png")
	planBadgeOut = out

	bdg := badge.Tier0("ka-0000007031", "Every gardenio plan gets its own icon")
	if err := emitPlanBadgePNG(newBadgeTestCmd(), "ka-0000007031", "Every gardenio plan", bdg, badge.Arms{}); err != nil {
		t.Fatalf("emitPlanBadgePNG: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	if img.Bounds().Dx() != badge.DefaultRasterSize {
		t.Errorf("width = %d, want %d", img.Bounds().Dx(), badge.DefaultRasterSize)
	}
}

func TestEmitPlanBadgePNG_CustomSize(t *testing.T) {
	resetBadgeFlags()
	dir := t.TempDir()
	out := filepath.Join(dir, "badge.png")
	planBadgeOut = out
	planBadgeSize = 64

	bdg := badge.Tier0("gd-x", "Title")
	if err := emitPlanBadgePNG(newBadgeTestCmd(), "gd-x", "Title", bdg, badge.Arms{}); err != nil {
		t.Fatalf("emitPlanBadgePNG: %v", err)
	}
	data, _ := os.ReadFile(out)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 64 {
		t.Errorf("width = %d, want 64", img.Bounds().Dx())
	}
}

// The --size table: every size gb badge renders, up to the 512px poster size,
// comes out at that edge; past the maximum is an error, not a huge canvas.
func TestEmitPlanBadgePNG_Sizes(t *testing.T) {
	for _, size := range []int{24, 64, 128, 256, 512} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			resetBadgeFlags()
			t.Cleanup(resetBadgeFlags)
			planBadgeOut = filepath.Join(t.TempDir(), "badge.png")
			planBadgeSize = size

			bdg := badge.Tier0("gd-x", "Title")
			if err := emitPlanBadgePNG(newBadgeTestCmd(), "gd-x", "Title", bdg, badge.Arms{}); err != nil {
				t.Fatalf("emitPlanBadgePNG: %v", err)
			}
			data, err := os.ReadFile(planBadgeOut)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
				t.Errorf("bounds = %v, want %dx%d", img.Bounds(), size, size)
			}
		})
	}
	t.Run("over max", func(t *testing.T) {
		resetBadgeFlags()
		t.Cleanup(resetBadgeFlags)
		planBadgeOut = filepath.Join(t.TempDir(), "badge.png")
		planBadgeSize = badge.MaxRasterSize * 2
		if err := emitPlanBadgePNG(newBadgeTestCmd(), "gd-x", "Title", badge.Tier0("gd-x", "Title"), badge.Arms{}); err == nil {
			t.Errorf("--size %d succeeded, want an error", planBadgeSize)
		}
	})
}

// A poster is refused at a small-slot raster size and an unknown style is
// refused outright; a poster as SVG, or at 128px and up, is fine.
func TestCheckBadgeStyle(t *testing.T) {
	for _, c := range []struct {
		name    string
		style   string
		png     bool
		size    int
		wantErr bool
	}{
		{"bold at 24", badge.StyleLineArtBold, true, 24, false},
		{"poster at 512", badge.StylePoster, true, 512, false},
		{"poster at 128", badge.StylePoster, true, badge.PosterMinSize, false},
		{"poster at 24", badge.StylePoster, true, 24, true},
		{"poster svg", badge.StylePoster, false, 24, false},
		{"unknown style", "painterly", false, 128, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetBadgeFlags()
			t.Cleanup(resetBadgeFlags)
			planBadgeStyle, planBadgePNG, planBadgeSize = c.style, c.png, c.size
			if err := checkBadgeStyle(); (err != nil) != c.wantErr {
				t.Errorf("checkBadgeStyle() = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// TestUploadBadge: --upload puts the SVG and the PNG in the bead's badge
// directory under images/, the tree the viewer image proxy serves (the old
// <prefix>/badges/ PNG key was outside it, so its URL never opened).
func TestUploadBadge(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("COOP_S3_REGION", "us-east-1")
	t.Setenv("BEADS_VIEWER_URL", "https://viewer.example")

	fake := &fakeUploader{}
	withFakeUploader(t, fake)

	bdg := badge.Tier0("ka-0000007031", "Every gardenio plan")
	raster, err := bdg.Rasterize(128)
	if err != nil {
		t.Fatalf("rasterize: %v", err)
	}
	up, err := uploadBadge(context.Background(), "ka-0000007031", bdg, raster)
	if err != nil {
		t.Fatalf("uploadBadge: %v", err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("PutObject called %d times, want 2 (svg, png)", len(fake.puts))
	}
	svgPut, pngPut := fake.puts[0], fake.puts[1]
	for _, c := range []struct {
		put           putCall
		key, ct, tail string
		body          []byte
	}{
		{svgPut, up.SVGKey, "image/svg+xml", "-tier0.svg", []byte(bdg.SVG)},
		{pngPut, up.PNGKey, "image/png", "-tier0-128.png", raster.PNG},
	} {
		if c.put.Bucket != "test-bucket" {
			t.Errorf("bucket = %q", c.put.Bucket)
		}
		if c.put.ContentType != c.ct {
			t.Errorf("content-type = %q, want %s", c.put.ContentType, c.ct)
		}
		if c.put.Key != c.key {
			t.Errorf("uploaded key %q != returned key %q", c.put.Key, c.key)
		}
		if !bytes.Equal(c.put.Body, c.body) {
			t.Errorf("%s: uploaded body is not the badge bytes", c.key)
		}
		if !strings.HasPrefix(c.key, "coop/sessions/images/badges/ka-0000007031/") || !strings.HasSuffix(c.key, c.tail) {
			t.Errorf("key = %q, want coop/sessions/images/badges/ka-0000007031/<sha8>%s", c.key, c.tail)
		}
	}
	if up.SVGURL != "https://viewer.example/image/"+up.SVGKey || up.PNGURL != "https://viewer.example/image/"+up.PNGKey {
		t.Errorf("URLs must be the viewer image proxy's: svg=%q png=%q", up.SVGURL, up.PNGURL)
	}
	if !strings.Contains(up.PNGStorageURL, "test-bucket") {
		t.Errorf("storageURL = %q", up.PNGStorageURL)
	}
}

func TestUploadBadge_NoBucket(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	bdg := badge.Tier0("gd-x", "T")
	raster, _ := bdg.Rasterize(64)
	if _, err := uploadBadge(context.Background(), "gd-x", bdg, raster); err == nil {
		t.Error("expected an error when COOP_S3_BUCKET is unset")
	}
}

// TestEmitPlanBadgePNG_UploadWritesFields is gd-NyYxIxxbuw's write-back: gb
// badge --upload on a task (not a plan) sets icon_svg_url and icon_png_url on
// it, through the daemon, to the uploaded files' viewer URLs, and sends no
// other field.
func TestEmitPlanBadgePNG_UploadWritesFields(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("BEADS_VIEWER_URL", "https://viewer.example")
	fake := &fakeUploader{}
	withFakeUploader(t, fake)

	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/beads/gd-task1" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPatch {
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			patched = body.Fields
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-task1", "type": "task", "fields": map[string]any{"operated_by": "x"}})
	}))
	t.Cleanup(srv.Close)
	installBadgeTestDaemon(t, srv.URL)

	resetBadgeFlags()
	planBadgeUpload = true
	t.Cleanup(resetBadgeFlags)

	bdg := badge.Tier0("gd-task1", "A task")
	if err := emitPlanBadgePNG(newBadgeTestCmd(), "gd-task1", "A task", bdg, badge.Arms{}); err != nil {
		t.Fatalf("emitPlanBadgePNG --upload: %v", err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("want the svg and the png uploaded, got %d puts", len(fake.puts))
	}
	want := map[string]any{
		"icon_svg_url": "https://viewer.example/image/" + fake.puts[0].Key,
		"icon_png_url": "https://viewer.example/image/" + fake.puts[1].Key,
	}
	if !reflect.DeepEqual(patched, want) {
		t.Fatalf("PATCH fields = %v, want %v", patched, want)
	}
}

// TestEmitPlanBadgePNG_UploadFieldWriteFails: a refused write is an error that
// names the fields, after the files are up.
func TestEmitPlanBadgePNG_UploadFieldWriteFails(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	fake := &fakeUploader{}
	withFakeUploader(t, fake)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			http.Error(w, `{"error":"icon_png_url: unknown field"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-d1", "type": "decision"})
	}))
	t.Cleanup(srv.Close)
	installBadgeTestDaemon(t, srv.URL)

	resetBadgeFlags()
	planBadgeUpload = true
	t.Cleanup(resetBadgeFlags)

	err := emitPlanBadgePNG(newBadgeTestCmd(), "gd-d1", "D", badge.Tier0("gd-d1", "D"), badge.Arms{})
	if err == nil || !strings.Contains(err.Error(), "icon_svg_url and icon_png_url") {
		t.Fatalf("want an error naming the fields, got %v", err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("the files go up before the write, got %d puts", len(fake.puts))
	}
}
