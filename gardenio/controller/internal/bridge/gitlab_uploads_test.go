package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// uploadServer stands in for GitLab's POST /projects/:id/uploads, capturing
// what the client actually sent on the wire.
type uploadCapture struct {
	path        string
	privToken   string
	filename    string
	partType    string
	body        []byte
	contentType string
}

func newUploadServer(t *testing.T, status int, respBody string) (*GitLabClient, *uploadCapture) {
	t.Helper()
	cap := &uploadCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI, not URL.Path: the server decodes %2F back to "/", and
		// whether the project path arrives ENCODED is exactly what we assert
		// (GitLab 404s an unencoded one).
		cap.path = r.RequestURI
		cap.privToken = r.Header.Get("PRIVATE-TOKEN")
		cap.contentType = r.Header.Get("Content-Type")

		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			mr := multipart.NewReader(r.Body, params["boundary"])
			if part, err := mr.NextPart(); err == nil {
				cap.filename = part.FileName()
				cap.partType = part.Header.Get("Content-Type")
				cap.body, _ = io.ReadAll(part)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)

	return NewGitLabClient(GitLabClientConfig{
		BaseURL: srv.URL,
		Token:   "test-token",
		Logger:  quietLogger(),
	}), cap
}

const okUploadResp = `{
  "alt": "shot.png",
  "url": "/uploads/deadbeef/shot.png",
  "full_path": "/acme/monorepo/uploads/deadbeef/shot.png",
  "markdown": "![shot.png](/uploads/deadbeef/shot.png)"
}`

func TestGitLabUploadFile_PostsMultipartToProjectUploads(t *testing.T) {
	c, cap := newUploadServer(t, http.StatusCreated, okUploadResp)

	up, err := c.UploadFile(context.Background(), "acme/monorepo", "shot.png", []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	if want := "/api/v4/projects/acme%2Fmonorepo/uploads"; cap.path != want {
		t.Errorf("path = %q, want %q (the project path must be %%2F-encoded)", cap.path, want)
	}
	if cap.privToken != "test-token" {
		t.Errorf("PRIVATE-TOKEN = %q, want the configured token", cap.privToken)
	}
	if !strings.HasPrefix(cap.contentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", cap.contentType)
	}
	if cap.filename != "shot.png" {
		t.Errorf("uploaded filename = %q, want shot.png", cap.filename)
	}
	if string(cap.body) != "PNGDATA" {
		t.Errorf("uploaded bytes = %q, want PNGDATA", cap.body)
	}
	if up.FullPath != "/acme/monorepo/uploads/deadbeef/shot.png" {
		t.Errorf("full_path not parsed: %#v", up)
	}
}

// TestGitLabUploadFile_SetsImageContentType: an octet-stream part makes GitLab
// serve the file as a download instead of rendering it in the MR — which
// defeats the entire purpose.
func TestGitLabUploadFile_SetsImageContentType(t *testing.T) {
	for name, want := range map[string]string{
		"shot.png":   "image/png",
		"shot.jpg":   "image/jpeg",
		"shot.jpeg":  "image/jpeg",
		"shot.gif":   "image/gif",
		"shot.webp":  "image/webp",
		"clip.webm":  "video/webm",
		"noext":      "image/png", // screenshots are the overwhelming case
		"weird.xyz":  "image/png",
		"vector.svg": "image/svg+xml",
	} {
		t.Run(name, func(t *testing.T) {
			c, cap := newUploadServer(t, http.StatusCreated, okUploadResp)
			if _, err := c.UploadFile(context.Background(), "g/p", name, []byte("x")); err != nil {
				t.Fatalf("UploadFile: %v", err)
			}
			if cap.partType != want {
				t.Errorf("part Content-Type = %q, want %q", cap.partType, want)
			}
		})
	}
}

// TestGitLabUploadFile_SanitizesFilename: a caller passing a full local path
// would otherwise leak the agent's directory layout into the MR.
func TestGitLabUploadFile_SanitizesFilename(t *testing.T) {
	for in, want := range map[string]string{
		"/home/agent/work/tmp/before.png": "before.png",
		"../../../etc/passwd":             "passwd",
		"":                                "upload.png",
		"   ":                             "upload.png",
		"..":                              "upload.png",
	} {
		c, cap := newUploadServer(t, http.StatusCreated, okUploadResp)
		if _, err := c.UploadFile(context.Background(), "g/p", in, []byte("x")); err != nil {
			t.Fatalf("UploadFile(%q): %v", in, err)
		}
		if cap.filename != want {
			t.Errorf("filename %q sanitized to %q, want %q", in, cap.filename, want)
		}
	}
}

func TestGitLabUploadFile_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		project string
		data    []byte
		wantErr string
	}{
		{"empty project", "", []byte("x"), "empty project path"},
		{"empty data", "g/p", nil, "is empty"},
		{"oversize", "g/p", make([]byte, gitlabUploadMaxBytes+1), "over the"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := newUploadServer(t, http.StatusCreated, okUploadResp)
			_, err := c.UploadFile(context.Background(), tc.project, "shot.png", tc.data)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
			if cap.path != "" {
				t.Error("want the request refused locally, but it hit the server")
			}
		})
	}
}

func TestGitLabUploadFile_SurfacesAPIError(t *testing.T) {
	c, _ := newUploadServer(t, http.StatusForbidden, `{"message":"403 Forbidden"}`)
	_, err := c.UploadFile(context.Background(), "g/p", "shot.png", []byte("x"))
	if err == nil {
		t.Fatal("want an error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should carry the status code", err)
	}
}

func TestGitLabUploadFile_RejectsResponseWithoutURL(t *testing.T) {
	c, _ := newUploadServer(t, http.StatusCreated, `{"alt":"shot.png"}`)
	_, err := c.UploadFile(context.Background(), "g/p", "shot.png", []byte("x"))
	if err == nil {
		t.Fatal("want an error when the response carries no url — there is nothing to embed")
	}
	if !strings.Contains(err.Error(), "no url") {
		t.Errorf("error %q should say the url was missing", err)
	}
}

func TestGitLabUploadFile_NilClient(t *testing.T) {
	var c *GitLabClient
	if _, err := c.UploadFile(context.Background(), "g/p", "x.png", []byte("x")); err == nil {
		t.Error("want an error from a nil client, not a panic")
	}
}

// --- MarkdownRef. ------------------------------------------------------------

// TestGitLabUploadMarkdownRef_PrefersURL: the bare /uploads/<sha>/<file> form is
// the ONLY form GitLab's GFM upload-detection rewrites to a real inline image.
// The fully-qualified FullPath (/-/project/<id>/uploads/...) bypasses that
// rewriter and renders a broken-image icon (ka-0000007083), so MarkdownRef must
// emit URL — never FullPath — when URL is present.
func TestGitLabUploadMarkdownRef_PrefersURL(t *testing.T) {
	up := &GitLabUpload{
		Alt:      "shot.png",
		URL:      "/uploads/deadbeef/shot.png",
		FullPath: "/-/project/79511414/uploads/deadbeef/shot.png",
		Markdown: "![shot.png](/uploads/deadbeef/shot.png)",
	}
	got := up.MarkdownRef("before")
	if want := "![before](/uploads/deadbeef/shot.png)"; got != want {
		t.Errorf("MarkdownRef = %q, want %q", got, want)
	}
	// Regression guard: the emitted path must be the bare upload form, never
	// the fully-qualified /-/project/<id>/... form that 404s.
	if strings.Contains(got, "/-/project/") {
		t.Errorf("MarkdownRef emitted a /-/project/ path that GFM does not rewrite: %q", got)
	}
	if !strings.Contains(got, "(/uploads/") {
		t.Errorf("MarkdownRef must embed a bare /uploads/ path: %q", got)
	}
}

func TestGitLabUploadMarkdownRef_Fallbacks(t *testing.T) {
	tests := []struct {
		name string
		up   *GitLabUpload
		alt  string
		want string
	}{
		{
			name: "url is preferred over full_path",
			up:   &GitLabUpload{URL: "/uploads/h/f.png", FullPath: "/-/project/1/uploads/h/f.png"},
			alt:  "after",
			want: "![after](/uploads/h/f.png)",
		},
		{
			name: "no url falls back to full_path",
			up:   &GitLabUpload{FullPath: "/-/project/1/uploads/h/f.png"},
			alt:  "after",
			want: "![after](/-/project/1/uploads/h/f.png)",
		},
		{
			name: "empty alt uses the upload's own alt",
			up:   &GitLabUpload{Alt: "shot.png", URL: "/uploads/h/f.png"},
			want: "![shot.png](/uploads/h/f.png)",
		},
		{
			name: "no alt anywhere gets a sane default",
			up:   &GitLabUpload{URL: "/uploads/h/f.png"},
			want: "![screenshot](/uploads/h/f.png)",
		},
		{
			name: "no paths falls back to GitLab's own markdown",
			up:   &GitLabUpload{Markdown: "![x](/uploads/h/x.png)"},
			want: "![x](/uploads/h/x.png)",
		},
		{name: "nil upload", up: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.up.MarkdownRef(tc.alt); got != tc.want {
				t.Errorf("MarkdownRef = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeProjectPath(t *testing.T) {
	for in, want := range map[string]string{
		"acme/monorepo":            "acme%2Fmonorepo",
		"/acme/platform/gardenio/": "acme%2Fplatform%2Fgardenio",
		"single":                       "single",
	} {
		if got := encodeProjectPath(in); got != want {
			t.Errorf("encodeProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGitLabUpload_ResponseShapeMatchesGitLab documents the wire contract this
// client depends on, so a GitLab API change surfaces here rather than as a
// silently-empty embed in a customer-visible MR.
func TestGitLabUpload_ResponseShapeMatchesGitLab(t *testing.T) {
	var up GitLabUpload
	if err := json.Unmarshal([]byte(okUploadResp), &up); err != nil {
		t.Fatal(err)
	}
	if up.Alt == "" || up.URL == "" || up.FullPath == "" || up.Markdown == "" {
		t.Errorf("every documented field should decode: %#v", up)
	}
}

// TestGitLabUploadMarkdownRef_RendersInlineImage is the empirical regression
// guard for ka-0000007083: it feeds the markdown MarkdownRef produces through
// GitLab's own /api/v4/markdown renderer (project-scoped) and asserts the
// rendered HTML carries a real inline <img>, not a `no-attachment-icon` broken
// link. It is opt-in — it SKIPS LOUD unless GITLAB_TOKEN, GITLAB_BASE_URL and
// GITLAB_MARKDOWN_PROJECT are all set — so the fast unit tests above are what
// gate every MR, and this exercises the true GFM behavior when a token exists.
//
// Env contract:
//   - GITLAB_TOKEN            PRIVATE-TOKEN with api scope on the project
//   - GITLAB_BASE_URL         e.g. https://gitlab.com (no trailing /api/v4)
//   - GITLAB_MARKDOWN_PROJECT numeric id or url-encoded path of a project the
//     token can read (used as the :id in POST /projects/:id/markdown)
//
// It does NOT upload anything — a real upload sha is unnecessary to prove the
// rewrite rule; GFM detects the /uploads/<sha>/<file> SHAPE and emits the
// data-canonical-src <img> regardless of whether the blob exists.
func TestGitLabUploadMarkdownRef_RendersInlineImage(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GITLAB_TOKEN"))
	baseURL := strings.TrimSpace(os.Getenv("GITLAB_BASE_URL"))
	project := strings.TrimSpace(os.Getenv("GITLAB_MARKDOWN_PROJECT"))
	if token == "" || baseURL == "" || project == "" {
		t.Skip("SKIP (LOUD): GITLAB_TOKEN/GITLAB_BASE_URL/GITLAB_MARKDOWN_PROJECT unset — this run did NOT assert the produced markdown renders as an inline image against the real GitLab GFM renderer")
	}

	up := &GitLabUpload{
		Alt:      "comp-1.png",
		URL:      "/uploads/124a7e37deadbeefcafe0000/comp-1.png",
		FullPath: "/-/project/79511414/uploads/124a7e37deadbeefcafe0000/comp-1.png",
		Markdown: "![comp-1.png](/uploads/124a7e37deadbeefcafe0000/comp-1.png)",
	}
	md := up.MarkdownRef("evidence")

	reqBody, err := json.Marshal(map[string]any{"text": md, "gfm": true})
	if err != nil {
		t.Fatal(err)
	}
	reqURL := strings.TrimRight(baseURL, "/") + "/api/v4/projects/" + encodeProjectPath(project) + "/markdown"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", reqURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		t.Fatalf("GitLab markdown API returned %d: %s", resp.StatusCode, truncate(string(body), 512))
	}
	var rendered struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(body, &rendered); err != nil {
		t.Fatalf("decoding markdown response: %v (body: %s)", err, truncate(string(body), 256))
	}

	if strings.Contains(rendered.HTML, "no-attachment-icon") {
		t.Errorf("produced markdown %q rendered a broken-image link (no-attachment-icon): %s", md, rendered.HTML)
	}
	if !strings.Contains(rendered.HTML, "data-canonical-src=\"/uploads/") {
		t.Errorf("produced markdown %q did NOT render an inline <img data-canonical-src=\"/uploads/...\">: %s", md, rendered.HTML)
	}
}
