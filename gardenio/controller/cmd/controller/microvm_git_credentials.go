// MicroVM per-run git credentials (task ka-0000004762, epic ka-0000003545).
//
// The Kubernetes pod path delivers git credentials two ways an AgentSpec never
// carries: GIT_USERNAME/GIT_TOKEN/GITLAB_TOKEN Secret env-refs on the agent
// container, and the projected /etc/git-creds volume on init-clone. Neither
// exists in a Lambda MicroVM, so a satellite had NO git credential (nfd!15
// deferred: production credential rotation and revocation).
//
// This file closes that gap with two modes, mirroring the per-agent beads
// token duality in microvm_credentials.go:
//
//   - Minting mode (MICROVM_GIT_FACTORY_TOKEN set): mint a per-run,
//     project-scoped GitLab project access token at launch
//     (gardenio-microvm-<beadID>, read_repository+write_repository+api,
//     Developer, expires_at = +2 days as the TTL backstop), and revoke it
//     eagerly on all four exit paths via microvmCredentialRevoker: normal
//     termination, timeout (reconcile terminal-close), failed launch, and leak
//     GC. The api scope lets an in-VM agent finalize its own MR (set the
//     description/PROOF block, un-draft, merge — ka-0000007908); it is
//     project-scoped so those verbs cannot reach any other repo.
//   - Shared fallback (factory unset): resolve the SAME per-project K8s
//     Secrets the pod path projects (GitlabTokenSecret key "token",
//     GitCredentialsSecret keys "username"/"token") and deliver their values.
//     The credential is shared, so revocation is delivery-vehicle-only (the
//     per-run Secrets Manager document is deleted with the runtime).
//
// Either way the values ride ONLY the per-run task-scoped Secrets Manager
// document — never the run-hook payload, never logs (the least-privilege
// invariant in internal/microvm/runtime.go). In-VM consumption needs no image
// change: `gb agent start` already reads GITLAB_TOKEN / GIT_USERNAME /
// GIT_TOKEN from process env (which the image materializes from the runtime
// document) and writes ~/.git-credentials for the hosts in BOAT_PROJECTS.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// microvmGitTokenScopes are the scopes minted onto every per-run project
// access token:
//
//   - read_repository/write_repository: git-over-https clone/push.
//   - api: the /api/v4 verbs an in-VM agent needs to FINALIZE its own MR —
//     set the multi-line description (so the required PROOF block lands),
//     un-draft, and merge (ka-0000007908). Without it a satellite could push
//     a branch but every MR create/update/merge returned HTTP 403
//     insufficient_scope, breaking the self-merge contract (advice
//     ka-0000002230) for microVM agents. GitLab has no MR-only project-token
//     scope, so api is the narrowest scope that unblocks MR finalization.
//
// This stays narrowly scoped along every OTHER axis that matters: the token is
// project-scoped (one project, api verbs cannot reach any other repo),
// Developer access level (cannot touch protected refs or project settings), TTL
// +2 days, and eagerly revoked on all four exit paths.
var microvmGitTokenScopes = []string{"read_repository", "write_repository", "api"}

// microvmGitTokenAccessLevel is the GitLab access level of minted tokens:
// 30 = Developer (push non-protected branches; cannot touch protected refs or
// project settings).
const microvmGitTokenAccessLevel = 30

// microvmGitTokenExpiryDays is the expires_at horizon for minted tokens.
// GitLab expiry has date granularity and tokens die at 00:00 UTC of the
// expires_at date, so +2 days guarantees >=24h of validity even for a launch
// at 23:59 — the same order of backstop as the beads token's 24h TTL. Eager
// revocation on the four exit paths is the primary mechanism; this only
// bounds the damage when every revoke path failed (controller gone for good).
const microvmGitTokenExpiryDays = 2

// Bead fields persisted at mint time so revocation survives a controller
// restart: the numeric token id and the project path are the DELETE
// coordinates, and neither is secret (the token VALUE is never persisted
// anywhere but the per-run Secrets Manager document).
const (
	fieldMicrovmGitTokenID = "microvm_git_token_id"
	fieldMicrovmGitProject = "microvm_git_project"
)

// microvmGitHTTPClient is shared by every minter instance (one per spawn
// event) so connection reuse works across launches.
var microvmGitHTTPClient = &http.Client{Timeout: 30 * time.Second}

// microvmGitMinter mints and revokes per-run GitLab project access tokens
// with the factory token. Nil when minting is not configured.
type microvmGitMinter struct {
	baseURL string // e.g. https://gitlab.com (no trailing slash)
	factory string // privileged token (api scope, Maintainer); never logged
	hc      *http.Client
	logger  *slog.Logger
}

// newMicrovmGitMinter builds a minter from config, or nil when the factory
// token is unset (shared-fallback mode).
func newMicrovmGitMinter(cfg *config.Config, logger *slog.Logger) *microvmGitMinter {
	if cfg == nil || strings.TrimSpace(cfg.MicrovmGitFactoryToken) == "" {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.MicrovmGitLabBaseURL), "/")
	if base == "" {
		base = "https://gitlab.com"
	}
	return &microvmGitMinter{
		baseURL: base,
		factory: strings.TrimSpace(cfg.MicrovmGitFactoryToken),
		hc:      microvmGitHTTPClient,
		logger:  logger,
	}
}

// host returns the bare host of the minter's GitLab instance, for matching
// against a project's GitURL (the minter can only cover repos on its own
// instance).
func (m *microvmGitMinter) host() string {
	u, err := url.Parse(m.baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// mintedGitToken is one successfully minted per-run token: the secret value
// (delivered only via the per-run document) plus the non-secret revocation
// coordinates (persisted onto the agent bead).
type mintedGitToken struct {
	Token       string // secret — never logged, never persisted
	TokenID     int64
	ProjectPath string // e.g. "acme/platform/gardenio"
}

// Mint creates a project access token named for the agent bead. The name is
// deterministic on the bead ID so a duplicate spawn event's second mint is
// discoverable/attributable, and CloudTrail-style audit in GitLab reads
// straight back to the run.
func (m *microvmGitMinter) Mint(ctx context.Context, projectPath, beadID string) (mintedGitToken, error) {
	body, err := json.Marshal(map[string]any{
		"name":         "gardenio-microvm-" + beadID,
		"scopes":       microvmGitTokenScopes,
		"access_level": microvmGitTokenAccessLevel,
		"expires_at":   time.Now().UTC().AddDate(0, 0, microvmGitTokenExpiryDays).Format("2006-01-02"),
	})
	if err != nil {
		return mintedGitToken{}, fmt.Errorf("marshal mint request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/access_tokens", m.baseURL, url.PathEscape(projectPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return mintedGitToken{}, err
	}
	req.Header.Set("PRIVATE-TOKEN", m.factory)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.hc.Do(req)
	if err != nil {
		return mintedGitToken{}, fmt.Errorf("mint project token for %s: %w", projectPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		// Read a bounded error body for diagnosis; GitLab error bodies carry no
		// credential material.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return mintedGitToken{}, fmt.Errorf("mint project token for %s: HTTP %d: %s", projectPath, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return mintedGitToken{}, fmt.Errorf("decode mint response for %s: %w", projectPath, err)
	}
	if out.Token == "" || out.ID == 0 {
		return mintedGitToken{}, fmt.Errorf("mint response for %s missing token/id", projectPath)
	}
	return mintedGitToken{Token: out.Token, TokenID: out.ID, ProjectPath: projectPath}, nil
}

// Revoke deletes a minted project access token. Idempotent: a 404 (already
// revoked, or the project/token is gone) is success, so every exit path can
// call it freely and retries never warn-spam.
func (m *microvmGitMinter) Revoke(ctx context.Context, projectPath string, tokenID int64) error {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/access_tokens/%d", m.baseURL, url.PathEscape(projectPath), tokenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", m.factory)
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("revoke project token %d for %s: %w", tokenID, projectPath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("revoke project token %d for %s: HTTP %d: %s", tokenID, projectPath, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// gitlabProjectPath extracts the URL-decodable project path ("group/sub/repo")
// from an https clone URL on the given host, or "" when the URL is not an
// https URL on that host. The .git suffix is stripped.
func gitlabProjectPath(gitURL, host string) string {
	gitURL = strings.TrimSpace(gitURL)
	const scheme = "https://"
	if host == "" || !strings.HasPrefix(gitURL, scheme) {
		return ""
	}
	rest := gitURL[len(scheme):]
	slash := strings.Index(rest, "/")
	if slash < 0 || rest[:slash] != host {
		return ""
	}
	path := strings.Trim(rest[slash+1:], "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || !strings.Contains(path, "/") {
		return "" // a bare group or empty path is not a project
	}
	return path
}

// microvmGitCredential is the resolved git credential set for one satellite:
// the values that ride the per-run document, plus the revocation coordinates
// when a per-run token was minted (zero when shared/none).
type microvmGitCredential struct {
	GitlabToken string // GITLAB_TOKEN env (minted or shared)
	GitUsername string // GIT_USERNAME env (shared GitHub credentials)
	GitToken    string // GIT_TOKEN env

	// Minted-token revocation coordinates; TokenID==0 means nothing was minted
	// (shared mode or resolution failure) and there is nothing to revoke.
	TokenID     int64
	ProjectPath string
}

// resolveMicrovmGitCredentials sources the git credential set for a satellite.
//
// Minting mode (minter non-nil) is authoritative for GitLab: a mint failure
// delivers NO GitLab token (visible degradation) rather than silently widening
// to the shared credential. GitHub credentials (GitCredentialsSecret) are
// outside the minter's reach and resolve from the shared secret in both modes.
// Reference repos on the same GitLab host will NOT authenticate with the
// minted project-scoped token (one token per host in the in-VM credential
// store); that widening is deliberate follow-up work, logged at launch.
// Every branch degrades gracefully — a satellite with no git credential still
// launches (matches the beads/Claude resolution discipline in
// microvm_credentials.go). No credential VALUE is ever logged.
func resolveMicrovmGitCredentials(ctx context.Context, logger *slog.Logger, minter *microvmGitMinter, k8sClient kubernetes.Interface, spec podmanager.AgentPodSpec) microvmGitCredential {
	var cred microvmGitCredential

	// Shared GitHub credentials (username+token) when the project declares them
	// — the pod path projects the same secret at /etc/git-creds/github/.
	if spec.GitCredentialsSecret != "" {
		cred.GitUsername = readSecretKey(ctx, logger, k8sClient, spec.Namespace, spec.GitCredentialsSecret, "username", spec.BeadID)
		cred.GitToken = readSecretKey(ctx, logger, k8sClient, spec.Namespace, spec.GitCredentialsSecret, "token", spec.BeadID)
	}

	if minter != nil {
		projectPath := gitlabProjectPath(spec.GitURL, minter.host())
		if projectPath == "" {
			logger.Warn("lambda_microvm: git minting configured but project GitURL is not on the minter's GitLab host; no GitLab token delivered",
				"bead", spec.BeadID, "project", spec.Project, "git_url", spec.GitURL, "gitlab_host", minter.host())
			return cred
		}
		minted, err := minter.Mint(ctx, projectPath, spec.BeadID)
		if err != nil {
			// Authoritative: no fallback to the shared token — a mint outage must
			// surface as a satellite without git access, not as a silent
			// privilege widening.
			logger.Error("lambda_microvm: per-run git token mint failed; satellite will have no GitLab credential",
				"bead", spec.BeadID, "project_path", projectPath, "error", err)
			return cred
		}
		logger.Info("lambda_microvm: minted per-run git token",
			"bead", spec.BeadID, "project_path", projectPath, "token_id", minted.TokenID)
		if len(spec.ReferenceRepos) > 0 {
			logger.Warn("lambda_microvm: reference repos will not authenticate with the per-run project-scoped token",
				"bead", spec.BeadID, "reference_repos", len(spec.ReferenceRepos))
		}
		cred.GitlabToken = minted.Token
		cred.TokenID = minted.TokenID
		cred.ProjectPath = minted.ProjectPath
		return cred
	}

	// Shared fallback: the project's GitLab token secret, same value the pod
	// path projects at /etc/git-creds/gitlab/token.
	if spec.GitlabTokenSecret != "" {
		cred.GitlabToken = readSecretKey(ctx, logger, k8sClient, spec.Namespace, spec.GitlabTokenSecret, "token", spec.BeadID)
	}
	if cred.GitlabToken == "" && cred.GitToken == "" {
		logger.Warn("lambda_microvm: no git credential resolvable for satellite (no factory, no project git secrets)",
			"bead", spec.BeadID, "project", spec.Project)
	}
	return cred
}

// readSecretKey reads one key of a K8s Secret, returning "" (with a warning)
// on any failure — the same graceful-degradation contract as
// resolveMicrovmClaudeOAuthToken. The VALUE is never logged.
func readSecretKey(ctx context.Context, logger *slog.Logger, k8sClient kubernetes.Interface, namespace, name, key, beadID string) string {
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		logger.Warn("lambda_microvm: git secret not readable",
			"bead", beadID, "secret", name, "key", key, "error", err)
		return ""
	}
	return strings.TrimSpace(string(secret.Data[key]))
}

// applyMicrovmGitCredentials folds the resolved git credential set into the
// runtime-neutral AgentSpec environment, so backend.go writes it ONLY into the
// per-run task-scoped secret. Empty values set no keys (an absent credential
// must not look authoritative). Pure for unit testing.
func applyMicrovmGitCredentials(spec *microvm.AgentSpec, cred microvmGitCredential) {
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	if cred.GitlabToken != "" {
		spec.Environment["GITLAB_TOKEN"] = cred.GitlabToken
	}
	if cred.GitUsername != "" && cred.GitToken != "" {
		spec.Environment["GIT_USERNAME"] = cred.GitUsername
		spec.Environment["GIT_TOKEN"] = cred.GitToken
	}
}

// persistMicrovmGitToken records the minted token's revocation coordinates
// (id + project path — NOT the value) onto the agent bead, so revocation
// survives a controller restart and the leak-GC/timeout paths can find them.
// Best-effort: a write failure is logged and the launch proceeds — the
// expires_at backstop bounds an unrevokable token.
func persistMicrovmGitToken(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, beadID string, cred microvmGitCredential) {
	if cred.TokenID == 0 {
		return
	}
	err := daemon.UpdateBeadFields(ctx, beadID, map[string]string{
		fieldMicrovmGitTokenID: strconv.FormatInt(cred.TokenID, 10),
		fieldMicrovmGitProject: cred.ProjectPath,
	})
	if err != nil {
		logger.Warn("lambda_microvm: failed to persist git token coordinates (expiry backstops revocation after a controller restart)",
			"bead", beadID, "token_id", cred.TokenID, "project_path", cred.ProjectPath, "error", err)
	}
}

// beadFieldReader is the subset of *beadsapi.Client the revoker reads token
// coordinates through. Narrowed for tests.
type beadFieldReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// microvmCredentialRevoker implements microvm.CredentialRevoker: it is the
// single owner of per-run credential revocation, invoked by the lifecycle on
// normal termination, timeout (terminal reconcile-close) and leak GC, and by
// the spawn door on failed launch. It revokes the per-agent beads token
// (no-op in shared-token mode) and the per-run git token (no-op when nothing
// was minted). Both are idempotent + best-effort — their TTL/expiry is the
// backstop.
type microvmCredentialRevoker struct {
	logger *slog.Logger
	daemon *beadsapi.Client
	cfg    *config.Config
	git    *microvmGitMinter // nil when minting is not configured
	beads  beadFieldReader   // token-coordinate reads; normally == daemon
}

func newMicrovmCredentialRevoker(logger *slog.Logger, daemon *beadsapi.Client, cfg *config.Config) *microvmCredentialRevoker {
	return &microvmCredentialRevoker{
		logger: logger,
		daemon: daemon,
		cfg:    cfg,
		git:    newMicrovmGitMinter(cfg, logger),
		beads:  daemon,
	}
}

// RevokeAgentCredentials revokes every per-run credential minted for the bead.
func (r *microvmCredentialRevoker) RevokeAgentCredentials(ctx context.Context, beadID, reason string) {
	revokeMicrovmBeadsToken(ctx, r.logger, r.daemon, r.cfg, beadID)
	r.revokeGitToken(ctx, beadID, reason)
}

// revokeGitToken looks up the minted token's coordinates from the agent bead
// (works for closed beads — leak GC revokes after the bead closed) and deletes
// it in GitLab. Absent coordinates mean nothing was minted for this run.
func (r *microvmCredentialRevoker) revokeGitToken(ctx context.Context, beadID, reason string) {
	if r.git == nil || r.beads == nil {
		return
	}
	bead, err := r.beads.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		r.logger.Warn("lambda_microvm: cannot read bead for git token revocation (expiry backstops)",
			"bead", beadID, "reason", reason, "error", err)
		return
	}
	idStr := strings.TrimSpace(bead.Fields[fieldMicrovmGitTokenID])
	projectPath := strings.TrimSpace(bead.Fields[fieldMicrovmGitProject])
	if idStr == "" || projectPath == "" {
		return // nothing minted for this run
	}
	tokenID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tokenID <= 0 {
		r.logger.Warn("lambda_microvm: malformed git token id on bead; cannot revoke (expiry backstops)",
			"bead", beadID, "token_id", idStr)
		return
	}
	if err := r.git.Revoke(ctx, projectPath, tokenID); err != nil {
		r.logger.Warn("lambda_microvm: failed to revoke per-run git token (expiry backstops)",
			"bead", beadID, "project_path", projectPath, "token_id", tokenID, "reason", reason, "error", err)
		return
	}
	r.logger.Info("lambda_microvm: revoked per-run git token",
		"bead", beadID, "project_path", projectPath, "token_id", tokenID, "reason", reason)
}
