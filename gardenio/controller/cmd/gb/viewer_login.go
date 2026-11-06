package main

// gb viewer-login: pod-side (headless) authentication to beads-viewer using the
// pod's own Kubernetes ServiceAccount identity (epic ka-0000007323, task
// ka-0000000118). This is the machine-to-machine counterpart of `gb login`,
// which is a browser + AVA-cookie handoff that no headless agent pod can drive.
//
// Why a second login path:
//
//   `gb login` captures a HUMAN's AWS Verified Access session cookie from a
//   browser. An agent pod has no browser and no human session — it only has a
//   projected Kubernetes ServiceAccount token. beads-viewer learned to accept
//   an audience-pinned EKS ServiceAccount token as a SECOND verified issuer
//   (oidcauth.K8sSAConfig / cmd/beads-viewer/k8ssa.go, MR !6228 / ka-0000006288):
//   a real verification (JWKS + iss + aud + exp + signature), mapping the
//   verified `sub` (system:serviceaccount:<ns>:<sa>) to a synthetic operator.
//
// What this command does:
//
//   1. Mint an audience-pinned projected token for the pod's ServiceAccount via
//      the Kubernetes TokenRequest API (aud = the viewer's K8S_SA_AUDIENCE).
//      The token is signed by the EKS OIDC issuer the viewer trusts. It NEVER
//      touches node disk — TokenRequest returns it in-process.
//   2. Persist it as a bearer session in the shared clicreds credstore (the same
//      store `gb login` / `kd` read), so subsequent `kd` / `gb` / curl calls
//      replay `Authorization: Bearer <token>` against the viewer and its session
//      persists across commands until the token's short TTL lapses. Re-run to
//      refresh (idempotent).
//   3. Optionally probe /health (--check) to confirm the edge accepts the token
//      end-to-end before persisting, and print the raw token (--print-token) so
//      it can be piped into ad-hoc curl for navigating beads / logs.
//
// The identity gate lives at the viewer, not here: minting a token always
// succeeds for any pod whose ServiceAccount can call the TokenRequest API, but
// the viewer only ACCEPTS a `sub` on its explicit K8S_SA_ALLOWED_SUBJECTS
// allow-list. An un-allow-listed pod mints a valid token and still gets 403.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gardenio/clicreds/credstore"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// viewerLoginDefaultAudience is the audience the viewer pins its SA-token
	// verifier to (K8S_SA_AUDIENCE). A token minted for any other audience —
	// notably the apiserver ("https://kubernetes.default.svc") or IRSA
	// ("sts.amazonaws.com") audiences a pod already carries — is rejected by
	// the verifier (forbiddenSAAudiences), so this dedicated value is the only
	// one that authenticates.
	viewerLoginDefaultAudience = "gardenio-beads-viewer"

	// viewerLoginDefaultInClusterURL is the zero-config in-cluster target: the
	// beads-viewer ClusterIP Service in the release namespace. An in-pod agent
	// reaches the viewer here directly — no public route, no AVA edge. The host
	// is namespace-relative so it resolves in whichever gardenio namespace the
	// pod runs.
	viewerLoginDefaultInClusterURL = "http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095"

	// viewerLoginDefaultTTL is the minted token's lifetime. Kept short (matches
	// the ziti broker-token posture) because the credstore re-mint is cheap:
	// re-run `gb viewer-login` to refresh. The apiserver clamps this to the
	// cluster's configured bounds.
	viewerLoginDefaultTTL = 30 * time.Minute
)

var (
	viewerLoginURL        string
	viewerLoginAudience   string
	viewerLoginSAName     string
	viewerLoginNamespace  string
	viewerLoginTTL        time.Duration
	viewerLoginContext    string
	viewerLoginCheck      bool
	viewerLoginPrintToken bool
	viewerLoginStatus     bool
)

var viewerLoginCmd = &cobra.Command{
	Use:     "viewer-login",
	Short:   "Authenticate to beads-viewer from a pod using its ServiceAccount identity",
	GroupID: "session",
	Long: `Authenticate to beads-viewer from inside a Kubernetes pod, headlessly.

Unlike 'gb login' (a browser + AWS Verified Access cookie handoff, for humans),
'gb viewer-login' mints an audience-pinned projected token for the pod's own
ServiceAccount via the Kubernetes TokenRequest API and persists it as a bearer
session in the credential store. beads-viewer verifies that token against the
EKS OIDC issuer (real JWKS + iss + aud + exp) and maps the ServiceAccount to a
synthetic operator identity, so the pod can navigate beads and logs.

The pod's ServiceAccount must (1) be able to call the TokenRequest API and
(2) appear on the viewer's K8S_SA_ALLOWED_SUBJECTS allow-list. Minting always
succeeds for (1); the viewer returns 403 for a subject that fails (2).

Session persistence: the minted token is stored in the same credstore that kd
and gb read, so subsequent commands replay it automatically until it expires.
Re-run to refresh (idempotent).

Examples:
  gb viewer-login                       # zero-config: in-cluster viewer Service
  gb viewer-login --check               # probe /health before persisting
  gb viewer-login --print-token         # also print the raw bearer (for curl)
  gb viewer-login --viewer-url http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095
  gb viewer-login --status              # show current viewer session state
  TOKEN=$(gb viewer-login --print-token 2>/dev/null)   # capture for scripted curl`,
	RunE: runViewerLogin,
}

func init() {
	// NOTE: deliberately does NOT default to $BEADS_VIEWER_URL. That env var
	// points every agent pod at the PUBLIC AVA edge (<host>/beads),
	// and AWS Verified Access 303s any request that lacks its own human session
	// cookie — a machine bearer token cannot traverse it. The SA-token verifier
	// runs at the pod behind AVA, so the pod-side path is the in-cluster Service
	// (or a non-AVA host), which is the default here. Override with --viewer-url.
	viewerLoginCmd.Flags().StringVar(&viewerLoginURL, "viewer-url", "",
		"beads-viewer base URL (default: in-cluster Service "+viewerLoginDefaultInClusterURL+"; NOT the AVA edge)")
	viewerLoginCmd.Flags().StringVar(&viewerLoginAudience, "audience", envOr("BEADS_VIEWER_SA_AUDIENCE", viewerLoginDefaultAudience),
		"audience to pin the minted SA token to (must match the viewer's K8S_SA_AUDIENCE)")
	viewerLoginCmd.Flags().StringVar(&viewerLoginSAName, "service-account", "",
		"ServiceAccount to mint the token for (default: the pod's own SA, auto-detected)")
	viewerLoginCmd.Flags().StringVar(&viewerLoginNamespace, "namespace", "",
		"namespace of the ServiceAccount (default: the pod's own namespace, auto-detected)")
	viewerLoginCmd.Flags().DurationVar(&viewerLoginTTL, "ttl", viewerLoginDefaultTTL,
		"requested token lifetime (apiserver clamps to cluster bounds)")
	viewerLoginCmd.Flags().StringVar(&viewerLoginContext, "context", "", "credential context name (default: current)")
	viewerLoginCmd.Flags().BoolVar(&viewerLoginCheck, "check", false,
		"probe <viewer>/health with the minted token before persisting; fail if not accepted")
	viewerLoginCmd.Flags().BoolVar(&viewerLoginPrintToken, "print-token", false,
		"print the raw bearer token to stdout (for piping into curl)")
	viewerLoginCmd.Flags().BoolVar(&viewerLoginStatus, "status", false, "show current viewer session status")
}

// saTokenPath is where kubelet mounts the pod's ServiceAccount token.
var saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// errPodNoSAToken explains the one failure --namespace cannot fix: minting a
// viewer token calls TokenRequest, which needs a ServiceAccount credential.
var errPodNoSAToken = errors.New("this pod has no ServiceAccount token, so gb viewer-login cannot " +
	"mint one (agent pods only get a token when their spec sets NeedsK8sAPI; passing --namespace " +
	"will not help). Run viewer-login from a pod/host that has K8s credentials, or use an agent " +
	"with NeedsK8sAPI — see gardenio/docs/agent-viewer-login.md")

// podWithoutSAToken reports whether we are running inside a Kubernetes pod
// (the kubelet injects KUBERNETES_SERVICE_HOST) that has no mounted
// ServiceAccount token and no usable kubeconfig fallback.
func podWithoutSAToken() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return false
	}
	if _, err := os.Stat(saTokenPath); err == nil {
		return false
	}
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		if _, err := os.Stat(kc); err == nil {
			return false
		}
	}
	return true
}

func runViewerLogin(cmd *cobra.Command, _ []string) error {
	if viewerLoginStatus {
		return showViewerLoginStatus()
	}

	viewerURL, err := resolveViewerLoginURL(viewerLoginURL)
	if err != nil {
		return err
	}
	audience := strings.TrimSpace(viewerLoginAudience)
	if audience == "" {
		return fmt.Errorf("--audience must not be empty (it must match the viewer's K8S_SA_AUDIENCE)")
	}

	namespace := strings.TrimSpace(viewerLoginNamespace)
	if namespace == "" {
		namespace = readK8sNamespace()
		if namespace == "" {
			if podWithoutSAToken() {
				return errPodNoSAToken
			}
			return fmt.Errorf("could not auto-detect pod namespace; pass --namespace " +
				"($POD_NAMESPACE unset and no ServiceAccount namespace file mounted)")
		}
	}

	k8s, err := buildK8sClient()
	if err != nil {
		if podWithoutSAToken() {
			return fmt.Errorf("%w (K8s client: %v)", errPodNoSAToken, err)
		}
		return fmt.Errorf("building K8s client (are you in a pod with a mounted ServiceAccount?): %w", err)
	}

	saName := strings.TrimSpace(viewerLoginSAName)
	if saName == "" {
		saName, err = detectPodServiceAccount(cmd.Context(), k8s, namespace)
		if err != nil {
			return fmt.Errorf("could not auto-detect the pod's ServiceAccount; pass --service-account: %w", err)
		}
	}

	token, err := mintViewerSAToken(cmd.Context(), k8s, namespace, saName, audience, viewerLoginTTL)
	if err != nil {
		return fmt.Errorf("minting SA token for %s/%s (aud=%s): %w", namespace, saName, audience, err)
	}

	subject := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName)
	fmt.Fprintf(os.Stderr, "Minted audience-pinned token for %s (aud=%s, ttl~%s).\n",
		subject, audience, viewerLoginTTL.Truncate(time.Second))

	// --check: prove the viewer accepts the token end-to-end BEFORE we persist,
	// so a 403 (subject not allow-listed) or a disabled SA branch surfaces here
	// with an actionable message instead of silently storing a dead session.
	if viewerLoginCheck {
		if err := probeViewerHealth(cmd.Context(), viewerURL, token); err != nil {
			return fmt.Errorf("viewer rejected the minted token — %w\n"+
				"  Subject: %s\n"+
				"  This means either the viewer's K8S_SA branch is not enabled on this\n"+
				"  deployment, or %q is not on its K8S_SA_ALLOWED_SUBJECTS allow-list.\n"+
				"  See gardenio/docs/agent-viewer-login.md for enable + provisioning steps.",
				err, subject, subject)
		}
		fmt.Fprintln(os.Stderr, "Viewer accepted the token (GET /health -> 200).")
	}

	if err := persistViewerBearer(viewerURL, token, viewerLoginTTL); err != nil {
		return err
	}

	if viewerLoginPrintToken {
		// Raw token to STDOUT (everything else goes to stderr) so
		// `TOKEN=$(gb viewer-login --print-token --quiet)` captures cleanly.
		fmt.Println(token)
	}
	return nil
}

// resolveViewerLoginURL picks the viewer base URL: the explicit flag / env,
// else the in-cluster Service default. Rejects non-http(s) up front.
func resolveViewerLoginURL(explicit string) (string, error) {
	raw := strings.TrimSpace(explicit)
	if raw == "" {
		raw = viewerLoginDefaultInClusterURL
	}
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid viewer URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("viewer URL must be http(s): %q", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("viewer URL missing host: %q", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// mintViewerSAToken requests an audience-pinned projected token for the named
// ServiceAccount via the Kubernetes TokenRequest API. The returned token is
// signed by the cluster's OIDC issuer (the EKS issuer beads-viewer trusts) and
// carries exactly the requested audience, so it is accepted only by the viewer's
// SA-token verifier and rejected everywhere the pod's default/IRSA tokens are
// used. audiences is a single dedicated value — never the apiserver or IRSA
// audience (the viewer rejects those defensively).
func mintViewerSAToken(ctx context.Context, k8s kubernetes.Interface, namespace, saName, audience string, ttl time.Duration) (string, error) {
	seconds := int64(ttl.Seconds())
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &seconds,
		},
	}
	out, err := k8s.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if out.Status.Token == "" {
		return "", fmt.Errorf("TokenRequest returned an empty token")
	}
	return out.Status.Token, nil
}

// probeViewerHealth issues an authenticated GET <viewer>/health and reports
// whether the edge accepts the bearer. Redirects are NOT followed: a rejected
// token can be answered with a 3xx to re-auth, and chasing it would obscure the
// real signal. Any non-200 is an error carrying the observed status.
func probeViewerHealth(ctx context.Context, viewerURL, token string) error {
	probeURL := strings.TrimRight(viewerURL, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probing %s: %w", probeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("viewer returned HTTP %d", resp.StatusCode)
}

// persistViewerBearer stores the minted token as a bearer session in the shared
// credstore context, preserving any pre-existing OAuth/cookie fields so a
// concurrent human/browser session is not clobbered. Sets ExpiresAt from the
// requested TTL so `gb viewer-login --status` and the daemon client's
// expiry-aware paths know when to re-mint.
func persistViewerBearer(server, token string, ttl time.Duration) error {
	store, err := credstore.Load("")
	if err != nil {
		return fmt.Errorf("loading credential store: %w", err)
	}
	ctxName := viewerLoginContext
	if ctxName == "" {
		ctxName = store.CurrentContext()
	}
	c := &credstore.Context{
		Server:      server,
		AccessToken: token,
		ExpiresAt:   nowUTC().Add(ttl).Format(time.RFC3339),
	}
	if existing := store.Context(ctxName); existing != nil {
		// Preserve fields that belong to other auth modes so viewer-login does
		// not evict a GitLab OAuth refresh token.
		c.GitLabURL = existing.GitLabURL
		c.ClientID = existing.ClientID
		c.User = existing.User
		c.RefreshToken = existing.RefreshToken
		c.IDToken = existing.IDToken
	}
	store.SetContext(ctxName, c)
	if viewerLoginContext != "" {
		store.SetCurrentContext(viewerLoginContext)
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Authenticated. Bearer session stored in %s (context: %s, server: %s)\n",
		store.Describe(), ctxName, server)
	return nil
}

func showViewerLoginStatus() error {
	store, err := credstore.Load("")
	if err != nil {
		return fmt.Errorf("loading credential store: %w", err)
	}
	cur := store.Current()
	if cur == nil || cur.AccessToken == "" {
		fmt.Println("No viewer bearer session. Run 'gb viewer-login' from inside a pod.")
		return nil
	}
	fmt.Printf("Context: %s\n", store.CurrentContext())
	fmt.Printf("Server:  %s\n", cur.Server)
	if cur.ExpiresAt != "" {
		if cur.IsExpired() {
			fmt.Println("Bearer:  expired — re-run 'gb viewer-login' to refresh")
		} else {
			t, _ := time.Parse(time.RFC3339, cur.ExpiresAt)
			fmt.Printf("Bearer:  valid (expires in %s)\n", time.Until(t).Truncate(time.Second))
		}
	} else {
		fmt.Println("Bearer:  present (no expiry recorded)")
	}
	return nil
}

// detectPodServiceAccount resolves the ServiceAccount of the pod this process
// runs in. It looks the pod up by name (the pod's HOSTNAME equals its metadata
// name under the default Kubernetes DNS/hostname policy) and reads
// spec.serviceAccountName. Falls back to $POD_NAME, then the mounted-token
// heuristic is intentionally NOT used — a projected token carries no SA-name
// claim we can read without the apiserver. Returns an error the caller turns
// into "pass --service-account".
func detectPodServiceAccount(ctx context.Context, k8s kubernetes.Interface, namespace string) (string, error) {
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	if podName == "" {
		podName = strings.TrimSpace(os.Getenv("HOSTNAME"))
	}
	if podName == "" {
		return "", fmt.Errorf("neither $POD_NAME nor $HOSTNAME is set")
	}
	pod, err := k8s.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("looking up pod %s/%s: %w", namespace, podName, err)
	}
	sa := strings.TrimSpace(pod.Spec.ServiceAccountName)
	if sa == "" {
		// An empty spec.serviceAccountName means the namespace "default" SA.
		return "default", nil
	}
	return sa, nil
}

// nowUTC is the wall-clock source for expiry stamping. A package var so tests
// can pin it deterministically.
var nowUTC = func() time.Time { return time.Now().UTC() }
