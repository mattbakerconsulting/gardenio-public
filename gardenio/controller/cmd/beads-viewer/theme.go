package main

import (
	"mime"
	"strings"
)

// The viewer look (S1j, gd-cxU8eyiGyR). themeMeadow is the Dawn Meadow scene
// the login page ships (S1h, gd-0SYmLWCCM5): the same tiles, tokens and pixel
// sign, served from templates/static/meadow/ and switched on by
// html[data-theme="meadow"] in layout.html and login.html. themePlain is the
// pre-theme look, byte-for-byte: no attribute, no stylesheet, no scene markup.
//
// The kill switch is the BEADS_VIEWER_THEME env (chart value
// beadsViewer.theme): "plain" restores the old look at runtime; anything else,
// including unset, is the meadow, so rollback is a value like S1f's and never
// needs an image.
const (
	themeMeadow = "meadow"
	themePlain  = "plain"
)

// themeFromEnv maps the BEADS_VIEWER_THEME value onto a theme name. Only an
// explicit "plain" (any case, surrounding space ignored) turns the scene off;
// unset, "meadow" and unknown values all mean the default look, so a typo in
// a deploy value can never silently strip the product's face.
func themeFromEnv(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), themePlain) {
		return themePlain
	}
	return themeMeadow
}

func init() {
	// Go's built-in table has no entry for woff2, so the embedded pixel font
	// would otherwise be sniffed as application/octet-stream. Browsers load it
	// either way; the right type keeps proxies and audits honest.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}
