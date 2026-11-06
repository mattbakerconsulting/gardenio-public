package tests

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The landing serves one of two pages (gd-gQ8hZLd4AQ). landing.page=configmap
// (the default) is the chart's own index.html on upstream nginx. With
// landing.page=image the page is baked into gardenio's landing image (the
// splash, the clonk.tv front door): the chart mounts only default.conf over
// it, the image is tagged like every gardenio-owned image, index.html is
// revalidated on every load and the two ?v=<hash> assets are cached a year.

const (
	landingConfigMap  = "templates/landing/configmap.yaml"
	landingDeployment = "templates/landing/deployment.yaml"
	yearCacheHeader   = "public, max-age=31536000, immutable"
)

// landingConfigData renders the landing ConfigMap and returns its data.
func landingConfigData(t *testing.T, set ...string) map[string]string {
	t.Helper()
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(helmTemplate(t, landingConfigMap, append([]string{"landing.enabled=true"}, set...)...)), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["default.conf"] == "" {
		t.Fatal("landing ConfigMap has no default.conf")
	}
	return cm.Data
}

func TestLanding_ConfigMapPageByDefault(t *testing.T) {
	t.Parallel()
	data := landingConfigData(t)
	if !strings.Contains(data["index.html"], "<h1>Gardenio</h1>") {
		t.Error("landing.page=configmap must carry the chart's index.html")
	}
	if !strings.Contains(data["default.conf"], "try_files $uri /index.html;") {
		t.Errorf("landing.page=configmap default.conf changed:\n%s", data["default.conf"])
	}
	if !strings.Contains(data["default.conf"], "location = /403 {") {
		t.Error("landing.page=configmap keeps the /403 force-logout page under keycloak forwardAuth")
	}
	dep := helmTemplate(t, landingDeployment, "landing.enabled=true")
	if !strings.Contains(dep, `image: "docker.io/library/nginx:1-alpine"`) {
		t.Errorf("landing.page=configmap must run upstream nginx:\n%s", dep)
	}
	if !strings.Contains(dep, "mountPath: /usr/share/nginx/html/index.html") {
		t.Error("landing.page=configmap must mount index.html from the ConfigMap")
	}
}

func TestLanding_ImagePage(t *testing.T) {
	t.Parallel()
	set := []string{"landing.page=image", "global.imageRegistry=registry.example.test/gardenio", "global.imageTag=2026.302.1"}
	data := landingConfigData(t, set...)
	if _, ok := data["index.html"]; ok {
		t.Error("landing.page=image must not carry an index.html in the ConfigMap: the image holds the page")
	}
	conf := data["default.conf"]
	for _, want := range []string{
		"try_files $uri $uri/ =404;",
		"location /healthz {",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("landing.page=image default.conf missing %q", want)
		}
	}
	for loc, cache := range map[string]string{
		"location = /index.html":          "no-cache",
		"location = /three.module.min.js": yearCacheHeader,
		"location = /still.png":           yearCacheHeader,
	} {
		if !strings.Contains(nginxBlock(conf, loc), `add_header Cache-Control "`+cache+`" always;`) {
			t.Errorf("landing.page=image %s must send Cache-Control %q", loc, cache)
		}
	}

	// The image page is public: a link to /403 there would sign a visitor
	// out, so it is served only while the errors middleware needs it.
	if strings.Contains(conf, "location = /403 {") || strings.Contains(conf, "$forcelogout_host") {
		t.Error("landing.page=image must not serve /403 while forceLogoutOn403 is off")
	}
	if on := landingConfigData(t, append(set, "keycloak.forwardAuth.forceLogoutOn403.enabled=true")...)["default.conf"]; !strings.Contains(on, "location = /403 {") {
		t.Error("landing.page=image must serve /403 when forceLogoutOn403 is on: the errors middleware sends 403s there")
	}

	dep := helmTemplate(t, landingDeployment, append([]string{"landing.enabled=true"}, set...)...)
	if !strings.Contains(dep, `image: "registry.example.test/gardenio/landing:2026.302.1"`) {
		t.Errorf("landing.page=image must run the gardenio landing image at the release tag:\n%s", dep)
	}
	if strings.Contains(dep, "/usr/share/nginx/html/index.html") {
		t.Error("landing.page=image must not mount index.html over the image's page")
	}
	if !strings.Contains(dep, "mountPath: /etc/nginx/conf.d/default.conf") {
		t.Error("landing.page=image must still mount default.conf")
	}
}

func TestLanding_PageRejectsUnknown(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, landingDeployment, "landing.enabled=true", "landing.page=splash")
	if err == nil || !strings.Contains(out, "landing.page must be configmap or image") {
		t.Errorf("landing.page=splash must fail the render, got err=%v:\n%s", err, out)
	}
}

// The public edge proxies clonk.tv straight to the landing Service, so under
// the namespace's default-deny ingress the landing admits the edge namespace.
func TestLanding_NetworkPolicyAdmitsPublicEdge(t *testing.T) {
	t.Parallel()
	const tmpl = "templates/networkpolicies/networkpolicies.yaml"
	landingPolicy := func(set ...string) string {
		out := helmTemplate(t, tmpl, append([]string{"networkPolicies.enabled=true", "landing.enabled=true"}, set...)...)
		for doc := range strings.SplitSeq(out, "\n---") {
			if strings.Contains(doc, "-landing-ingress\n") {
				return doc
			}
		}
		t.Fatalf("no landing-ingress NetworkPolicy:\n%s", out)
		return ""
	}
	if doc := landingPolicy("edgeBackends.enabled=true"); !strings.Contains(doc, "kubernetes.io/metadata.name: \"public-edge\"") {
		t.Errorf("with edgeBackends on, the landing must admit the public-edge namespace:\n%s", doc)
	}
	if doc := landingPolicy(); strings.Contains(doc, "public-edge") {
		t.Errorf("with edgeBackends off, the landing must not admit the public-edge namespace:\n%s", doc)
	}
}

// The image page's default.conf, served by a real nginx over the three files
// the splash export writes: the cache headers as above, /healthz, and 404 for
// anything else.
func TestLanding_ImagePageNginx(t *testing.T) {
	t.Parallel()
	bin := edgeNginxBinary(t)
	conf := landingConfigData(t, "landing.page=image")["default.conf"]
	root := t.TempDir()
	for _, f := range []string{"index.html", "three.module.min.js", "still.png"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	port := freePort(t)
	httpLevel, _, _ := strings.Cut(conf, "server {")
	srv := nginxBlock(conf, "server")
	for _, r := range []struct{ old, new string }{
		{"listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", port)},
		{"root /usr/share/nginx/html;", "root " + root + ";"},
	} {
		if !strings.Contains(srv, r.old) {
			t.Fatalf("landing default.conf has no %q", r.old)
		}
		srv = strings.ReplaceAll(srv, r.old, r.new)
	}
	url := runNginx(t, bin, httpLevel, srv, port)

	for _, c := range []struct {
		path, body, cache string
		code              int
	}{
		{"/", "index.html", "no-cache", http.StatusOK},
		{"/index.html", "index.html", "no-cache", http.StatusOK},
		{"/three.module.min.js?v=0123abcd", "three.module.min.js", yearCacheHeader, http.StatusOK},
		{"/still.png?v=0123abcd", "still.png", yearCacheHeader, http.StatusOK},
		{"/healthz", "ok", "", http.StatusOK},
		{"/garden", "", "", http.StatusNotFound},
		{"/lands/", "", "", http.StatusNotFound},
	} {
		resp, err := http.Get(url + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("GET %s = %d, want %d", c.path, resp.StatusCode, c.code)
			continue
		}
		if c.code != http.StatusOK {
			continue
		}
		if string(body) != c.body {
			t.Errorf("GET %s body = %q, want %q", c.path, body, c.body)
		}
		if got := resp.Header.Get("Cache-Control"); got != c.cache {
			t.Errorf("GET %s Cache-Control = %q, want %q", c.path, got, c.cache)
		}
	}
}
