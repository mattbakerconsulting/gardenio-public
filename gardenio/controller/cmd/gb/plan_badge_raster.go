// Raster (PNG) output + content-addressed S3 upload for `gb badge`.
//
// SVG is the badge's source of truth (plan_badge.go); this file derives a PNG
// for the consumers that cannot render SVG inline — Slack unfurls, GitLab
// MR/Jira <img>, OpenGraph cards, favicons. Rasterization is pure-Go
// (badge.Rasterize -> oksvg/rasterx); see internal/badge/raster.go and
// docs/badge-art-spec.md (Raster) for the toolchain choice and its cost/benefit.
//
// The upload reuses the exact S3 bucket/prefix and viewer-proxy plumbing that
// `gb image upload` already uses, so the badge's SVG and PNG land in the same
// private bucket, under images/ where the viewer image proxy serves them to a
// logged-in human, and their URLs are written onto the bead (icon_svg_url,
// icon_png_url; gd-NyYxIxxbuw). Each S3 key is content-addressed on the file's
// SHA-256, so re-uploading an unchanged badge is idempotent — the same bytes
// map to the same key, and re-upload is a no-op overwrite of identical content.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
)

// emitPlanBadgePNG rasterizes the badge and either writes it to --out, uploads
// it to the content-addressed S3 badge cache (--upload), or writes the PNG
// bytes to stdout. With --json it prints structured metadata (content address,
// size, tier, arms, and the upload URLs when --upload is set).
func emitPlanBadgePNG(cmd *cobra.Command, id, title string, bdg badge.Badge, arms badge.Arms) error {
	size := planBadgeSize
	if size <= 0 {
		size = badge.DefaultRasterSize
	}
	raster, err := bdg.Rasterize(size)
	if err != nil {
		return fmt.Errorf("rasterizing badge for %s: %w", id, err)
	}

	// Write to a file when asked; the file path is the deliverable, so stdout
	// stays clean for scripting.
	if planBadgeOut != "" {
		if err := os.WriteFile(planBadgeOut, raster.PNG, 0o644); err != nil {
			return fmt.Errorf("writing PNG to %s: %w", planBadgeOut, err)
		}
		if !planBadgeUpload {
			if jsonOutput {
				printJSON(planBadgeRasterMeta(id, title, bdg, arms, raster, badgeUpload{}))
				return nil
			}
			fmt.Fprintf(os.Stderr, "wrote %d-byte PNG (%dx%d, sha256=%s) to %s\n",
				len(raster.PNG), raster.Size, raster.Size, raster.SHA256, planBadgeOut)
			return nil
		}
	}

	if planBadgeUpload {
		up, err := uploadBadge(cmd.Context(), id, bdg, raster)
		if err != nil {
			return err
		}
		// The files are up before the bead is touched, so a failed write
		// still leaves URLs worth printing; the error then says the bead
		// was not updated.
		fieldsErr := writeBadgeFields(cmd.Context(), id, up)
		if jsonOutput {
			meta := planBadgeRasterMeta(id, title, bdg, arms, raster, up)
			meta["fields_written"] = fieldsErr == nil
			printJSON(meta)
		} else {
			// Primary (stdout): the viewer-proxied URL — the link a human can open.
			fmt.Println(up.PNGURL)
			fmt.Fprintf(os.Stderr, "svg: %s\n", up.SVGURL)
			fmt.Fprintf(os.Stderr, "storage location (private S3, not human-viewable; use the URL above): %s\n", up.PNGStorageURL)
			if fieldsErr == nil {
				fmt.Fprintf(os.Stderr, "wrote icon_svg_url and icon_png_url onto %s\n", id)
			}
		}
		return fieldsErr
	}

	// No --out, no --upload: emit the raw PNG bytes on stdout so a caller can
	// pipe them (e.g. `gb badge <id> --png > badge.png`).
	if jsonOutput {
		printJSON(planBadgeRasterMeta(id, title, bdg, arms, raster, badgeUpload{}))
		return nil
	}
	if _, err := os.Stdout.Write(raster.PNG); err != nil {
		return fmt.Errorf("writing PNG to stdout: %w", err)
	}
	return nil
}

// planBadgeRasterMeta builds the --json object for a rasterized badge. The
// URL keys are set only when up holds an upload.
func planBadgeRasterMeta(id, title string, bdg badge.Badge, arms badge.Arms, raster badge.RasterResult, up badgeUpload) map[string]any {
	out := map[string]any{
		"id":        id,
		"title":     title,
		"tier":      bdg.Tier,
		"arms":      beadsapi.HeraldryRun(arms.Heraldry),
		"arms_from": arms.From,
		"format":    "png",
		"size":      raster.Size,
		"bytes":     len(raster.PNG),
		"sha256":    raster.SHA256,
	}
	if up.PNGKey != "" {
		out["key"] = up.PNGKey
		out["url"] = up.PNGURL
		out["storage_url"] = up.PNGStorageURL
		out["svg_key"] = up.SVGKey
		out["svg_url"] = up.SVGURL
	}
	return out
}

// badgeUpload is where gb badge --upload put a badge: the S3 key and
// viewer-proxied URL of its SVG and of its PNG, and the PNG's raw (private)
// storage URL.
type badgeUpload struct {
	SVGKey, SVGURL                string
	PNGKey, PNGURL, PNGStorageURL string
}

// uploadBadge uploads the badge's SVG and its rasterized PNG to the same S3
// bucket/prefix as `gb image upload`, beside the cached pictures in
// <prefix>/images/badges/<id>/, so the viewer image proxy serves both.
//
// Key layout: <prefix>/images/badges/<id>/<sha8>-<tier>.svg and
// <sha8>-<tier>-<size>.png
//   - scoped to the bead id so a human browsing S3 finds a bead's badges together;
//   - prefixed with the file's sha8 so identical bytes are idempotent and a
//     redraw produces a NEW object rather than silently overwriting the old
//     one (content-addressing, matching gb image's imageKey). The cache's own
//     objects are named by a full 64-hex key, so the two never collide.
func uploadBadge(ctx context.Context, id string, bdg badge.Badge, raster badge.RasterResult) (badgeUpload, error) {
	bucket := s3ImagesBucket()
	if bucket == "" {
		return badgeUpload{}, fmt.Errorf("COOP_S3_BUCKET is not set — cannot upload badge")
	}
	svg := []byte(bdg.SVG)
	svgSum := sha256.Sum256(svg)
	up := badgeUpload{
		SVGKey: badgeKey(s3Prefix(), id, bdg.Tier+".svg", hex.EncodeToString(svgSum[:])),
		PNGKey: badgeKey(s3Prefix(), id, fmt.Sprintf("%s-%d.png", bdg.Tier, raster.Size), raster.SHA256),
	}
	up.SVGURL = imageProxyURL(up.SVGKey)
	up.PNGURL = imageProxyURL(up.PNGKey)
	up.PNGStorageURL = imageURL(bucket, os.Getenv("COOP_S3_REGION"), up.PNGKey)

	client, err := newS3Uploader(ctx)
	if err != nil {
		return badgeUpload{}, err
	}
	putCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	for _, f := range []struct {
		key, contentType string
		data             []byte
	}{
		{up.SVGKey, "image/svg+xml", svg},
		{up.PNGKey, "image/png", raster.PNG},
	} {
		if _, err := client.PutObject(putCtx, putImageInput(bucket, f.key, f.data, f.contentType)); err != nil {
			return badgeUpload{}, fmt.Errorf("uploading badge to s3://%s/%s: %w", bucket, f.key, err)
		}
	}
	return up, nil
}

// writeBadgeFields writes the uploaded badge's viewer URLs onto the bead, of
// any type, as icon_svg_url and icon_png_url, so the viewer and any other
// reader find the picture on the bead itself.
func writeBadgeFields(ctx context.Context, id string, up badgeUpload) error {
	if err := daemon.UpdateBeadFieldsTyped(ctx, id, map[string]any{
		"icon_svg_url": up.SVGURL,
		"icon_png_url": up.PNGURL,
	}); err != nil {
		return fmt.Errorf("badge uploaded, but writing icon_svg_url and icon_png_url onto %s failed: %w", id, err)
	}
	return nil
}

// badgeKey builds the content-addressed S3 key for an uploaded badge file. It
// mirrors imageKey (sha8 prefix for idempotency) but lives in the bead's badge
// directory (badge.BadgeDir), under the images/ tree the viewer proxy serves.
func badgeKey(prefix, id, basename, sha256hex string) string {
	sha8 := sha256hex
	if len(sha8) > 8 {
		sha8 = sha8[:8]
	}
	return fmt.Sprintf("%s/%s-%s", badge.BadgeDir(prefix, id), sha8, sanitizeBasename(basename))
}
