package main

import (
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// loginPickerConfig holds the runtime config for the /login picker page.
// Populated from env vars in main; zero value means "picker disabled".
type loginPickerConfig struct {
	enabled              bool
	forwardAuthURL       string
	keycloakLocalEnabled bool
	keycloakLocalLabel   string
	gitlabEnabled        bool
	gitlabLabel          string
}

// loginPickerConfigFromEnv reads the LOGIN_PICKER_* env vars.
func loginPickerConfigFromEnv() loginPickerConfig {
	return loginPickerConfig{
		enabled:              os.Getenv("LOGIN_PICKER_ENABLED") == "true",
		forwardAuthURL:       os.Getenv("LOGIN_PICKER_FORWARDAUTH_URL"),
		keycloakLocalEnabled: os.Getenv("LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED") == "true",
		keycloakLocalLabel:   envOrDefault("LOGIN_PICKER_KEYCLOAK_LOCAL_LABEL", "Keycloak (local)"),
		gitlabEnabled:        os.Getenv("LOGIN_PICKER_GITLAB_ENABLED") == "true",
		gitlabLabel:          envOrDefault("LOGIN_PICKER_GITLAB_LABEL", "GitLab"),
	}
}

// loginPickerProvider is one row in the picker page's button list.
type loginPickerProvider struct {
	Label string
	Href  template.URL
}

// loginPickerView is the template view model for templates/login.html.
type loginPickerView struct {
	Providers []loginPickerProvider
	// Theme / BasePath mirror layout.html's contract so the picker can carry the
	// Dawn Meadow scene (S1j, gd-cxU8eyiGyR) and find its stylesheet.
	Theme    string
	BasePath string
}

// buildLoginRedirect constructs the oauth2-proxy entry URL for a picker button.
// The redirect target (rd) sends the user back to next (loginNext) after auth,
// else to the viewer root.
// idpHint is the Keycloak IdP alias (e.g. "gitlab"); empty omits the hint so
// the user lands on Keycloak's local username/password form.
func buildLoginRedirect(forwardAuthURL, viewerBase, next, idpHint string) string {
	rd := viewerBase
	if rd == "" {
		rd = "/"
	}
	if !strings.HasSuffix(rd, "/") {
		rd += "/"
	}
	if next != "" { // loginNext: a rooted path under the base path, so it goes on the viewer's origin
		rd = next
		if u, err := url.Parse(viewerBase); err == nil && u.Host != "" {
			rd = u.Scheme + "://" + u.Host + next
		}
	}
	q := url.Values{}
	q.Set("rd", rd)
	if idpHint != "" {
		q.Set("kc_idp_hint", idpHint)
	}
	base := strings.TrimRight(forwardAuthURL, "/")
	return base + "/oauth2/start?" + q.Encode()
}

// viewerBaseFromRequest returns the absolute URL of the viewer root for use
// as the oauth2-proxy `rd` redirect target. Honors X-Forwarded-Proto/Host so
// the URL matches what the user sees in the address bar.
func viewerBaseFromRequest(r *http.Request, basePath string) string {
	scheme := "https"
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	bp := strings.TrimRight(basePath, "/")
	return scheme + "://" + host + bp
}

// loginNext is /login's ?next=, the same-origin path (and query) a sign-in
// returns to, under basePath, or "" for the viewer root. Only a rooted path
// is taken ("/garden?at=gardenio/gd-X"), never "//host" or "/\\host", which a
// browser reads as another origin, so the picker is no open redirect. Before
// this the picker dropped next and every sign-in landed on the root, losing a
// deep link such as /garden?at=<project>/<plan> (gd-kNV6Eos4ON).
func loginNext(r *http.Request, basePath string) string {
	next := strings.TrimSpace(r.URL.Query().Get("next"))
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.ContainsAny(next, "\r\n") {
		return ""
	}
	if bp := strings.TrimRight(basePath, "/"); bp != "" && next != bp && !strings.HasPrefix(next, bp+"/") && !strings.HasPrefix(next, bp+"?") {
		return ""
	}
	return next
}

// handleLogin renders the login picker. When the picker is disabled (or no
// providers are enabled) it 302s straight to oauth2-proxy with no kc_idp_hint
// — preserving the pre-picker behavior.
func (s *Server) handleLogin(cfg loginPickerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base, next := viewerBaseFromRequest(r, s.basePath), loginNext(r, s.basePath)

		providers := make([]loginPickerProvider, 0, 2)
		if cfg.enabled {
			if cfg.keycloakLocalEnabled {
				providers = append(providers, loginPickerProvider{
					Label: cfg.keycloakLocalLabel,
					Href:  template.URL(buildLoginRedirect(cfg.forwardAuthURL, base, next, "")),
				})
			}
			if cfg.gitlabEnabled {
				providers = append(providers, loginPickerProvider{
					Label: cfg.gitlabLabel,
					Href:  template.URL(buildLoginRedirect(cfg.forwardAuthURL, base, next, "gitlab")),
				})
			}
		}

		// Disabled or empty picker → straight pass-through to oauth2-proxy.
		if len(providers) == 0 {
			http.Redirect(w, r, buildLoginRedirect(cfg.forwardAuthURL, base, next, ""), http.StatusFound)
			return
		}

		tmpl, ok := s.pages["login.html"]
		if !ok {
			s.logger.Error("login template not loaded")
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		// Clone before executing: the parsed page set is shared across Server
		// instances (loadTemplateAssets), and html/template forbids Clone
		// after a direct execute — one bare ExecuteTemplate here would poison
		// every later render of the shared login template.
		bound, err := tmpl.Clone()
		if err != nil {
			s.logger.Error("cloning login template", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := bound.ExecuteTemplate(w, "login", loginPickerView{Providers: providers, Theme: s.theme, BasePath: s.basePath}); err != nil {
			if isClientDisconnect(err) {
				s.logger.Debug("login template render aborted: client disconnect", "error", err)
			} else {
				s.logger.Error("login template render error", "error", err)
			}
		}
	}
}
