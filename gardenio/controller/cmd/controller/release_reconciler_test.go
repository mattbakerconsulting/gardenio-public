package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"gardenio/controller/internal/beadsapi"
)

// helmReleaseSecret is a Helm 3 release Secret for revision rev, as helm
// writes it: base64 of gzipped release JSON, labels owner/name/status/version.
func helmReleaseSecret(t *testing.T, rev int, tag, status string, deployed time.Time) (*corev1.Secret, *metav1.PartialObjectMetadata) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":    "gardenio",
		"version": rev,
		"info":    map[string]any{"status": status, "last_deployed": deployed.Format(time.RFC3339Nano)},
		"config":  map[string]any{"global": map[string]any{"imageTag": tag}},
		"chart":   map[string]any{"metadata": map[string]any{"appVersion": "2026.266.3"}},
	})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(body)
	zw.Close()
	meta := metav1.ObjectMeta{
		Name:      fmt.Sprintf("sh.helm.release.v1.gardenio.v%d", rev),
		Namespace: "gardenio",
		Labels:    map[string]string{"owner": "helm", "name": "gardenio", "status": status, "version": fmt.Sprint(rev)},
	}
	sec := &corev1.Secret{ObjectMeta: meta, Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))}}
	pom := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta}
	return sec, pom
}

// fakeReleaseDaemon holds the open prod row and records deployments.
type fakeReleaseDaemon struct {
	open    beadsapi.ReleaseDeployment
	known   map[string]bool
	deploys []beadsapi.DeployReleaseRequest
}

func (f *fakeReleaseDaemon) ListReleases(_ context.Context, project, status string, _ int) ([]beadsapi.Release, error) {
	if project != "gardenio" || status != beadsapi.ReleaseStatusLive {
		return nil, fmt.Errorf("unexpected list %s %s", project, status)
	}
	if f.open.Target == "" {
		return nil, nil
	}
	return []beadsapi.Release{{Tag: f.open.ImageTag, Deployments: []beadsapi.ReleaseDeployment{f.open}}}, nil
}

func (f *fakeReleaseDaemon) DeployRelease(_ context.Context, ref, _ string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error) {
	if !f.known[ref] {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "release not found"}
	}
	f.deploys = append(f.deploys, req)
	f.open = beadsapi.ReleaseDeployment{Target: req.Target, Since: req.Since, Revision: req.Revision, ImageTag: req.ImageTag}
	return &beadsapi.Release{Tag: ref}, nil
}

func TestReleaseReconciler_TagChangeAndRollback(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	old, oldMeta := helmReleaseSecret(t, 259, "2026.303.2", "superseded", now.Add(-30*time.Hour))
	cur, curMeta := helmReleaseSecret(t, 260, "2026.304.1", "deployed", now.Add(-20*time.Minute))
	core := k8sfake.NewSimpleClientset(old, cur)
	scheme := runtime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	meta := metadatafake.NewSimpleMetadataClient(scheme, oldMeta, curMeta)
	helm := &helmSecrets{meta: meta, core: core, namespace: "gardenio", release: "gardenio"}

	d := &fakeReleaseDaemon{
		open:  beadsapi.ReleaseDeployment{Target: "prod", Since: "2026-10-30T06:00:00Z", Revision: "259", ImageTag: "2026.303.2"},
		known: map[string]bool{"2026.303.2": true, "2026.304.1": true},
	}
	r := newReleaseReconciler(d, helm, "2026.304.1", func(string) string { return "" }, quiet)
	r.now = func() time.Time { return now }

	// A manual helm upgrade to 2026.304.1 (revision 260) nobody reported. It
	// settles from when the reconciler first sees it, not from Helm's deploy
	// time (a --wait roll can take longer than the settle).
	if r.reconcileOnce(context.Background()) {
		t.Fatalf("written on first sight, before it settled: %+v", d.deploys)
	}
	now = now.Add(10 * time.Minute)
	if !r.reconcileOnce(context.Background()) {
		t.Fatalf("tag change: no deployment written")
	}
	want := beadsapi.DeployReleaseRequest{Target: "prod", Since: "2026-10-31T11:40:00Z", By: "observed", Via: "reconciler", Revision: "260", ImageTag: "2026.304.1"}
	if len(d.deploys) != 1 || d.deploys[0] != want {
		t.Fatalf("deploys = %+v, want %+v", d.deploys, want)
	}

	// In sync: nothing more, and the release body is not read again.
	gets := len(core.Actions())
	if r.reconcileOnce(context.Background()) || len(d.deploys) != 1 {
		t.Errorf("in sync: wrote %+v", d.deploys)
	}
	if len(core.Actions()) != gets {
		t.Errorf("an unchanged revision re-read its secret: %v", core.Actions()[gets:])
	}

	// helm rollback to 2026.303.2 makes revision 261; it settles first.
	back, backMeta := helmReleaseSecret(t, 261, "2026.303.2", "deployed", now.Add(-time.Minute))
	curMeta.Labels["status"] = "superseded"
	meta = metadatafake.NewSimpleMetadataClient(scheme, oldMeta, curMeta, backMeta)
	core = k8sfake.NewSimpleClientset(old, cur, back)
	helm.meta, helm.core = meta, core
	if r.reconcileOnce(context.Background()) {
		t.Fatalf("an unsettled rollback was written")
	}
	now = now.Add(10 * time.Minute)
	if !r.reconcileOnce(context.Background()) {
		t.Fatalf("rollback: no deployment written")
	}
	if got := d.deploys[1]; got.ImageTag != "2026.303.2" || got.Revision != "261" || got.By != "observed" || got.Since != "2026-10-31T12:09:00Z" {
		t.Errorf("rollback row = %+v", got)
	}
}

// Without a readable Helm release the controller's own version is what runs;
// with no revision a matching tag is in sync, and an unrecorded tag is
// skipped without a write.
func TestReleaseReconciler_OwnVersionFallback(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &fakeReleaseDaemon{
		open:  beadsapi.ReleaseDeployment{Target: "prod", Since: "2026-10-30T06:00:00Z", Revision: "259", ImageTag: "2026.303.2"},
		known: map[string]bool{"2026.303.2": true},
	}
	r := newReleaseReconciler(d, nil, "2026.303.2", func(string) string { return "" }, quiet)
	r.now = func() time.Time { return now }
	r.reconcileOnce(context.Background())
	now = now.Add(11 * time.Minute)
	if r.reconcileOnce(context.Background()) || len(d.deploys) != 0 {
		t.Errorf("same tag, no revision: wrote %+v", d.deploys)
	}

	r = newReleaseReconciler(d, nil, "2026.305.1", func(k string) string {
		return map[string]string{"GARDENIO_RELEASE_SETTLE": "0s"}[k]
	}, quiet)
	r.now = func() time.Time { return now }
	if r.reconcileOnce(context.Background()) || len(d.deploys) != 0 {
		t.Errorf("unrecorded tag: wrote %+v", d.deploys)
	}

	r = newReleaseReconciler(d, nil, "dev", func(string) string { return "" }, quiet)
	if r.reconcileOnce(context.Background()) {
		t.Errorf("a dev build was reported as a release")
	}
}

// An open row of the running tag with no revision (a helm_deploy close that
// named none) is in sync: the reconciler does not add a row for it each pass.
func TestReleaseReconciler_OpenRowWithoutRevision(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cur, curMeta := helmReleaseSecret(t, 260, "2026.304.1", "deployed", now.Add(-20*time.Minute))
	scheme := runtime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	helm := &helmSecrets{meta: metadatafake.NewSimpleMetadataClient(scheme, curMeta), core: k8sfake.NewSimpleClientset(cur), namespace: "gardenio", release: "gardenio"}
	d := &fakeReleaseDaemon{
		open:  beadsapi.ReleaseDeployment{Target: "prod", Since: "2026-10-31T11:45:00Z", ImageTag: "2026.304.1"},
		known: map[string]bool{"2026.304.1": true},
	}
	r := newReleaseReconciler(d, helm, "2026.304.1", func(string) string { return "" }, quiet)
	r.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		r.reconcileOnce(context.Background())
		now = now.Add(10 * time.Minute)
	}
	if len(d.deploys) != 0 {
		t.Errorf("same tag, open row without a revision: wrote %+v", d.deploys)
	}
}

func TestDecodeHelmRelease_UngzippedAndAppVersion(t *testing.T) {
	body := `{"version":12,"info":{"last_deployed":"2026-10-30T16:05:12.5+00:00"},"chart":{"metadata":{"appVersion":"2026.266.3"}}}`
	obs, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString([]byte(body))))
	if err != nil {
		t.Fatal(err)
	}
	if obs.ImageTag != "2026.266.3" || obs.Revision != "12" || !obs.Since.Equal(time.Date(2026, 10, 30, 16, 5, 12, 500000000, time.UTC)) {
		t.Errorf("obs = %+v", obs)
	}
	if _, err := decodeHelmRelease([]byte("not base64!")); err == nil {
		t.Error("garbage decoded")
	}
}

// A box reports what it runs to the home daemon on target box-<name>, kind
// box, by box:<name>; its own GARDENIO_RELEASE_PROJECT/TARGET do not apply
// there. Without GARDENIO_BOX_NAME there is no box reporter.
func TestBoxReleaseReconciler_ReportsToHome(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := map[string]string{
		"GARDENIO_BOX_NAME":        "acme",
		"GARDENIO_RELEASE_PROJECT": "acme-box",
		"GARDENIO_RELEASE_TARGET":  "box-acme",
		"GARDENIO_RELEASE_SETTLE":  "0s",
	}
	home := &fakeReleaseDaemon{known: map[string]bool{"2026.304.1": true}}
	r := newBoxReleaseReconciler(home, nil, "2026.304.1", func(k string) string { return env[k] }, quiet)
	if !r.reconcileOnce(context.Background()) {
		t.Fatalf("box upgrade: no deployment written to the home daemon")
	}
	got := home.deploys[0]
	got.Since = ""
	want := beadsapi.DeployReleaseRequest{Target: "box-acme", Kind: "box", By: "box:acme", Via: "reconciler", ImageTag: "2026.304.1"}
	if got != want || r.project != "gardenio" {
		t.Errorf("deploy = %+v on %s, want %+v on gardenio", got, r.project, want)
	}

	if newBoxReleaseReconciler(home, nil, "2026.304.1", func(string) string { return "" }, quiet) != nil {
		t.Error("a box reporter without GARDENIO_BOX_NAME")
	}
}

// stubHelm is a Helm release reader that always reports one release.
type stubHelm observedRelease

func (h stubHelm) Deployed(context.Context) (observedRelease, error) { return observedRelease(h), nil }

// startBoxReleaseReporter authenticates to the home daemon with a
// client-credentials token from the home realm (the daemon refuses static
// tokens for release writes), and writes the box's row with it. Without a box
// name, an address or the full set of client credentials it starts nothing,
// and a whitespace-only box name does not panic.
func TestStartBoxReleaseReporter(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type write struct {
		auth string
		req  beadsapi.DeployReleaseRequest
	}
	writes := make(chan write, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if id, sec, _ := r.BasicAuth(); id != "box-acme" || sec != "s3cret" {
			if r.ParseForm(); r.PostForm.Get("client_id") != "box-acme" || r.PostForm.Get("client_secret") != "s3cret" {
				http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"jwt-1","token_type":"Bearer","expires_in":300}`)
	})
	mux.HandleFunc("GET /v1/releases", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[]}`)
	})
	mux.HandleFunc("POST /v1/releases/{ref}/deployments", func(w http.ResponseWriter, r *http.Request) {
		var req beadsapi.DeployReleaseRequest
		json.NewDecoder(r.Body).Decode(&req)
		writes <- write{auth: r.Header.Get("Authorization"), req: req}
		fmt.Fprintf(w, `{"tag":%q}`, r.PathValue("ref"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	full := map[string]string{
		"GARDENIO_BOX_NAME":                        "acme",
		"GARDENIO_RELEASE_HOME_ADDR":               srv.URL,
		"GARDENIO_RELEASE_HOME_TOKEN_URL":          srv.URL + "/token",
		"GARDENIO_RELEASE_HOME_CLIENT_ID":          "box-acme",
		"GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE": secret,
		"GARDENIO_RELEASE_SETTLE":                  "0s",
	}
	with := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return full[k]
		}
	}
	for name, over := range map[string]map[string]string{
		"whitespace box name": {"GARDENIO_BOX_NAME": "  "},
		"no home address":     {"GARDENIO_RELEASE_HOME_ADDR": ""},
		"no client secret":    {"GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE": ""},
		"static token only": {"GARDENIO_RELEASE_HOME_TOKEN": "static", "GARDENIO_RELEASE_HOME_TOKEN_URL": "",
			"GARDENIO_RELEASE_HOME_CLIENT_ID": "", "GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE": ""},
	} {
		if startBoxReleaseReporter(context.Background(), nil, time.Hour, with(over), quiet) {
			t.Errorf("%s: a box reporter started", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	helm := stubHelm{ImageTag: "2026.304.1", Revision: "12"}
	if !startBoxReleaseReporter(ctx, helm, time.Hour, with(nil), quiet) {
		t.Fatal("no box reporter with the full configuration")
	}
	select {
	case w := <-writes:
		if w.auth != "Bearer jwt-1" {
			t.Errorf("Authorization = %q, want the client-credentials token", w.auth)
		}
		if w.req.Target != "box-acme" || w.req.Kind != "box" || w.req.By != "box:acme" || w.req.ImageTag != "2026.304.1" || w.req.Revision != "12" {
			t.Errorf("deploy = %+v, want box-acme/box/box:acme at 2026.304.1 rev 12", w.req)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the box reporter wrote nothing to the home daemon")
	}
}
