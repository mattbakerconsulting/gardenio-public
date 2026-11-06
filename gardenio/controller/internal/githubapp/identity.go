package githubapp

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Gardenio runs two GitHub Apps, and a caller names the one it wants (Lands
// Track B spec gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md
// B-D3, step 7.6):
//
//   - Fleet ("gardenio-io") is the operator's own App. Agent pods and the
//     github-bridge mint tokens from it, with the fleet's broad permission set
//     (AgentTokenPermissions) and the bridge's webhook. Strangers never install it.
//   - Lands ("gardenio-lands") is the public App a Lands applicant installs on
//     the one repository their land works on. Its setup URL proves repo control
//     (setup.go) and its tokens carry only LandsTokenPermissions.
//
// Keeping them separate means a stranger's install can never reach the fleet's
// permissions or webhook, and a fleet credential can never be handed to a
// tenant. The App manifests are gardenio/scripts/github-app-register.py
// MANIFESTS ("agents" is Fleet, "lands" is Lands); docs/lands/github-app.md
// says how the operator registers the Lands one.

// AppName names one of gardenio's GitHub Apps.
type AppName string

const (
	Fleet AppName = "fleet"
	Lands AppName = "lands"
)

func (n AppName) known() bool { return n == Fleet || n == Lands }

// ErrNotConfigured is returned for an App this process has no credentials for.
// Callers must treat it as a refusal, never fall back to the other App.
var ErrNotConfigured = errors.New("githubapp: app is not configured")

// LandsTokenPermissions is the Lands App's whole permission set, and the set
// every tenant installation token requests: push a branch, open a pull
// request, comment on issues, read checks. metadata:read is implied by GitHub
// for every App and is listed so the manifest and this set read the same. No
// actions, workflows or packages: a tenant agent cannot rerun or edit CI. A
// fresh map per call.
func LandsTokenPermissions() map[string]string {
	return map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"checks":        "read",
		"metadata":      "read",
	}
}

// TokenPermissions is the permission set MintInstallationToken requests for
// the named App. An unknown name gets nil, which the mint refuses.
func TokenPermissions(name AppName) map[string]string {
	switch name {
	case Fleet:
		return AgentTokenPermissions()
	case Lands:
		return LandsTokenPermissions()
	}
	return nil
}

// Identities holds the Apps one process is configured for, by name.
type Identities struct {
	apps map[AppName]*Client
}

// NewIdentities collects clients by their Name. Nil clients (an App left
// unconfigured) are skipped; two clients with one name are refused.
func NewIdentities(clients ...*Client) (*Identities, error) {
	ids := &Identities{apps: map[AppName]*Client{}}
	for _, c := range clients {
		if c == nil {
			continue
		}
		if _, dup := ids.apps[c.name]; dup {
			return nil, fmt.Errorf("githubapp: two clients for the %s app", c.name)
		}
		ids.apps[c.name] = c
	}
	return ids, nil
}

// App returns the client for name, or ErrNotConfigured.
func (ids *Identities) App(name AppName) (*Client, error) {
	if ids != nil {
		if c, ok := ids.apps[name]; ok {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotConfigured, name)
}

// EnvConfig is an App's settings as read from the environment by
// ConfigFromEnv: <PREFIX>_ID, <PREFIX>_PRIVATE_KEY_PATH, <PREFIX>_CLIENT_ID,
// <PREFIX>_CLIENT_SECRET_PATH, <PREFIX>_SLUG, <PREFIX>_API_URL and
// <PREFIX>_WEB_URL. Secrets are read from files (mounted Secret keys), never
// from the environment, so they do not show in a pod spec or /proc/*/environ.
type EnvConfig struct {
	Config
	Slug string // the App's URL name, for its install page
}

// ConfigFromEnv reads the App named name under prefix (for example
// LANDS_GITHUB_APP). It returns ok=false with no error when nothing at all is
// set (the App is simply off), and an error when only part of it is: a half
// configured App must fail closed, not run with a hole in it. needOAuth makes
// the client id, client secret and slug required too (the Lands App's setup
// leg needs all three).
func ConfigFromEnv(name AppName, prefix string, needOAuth bool, getenv func(string) string, readFile func(string) ([]byte, error)) (EnvConfig, bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	get := func(k string) string { return strings.TrimSpace(getenv(prefix + "_" + k)) }
	required := []string{"ID", "PRIVATE_KEY_PATH"}
	if needOAuth {
		required = append(required, "CLIENT_ID", "CLIENT_SECRET_PATH", "SLUG")
	}
	var set, missing []string
	for _, k := range append(append([]string{}, required...), "CLIENT_ID", "CLIENT_SECRET_PATH", "SLUG") {
		if get(k) != "" {
			set = append(set, k)
		}
	}
	for _, k := range required {
		if get(k) == "" {
			missing = append(missing, prefix+"_"+k)
		}
	}
	if len(set) == 0 {
		return EnvConfig{}, false, nil
	}
	if len(missing) > 0 {
		return EnvConfig{}, false, fmt.Errorf("githubapp: %s app is partly configured; missing %s", name, strings.Join(missing, ", "))
	}
	cfg := EnvConfig{
		Config: Config{
			Name:     name,
			AppID:    get("ID"),
			ClientID: get("CLIENT_ID"),
			BaseURL:  get("API_URL"),
			WebURL:   get("WEB_URL"),
		},
		Slug: get("SLUG"),
	}
	key, err := readFile(get("PRIVATE_KEY_PATH"))
	if err != nil {
		return EnvConfig{}, false, fmt.Errorf("githubapp: %s app private key unreadable at %s", name, get("PRIVATE_KEY_PATH"))
	}
	cfg.PrivateKey = key
	if p := get("CLIENT_SECRET_PATH"); p != "" {
		secret, err := readFile(p)
		if err != nil {
			return EnvConfig{}, false, fmt.Errorf("githubapp: %s app client secret unreadable at %s", name, p)
		}
		cfg.ClientSecret = secret
	}
	return cfg, true, nil
}
