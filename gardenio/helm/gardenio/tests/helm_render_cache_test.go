package tests

// A caching `helm` for the whole package (gd-iQ99AoxcdR).
//
// Every test here shells out to `helm template` -- ~1,950 calls a run, all
// serial, each re-parsing and re-rendering every template in the chart even
// when `--show-only` keeps one of them. That was 772s of a 799s run, and on a
// contended CI node it ran past the 18m `-timeout`. A third of those calls are
// the same render: identical except for which `--show-only` they filter to,
// or identical outright.
//
// TestMain puts this test binary on PATH as `helm` (a symlink), so every
// caller -- exec.Command("helm", ...) in a helper, or `helm` inside a
// `bash -c` pipeline -- goes through helmShim with no change at the call
// site. helmShim renders the chart once per distinct set of values and
// answers `--show-only` itself, the way helm does it (helmShowOnly). Anything
// it does not fully understand runs the real helm, untouched.
//
// A cache key covers the arguments minus --show-only, the working directory,
// the environment, and the content of every file an argument names, so a test
// that rewrites a values file between two renders gets two renders. A chart
// directory other than ../ is never cached: a test may be editing its own copy.
//
// One render serves many calls, so a randAlphaNum secret (minted fresh on
// every real render: there is no cluster for `lookup` to find the old one)
// now repeats across them. Nothing here may depend on it changing.
//
// GARDENIO_TEST_HELM_VERIFY=1 also runs the real helm with the original
// arguments on every call and fails the call (exit 97) when the exit code,
// stderr or stdout differ -- stdout with those random tokens masked
// (helmMaskRandom). Run the suite that way after a helm upgrade.
// GARDENIO_TEST_HELM_CACHE=off turns the shim off.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const (
	helmShimRealEnv   = "GARDENIO_TEST_HELM_REAL"
	helmShimDirEnv    = "GARDENIO_TEST_HELM_CACHE_DIR"
	helmShimChartEnv  = "GARDENIO_TEST_HELM_CHART"
	helmShimVerifyEnv = "GARDENIO_TEST_HELM_VERIFY"
	helmShimOffEnv    = "GARDENIO_TEST_HELM_CACHE"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "helm" {
		os.Exit(helmShim(os.Args[1:]))
	}
	os.Exit(runWithHelmCache(m))
}

func runWithHelmCache(m *testing.M) int {
	if os.Getenv(helmShimOffEnv) == "off" {
		return m.Run()
	}
	dir, err := installHelmShim()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm render cache off: %v\n", err)
		return m.Run()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	code := m.Run()
	fmt.Fprintln(os.Stderr, helmShimSummary(dir))
	return code
}

// installHelmShim symlinks this binary into a fresh directory as `helm`,
// prepends that directory to PATH, and records where the real helm is.
func installHelmShim() (string, error) {
	realHelm, err := exec.LookPath("helm")
	if err != nil {
		return "", err
	}
	if realHelm, err = filepath.Abs(realHelm); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "helm-render-cache-")
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(dir, "renders"), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.Symlink(self, filepath.Join(dir, "helm")); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	for k, v := range map[string]string{
		helmShimRealEnv:  realHelm,
		helmShimDirEnv:   dir,
		helmShimChartEnv: filepath.Clean(filepath.Join(wd, "..")),
		"PATH":           dir + string(os.PathListSeparator) + os.Getenv("PATH"),
	} {
		if err := os.Setenv(k, v); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// helmShimSummary reads the one-letter-per-call tally helmShim appends to.
func helmShimSummary(dir string) string {
	tally, _ := os.ReadFile(filepath.Join(dir, "tally"))
	return fmt.Sprintf("helm render cache: %d template calls, %d rendered, %d served from cache, %d passed through",
		bytes.Count(tally, []byte("h"))+bytes.Count(tally, []byte("m")),
		bytes.Count(tally, []byte("m")), bytes.Count(tally, []byte("h")), bytes.Count(tally, []byte("p")))
}

type helmResult struct {
	stdout, stderr []byte
	code           int
}

func helmShim(args []string) int {
	realHelm := os.Getenv(helmShimRealEnv)
	dir := os.Getenv(helmShimDirEnv)
	if realHelm == "" || dir == "" {
		fmt.Fprintln(os.Stderr, "helm render cache: invoked as helm without "+helmShimRealEnv+" (a cmd.Env that drops it?); set GARDENIO_TEST_HELM_CACHE=off")
		return 127
	}
	base, show, ok := helmCacheableArgs(args)
	var key string
	if ok {
		key, ok = helmRenderKey(base, os.Getenv(helmShimChartEnv))
	}
	if !ok {
		helmShimTally(dir, "p")
		return helmExec(realHelm, args)
	}
	full, hit, err := helmCachedRender(realHelm, filepath.Join(dir, "renders", key), base)
	if err != nil {
		helmShimTally(dir, "p")
		return helmExec(realHelm, args)
	}
	if hit {
		helmShimTally(dir, "h")
	} else {
		helmShimTally(dir, "m")
	}
	res := full
	if res.code == 0 && len(show) > 0 {
		res = helmShowOnly(full, show)
	}
	if os.Getenv(helmShimVerifyEnv) != "" {
		want, err := helmRun(realHelm, args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helm render cache verify: %v\n", err)
			return 97
		}
		if msg := helmResultDiff(want, res); msg != "" {
			fmt.Fprintf(os.Stderr, "helm render cache verify: `helm %s` differs from real helm: %s\n", strings.Join(args, " "), msg)
			return 97
		}
	}
	_, _ = os.Stderr.Write(res.stderr)
	_, _ = os.Stdout.Write(res.stdout)
	return res.code
}

// helmCacheableArgs splits a `helm template` command line into the render
// arguments and the --show-only filters. ok is false for anything the cache
// must not answer: another subcommand, --debug (prints the manifest on a
// failed render), output to a directory, cluster access, stdin, a
// post-renderer.
func helmCacheableArgs(args []string) (base, show []string, ok bool) {
	if len(args) == 0 || args[0] != "template" {
		return nil, nil, false
	}
	base = []string{args[0]}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--show-only" || a == "-s":
			if i+1 == len(args) {
				return nil, nil, false
			}
			i++
			show = append(show, args[i])
		case strings.HasPrefix(a, "--show-only="):
			show = append(show, strings.TrimPrefix(a, "--show-only="))
		case strings.HasPrefix(a, "-s="):
			show = append(show, strings.TrimPrefix(a, "-s="))
		case a == "-" || a == "--" || a == "--debug",
			strings.HasPrefix(a, "--output-dir"),
			strings.HasPrefix(a, "--validate"),
			strings.HasPrefix(a, "--dry-run"),
			strings.HasPrefix(a, "--post-renderer"):
			return nil, nil, false
		default:
			base = append(base, a)
		}
	}
	return base, show, true
}

// helmRenderKey hashes everything a render can depend on. Any argument, or
// the value after an `=` in it, split on commas, that names a file adds the
// file's content; one that names a directory must be the chart under test.
func helmRenderKey(base []string, chart string) (string, bool) {
	h := sha256.New()
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	_, _ = fmt.Fprintf(h, "v1\x00%s\x00", wd)
	env := os.Environ()
	sort.Strings(env)
	for _, e := range env {
		_, _ = fmt.Fprintf(h, "env %s\x00", e)
	}
	for _, a := range base {
		_, _ = fmt.Fprintf(h, "arg %s\x00", a)
		candidates := []string{a}
		if _, v, found := strings.Cut(a, "="); found {
			candidates = append(candidates, v)
		}
		for _, c := range candidates {
			for p := range strings.SplitSeq(c, ",") {
				if p == "" {
					continue
				}
				fi, err := os.Stat(p)
				if err != nil {
					continue
				}
				if fi.IsDir() {
					abs, err := filepath.Abs(p)
					if err != nil || chart == "" || abs != chart {
						return "", false
					}
					continue
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return "", false
				}
				_, _ = fmt.Fprintf(h, "file %s %x\x00", p, sha256.Sum256(data))
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// helmCachedRender returns the render stored at path, or runs it and stores it.
func helmCachedRender(realHelm, path string, base []string) (helmResult, bool, error) {
	if data, err := os.ReadFile(path); err == nil {
		if res, ok := decodeHelmResult(data); ok {
			return res, true, nil
		}
	}
	res, err := helmRun(realHelm, base)
	if err != nil {
		return helmResult{}, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err == nil {
		_, werr := tmp.Write(encodeHelmResult(res))
		if cerr := tmp.Close(); werr == nil && cerr == nil {
			_ = os.Rename(tmp.Name(), path)
		}
		_ = os.Remove(tmp.Name())
	}
	return res, false, nil
}

func helmRun(realHelm string, args []string) (helmResult, error) {
	cmd := exec.Command(realHelm, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if _, exited := err.(*exec.ExitError); !exited || cmd.ProcessState.ExitCode() < 0 {
			return helmResult{}, err
		}
	}
	return helmResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: cmd.ProcessState.ExitCode()}, nil
}

// helmExec replaces this process with the real helm.
func helmExec(realHelm string, args []string) int {
	err := syscall.Exec(realHelm, append([]string{"helm"}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "helm render cache: exec %s: %v\n", realHelm, err)
	return 127
}

func helmShimTally(dir, mark string) {
	f, err := os.OpenFile(filepath.Join(dir, "tally"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = io.WriteString(f, mark)
	_ = f.Close()
}

func encodeHelmResult(r helmResult) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d %d %d\n", r.code, len(r.stdout), len(r.stderr))
	b.Write(r.stdout)
	b.Write(r.stderr)
	return b.Bytes()
}

func decodeHelmResult(data []byte) (helmResult, bool) {
	header, body, found := bytes.Cut(data, []byte("\n"))
	if !found {
		return helmResult{}, false
	}
	f := strings.Fields(string(header))
	if len(f) != 3 {
		return helmResult{}, false
	}
	code, err1 := strconv.Atoi(f[0])
	nOut, err2 := strconv.Atoi(f[1])
	nErr, err3 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil || err3 != nil || nOut+nErr != len(body) {
		return helmResult{}, false
	}
	return helmResult{stdout: body[:nOut], stderr: body[nOut:], code: code}, true
}

// helmManifestSep and helmSourceRe are helm's own (pkg/releaseutil.sep and
// the manifestNameRegex in cmd/helm/template.go, v3.16).
var (
	helmManifestSep = regexp.MustCompile("(?:^|\\s*\n)---\\s*")
	helmSourceRe    = regexp.MustCompile("# Source: [^/]+/(.+)")
)

// helmShowOnly applies --show-only to a full, successful `helm template`
// output exactly as cmd/helm/template.go does: split the stream into
// documents, keep, per filter in order, every document whose `# Source:`
// path (minus the chart name) matches it as a filepath.Match pattern, and
// fail on a filter that matches nothing.
func helmShowOnly(full helmResult, show []string) helmResult {
	var docs []string
	for _, d := range helmManifestSep.Split(strings.TrimSpace(string(full.stdout)), -1) {
		if d != "" {
			docs = append(docs, strings.TrimSpace(d))
		}
	}
	var out bytes.Buffer
	var keep []string
	for _, f := range show {
		f = filepath.ToSlash(f)
		missing := true
		for _, d := range docs {
			m := helmSourceRe.FindStringSubmatch(d)
			if len(m) == 0 {
				continue
			}
			if matched, _ := filepath.Match(f, m[1]); !matched {
				continue
			}
			keep = append(keep, d)
			missing = false
		}
		if missing {
			stderr := append(append([]byte{}, full.stderr...), fmt.Sprintf("Error: could not find template %s in chart\n", f)...)
			return helmResult{stderr: stderr, code: 1}
		}
	}
	for _, d := range keep {
		fmt.Fprintf(&out, "---\n%s\n", d)
	}
	return helmResult{stdout: out.Bytes(), stderr: full.stderr, code: 0}
}

func helmResultDiff(want, got helmResult) string {
	switch {
	case want.code != got.code:
		return fmt.Sprintf("exit code %d, want %d (stderr %q)", got.code, want.code, got.stderr)
	case !bytes.Equal(want.stderr, got.stderr):
		return fmt.Sprintf("stderr %q, want %q", got.stderr, want.stderr)
	case !bytes.Equal(helmMaskRandom(want.stdout), helmMaskRandom(got.stdout)):
		return fmt.Sprintf("stdout differs (%d bytes, want %d)", len(got.stdout), len(want.stdout))
	}
	return ""
}

var helmRandomToken = regexp.MustCompile(`[A-Za-z0-9+/=]{24,}`)

// helmMaskRandom blanks every long base64/hex-looking run, keeping its
// length: the chart's randAlphaNum secrets, and the checksums over them, are
// different on every real render.
func helmMaskRandom(b []byte) []byte {
	return helmRandomToken.ReplaceAllFunc(b, func(m []byte) []byte {
		return bytes.Repeat([]byte("~"), len(m))
	})
}

// TestHelmRenderCache_MatchesRealHelm runs the same command lines through the
// shim (`helm` on PATH) and the real helm, and wants the same bytes back. It
// covers what helmShowOnly re-implements: one template, a glob, two filters,
// and a filter that matches nothing.
func TestHelmRenderCache_MatchesRealHelm(t *testing.T) {
	t.Parallel()
	realHelm := os.Getenv(helmShimRealEnv)
	if realHelm == "" {
		t.Skip("helm render cache is off")
	}
	chart := chartDir(t)
	for _, args := range [][]string{
		{"template", "test", chart},
		{"template", "test", chart, "--set", "grafana.enabled=true", "--show-only", "templates/grafana/deployment.yaml"},
		{"template", "test", chart, "--set", "grafana.enabled=true", "--show-only=templates/grafana/*"},
		{"template", "test", chart, "--set", "grafana.enabled=true", "-s", "templates/grafana/service.yaml", "-s", "templates/grafana/deployment.yaml"},
		{"template", "test", chart, "--show-only", "templates/no-such-template.yaml"},
	} {
		want, err := helmRun(realHelm, args)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("helm", args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		_ = cmd.Run() // a non-zero exit is compared below, not an error here
		if cmd.Path == realHelm {
			t.Fatalf("`helm` on PATH is the real helm %s, not the shim", realHelm)
		}
		got := helmResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: cmd.ProcessState.ExitCode()}
		if msg := helmResultDiff(want, got); msg != "" {
			t.Errorf("helm %s: %s", strings.Join(args[3:], " "), msg)
		}
	}
}
