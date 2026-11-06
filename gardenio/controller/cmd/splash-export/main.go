// Command splash-export writes the splash (gardenhub's /garden/splash) as a static site for the landing image, the front
// door served on a host other than the app's (plan gd-tLxfoaw366, route gd-Z3eh4eEJ87): index.html, three.module.min.js
// and still.png, the assets by relative URL and Request an invite and Sign in absolute on the app's origin, so the page
// needs nothing from beads-viewer.
//
// Usage:
//
//	splash-export -out dist -app-origin https://gardenio.clonk.tv
package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"

	"gardenio/controller/internal/gardenhub"
)

func main() {
	out := flag.String("out", "splash", "directory to write index.html, three.module.min.js and still.png to")
	origin := flag.String("app-origin", "", "the app's origin the links go to, e.g. https://gardenio.clonk.tv (required)")
	flag.Parse()
	if err := run(*out, *origin); err != nil {
		fmt.Fprintln(os.Stderr, "splash-export:", err)
		os.Exit(1)
	}
}

func run(out, origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("-app-origin %q: want an origin such as https://gardenio.clonk.tv", origin)
	}
	return gardenhub.ExportSplash(out, u.Scheme+"://"+u.Host)
}
