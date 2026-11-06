package tests

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
)

// Login-theme plumbing (passkey/login-theme plan gd-QXq1fjYuRD, step gd-S6sixAadTe;
// restructured in gd-qiUiXvTX0k): the theme tree lives OUTSIDE the chart, in
// gardenio/keycloak-theme/gardenio/**. The release deployer renders it into a
// ConfigMap (gardenio/scripts/keycloak-theme-configmap.py) the chart only
// references; a theme-unpack initContainer rebuilds the tree from the
// ConfigMap keys into an emptyDir mounted read-only at
// /opt/keycloak/themes/<name>. Dormant by default. Inside the chart the theme
// was stored twice in the 1 MiB Helm release Secret and broke every release.

const (
	themeSS        = "templates/keycloak-runtime/statefulset.yaml"
	themeCMName    = "test-keycloak-runtime-theme-files"
	themeGenerator = "gardenio/scripts/keycloak-theme-configmap.py"
)

var themeBase = []string{
	"postgres.enabled=true",
	"keycloak.enabled=true",
	"keycloak.runtime.enabled=true",
}

func themeOn(extra ...string) []string {
	return append(append(append([]string{}, themeBase...), "keycloak.runtime.theme.enabled=true"), extra...)
}

// repoRoot is the monorepo root: the chart is gardenio/helm/gardenio.
func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), "..", "..", "..")
}

// themeDir is the shipped theme tree, outside the chart.
func themeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "gardenio", "keycloak-theme", "gardenio")
}

// themeFiles returns the theme file paths relative to the theme root, sorted,
// slash-separated.
func themeFilesIn(t *testing.T, root string) []string {
	t.Helper()
	var rel []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		r, _ := filepath.Rel(root, p)
		rel = append(rel, filepath.ToSlash(r))
		return nil
	})
	if err != nil || len(rel) == 0 {
		t.Fatalf("no theme files under %s: %v", root, err)
	}
	sort.Strings(rel)
	return rel
}

func themeFiles(t *testing.T) []string { return themeFilesIn(t, themeDir(t)) }

// themeConfigMap is the generator's output.
type themeConfigMap struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data       map[string]string `json:"data"`
	BinaryData map[string][]byte `json:"binaryData"`
}

// generateTheme runs the deployer's generator over a theme tree exactly as
// release-deploy.sh does (release + namespace), and parses the manifest.
func generateTheme(t *testing.T, dir string, extra ...string) themeConfigMap {
	t.Helper()
	args := append([]string{filepath.Join(repoRoot(t), themeGenerator), "--theme-dir", dir, "--release", "test", "--namespace", "gardenio"}, extra...)
	out, err := exec.Command("python3", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", themeGenerator, err, out)
	}
	var cm themeConfigMap
	if err := json.Unmarshal(out, &cm); err != nil {
		t.Fatalf("generator output is not JSON: %v\n%s", err, out)
	}
	return cm
}

func TestKeycloakTheme_DefaultOff(t *testing.T) {
	t.Parallel()
	ss := helmTemplateNS(t, "gardenio", themeSS, themeBase...)
	for _, bad := range []string{"checksum/theme", "name: theme", "/opt/keycloak/themes", "-theme", "theme-unpack", "theme-src"} {
		if strings.Contains(ss, bad) {
			t.Errorf("theme off: statefulset must not contain %q", bad)
		}
	}
	// Nothing in the whole chart mentions the theme ConfigMap.
	if full := helmTemplateFull(t, nil, themeBase...); strings.Contains(full, "keycloak-runtime-theme") {
		t.Errorf("theme off: no theme ConfigMap reference expected anywhere in the render")
	}
}

// gd-qiUiXvTX0k: the chart neither renders the theme ConfigMap nor packages
// the theme files. Both halves are asserted on the packaged chart, which is
// what the release Secret stores.
func TestKeycloakTheme_ChartCarriesNoThemeFiles(t *testing.T) {
	t.Parallel()
	full := helmTemplateFull(t, nil, themeOn()...)
	for _, bad := range []string{"login__theme.properties", "parent=keycloak.v2", "binaryData:"} {
		if strings.Contains(full, bad) {
			t.Errorf("chart must not render theme content, found %q", bad)
		}
	}
	if regexp.MustCompile(`(?m)^  name: test-keycloak-runtime-theme(-files)?$`).MatchString(full) {
		t.Errorf("chart must not render a theme ConfigMap object")
	}

	dest := t.TempDir()
	if out, err := exec.Command("helm", "package", chartDir(t), "--destination", dest).CombinedOutput(); err != nil {
		t.Fatalf("helm package: %v\n%s", err, out)
	}
	matches, _ := filepath.Glob(filepath.Join(dest, "gardenio-*.tgz"))
	if len(matches) != 1 {
		t.Fatalf("expected one packaged chart, got %v", matches)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	for _, n := range names {
		switch {
		case strings.Contains(n, "keycloak-theme"):
			t.Errorf("packaged chart carries a theme file: %s", n)
		case strings.HasSuffix(n, "/values-fleet.yaml"), strings.HasSuffix(n, "/values-test.yaml"):
			t.Errorf("packaged chart carries a values overlay (.helmignore): %s", n)
		}
	}
	// Positive control: the package is a real chart with the litellm file the
	// chart does read at render time (.helmignore must not swallow files/).
	if !slices.Contains(names, "gardenio/files/litellm/litellm_thinking_callback.py") {
		t.Errorf("packaged chart lost files/litellm (did .helmignore grow a files/ rule?): %v", names)
	}
}

func TestKeycloakTheme_OnRendersUnpackInitContainerAndMounts(t *testing.T) {
	t.Parallel()
	ss := helmTemplateNS(t, "gardenio", themeSS, themeOn()...)

	for _, want := range []string{
		"- name: theme-unpack\n",
		"- name: theme-src\n              mountPath: /theme-src\n              readOnly: true\n",
		"- name: theme\n              mountPath: /theme\n",
		"- name: theme-src\n          configMap:\n            name: \"" + themeCMName + "\"\n",
		"- name: theme\n          emptyDir: {}\n",
		"sed 's#__#/#g'",
	} {
		if !strings.Contains(ss, want) {
			t.Errorf("statefulset is missing %q:\n%s", want, ss)
		}
	}
	// The chart no longer knows the file list: no items mapping.
	if strings.Contains(ss, "items:") {
		t.Errorf("statefulset must not carry a configMap items list (the chart does not know the theme files)")
	}
	mount := regexp.MustCompile(`- name: theme\n\s+mountPath: "/opt/keycloak/themes/gardenio"\n\s+readOnly: true`)
	if !mount.MatchString(ss) {
		t.Errorf("expected a read-only mount at /opt/keycloak/themes/gardenio, got:\n%s", ss)
	}
	// Only the gardenio subdirectory is mounted, never the whole themes dir.
	if regexp.MustCompile(`mountPath: "?/opt/keycloak/themes/?"?\s`).MatchString(ss) {
		t.Errorf("must not mount over /opt/keycloak/themes itself")
	}
	// The create-db initContainer (autoWire) and theme-unpack share one list.
	both := helmTemplateNS(t, "gardenio", themeSS, themeOn("keycloak.runtime.db.autoWire=true")...)
	if strings.Count(both, "initContainers:") != 1 || !strings.Contains(both, "- name: create-db") || !strings.Contains(both, "- name: theme-unpack") {
		t.Errorf("autoWire + theme must render ONE initContainers list with both containers:\n%s", both)
	}
	onlyDB := helmTemplateNS(t, "gardenio", themeSS, append(themeBase, "keycloak.runtime.db.autoWire=true")...)
	if strings.Count(onlyDB, "initContainers:") != 1 || !strings.Contains(onlyDB, "- name: create-db") || strings.Contains(onlyDB, "theme-unpack") {
		t.Errorf("autoWire without theme must render only create-db:\n%s", onlyDB)
	}
}

func TestKeycloakTheme_ConfigMapNameOverride(t *testing.T) {
	t.Parallel()
	ss := helmTemplateNS(t, "gardenio", themeSS, themeOn("keycloak.runtime.theme.configMapName=my-theme")...)
	if !strings.Contains(ss, "configMap:\n            name: \"my-theme\"\n") {
		t.Errorf("configMapName should name the theme-src volume's ConfigMap, got:\n%s", ss)
	}
	if strings.Contains(ss, themeCMName) {
		t.Errorf("default ConfigMap name must not appear when overridden")
	}
}

func TestKeycloakTheme_MountFollowsName(t *testing.T) {
	t.Parallel()
	ss := helmTemplateNS(t, "gardenio", themeSS, themeOn("keycloak.runtime.theme.name=acme")...)
	if !strings.Contains(ss, `mountPath: "/opt/keycloak/themes/acme"`) {
		t.Errorf("mount path should follow theme.name, got:\n%s", ss)
	}
}

// themeChecksumOf reproduces the generator's checksum from the theme files on
// disk: one "<path relative to the theme root> <sha256 of content>" line per
// file, paths sorted, then sha256 of the whole.
func themeChecksumOf(t *testing.T, root string) string {
	t.Helper()
	var acc strings.Builder
	for _, p := range themeFilesIn(t, root) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		acc.WriteString(p)
		acc.WriteString(" ")
		acc.WriteString(hex.EncodeToString(sum[:]))
		acc.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(acc.String()))
	return hex.EncodeToString(sum[:])
}

var themeChecksumRE = regexp.MustCompile(`checksum/theme: "([0-9a-f]*)"\n`)

func themeChecksumIn(t *testing.T, ss string) string {
	t.Helper()
	m := themeChecksumRE.FindStringSubmatch(ss)
	if m == nil {
		t.Fatalf("checksum/theme annotation missing or not a quoted hex string:\n%s", ss)
	}
	return m[1]
}

// checksum/theme comes from keycloak.runtime.theme.checksum when set, else
// from the live ConfigMap's annotation (lookup) — which `helm template` cannot
// do, so it renders empty (never absent, never null).
func TestKeycloakTheme_ChecksumAnnotation(t *testing.T) {
	t.Parallel()
	sum := themeChecksumOf(t, themeDir(t))
	ss := helmTemplateNS(t, "gardenio", themeSS, themeOn("keycloak.runtime.theme.checksum="+sum)...)
	if got := themeChecksumIn(t, ss); got != sum {
		t.Errorf("checksum/theme = %q, want the configured %s", got, sum)
	}
	if got := themeChecksumIn(t, helmTemplateNS(t, "gardenio", themeSS, themeOn()...)); got != "" {
		t.Errorf("without a cluster and without the value, checksum/theme must render empty, got %q", got)
	}
	// The lookup is wired to the SAME ConfigMap name the volume mounts.
	helpers, err := os.ReadFile(filepath.Join(chartDir(t), "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`lookup "v1" "ConfigMap" \.Release\.Namespace \(include "gardenio\.keycloakRuntime\.themeConfigMapName" \.\)`).Match(helpers) {
		t.Errorf("themeChecksum must look up the ConfigMap named by themeConfigMapName")
	}
	if !strings.Contains(string(helpers), `"gardenio.dev/theme-checksum"`) {
		t.Errorf("themeChecksum must read the gardenio.dev/theme-checksum annotation the generator stamps")
	}
}

// gd-cW3og8FqhN: a calver release with no theme edit must not restart Keycloak.
// The checksum is the same at two chart versions. The StatefulSet, which does
// carry helm.sh/chart, is the positive control: the same comparison must see
// it differ, so the version bump is known to reach the render.
func TestKeycloakTheme_ChecksumIgnoresChartVersion(t *testing.T) {
	t.Parallel()
	sum := themeChecksumOf(t, themeDir(t))
	flags := themeOn("keycloak.runtime.theme.checksum=" + sum)
	v1 := helmTemplatePackaged(t, "2026.100.1", flags...)
	v2 := helmTemplatePackaged(t, "2026.200.9", flags...)

	ssV1 := renderedDoc(t, v1, "StatefulSet", "test-keycloak-runtime")
	ssV2 := renderedDoc(t, v2, "StatefulSet", "test-keycloak-runtime")
	if ssV1 == ssV2 || !strings.Contains(ssV1, "gardenio-2026.100.1") || !strings.Contains(ssV2, "gardenio-2026.200.9") {
		t.Fatal("control: the StatefulSet carries helm.sh/chart and must differ between chart versions; the version bump is not reaching the render")
	}
	if c1, c2 := themeChecksumIn(t, ssV1), themeChecksumIn(t, ssV2); c1 != c2 || c1 != sum {
		t.Errorf("checksum/theme must not change with the chart version: %s (2026.100.1) vs %s (2026.200.9)", c1, c2)
	}
}

// The generator's manifest is what the StatefulSet expects: the default name
// the chart derives for the same release, every theme file as one key with
// "/" encoded as "__" (text in data byte-for-byte, binary in binaryData
// byte-for-byte, S1h gd-0SYmLWCCM5), and the checksum annotation the chart
// reads back. Labels carry no helm.sh/chart (nothing restarts on a calver).
func TestKeycloakTheme_GeneratorMatchesChartContract(t *testing.T) {
	t.Parallel()
	cm := generateTheme(t, themeDir(t))
	if cm.Kind != "ConfigMap" || cm.APIVersion != "v1" {
		t.Fatalf("expected a v1 ConfigMap, got %s/%s", cm.APIVersion, cm.Kind)
	}
	ss := helmTemplateNS(t, "gardenio", themeSS, themeOn()...)
	if cm.Metadata.Name != themeCMName || !strings.Contains(ss, "name: \""+cm.Metadata.Name+"\"\n") {
		t.Errorf("generator names the ConfigMap %q; the StatefulSet mounts %q", cm.Metadata.Name, themeCMName)
	}
	if cm.Metadata.Namespace != "gardenio" {
		t.Errorf("namespace = %q", cm.Metadata.Namespace)
	}
	for _, bad := range []string{"helm.sh/chart"} {
		if _, ok := cm.Metadata.Labels[bad]; ok {
			t.Errorf("theme ConfigMap must not carry label %q", bad)
		}
	}
	for k, want := range map[string]string{"app.kubernetes.io/name": "gardenio", "app.kubernetes.io/instance": "test", "app.kubernetes.io/component": "keycloak-runtime"} {
		if cm.Metadata.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, cm.Metadata.Labels[k], want)
		}
	}
	if got, want := cm.Metadata.Annotations["gardenio.dev/theme-checksum"], themeChecksumOf(t, themeDir(t)); got != want {
		t.Errorf("gardenio.dev/theme-checksum = %s, want the sha256 over the theme files %s", got, want)
	}

	keyRE := regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	sawBinary := false
	seen := map[string]bool{}
	for _, f := range themeFiles(t) {
		key := strings.ReplaceAll(f, "/", "__")
		seen[key] = true
		if !keyRE.MatchString(key) {
			t.Errorf("key %q is not a valid ConfigMap key", key)
		}
		// The unpack script's inverse mapping must give the path back.
		if strings.ReplaceAll(key, "__", "/") != f {
			t.Errorf("key %q does not decode back to %q", key, f)
		}
		want, err := os.ReadFile(filepath.Join(themeDir(t), filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		if themeIsBinary(f) {
			sawBinary = true
			if _, inData := cm.Data[key]; inData {
				t.Errorf("%s is binary and must not be in data", f)
			}
			if got, ok := cm.BinaryData[key]; !ok {
				t.Errorf("%s is binary and must be in binaryData", f)
			} else if string(got) != string(want) {
				t.Errorf("%s: binaryData does not decode to the file on disk (%d vs %d bytes)", f, len(got), len(want))
			}
			continue
		}
		if _, inBin := cm.BinaryData[key]; inBin {
			t.Errorf("%s is text and must not be in binaryData", f)
		}
		got, ok := cm.Data[key]
		if !ok {
			t.Errorf("%s is missing from data", f)
			continue
		}
		if got != string(want) {
			t.Errorf("%s: data differs from the file on disk", f)
		}
		if strings.ContainsRune(got, '\uFFFD') {
			t.Errorf("%s: data carries U+FFFD, a byte was mangled on the text path", f)
		}
	}
	if !sawBinary {
		t.Fatal("expected at least one binary theme file (the pixel heading font); the binaryData path is untested")
	}
	for k := range cm.Data {
		if !seen[k] {
			t.Errorf("data carries a key with no file on disk: %s", k)
		}
	}
	for k := range cm.BinaryData {
		if !seen[k] {
			t.Errorf("binaryData carries a key with no file on disk: %s", k)
		}
	}
}

// themeIsBinary mirrors the generator: fonts and raster images by extension.
func themeIsBinary(rel string) bool {
	return regexp.MustCompile(`\.(woff2?|ttf|otf|eot|png|jpe?g|gif|ico|webp)$`).MatchString(rel)
}

// End to end on this machine: lay the generator's keys out as the kubelet
// would (one file per key), run the StatefulSet's own theme-unpack script
// over them, and the result must be the shipped tree byte-for-byte.
func TestKeycloakTheme_UnpackScriptRebuildsTheTree(t *testing.T) {
	t.Parallel()
	cm := generateTheme(t, themeDir(t))
	src := filepath.Join(t.TempDir(), "theme-src")
	dst := filepath.Join(t.TempDir(), "theme")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range cm.Data {
		if err := os.WriteFile(filepath.Join(src, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range cm.BinaryData {
		if err := os.WriteFile(filepath.Join(src, k), v, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The script, verbatim from the rendered StatefulSet.
	rendered := helmTemplateNS(t, "gardenio", themeSS, themeOn()...)
	var ss struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []struct {
						Name    string   `json:"name"`
						Command []string `json:"command"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := sigsyaml.Unmarshal([]byte(rendered), &ss); err != nil {
		t.Fatalf("parse statefulset: %v", err)
	}
	var script string
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		if c.Name == "theme-unpack" {
			if len(c.Command) != 3 || c.Command[0] != "/bin/sh" || c.Command[1] != "-c" {
				t.Fatalf("theme-unpack command must be /bin/sh -c <script>, got %v", c.Command)
			}
			script = c.Command[2]
		}
	}
	if script == "" {
		t.Fatal("no theme-unpack initContainer in the render")
	}
	script = strings.ReplaceAll(script, "/theme-src", src)
	script = strings.ReplaceAll(script, "/theme/", dst+"/")
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("theme-unpack script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "theme-unpack: ") {
		t.Errorf("script should report the file count, got: %s", out)
	}
	got := themeFilesIn(t, dst)
	want := themeFiles(t)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unpacked tree differs from the shipped tree.\n got: %v\nwant: %v", got, want)
	}
	for _, f := range want {
		a, _ := os.ReadFile(filepath.Join(themeDir(t), filepath.FromSlash(f)))
		b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(f)))
		if string(a) != string(b) {
			t.Errorf("%s: unpacked bytes differ from the shipped file", f)
		}
	}

	// An empty ConfigMap (the deployer never applied it) must fail the pod,
	// not start Keycloak on its stock theme.
	empty := filepath.Join(t.TempDir(), "empty-src")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	script2 := strings.ReplaceAll(strings.ReplaceAll(script, src, empty), dst+"/", filepath.Join(t.TempDir(), "x")+"/")
	if out, err := exec.Command("/bin/sh", "-c", script2).CombinedOutput(); err == nil {
		t.Errorf("theme-unpack must fail on an empty ConfigMap, got exit 0:\n%s", out)
	} else if !strings.Contains(string(out), "no theme files") {
		t.Errorf("theme-unpack should name the empty ConfigMap, got:\n%s", out)
	}
}

// A real theme edit (changed file, new file, flipped font byte) must change
// the checksum the deployer stamps, and so checksum/theme on the pod.
func TestKeycloakTheme_ChecksumTracksThemeFiles(t *testing.T) {
	t.Parallel()
	base := generateTheme(t, themeDir(t)).Metadata.Annotations["gardenio.dev/theme-checksum"]
	copyTheme := func() string {
		dir := filepath.Join(t.TempDir(), "gardenio")
		if err := os.CopyFS(dir, os.DirFS(themeDir(t))); err != nil {
			t.Fatalf("copy theme: %v", err)
		}
		return dir
	}
	sumOf := func(dir string) string {
		got := generateTheme(t, dir).Metadata.Annotations["gardenio.dev/theme-checksum"]
		if want := themeChecksumOf(t, dir); got != want {
			t.Errorf("%s: checksum = %s, want %s", dir, got, want)
		}
		return got
	}

	edited := copyTheme()
	css := filepath.Join(edited, "login", "resources", "css", "gardenio.css")
	f, err := os.OpenFile(css, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n/* gd-cW3og8FqhN test edit */\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if sumOf(edited) == base {
		t.Errorf("editing login/resources/css/gardenio.css must change the checksum")
	}

	added := copyTheme()
	if err := os.WriteFile(filepath.Join(added, "login", "extra.ftl"), []byte("<#-- new -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sumOf(added) == base {
		t.Errorf("adding a theme file must change the checksum")
	}

	flipped := copyTheme()
	font := filepath.Join(flipped, "login", "resources", "fonts", "pixelify-sans-latin-600-normal.woff2")
	b, err := os.ReadFile(font)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(font, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if sumOf(flipped) == base {
		t.Errorf("flipping a byte of the font must change the checksum (raw bytes are hashed)")
	}
}

func TestKeycloakTheme_LoginThemeOnRealm(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/keycloak/realm.yaml",
		"keycloak.enabled=true", "keycloak.realm.themes.loginTheme=gardenio")
	if !strings.Contains(out, `loginTheme: "gardenio"`) {
		t.Errorf("realm should render loginTheme gardenio, got:\n%s", out)
	}
	def := helmTemplateNS(t, "gardenio", "templates/keycloak/realm.yaml", "keycloak.enabled=true")
	if !strings.Contains(def, `loginTheme: "keycloak"`) {
		t.Errorf("default loginTheme should stay keycloak, got:\n%s", def)
	}
}

// S1e: the email and account themes ride in the same ConfigMap as the login
// theme (the generator takes every file), and the realm selects them by name.
func TestKeycloakTheme_AccountAndEmailThemesOnRealm(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/keycloak/realm.yaml",
		"keycloak.enabled=true",
		"keycloak.realm.themes.accountTheme=gardenio",
		"keycloak.realm.themes.emailTheme=gardenio")
	for _, want := range []string{`accountTheme: "gardenio"`, `emailTheme: "gardenio"`} {
		if !strings.Contains(out, want) {
			t.Errorf("realm should render %s, got:\n%s", want, out)
		}
	}
	// Rollback is dropping the values: the default realm names neither.
	def := helmTemplateNS(t, "gardenio", "templates/keycloak/realm.yaml", "keycloak.enabled=true")
	for _, bad := range []string{"accountTheme:", "emailTheme:"} {
		if strings.Contains(def, bad) {
			t.Errorf("default realm must not render %s, got:\n%s", bad, def)
		}
	}
}

func TestKeycloakTheme_AccountAndEmailTreeShape(t *testing.T) {
	t.Parallel()
	root := themeDir(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("theme file %s: %v", rel, err)
		}
		return string(b)
	}
	if p := read("email/theme.properties"); !regexp.MustCompile(`(?m)^parent=base$`).MatchString(p) {
		t.Errorf("email theme must extend base (the keycloak email theme is abstract):\n%s", p)
	}
	acct := read("account/theme.properties")
	if !regexp.MustCompile(`(?m)^parent=keycloak\.v3$`).MatchString(acct) {
		t.Errorf("account theme must extend keycloak.v3:\n%s", acct)
	}
	if !regexp.MustCompile(`(?m)^styles=css/gardenio-account\.css$`).MatchString(acct) {
		t.Errorf("account theme must load css/gardenio-account.css:\n%s", acct)
	}
	// CSS only: the account console is a React app and its index.ftl is not ours.
	for _, f := range themeFiles(t) {
		if strings.HasPrefix(f, "account/") && strings.HasSuffix(f, ".ftl") {
			t.Errorf("account theme is CSS only, found template %s", f)
		}
	}
	// Mail clients ignore stylesheets and remote images: the email layout is
	// inline-styled, and nothing in the email theme fetches a remote resource.
	layout := read("email/html/template.ftl")
	if !strings.Contains(layout, `style="`) {
		t.Errorf("email layout must use inline styles")
	}
	for _, f := range themeFiles(t) {
		if !strings.HasPrefix(f, "email/") {
			continue
		}
		body := read(f)
		for _, bad := range []string{"<img", "@import", "url(", "<link", "http://", "https://"} {
			if strings.Contains(body, bad) {
				t.Errorf("email theme file %s must not contain %q (no remote resources)", f, bad)
			}
		}
	}
}

// The scene is wired in and self-contained: the two scene stylesheets load
// after the flat theme's, every url() in the login CSS resolves to a shipped
// file (same origin, no data: URI, no remote fetch), the font's licence rides
// beside it, and the template is the production one (no inline script/style).
func TestKeycloakTheme_SceneIsWiredAndSelfContained(t *testing.T) {
	t.Parallel()
	login := filepath.Join(themeDir(t), "login")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(login, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("theme file %s: %v", rel, err)
		}
		return string(b)
	}
	props := read("theme.properties")
	if !regexp.MustCompile(`(?m)^styles=css/styles\.css css/gardenio\.css css/scene-base\.css css/scene-meadow\.css$`).MatchString(props) {
		t.Errorf("login theme must load the scene stylesheets after the flat theme's:\n%s", props)
	}
	if !regexp.MustCompile(`(?m)^parent=keycloak\.v2$`).MatchString(props) {
		t.Errorf("login theme must stay a child of keycloak.v2:\n%s", props)
	}
	urlRE := regexp.MustCompile(`url\(\s*["']?([^"')]+)["']?\s*\)`)
	for _, css := range []string{"gardenio.css", "scene-base.css", "scene-meadow.css"} {
		body := read("resources/css/" + css)
		for _, m := range urlRE.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			if strings.HasPrefix(ref, "data:") || strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
				t.Errorf("%s references %q: the scene must be same-origin with no data: URI", css, ref)
				continue
			}
			target := filepath.Join(login, "resources", "css", filepath.FromSlash(ref))
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s references %q which is not a shipped theme file", css, ref)
			}
		}
	}
	for _, f := range []string{"resources/fonts/pixelify-sans-latin-600-normal.woff2", "resources/fonts/OFL-PixelifySans.txt", "resources/img/sprout.svg"} {
		if _, err := os.Stat(filepath.Join(login, filepath.FromSlash(f))); err != nil {
			t.Errorf("scene file %s must ship: %v", f, err)
		}
	}
	if lic := read("resources/fonts/OFL-PixelifySans.txt"); !strings.Contains(lic, "SIL OPEN FONT LICENSE") {
		t.Errorf("the font licence file must be the OFL text")
	}
	// The scene is CSS only: nothing inline reached the template (Keycloak's
	// own <script> tags are the production ones and stay).
	tmpl := read("template.ftl")
	for _, bad := range []string{"<style", "style=\"", "url("} {
		if strings.Contains(tmpl, bad) {
			t.Errorf("template.ftl must stay free of inline %q (the scene is CSS only)", bad)
		}
	}
}
