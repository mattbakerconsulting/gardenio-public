package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/clicreds/credstore"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// --- resolveViewerLoginURL --------------------------------------------------

func TestResolveViewerLoginURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty falls back to in-cluster svc", "", viewerLoginDefaultInClusterURL, false},
		{"trailing slash trimmed", "http://viewer:8095/", "http://viewer:8095", false},
		{"https ok", "https://cli.gardenio.acme.example", "https://cli.gardenio.acme.example", false},
		{"non-http rejected", "ftp://viewer", "", true},
		{"missing host rejected", "http://", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveViewerLoginURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (%q)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("resolveViewerLoginURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- mintViewerSAToken ------------------------------------------------------

// fakeClientWithToken returns a fake clientset whose CreateToken action returns
// a TokenRequest carrying wantToken in its Status, so mintViewerSAToken can be
// exercised without a live apiserver. It also captures the audiences the caller
// requested so the test can assert the audience pinning.
func fakeClientWithToken(t *testing.T, wantToken string, gotAudiences *[]string) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts/token", func(action ktesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		tr, ok := create.GetObject().(*authenticationv1.TokenRequest)
		if !ok {
			return false, nil, nil
		}
		if gotAudiences != nil {
			*gotAudiences = tr.Spec.Audiences
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: wantToken},
		}, nil
	})
	return cs
}

func TestMintViewerSAToken(t *testing.T) {
	var gotAud []string
	cs := fakeClientWithToken(t, "signed.jwt.token", &gotAud)

	tok, err := mintViewerSAToken(context.Background(), cs, "gardenio-system", "gardenio-agent", "gardenio-beads-viewer", 15*time.Minute)
	if err != nil {
		t.Fatalf("mintViewerSAToken: %v", err)
	}
	if tok != "signed.jwt.token" {
		t.Errorf("token = %q, want %q", tok, "signed.jwt.token")
	}
	if len(gotAud) != 1 || gotAud[0] != "gardenio-beads-viewer" {
		t.Errorf("requested audiences = %v, want exactly [gardenio-beads-viewer]", gotAud)
	}
}

func TestMintViewerSATokenEmptyIsError(t *testing.T) {
	cs := fakeClientWithToken(t, "", nil)
	if _, err := mintViewerSAToken(context.Background(), cs, "ns", "sa", "aud", time.Minute); err == nil {
		t.Fatal("expected error on empty token, got nil")
	}
}

// --- probeViewerHealth ------------------------------------------------------

func TestProbeViewerHealth(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"200 accepted", http.StatusOK, false},
		{"401 rejected", http.StatusUnauthorized, true},
		{"403 not-allow-listed", http.StatusForbidden, true},
		{"303 ava-bounce", http.StatusSeeOther, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				if tt.status >= 300 && tt.status < 400 {
					w.Header().Set("Location", "https://idp.example/login")
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := probeViewerHealth(context.Background(), srv.URL, "abc.def.ghi")
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for status %d, got nil", tt.status)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for status %d: %v", tt.status, err)
			}
			if gotAuth != "Bearer abc.def.ghi" {
				t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer abc.def.ghi")
			}
		})
	}
}

// TestProbeViewerHealthDoesNotFollowRedirect asserts the probe reports the 3xx
// itself rather than chasing the Location (which would hit an IdP challenge and
// mask the real "cookie not accepted" signal).
func TestProbeViewerHealthDoesNotFollowRedirect(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusOK) // /login would be 200 if followed
	}))
	defer srv.Close()

	if err := probeViewerHealth(context.Background(), srv.URL, "t"); err == nil {
		t.Fatal("expected error on 303, got nil")
	}
	if hits != 1 {
		t.Errorf("probe made %d requests, want 1 (must not follow the redirect)", hits)
	}
}

// --- persistViewerBearer ----------------------------------------------------

func TestPersistViewerBearerRoundTrip(t *testing.T) {
	// Isolate the credstore to a temp dir so we don't touch the real one.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	fixed := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	orig := nowUTC
	nowUTC = func() time.Time { return fixed }
	defer func() { nowUTC = orig }()

	viewerLoginContext = ""
	server := "http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095"
	if err := persistViewerBearer(server, "signed.jwt.token", 30*time.Minute); err != nil {
		t.Fatalf("persistViewerBearer: %v", err)
	}

	// Re-read via the status path's store loader indirectly: reload and assert.
	if err := showViewerLoginStatus(); err != nil {
		t.Fatalf("showViewerLoginStatus: %v", err)
	}
}

// TestPersistViewerBearerPreservesOAuthFields ensures a viewer-login does not
// evict pre-existing OAuth fields belonging to a human session in the same
// context.
func TestPersistViewerBearerPreservesOAuthFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viewerLoginContext = ""

	// Seed a context with a GitLab OAuth refresh token (an older gb login).
	store, err := credstore.Load("")
	if err != nil {
		t.Fatalf("load credstore: %v", err)
	}
	store.SetContext(store.CurrentContext(), &credstore.Context{Server: "http://viewer", RefreshToken: "gitlab-refresh"})
	if err := store.Save(); err != nil {
		t.Fatalf("seed oauth context: %v", err)
	}
	if err := persistViewerBearer("http://viewer", "signed.jwt.token", time.Minute); err != nil {
		t.Fatalf("persistViewerBearer: %v", err)
	}
	// The bearer must be set AND the refresh token preserved.
	reloaded, err := credstore.Load("")
	if err != nil {
		t.Fatalf("reload credstore: %v", err)
	}
	cur := reloaded.Current()
	if cur == nil || cur.AccessToken != "signed.jwt.token" || cur.RefreshToken != "gitlab-refresh" {
		t.Fatalf("context after viewer-login = %+v, want bearer set and refresh token kept", cur)
	}
	if err := showViewerLoginStatus(); err != nil {
		t.Fatalf("showViewerLoginStatus: %v", err)
	}
}

// --- detectPodServiceAccount ------------------------------------------------

func TestDetectPodServiceAccount(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crew-agent-abc", Namespace: "gardenio-system"},
		Spec:       corev1.PodSpec{ServiceAccountName: "gardenio-agent-cluster-admin"},
	}
	cs := fake.NewSimpleClientset(pod)

	t.Run("from POD_NAME", func(t *testing.T) {
		t.Setenv("POD_NAME", "crew-agent-abc")
		t.Setenv("HOSTNAME", "")
		sa, err := detectPodServiceAccount(context.Background(), cs, "gardenio-system")
		if err != nil {
			t.Fatalf("detectPodServiceAccount: %v", err)
		}
		if sa != "gardenio-agent-cluster-admin" {
			t.Errorf("sa = %q, want gardenio-agent-cluster-admin", sa)
		}
	})

	t.Run("falls back to HOSTNAME", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "crew-agent-abc")
		sa, err := detectPodServiceAccount(context.Background(), cs, "gardenio-system")
		if err != nil {
			t.Fatalf("detectPodServiceAccount: %v", err)
		}
		if sa != "gardenio-agent-cluster-admin" {
			t.Errorf("sa = %q, want gardenio-agent-cluster-admin", sa)
		}
	})

	t.Run("no pod name is error", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "")
		if _, err := detectPodServiceAccount(context.Background(), cs, "gardenio-system"); err == nil {
			t.Fatal("expected error when neither POD_NAME nor HOSTNAME set")
		}
	})

	t.Run("empty spec SA defaults to default", func(t *testing.T) {
		bare := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "bare-pod", Namespace: "ns"},
		}
		cs2 := fake.NewSimpleClientset(bare)
		t.Setenv("POD_NAME", "bare-pod")
		sa, err := detectPodServiceAccount(context.Background(), cs2, "ns")
		if err != nil {
			t.Fatalf("detectPodServiceAccount: %v", err)
		}
		if sa != "default" {
			t.Errorf("sa = %q, want default", sa)
		}
	})
}

// --- podWithoutSAToken ------------------------------------------------------

func TestPodWithoutSAToken(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := saTokenPath
	t.Cleanup(func() { saTokenPath = orig })

	tests := []struct {
		name    string
		host    string
		token   string
		kubecfg string
		want    bool
	}{
		{"not in a pod", "", filepath.Join(dir, "missing"), "", false},
		{"pod with token", "10.0.0.1", tokenFile, "", false},
		{"pod without token", "10.0.0.1", filepath.Join(dir, "missing"), "", true},
		{"pod without token but kubeconfig present", "10.0.0.1", filepath.Join(dir, "missing"), tokenFile, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.host)
			t.Setenv("KUBECONFIG", tt.kubecfg)
			saTokenPath = tt.token
			if got := podWithoutSAToken(); got != tt.want {
				t.Fatalf("podWithoutSAToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrPodNoSATokenDoesNotSuggestNamespace(t *testing.T) {
	if !strings.Contains(errPodNoSAToken.Error(), "no ServiceAccount token") {
		t.Fatalf("message should name the missing token: %v", errPodNoSAToken)
	}
	if strings.Contains(errPodNoSAToken.Error(), "pass --namespace") {
		t.Fatalf("message must not tell the user to pass --namespace: %v", errPodNoSAToken)
	}
}
