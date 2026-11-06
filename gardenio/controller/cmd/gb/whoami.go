package main

// whoami.go implements `gb whoami` — one read-only command that answers
// "what can I actually do?" from wherever it runs (agent pod or operator
// laptop): AWS STS identity + assumable targets, the Kubernetes service
// account + the handful of `auth can-i` probes that matter, the GitLab user +
// per-repo access level, and which Secrets-Manager prefixes are readable.
//
// Motivation (feature ka-0000003630, escalation deep-dive bundle
// ka-0000002006): across 641 real escalations, 60 showed agents enumerating
// credential sets one-by-one to rediscover a constant, and 83 narrated a
// live-verified denial before escalating — real token spend that still
// produced wrong answers. The canonical miss (ka-0000003844) verified the AWS
// IAM access-entry was absent, concluded kubectl was unavailable, and was
// wrong: gardenio-project pods authenticate to Kubernetes via a projected
// ServiceAccount token, NOT the AWS identity. Two auth paths, one probed. This
// command probes BOTH (and the rest) once, so the escalate/do-not-escalate
// decision has a correct input.
//
// Contract:
//   - Read-only and offline-first. It overrides the root PersistentPreRunE so
//     it never dials the beads daemon — capability introspection must work even
//     when the daemon is down or the token is unset (mirrors `gb version`).
//   - Every section is fail-open and independent: a missing binary, a denied
//     API call, or a timeout records an `error` on that section and the rest
//     still render. `gb whoami` never exits non-zero for a probe failure.
//   - Nothing is hardcoded per-cluster: identity is read from the live process
//     environment + the tools' own responses, so the same binary reports
//     correctly across sessions, pods, and the operator's machine (AC #2).
//   - Active credential minting (STS AssumeRole) is opt-in behind --probe.
//     The default run is pure reads: sts get-caller-identity, kubectl auth
//     whoami / can-i, glab api /user + project permissions, and
//     secretsmanager list-secrets.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/jqout"

	"github.com/spf13/cobra"
)

// whoamiProbeTimeout bounds each external probe (aws/kubectl/glab). Kept short:
// these are single API round-trips and a hung probe must not stall the whole
// report. Each section runs concurrently, so wall-clock is ~one timeout, not
// the sum.
const whoamiProbeTimeout = 20 * time.Second

// whoamiSecretsMaxPages bounds the Secrets-Manager list-secrets pagination walk
// (100 secrets/page) so a very large account can't stall the report. 20 pages =
// up to 2000 secrets, ample to characterise which prefixes are readable.
const whoamiSecretsMaxPages = 20

// ── report shapes ───────────────────────────────────────────────────

// whoamiReport is the top-level structured answer. Every section carries its
// own Error string so JSON consumers can tell "not permitted / tool absent"
// apart from "empty".
type whoamiReport struct {
	GeneratedAt string        `json:"generated_at"`
	Pod         whoamiPod     `json:"pod"`
	AWS         whoamiAWS     `json:"aws"`
	Kubernetes  whoamiK8s     `json:"kubernetes"`
	GitLab      whoamiGitLab  `json:"gitlab"`
	Secrets     whoamiSecrets `json:"secrets"`
	LLM         *whoamiLLM    `json:"llm,omitempty"`
}

// whoamiPod is the session/pod context read straight from the environment —
// the "not hardcoded, captures pod environment" evidence (AC #2).
type whoamiPod struct {
	Agent     string `json:"agent,omitempty"`
	Role      string `json:"role,omitempty"`
	Project   string `json:"project,omitempty"`
	Account   string `json:"account,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
}

type whoamiAWS struct {
	RoleARN              string             `json:"role_arn,omitempty"`
	WebIdentityTokenFile string             `json:"web_identity_token_file,omitempty"`
	Region               string             `json:"region,omitempty"`
	CallerIdentity       *awsCallerIdentity `json:"caller_identity,omitempty"`
	AssumableTargets     []assumableTarget  `json:"assumable_targets"`
	Error                string             `json:"error,omitempty"`
}

type awsCallerIdentity struct {
	UserID  string `json:"UserId"`
	Account string `json:"Account"`
	Arn     string `json:"Arn"`
}

// assumableTarget is a discovered sts:AssumeRole candidate. Status is
// "candidate" until --probe actively verifies it into "allowed" / "denied" /
// "error".
type assumableTarget struct {
	Name    string `json:"name,omitempty"`
	RoleARN string `json:"role_arn"`
	Account string `json:"account,omitempty"`
	Source  string `json:"source"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

type whoamiK8s struct {
	ServiceAccount string       `json:"service_account,omitempty"`
	Namespace      string       `json:"namespace,omitempty"`
	Username       string       `json:"username,omitempty"`
	UID            string       `json:"uid,omitempty"`
	Groups         []string     `json:"groups,omitempty"`
	Node           string       `json:"node,omitempty"`
	PodName        string       `json:"pod_name,omitempty"`
	Can            []canIResult `json:"can"`
	Error          string       `json:"error,omitempty"`
}

// canIResult is one `kubectl auth can-i` probe outcome.
type canIResult struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
}

type whoamiGitLab struct {
	User  *gitlabUser  `json:"user,omitempty"`
	Repos []repoAccess `json:"repos"`
	Error string       `json:"error,omitempty"`
}

type gitlabUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Bot      bool   `json:"bot"`
}

// repoAccess is the caller's effective access level on one project, resolved as
// the higher of project-level and group-level membership.
type repoAccess struct {
	Path        string `json:"path"`
	AccessLevel int    `json:"access_level"`
	AccessName  string `json:"access_name"`
	Via         string `json:"via,omitempty"`
	Error       string `json:"error,omitempty"`
}

type whoamiSecrets struct {
	Prefixes  []secretPrefix `json:"prefixes"`
	Secrets   []secretEntry  `json:"secrets"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Error     string         `json:"error,omitempty"`
}

// secretPrefix summarises how many readable secrets share a namespace prefix —
// the "which SM prefixes are readable" answer.
type secretPrefix struct {
	Prefix string `json:"prefix"`
	Count  int    `json:"count"`
}

type secretEntry struct {
	Name         string `json:"name"`
	LastAccessed string `json:"last_accessed,omitempty"`
}

type whoamiLLM struct {
	BaseURL   string   `json:"base_url,omitempty"`
	Spend     *float64 `json:"spend,omitempty"`
	MaxBudget *float64 `json:"max_budget,omitempty"`
	ResetAt   string   `json:"budget_reset_at,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// ── command ─────────────────────────────────────────────────────────

var whoamiCmd = &cobra.Command{
	Use:     "whoami",
	Short:   "Show what you can actually do (AWS, K8s, GitLab, secrets) in one read-only block",
	GroupID: "session",
	Long: `whoami answers "what can I actually do?" from wherever it runs — an agent
pod or an operator laptop — in one read-only block:

  - AWS:     sts get-caller-identity (role/account) + discovered assume-role targets
  - K8s:     the ServiceAccount + the auth can-i probes that matter
             (get pods -A, create jobs, read pod logs)
  - GitLab:  the current user + access level on each repo this project touches
  - Secrets: which Secrets-Manager prefixes are readable (+ last-read timestamp)

It probes BOTH the AWS and the Kubernetes auth paths, because gardenio-project
pods authenticate to Kubernetes via a projected ServiceAccount — independent of
the AWS identity. Probing only one path is how a real escalation concluded
"kubectl unavailable" and was wrong.

Every section is independent and fail-open: a denied API call or an absent tool
annotates that section and the rest still print. The default run performs only
read-only calls; pass --probe to additionally verify each assume-role target by
actually attempting sts:AssumeRole (mints short-lived credentials).

  gb whoami                 # human-readable capability block
  gb whoami --json          # structured JSON (machine / prerun body)
  gb whoami --probe         # also actively verify assume-role targets
  gb whoami --jq '.aws.caller_identity.Arn'`,
	// No daemon round-trip: capability introspection must work offline and
	// against any daemon (mirrors `gb version`). Override the root
	// PersistentPreRunE, which otherwise dials the beads daemon.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		// --jq implies --json and must compile before we render, matching the
		// root hook's contract so `gb whoami --jq …` works.
		if jqFilter != "" {
			jsonOutput = true
			if compiledJQ == nil {
				f, err := jqout.Parse(jqFilter)
				if err != nil {
					return err
				}
				compiledJQ = f
			}
		}
		return nil
	},
	RunE: runWhoami,
}

func init() {
	whoamiCmd.Flags().Bool("probe", false, "actively verify assume-role targets by attempting sts:AssumeRole (mints short-lived credentials)")
	rootCmd.AddCommand(whoamiCmd)
}

func runWhoami(cmd *cobra.Command, args []string) error {
	probe, _ := cmd.Flags().GetBool("probe")
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	report := whoamiReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Pod:         collectWhoamiPod(),
	}

	// Each section is independent — run them concurrently so the whole report
	// completes in roughly one probe timeout rather than the sum of four.
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); report.AWS = collectWhoamiAWS(ctx, probe) }()
	go func() { defer wg.Done(); report.Kubernetes = collectWhoamiK8s(ctx) }()
	go func() { defer wg.Done(); report.GitLab = collectWhoamiGitLab(ctx) }()
	go func() { defer wg.Done(); report.Secrets = collectWhoamiSecrets(ctx) }()
	wg.Wait()

	if llm := collectWhoamiLLM(ctx); llm != nil {
		report.LLM = llm
	}

	if jsonOutput {
		printJSON(report)
		return nil
	}
	renderWhoami(os.Stdout, report)
	return nil
}

// ── collectors ──────────────────────────────────────────────────────

func collectWhoamiPod() whoamiPod {
	return whoamiPod{
		Agent:     os.Getenv("BOAT_AGENT"),
		Role:      os.Getenv("BOAT_ROLE"),
		Project:   os.Getenv("BOAT_PROJECT"),
		Account:   os.Getenv("BOAT_ACCOUNT"),
		TaskID:    os.Getenv("BOAT_TASK_ID"),
		Namespace: readK8sNamespace(),
		PodName:   os.Getenv("HOSTNAME"),
	}
}

func collectWhoamiAWS(ctx context.Context, probe bool) whoamiAWS {
	aws := whoamiAWS{
		RoleARN:              os.Getenv("AWS_ROLE_ARN"),
		WebIdentityTokenFile: os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"),
		Region:               firstNonEmpty(os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION")),
		AssumableTargets:     []assumableTarget{},
	}

	out, stderr, err := runProbe(ctx, "aws", "sts", "get-caller-identity", "--output", "json")
	if err != nil {
		aws.Error = probeError("aws sts get-caller-identity", stderr, err)
	} else {
		var id awsCallerIdentity
		if jErr := json.Unmarshal(out, &id); jErr != nil {
			aws.Error = fmt.Sprintf("parsing get-caller-identity: %v", jErr)
		} else {
			aws.CallerIdentity = &id
		}
	}

	// Discover assume-role targets from the environment + AWS config. This is
	// pure parsing (no network) — the current identity's ARN is excluded so we
	// only surface OTHER assumable roles.
	targets := discoverAssumableTargets(aws.RoleARN)
	if probe {
		for i := range targets {
			verifyAssumableTarget(ctx, &targets[i])
		}
	}
	aws.AssumableTargets = targets
	return aws
}

func collectWhoamiK8s(ctx context.Context) whoamiK8s {
	k := whoamiK8s{
		Namespace: readK8sNamespace(),
		Can:       []canIResult{},
	}

	out, stderr, err := runProbe(ctx, "kubectl", "auth", "whoami", "-o", "json")
	if err != nil {
		k.Error = probeError("kubectl auth whoami", stderr, err)
	} else {
		parseKubectlWhoami(out, &k)
	}

	// The can-i probes that actually matter for the escalate/do-not decision:
	// cluster-wide reads, job creation, and log access (the capability the
	// ka-0000003844 miss wrongly assumed absent).
	for _, p := range []struct {
		action string
		args   []string
	}{
		{"get pods (all namespaces)", []string{"auth", "can-i", "get", "pods", "-A"}},
		{"create jobs", []string{"auth", "can-i", "create", "jobs"}},
		{"read pod logs", []string{"auth", "can-i", "get", "pods", "--subresource=log"}},
	} {
		k.Can = append(k.Can, canIResult{Action: p.action, Allowed: kubectlCanI(ctx, p.args)})
	}
	return k
}

func collectWhoamiGitLab(ctx context.Context) whoamiGitLab {
	gl := whoamiGitLab{Repos: []repoAccess{}}

	out, stderr, err := runProbe(ctx, "glab", "api", "/user")
	if err != nil {
		gl.Error = probeError("glab api /user", stderr, err)
	} else {
		var u gitlabUser
		if jErr := json.Unmarshal(out, &u); jErr != nil {
			gl.Error = fmt.Sprintf("parsing /user: %v", jErr)
		} else {
			gl.User = &u
		}
	}

	// Access level on each repo this project touches (from the env the
	// controller injects), so the report answers "and what can I do in each of
	// them" without the agent guessing.
	for _, path := range discoverGitLabProjectPaths() {
		gl.Repos = append(gl.Repos, queryRepoAccess(ctx, path))
	}
	return gl
}

func collectWhoamiSecrets(ctx context.Context) whoamiSecrets {
	s := whoamiSecrets{Prefixes: []secretPrefix{}, Secrets: []secretEntry{}}

	// list-secrets pages 100 at a time; walk NextToken up to a bounded number
	// of pages so the report characterises the FULL readable set (a
	// "which prefixes can I read" answer that silently capped at 100 would be
	// misleading — the ka-0000003555 silent-truncation trap). whoamiSecretsMaxPages
	// bounds the walk so a huge account can't stall the report; Truncated records
	// when the cap was hit.
	prefixCounts := map[string]int{}
	nextToken := ""
	for page := 0; page < whoamiSecretsMaxPages; page++ {
		args := []string{"secretsmanager", "list-secrets", "--max-results", "100", "--output", "json"}
		if nextToken != "" {
			args = append(args, "--next-token", nextToken)
		}
		out, stderr, err := runProbe(ctx, "aws", args...)
		if err != nil {
			// A mid-walk failure still reports what we gathered so far.
			if s.Total == 0 {
				s.Error = probeError("aws secretsmanager list-secrets", stderr, err)
				return s
			}
			s.Error = probeError("aws secretsmanager list-secrets (partial)", stderr, err)
			break
		}
		var resp struct {
			SecretList []struct {
				Name             string `json:"Name"`
				LastAccessedDate string `json:"LastAccessedDate"`
			} `json:"SecretList"`
			NextToken string `json:"NextToken"`
		}
		if jErr := json.Unmarshal(out, &resp); jErr != nil {
			s.Error = fmt.Sprintf("parsing list-secrets: %v", jErr)
			break
		}
		for _, sec := range resp.SecretList {
			s.Secrets = append(s.Secrets, secretEntry{Name: sec.Name, LastAccessed: sec.LastAccessedDate})
			prefixCounts[secretPrefixOf(sec.Name)]++
		}
		s.Total = len(s.Secrets)
		nextToken = resp.NextToken
		if nextToken == "" {
			break
		}
		if page == whoamiSecretsMaxPages-1 {
			s.Truncated = true
		}
	}
	for p, c := range prefixCounts {
		s.Prefixes = append(s.Prefixes, secretPrefix{Prefix: p, Count: c})
	}
	sort.Slice(s.Prefixes, func(i, j int) bool { return s.Prefixes[i].Prefix < s.Prefixes[j].Prefix })
	sort.Slice(s.Secrets, func(i, j int) bool { return s.Secrets[i].Name < s.Secrets[j].Name })
	return s
}

// collectWhoamiLLM is best-effort: it only reports when a LiteLLM key + URL are
// present in the environment. Absent creds return nil (the section is omitted)
// rather than an error — LLM headroom is a bonus, not one of the required
// capability facets.
func collectWhoamiLLM(ctx context.Context) *whoamiLLM {
	base := firstNonEmpty(os.Getenv("LITELLM_URL"), os.Getenv("ANTHROPIC_BASE_URL"))
	key := firstNonEmpty(os.Getenv("LITELLM_MASTER_KEY"), os.Getenv("LITELLM_KEY"))
	if base == "" || key == "" {
		return nil
	}
	llm := &whoamiLLM{BaseURL: base}
	// Reuse the shared /key/info query used by `gb usage`/`gb account`.
	info, err := queryLiteLLMKeyInfo(ctx, &http.Client{Timeout: 10 * time.Second}, base, key, key)
	if err != nil {
		llm.Error = err.Error()
		return llm
	}
	spend := info.Spend
	llm.Spend = &spend
	llm.MaxBudget = info.MaxBudget
	llm.ResetAt = info.BudgetResetAt
	return llm
}

// ── probe helpers ───────────────────────────────────────────────────

// runProbe executes a read-only external command with a bounded timeout,
// returning stdout, stderr, and any error. cmd.Env inherits the process
// environment so the tools pick up the pod's injected credentials.
func runProbe(ctx context.Context, name string, args ...string) (stdout []byte, stderr string, err error) {
	cctx, cancel := context.WithTimeout(ctx, whoamiProbeTimeout)
	defer cancel()

	c := exec.CommandContext(cctx, name, args...)
	c.Env = os.Environ()
	var outBuf, errBuf strings.Builder
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	// Bound Wait() after a timeout kill: a stuck child holding the stdout pipe
	// must not defeat the context deadline (mirrors runPrerunCommand).
	c.WaitDelay = 2 * time.Second
	runErr := c.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, errBuf.String(), fmt.Errorf("timed out after %s", whoamiProbeTimeout)
	}
	return []byte(outBuf.String()), errBuf.String(), runErr
}

// kubectlCanI runs a single `kubectl auth can-i …` probe. can-i exits 0 for
// "yes" and non-zero for "no", printing the verdict on stdout; we treat a
// stdout of "yes" as allowed and everything else (including a tool error) as
// not-allowed. --quiet is intentionally NOT passed so a probe failure is still
// distinguishable in stdout.
func kubectlCanI(ctx context.Context, args []string) bool {
	out, _, _ := runProbe(ctx, "kubectl", args...)
	return strings.TrimSpace(strings.ToLower(string(out))) == "yes"
}

// verifyAssumableTarget actively attempts sts:AssumeRole against a candidate,
// promoting its Status to allowed/denied/error. Only invoked under --probe.
func verifyAssumableTarget(ctx context.Context, t *assumableTarget) {
	_, stderr, err := runProbe(ctx, "aws", "sts", "assume-role",
		"--role-arn", t.RoleARN,
		"--role-session-name", "gb-whoami-probe",
		"--duration-seconds", "900",
		"--output", "json")
	switch {
	case err == nil:
		t.Status = "allowed"
	case strings.Contains(stderr, "AccessDenied") || strings.Contains(stderr, "not authorized"):
		t.Status = "denied"
	default:
		t.Status = "error"
		t.Detail = firstLine(stderr)
	}
}

// ── pure parsing helpers (unit-tested) ──────────────────────────────

// parseKubectlWhoami maps `kubectl auth whoami -o json` (a SelfSubjectReview)
// into the K8s section. The username is the SA; namespace/node/pod come from
// the token's extra attributes.
func parseKubectlWhoami(data []byte, k *whoamiK8s) {
	var review struct {
		Status struct {
			UserInfo struct {
				Username string              `json:"username"`
				UID      string              `json:"uid"`
				Groups   []string            `json:"groups"`
				Extra    map[string][]string `json:"extra"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &review); err != nil {
		k.Error = fmt.Sprintf("parsing auth whoami: %v", err)
		return
	}
	ui := review.Status.UserInfo
	k.Username = ui.Username
	k.UID = ui.UID
	k.Groups = ui.Groups
	// A ServiceAccount username has the shape
	// system:serviceaccount:<ns>:<name>; surface the SA name plainly.
	if sa := strings.TrimPrefix(ui.Username, "system:serviceaccount:"); sa != ui.Username {
		if i := strings.LastIndex(sa, ":"); i >= 0 {
			k.ServiceAccount = sa[i+1:]
			if k.Namespace == "" {
				k.Namespace = sa[:i]
			}
		}
	}
	if v := ui.Extra["authentication.kubernetes.io/node-name"]; len(v) > 0 {
		k.Node = v[0]
	}
	if v := ui.Extra["authentication.kubernetes.io/pod-name"]; len(v) > 0 {
		k.PodName = v[0]
	}
}

// discoverAssumableTargets finds sts:AssumeRole candidates by scanning the
// environment for IAM role ARNs (in a var whose name signals an assume target)
// and parsing role_arn entries from ~/.aws/config. The caller's own identity
// ARN is excluded. IAM policy enumeration is deliberately NOT attempted — the
// agent role cannot read its own policy (iam:GetRole is denied), so we surface
// concrete, configured targets and let --probe verify them.
func discoverAssumableTargets(selfARN string) []assumableTarget {
	seen := map[string]bool{}
	if selfARN != "" {
		seen[selfARN] = true
	}
	var out []assumableTarget
	add := func(name, arn, source string) {
		arn = strings.TrimSpace(arn)
		if !isIAMRoleARN(arn) || seen[arn] {
			return
		}
		seen[arn] = true
		out = append(out, assumableTarget{
			Name:    name,
			RoleARN: arn,
			Account: accountFromARN(arn),
			Source:  source,
			Status:  "candidate",
		})
	}

	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name, val := kv[:eq], kv[eq+1:]
		// AWS_ROLE_ARN is the current identity, not an assume target.
		if name == "AWS_ROLE_ARN" {
			continue
		}
		upper := strings.ToUpper(name)
		if strings.HasSuffix(upper, "_ROLE_ARN") || strings.Contains(upper, "ASSUME_ROLE") {
			add("", val, "env:"+name)
		}
	}

	for _, t := range parseAWSConfigRoleARNs(readAWSConfig()) {
		add(t.Name, t.RoleARN, t.Source)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RoleARN < out[j].RoleARN })
	if out == nil {
		return []assumableTarget{}
	}
	return out
}

// parseAWSConfigRoleARNs extracts (profile, role_arn) pairs from an INI-shaped
// ~/.aws/config body. Minimal parser: tracks the current [profile …] header
// and picks up role_arn = … lines.
func parseAWSConfigRoleARNs(body string) []assumableTarget {
	var out []assumableTarget
	profile := ""
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			profile = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			profile = strings.TrimSpace(strings.TrimPrefix(profile, "profile "))
			continue
		}
		if k, v, ok := splitINIKeyValue(line); ok && k == "role_arn" {
			out = append(out, assumableTarget{
				Name:    profile,
				RoleARN: v,
				Account: accountFromARN(v),
				Source:  "aws-config:" + profile,
				Status:  "candidate",
			})
		}
	}
	return out
}

func splitINIKeyValue(line string) (key, value string, ok bool) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	return strings.TrimSpace(strings.ToLower(line[:eq])), strings.TrimSpace(line[eq+1:]), true
}

// discoverGitLabProjectPaths returns the deduped GitLab project paths this
// project touches, parsed from BOAT_PROJECTS and BOAT_REFERENCE_REPOS plus the
// checked-out origin remote. Each entry is a namespace/path suitable for
// `glab api projects/<url-encoded>`.
func discoverGitLabProjectPaths() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, spec := range strings.Split(os.Getenv("BOAT_PROJECTS"), ",") {
		add(gitlabPathFromSpec(spec))
	}
	for _, spec := range strings.Split(os.Getenv("BOAT_REFERENCE_REPOS"), ",") {
		add(gitlabPathFromSpec(spec))
	}
	if url, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
		add(gitlabPathFromURL(strings.TrimSpace(string(url))))
	}
	sort.Strings(out)
	return out
}

// gitlabPathFromSpec extracts the project path from a BOAT_PROJECTS entry of
// the form "name=https://gitlab.com/Group/Sub/repo.git:suffix".
func gitlabPathFromSpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	if eq := strings.IndexByte(spec, '='); eq >= 0 {
		spec = spec[eq+1:]
	}
	return gitlabPathFromURL(spec)
}

// gitlabPathFromURL turns a gitlab.com git URL (with optional trailing
// ":suffix" tag, ".git" suffix, or ssh form) into "Group/Sub/repo". Returns ""
// for non-gitlab.com URLs.
func gitlabPathFromURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// Strip a trailing ":<suffix>" tag used by BOAT_PROJECTS, but keep an
	// "ssh://" / "https://" scheme's own colon and the git@host: form intact.
	// Locate the host marker first, then split the path off it.
	const marker = "gitlab.com"
	idx := strings.Index(u, marker)
	if idx < 0 {
		return ""
	}
	rest := u[idx+len(marker):]
	// rest now begins with either '/' (https) or ':' (scp-like ssh).
	rest = strings.TrimLeft(rest, "/:")
	// Drop a trailing BOAT_PROJECTS ":suffix" tag if present.
	if c := strings.IndexByte(rest, ':'); c >= 0 {
		rest = rest[:c]
	}
	rest = strings.TrimSuffix(rest, ".git")
	rest = strings.Trim(rest, "/")
	return rest
}

// queryRepoAccess resolves the caller's effective access level on a project via
// glab, taking the higher of project-level and group-level membership.
func queryRepoAccess(ctx context.Context, path string) repoAccess {
	ra := repoAccess{Path: path}
	encoded := strings.ReplaceAll(path, "/", "%2F")
	out, stderr, err := runProbe(ctx, "glab", "api", "projects/"+encoded)
	if err != nil {
		ra.Error = probeError("glab api projects/"+path, stderr, err)
		return ra
	}
	var proj struct {
		Permissions struct {
			ProjectAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	if jErr := json.Unmarshal(out, &proj); jErr != nil {
		ra.Error = fmt.Sprintf("parsing project: %v", jErr)
		return ra
	}
	level, via := 0, ""
	if pa := proj.Permissions.ProjectAccess; pa != nil && pa.AccessLevel > level {
		level, via = pa.AccessLevel, "project"
	}
	if ga := proj.Permissions.GroupAccess; ga != nil && ga.AccessLevel > level {
		level, via = ga.AccessLevel, "group"
	}
	ra.AccessLevel = level
	ra.AccessName = gitlabAccessLevelName(level)
	ra.Via = via
	return ra
}

// gitlabAccessLevelName maps a GitLab numeric access level to its role name.
func gitlabAccessLevelName(level int) string {
	switch level {
	case 0:
		return "No access"
	case 5:
		return "Minimal access"
	case 10:
		return "Guest"
	case 15:
		return "Planner"
	case 20:
		return "Reporter"
	case 30:
		return "Developer"
	case 40:
		return "Maintainer"
	case 50:
		return "Owner"
	default:
		return fmt.Sprintf("level %d", level)
	}
}

// secretPrefixOf returns the namespace prefix of a secret name: everything up
// to and including the second path separator (e.g. "gardenio/prod/foo" ->
// "gardenio/prod"). Names with fewer than two separators return as-is.
func secretPrefixOf(name string) string {
	name = strings.TrimLeft(name, "/")
	parts := strings.Split(name, "/")
	if len(parts) <= 2 {
		return name
	}
	return strings.Join(parts[:2], "/")
}

func isIAMRoleARN(s string) bool {
	return strings.HasPrefix(s, "arn:aws:iam::") && strings.Contains(s, ":role/")
}

// accountFromARN extracts the 12-digit account id from an ARN
// (arn:aws:iam::<account>:role/…). Returns "" when the shape doesn't match.
func accountFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return ""
	}
	return parts[4]
}

// ── small utilities ─────────────────────────────────────────────────

func readK8sNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func readAWSConfig() string {
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = home + "/.aws/config"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// probeError renders a compact one-line reason for a failed probe, preferring
// the tool's own stderr (first line) over the raw exec error.
func probeError(what, stderr string, err error) string {
	if fl := firstLine(stderr); fl != "" {
		return fmt.Sprintf("%s: %s", what, fl)
	}
	return fmt.Sprintf("%s: %v", what, err)
}

// ── human-readable rendering ────────────────────────────────────────

// renderWhoami prints the capability block. Sections mirror the JSON report;
// each surfaces its own error inline rather than aborting the whole render.
func renderWhoami(w io.Writer, r whoamiReport) {
	fmt.Fprintln(w, "gb whoami — what can I actually do?")
	if ctx := whoamiPodLine(r.Pod); ctx != "" {
		fmt.Fprintf(w, "  %s\n", ctx)
	}
	fmt.Fprintln(w)

	// AWS
	fmt.Fprintln(w, "AWS")
	if r.AWS.CallerIdentity != nil {
		fmt.Fprintf(w, "  identity   %s\n", r.AWS.CallerIdentity.Arn)
		fmt.Fprintf(w, "  account    %s\n", r.AWS.CallerIdentity.Account)
	} else if r.AWS.RoleARN != "" {
		fmt.Fprintf(w, "  role_arn   %s (env; get-caller-identity unavailable)\n", r.AWS.RoleARN)
	}
	if r.AWS.Region != "" {
		fmt.Fprintf(w, "  region     %s\n", r.AWS.Region)
	}
	if r.AWS.Error != "" {
		fmt.Fprintf(w, "  ⚠ %s\n", r.AWS.Error)
	}
	if len(r.AWS.AssumableTargets) == 0 {
		fmt.Fprintln(w, "  assumable  (none discovered)")
	} else {
		fmt.Fprintln(w, "  assumable:")
		for _, t := range r.AWS.AssumableTargets {
			label := t.RoleARN
			if t.Name != "" {
				label = t.Name + " → " + t.RoleARN
			}
			extra := t.Status
			if t.Detail != "" {
				extra += ": " + t.Detail
			}
			fmt.Fprintf(w, "    - %s [%s, %s]\n", label, extra, t.Source)
		}
	}
	fmt.Fprintln(w)

	// Kubernetes
	fmt.Fprintln(w, "Kubernetes")
	if r.Kubernetes.ServiceAccount != "" {
		fmt.Fprintf(w, "  sa         %s (ns %s)\n", r.Kubernetes.ServiceAccount, r.Kubernetes.Namespace)
	} else if r.Kubernetes.Username != "" {
		fmt.Fprintf(w, "  user       %s\n", r.Kubernetes.Username)
	}
	if len(r.Kubernetes.Groups) > 0 {
		fmt.Fprintf(w, "  groups     %s\n", strings.Join(r.Kubernetes.Groups, ", "))
	}
	if r.Kubernetes.Node != "" {
		fmt.Fprintf(w, "  node       %s\n", r.Kubernetes.Node)
	}
	if r.Kubernetes.Error != "" {
		fmt.Fprintf(w, "  ⚠ %s\n", r.Kubernetes.Error)
	}
	for _, c := range r.Kubernetes.Can {
		fmt.Fprintf(w, "  %s  can-i %s\n", checkMark(c.Allowed), c.Action)
	}
	fmt.Fprintln(w)

	// GitLab
	fmt.Fprintln(w, "GitLab")
	if r.GitLab.User != nil {
		bot := ""
		if r.GitLab.User.Bot {
			bot = " (bot)"
		}
		fmt.Fprintf(w, "  user       %s%s [id %d, %s]\n", r.GitLab.User.Username, bot, r.GitLab.User.ID, r.GitLab.User.State)
	}
	if r.GitLab.Error != "" {
		fmt.Fprintf(w, "  ⚠ %s\n", r.GitLab.Error)
	}
	if len(r.GitLab.Repos) == 0 {
		fmt.Fprintln(w, "  repos      (none discovered)")
	} else {
		fmt.Fprintln(w, "  access:")
		for _, repo := range r.GitLab.Repos {
			if repo.Error != "" {
				fmt.Fprintf(w, "    - %s: ⚠ %s\n", repo.Path, repo.Error)
				continue
			}
			via := ""
			if repo.Via != "" {
				via = " (via " + repo.Via + ")"
			}
			fmt.Fprintf(w, "    - %s: %s%s\n", repo.Path, repo.AccessName, via)
		}
	}
	fmt.Fprintln(w)

	// Secrets. A partial error can accompany a non-empty result (the walk
	// failed part-way), so render whatever was gathered AND the error.
	fmt.Fprintln(w, "Secrets (AWS Secrets Manager)")
	if r.Secrets.Total > 0 {
		suffix := ""
		if r.Secrets.Truncated {
			suffix = "+"
		}
		fmt.Fprintf(w, "  %d%s readable across %d prefix(es):\n", r.Secrets.Total, suffix, len(r.Secrets.Prefixes))
		for _, p := range r.Secrets.Prefixes {
			fmt.Fprintf(w, "    - %s/*  (%d)\n", p.Prefix, p.Count)
		}
	} else if r.Secrets.Error == "" {
		fmt.Fprintln(w, "  (none readable)")
	}
	if r.Secrets.Error != "" {
		fmt.Fprintf(w, "  ⚠ %s\n", r.Secrets.Error)
	}

	// LLM (optional)
	if r.LLM != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "LLM")
		if r.LLM.BaseURL != "" {
			fmt.Fprintf(w, "  base       %s\n", r.LLM.BaseURL)
		}
		if r.LLM.Error != "" {
			fmt.Fprintf(w, "  ⚠ %s\n", r.LLM.Error)
		}
		if r.LLM.Spend != nil {
			line := fmt.Sprintf("  spend      $%.2f", *r.LLM.Spend)
			if r.LLM.MaxBudget != nil {
				line += fmt.Sprintf(" / $%.2f budget", *r.LLM.MaxBudget)
			}
			fmt.Fprintln(w, line)
		}
		if r.LLM.ResetAt != "" {
			fmt.Fprintf(w, "  resets     %s\n", r.LLM.ResetAt)
		}
	}
}

// whoamiPodLine summarises the pod/session context on one line for the header.
func whoamiPodLine(p whoamiPod) string {
	var parts []string
	if p.Agent != "" {
		parts = append(parts, p.Agent)
	}
	if p.Role != "" {
		parts = append(parts, "role="+p.Role)
	}
	if p.Project != "" {
		parts = append(parts, "project="+p.Project)
	}
	if p.TaskID != "" {
		parts = append(parts, "task="+p.TaskID)
	}
	return strings.Join(parts, "  ")
}

func checkMark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}
