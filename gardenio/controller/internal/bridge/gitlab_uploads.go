package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
)

// gitlab_uploads.go — attach image bytes TO GitLab so an MR description can
// render them (ka-0000001730).
//
// WHY THIS EXISTS. Agents capture evidence with `gb image upload`, which stores
// the bytes in a PRIVATE S3 bucket and prints a viewer-proxied URL
// (`<viewer>/beads/image/<key>`). That URL is reachable only through the
// viewer's OIDC chain — the bucket is private and the proxy sits behind
// forwardauth. So pasting `![before](<viewer>/beads/image/…)` into an MR
// description renders a BROKEN IMAGE for every reviewer: GitLab's markdown
// renderer fetches the src anonymously and gets bounced.
//
// docs/research/beads-viewer-image-rendering.md:99-105 documents this for
// Slack ("a known limitation, not a defect") and :158-161 names this exact
// use case — "embedding in external docs that beads-viewer auth can't reach,
// e.g. GitLab MR descriptions opened by reviewers without OIDC" — as the
// motivation for making the bucket public. That option was REJECTED (:186-198)
// pending a written publishability policy.
//
// This is the other way to get the same outcome without loosening anything:
// upload the bytes to GitLab itself. `POST /projects/:id/uploads` returns a
// `/uploads/<hash>/<file>` path served to anyone who can already see the
// project — exactly the audience that can see the MR. No new public surface,
// no bucket policy change, and the image renders.
//
// The same shape is why `gb slack upload` renders inline and a pasted link
// does not: the bytes live where the reader already is.

// gitlabUploadMaxBytes caps a single upload. GitLab's own default attachment
// limit is 10 MB; refusing locally gives a clear error instead of a 413 with
// an HTML body.
const gitlabUploadMaxBytes = 10 << 20

// GitLabUpload is the subset of GitLab's upload response we use.
type GitLabUpload struct {
	// Alt is the filename GitLab derived, used as the image alt text.
	Alt string `json:"alt"`
	// URL is the project-relative path, e.g. "/uploads/<hash>/shot.png".
	URL string `json:"url"`
	// FullPath is the instance-relative path including the project, e.g.
	// "/-/project/<id>/uploads/<hash>/shot.png". Do NOT embed this in image
	// markdown: it is a fully-qualified router path that GFM's upload-detection
	// does not rewrite, so it reaches the router as a project blob path and
	// 404s (renders a broken-image icon). Use URL instead — see MarkdownRef.
	FullPath string `json:"full_path"`
	// Markdown is the ready-to-paste snippet GitLab suggests, e.g.
	// "![shot](/uploads/<hash>/shot.png)".
	Markdown string `json:"markdown"`
}

// UploadFile uploads raw bytes to a project's markdown-upload store and
// returns the paths GitLab will serve them from.
//
// The returned URL is servable to any user who can view the project, which is
// the same audience that can view its MRs — so an `![](url)` in an MR
// description renders for every reviewer, with no anonymous surface created.
//
// projectPath is the URL-encoded-on-the-wire full path ("group/sub/project");
// callers pass it unencoded and this method escapes it.
func (c *GitLabClient) UploadFile(ctx context.Context, projectPath, filename string, data []byte) (*GitLabUpload, error) {
	if c == nil {
		return nil, fmt.Errorf("gitlab upload: nil client")
	}
	if strings.TrimSpace(projectPath) == "" {
		return nil, fmt.Errorf("gitlab upload: empty project path")
	}
	filename = sanitizeUploadFilename(filename)
	if len(data) == 0 {
		return nil, fmt.Errorf("gitlab upload: %s is empty", filename)
	}
	if len(data) > gitlabUploadMaxBytes {
		return nil, fmt.Errorf("gitlab upload: %s is %d bytes, over the %d-byte limit",
			filename, len(data), gitlabUploadMaxBytes)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// Set the part's Content-Type explicitly: GitLab stores what we send, and
	// a generic application/octet-stream makes the browser download the file
	// instead of rendering it inline in the MR.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", uploadContentType(filename))
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("gitlab upload: building multipart body: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("gitlab upload: writing multipart body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("gitlab upload: closing multipart body: %w", err)
	}

	reqURL := c.baseURL + "/api/v4/projects/" + encodeProjectPath(projectPath) + "/uploads"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &body)
	if err != nil {
		return nil, fmt.Errorf("gitlab upload: creating request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab upload: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gitlab upload: reading response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gitlab upload: POST %s returned %d: %s",
			reqURL, resp.StatusCode, truncate(string(respBody), 512))
	}

	var up GitLabUpload
	if err := json.Unmarshal(respBody, &up); err != nil {
		return nil, fmt.Errorf("gitlab upload: decoding response: %w", err)
	}
	if up.URL == "" {
		return nil, fmt.Errorf("gitlab upload: response carried no url: %s", truncate(string(respBody), 256))
	}
	return &up, nil
}

// MarkdownRef returns the markdown that embeds this upload as an image.
//
// Prefers URL (the bare `/uploads/<hash>/f.png` form) over FullPath. GitLab's
// GFM upload-detection specifically recognizes the bare `/uploads/<sha>/<file>`
// form and rewrites it to the correct absolute URL at render time (the rendered
// HTML carries `<img data-canonical-src="/uploads/...">`). FullPath — the
// fully-qualified `/-/project/<id>/uploads/...` form — BYPASSES that rewriter
// and reaches the router as a project blob path, which 404s and renders as a
// broken-image icon (ka-0000007083). Falls back to FullPath if URL is empty,
// then to GitLab's own suggested Markdown.
func (u *GitLabUpload) MarkdownRef(alt string) string {
	if u == nil {
		return ""
	}
	if alt == "" {
		alt = u.Alt
	}
	if alt == "" {
		alt = "screenshot"
	}
	path := u.URL
	if path == "" {
		path = u.FullPath
	}
	if path == "" {
		return u.Markdown
	}
	return fmt.Sprintf("![%s](%s)", alt, path)
}

// encodeProjectPath percent-encodes a project path for use as a GitLab :id
// path segment. GitLab requires the slashes encoded as %2F.
func encodeProjectPath(projectPath string) string {
	return strings.ReplaceAll(strings.Trim(projectPath, "/"), "/", "%2F")
}

// sanitizeUploadFilename reduces an arbitrary path to a safe base filename.
// Guards against a caller passing a full local path (leaking the agent's
// directory layout into the MR) or a traversal sequence.
func sanitizeUploadFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) || name == ".." {
		return "upload.png"
	}
	return name
}

// uploadContentType maps a filename extension to the Content-Type GitLab
// should store. Defaults to PNG rather than octet-stream: the overwhelmingly
// common case here is a screenshot, and octet-stream makes GitLab serve the
// file as a download instead of rendering it inline.
func uploadContentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".webm":
		return "video/webm"
	case ".mp4":
		return "video/mp4"
	default:
		return "image/png"
	}
}
