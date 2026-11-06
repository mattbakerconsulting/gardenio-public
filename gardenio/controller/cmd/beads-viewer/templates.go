package main

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html templates/partials/*.html
var templateFS embed.FS

//go:embed templates/static
var staticFS embed.FS

// vendoredJS are the upstream libraries fetch-vendored.sh downloads into
// templates/static at build time. They are gitignored, not committed
// (gd-wNQzR9ptZI), so a build that skipped the script compiles fine but
// embeds neither: diagrams stay code blocks and /dashboards has no charts.
var vendoredJS = []string{"mermaid.min.js", "echarts.min.js"}

// missingVendoredJS returns the vendoredJS files this binary did not embed.
func missingVendoredJS() []string {
	var missing []string
	for _, name := range vendoredJS {
		if _, err := fs.Stat(staticFS, "templates/static/"+name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}
