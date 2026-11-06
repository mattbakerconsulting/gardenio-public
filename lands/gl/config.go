package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gardenio/clicreds/oidclogin"
)

const (
	// defaultEdge is the public edge that carries every land's door.
	defaultEdge = "https://api.clonk.tv"
	// defaultWeb is where an escalation is approved (in the browser, never
	// from gl: design gd-uYNbyW4a9Z section 6 step 7).
	defaultWeb = "https://gardenio.clonk.tv"
)

// landName is the contract's LandName.
var landName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// config is what 'gl use' remembers. It holds no credential: sign-ins live in
// the shared session files (oidclogin.SessionPath), one per land.
type config struct {
	Edge string `json:"edge,omitempty"`
	Land string `json:"land,omitempty"`
	Web  string `json:"web,omitempty"`
}

// configPath is where config lives. A var so tests can move it.
var configPath = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gardenio", "gl.json"), nil
}

func (a *app) config() (*config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	c := &config{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	a.cfg = c
	return c, nil
}

func (a *app) saveConfig(c *config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	a.cfg = c
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// edge is the land door's scheme and host: --edge, GL_EDGE, the remembered
// edge, then defaultEdge. It must be https (plain http only to this machine)
// and carry no path: gl adds /land/<name>/v1 itself.
func (a *app) edge() (string, error) {
	raw := a.edgeFlag
	if raw == "" {
		raw = a.getenv("GL_EDGE")
	}
	if raw == "" {
		c, err := a.config()
		if err != nil {
			return "", err
		}
		raw = c.Edge
	}
	if raw == "" {
		raw = defaultEdge
	}
	return checkEdge(raw)
}

func checkEdge(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("edge %q is not an address like %s", raw, defaultEdge)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("edge %q must be only a scheme and host, like %s", raw, defaultEdge)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("edge %q must use https", raw)
		}
	default:
		return "", fmt.Errorf("edge %q must use https", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// web is where approvals open: --web, GL_WEB, the remembered address, then
// defaultWeb.
func (a *app) web() (string, error) {
	raw := a.webFlag
	if raw == "" {
		raw = a.getenv("GL_WEB")
	}
	if raw == "" {
		c, err := a.config()
		if err != nil {
			return "", err
		}
		raw = c.Web
	}
	if raw == "" {
		raw = defaultWeb
	}
	return checkEdge(raw)
}

// land is the land this command works: --as-tenant, --land, GL_LAND, the
// one 'gl use' remembered, and last the only land there is a sign-in for.
func (a *app) land() (string, error) {
	if a.asTenant != "" && a.landFlag != "" && a.asTenant != a.landFlag {
		return "", fmt.Errorf("--as-tenant %s and --land %s name different lands", a.asTenant, a.landFlag)
	}
	land := a.asTenant
	if land == "" {
		land = a.landFlag
	}
	if land == "" {
		land = a.getenv("GL_LAND")
	}
	if land == "" {
		c, err := a.config()
		if err != nil {
			return "", err
		}
		land = c.Land
	}
	if land == "" {
		lands, err := signedInLands()
		if err != nil {
			return "", err
		}
		switch len(lands) {
		case 0:
			return "", errors.New("no land chosen: run 'gl login --land <name>'")
		case 1:
			land = lands[0]
		default:
			return "", fmt.Errorf("you are signed in to %s: pick one with 'gl use <name>' or --land", strings.Join(lands, ", "))
		}
	}
	if !landName.MatchString(land) {
		return "", fmt.Errorf("%q is not a land name (lower-case letters, digits and dashes)", land)
	}
	return land, nil
}

// signedInLands lists the lands with a cached sign-in, sorted.
func signedInLands() ([]string, error) {
	path, err := oidclogin.SessionPath("")
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "oidc-login-"+oidclogin.LandAudiencePrefix+"*.json"))
	if err != nil {
		return nil, err
	}
	var lands []string
	for _, m := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "oidc-login-"+oidclogin.LandAudiencePrefix), ".json")
		if landName.MatchString(name) {
			lands = append(lands, name)
		}
	}
	return lands, nil // Glob returns names sorted
}

// baseURL is the land's door: https://<edge>/land/<land>/v1. The contract's
// paths (/lands/v1/lands/<land>/...) go after it.
func baseURL(edge, land string) string {
	return edge + "/land/" + land + "/v1"
}

// approvalURL is where an escalation is approved, in the browser.
func approvalURL(web, id string) string {
	return web + "/garden?bead=" + url.QueryEscape(id)
}
