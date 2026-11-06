package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// badge_image.go — a bead's badge drawn large where there is room (badges plan
// gd-pQZO9ie11D, step 9 gd-5qEMPflr4z).
//
// A badge is the derived picture of a bead's arms that `gb badge --upload`
// stores on any bead as fields.icon_svg_url and fields.icon_png_url
// (gd-NyYxIxxbuw). The arms run stays the mark everywhere small (heraldry spec
// R9); the badge shows only where a large picture belongs: the bead page and
// plan detail headers at 64 px beside the run, og:image on those pages, and
// the cover of a deck card. A bead with no badge shows nothing extra.
//
// Both URLs are read on every render off the bead the page already fetched,
// so an upload shows on the next page load with no viewer restart and no
// extra daemon call.

// badgeImage is a bead's badge as viewer-relative /image/ proxy paths (join
// them to BasePath). SVG is the header and cover picture, crisp at any size;
// PNG is the raster og:image needs. Either may be "" on its own.
type badgeImage struct {
	SVG string
	PNG string
}

// beadBadge returns b's badge, or nil when it has none. Only a URL whose path
// goes through a viewer /image/ proxy is used, and its host is dropped so the
// picture always loads from this viewer: the bucket is private, the proxy is
// the one way a browser can load the bytes, and gb may have written the URL
// with a different viewer host than the one serving the page.
func beadBadge(b *beadsapi.BeadDetail) *badgeImage {
	if b == nil {
		return nil
	}
	img := badgeImage{
		SVG: badgeProxyPath(b.Fields["icon_svg_url"]),
		PNG: badgeProxyPath(b.Fields["icon_png_url"]),
	}
	if img.SVG == "" && img.PNG == "" {
		return nil
	}
	return &img
}

// Src is the picture a page draws: the SVG when stored, else the PNG.
func (b *badgeImage) Src() string {
	if b == nil {
		return ""
	}
	if b.SVG != "" {
		return b.SVG
	}
	return b.PNG
}

// badgeProxyPath maps a stored badge URL (gb writes
// <BEADS_VIEWER_URL>/image/<encoded key>) to "/image/<encoded key>", or "" when
// the URL does not go through the image proxy. The last "/image/" wins, as in
// gb image fetch, so a base path that itself holds "/image/" still resolves.
func badgeProxyPath(raw string) string {
	raw = strings.TrimSpace(raw)
	i := strings.LastIndex(raw, "/image/")
	if i < 0 {
		return ""
	}
	switch prefix := raw[:i]; {
	case prefix == "", strings.HasPrefix(prefix, "/"),
		strings.HasPrefix(prefix, "https://"), strings.HasPrefix(prefix, "http://"):
	default:
		return "" // another scheme (javascript:, data:) is never a proxy URL
	}
	key := raw[i+len("/image/"):]
	if j := strings.IndexAny(key, "?#"); j >= 0 {
		key = key[:j]
	}
	if key == "" || strings.Contains(key, "..") {
		return ""
	}
	return "/image/" + key
}

// badgeOGImage is the absolute og:image URL for b's badge PNG on this viewer,
// or "" when b has none. og:image must be absolute and raster, so the SVG is
// never used here.
func badgeOGImage(r *http.Request, basePath string, b *beadsapi.BeadDetail) string {
	img := beadBadge(b)
	if img == nil || img.PNG == "" {
		return ""
	}
	return viewerBaseFromRequest(r, basePath) + img.PNG
}
