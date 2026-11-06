// Package runnerreg holds this machine's gardenio runner registration and the
// client for the daemon's runner routes (local-agents spec §4.2, §5).
//
// A registration is two files under Dir():
//
//	runner.json   the runner bead id, owner, daemon URL and project allowlist
//	runner.token  the gr_ bearer, returned once by POST /v1/runners/register
//
// Both are mode 0600. A third, runner.left, is touched by `gb runner leave`
// (MarkLeft) so attached joins stop heartbeating the runner online. The token is kept out of runner.json so the
// registration can be shown or copied without leaking the secret.
package runnerreg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotRegistered is returned by Load when runner.json is absent.
	ErrNotRegistered = errors.New("this machine is not registered; run gb runner register --invite <code>")
	// ErrNoHome is returned by Dir when neither GARDENIO_HOME nor a home
	// directory is set. There is no fallback: a shared temp dir could hold a
	// planted symlink that redirects where the token is written or read.
	ErrNoHome = errors.New("HOME is not set; set GARDENIO_HOME to a private directory for the runner registration")
	// ErrTokenRejected is returned for a 401 (unknown or revoked token) or a
	// 409 (revoked runner) from a runner route. Spec §10: no silent retry.
	ErrTokenRejected = errors.New("token revoked or expired; ask for a new invite")
)

// Runner is the non-secret part of a registration, stored in runner.json.
type Runner struct {
	RunnerID  string   `json:"runner_id"`
	Hostname  string   `json:"hostname"`
	Owner     string   `json:"owner"`
	DaemonURL string   `json:"daemon_url"`
	Projects  []string `json:"projects"`
	Tags      []string `json:"tags"`
}

// Dir is the directory that holds the registration: $GARDENIO_HOME, or
// ~/.config/gardenio. It returns ErrNoHome when neither is available.
func Dir() (string, error) {
	if d := os.Getenv("GARDENIO_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(home, ".config", "gardenio"), nil
}

// Save writes runner.json and runner.token, both mode 0600, replacing any
// earlier registration.
func Save(r Runner, token string) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save registration in %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile0600(filepath.Join(dir, "runner.token"), []byte(token)); err != nil {
		return fmt.Errorf("save registration in %s: %w", dir, err)
	}
	if err := writeFile0600(filepath.Join(dir, "runner.json"), raw); err != nil {
		return fmt.Errorf("save registration in %s: %w", dir, err)
	}
	return nil
}

// writeFile0600 writes data to path and forces mode 0600, which os.WriteFile
// alone does not do for a file that already exists.
func writeFile0600(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Load reads the registration. It returns ErrNotRegistered when runner.json
// is absent, and refuses one without a runner_id: join would then start the
// agent without --runner, outside the runner's token handling.
func Load() (Runner, string, error) {
	dir, err := Dir()
	if err != nil {
		return Runner{}, "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runner.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Runner{}, "", ErrNotRegistered
	}
	if err != nil {
		return Runner{}, "", err
	}
	var r Runner
	if err := json.Unmarshal(raw, &r); err != nil {
		return Runner{}, "", fmt.Errorf("runner.json: %w", err)
	}
	if strings.TrimSpace(r.RunnerID) == "" {
		return Runner{}, "", errors.New("runner.json has no runner_id; run gb runner register --invite <code> again")
	}
	tok, err := os.ReadFile(filepath.Join(dir, "runner.token"))
	if err != nil {
		return Runner{}, "", fmt.Errorf("runner.token: %w", err)
	}
	return r, strings.TrimSpace(string(tok)), nil
}

// MarkLeft touches runner.left, recording that `gb runner leave` ran now.
func MarkLeft() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return writeFile0600(filepath.Join(dir, "runner.left"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
}

// LeftSince reports whether MarkLeft ran at or after t. A missing or
// unreadable marker counts as not left.
func LeftSince(t time.Time) bool {
	dir, err := Dir()
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "runner.left"))
	return err == nil && !info.ModTime().Before(t)
}

// Client calls the daemon's runner routes. Token is the runner bearer; it is
// empty for Register, which authenticates with the invite code.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c Client) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusConflict {
		return ErrTokenRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s %s: %s", method, path, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Register redeems inviteCode (POST /v1/runners/register). The token is
// returned once; the caller must Save it.
func (c Client) Register(ctx context.Context, inviteCode, hostname, osArch string, tags []string) (Runner, string, error) {
	var resp struct {
		RunnerID string   `json:"runner_id"`
		Token    string   `json:"token"`
		Owner    string   `json:"owner"`
		Projects []string `json:"projects"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/runners/register", map[string]any{
		"invite_code": inviteCode, "hostname": hostname, "os_arch": osArch, "tags": tags,
	}, &resp); err != nil {
		if errors.Is(err, ErrTokenRejected) {
			// Register carries no bearer, so a 401/409 here is about the
			// invite, not a token.
			return Runner{}, "", errors.New("POST /v1/runners/register: the daemon refused the invite")
		}
		return Runner{}, "", err
	}
	if resp.RunnerID == "" || resp.Token == "" {
		return Runner{}, "", errors.New("POST /v1/runners/register: response has no runner_id or token")
	}
	return Runner{RunnerID: resp.RunnerID, Hostname: hostname, Owner: resp.Owner, DaemonURL: c.BaseURL, Projects: resp.Projects, Tags: tags}, resp.Token, nil
}

// Heartbeat marks runnerID online (state "") or away
// (POST /v1/runners/{id}/heartbeat). A 401 or 409 is ErrTokenRejected.
func (c Client) Heartbeat(ctx context.Context, runnerID, state string) error {
	var body any
	if state != "" {
		body = map[string]string{"state": state}
	}
	return c.do(ctx, http.MethodPost, "/v1/runners/"+url.PathEscape(runnerID)+"/heartbeat", body, nil)
}

// Get returns the runner bead as the daemon serves it (GET /v1/runners/{id}).
func (c Client) Get(ctx context.Context, runnerID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/v1/runners/"+url.PathEscape(runnerID), nil, &out)
	return out, err
}
