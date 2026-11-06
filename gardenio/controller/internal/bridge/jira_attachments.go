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
	"net/url"
	"path/filepath"
	"strings"
)

// jira_attachments.go — put screenshot bytes ON a Jira issue so they render
// inline in its Description or a comment (ka-0000001730).
//
// WHY THIS EXISTS. Same root cause as gitlab_uploads.go: `gb image upload`
// stores evidence in a PRIVATE bucket behind the viewer's OIDC chain, so a
// `<viewer>/beads/image/<key>` URL pasted into Jira renders as a broken image
// for every reader. Jira's renderer fetches anonymously and gets bounced.
//
// Jira's answer is its own attachment store. Attach the bytes to the issue,
// take the returned attachment id, and reference it from an ADF `media` node
// (adfMediaSingle, jira.go). The image then renders for exactly the audience
// that can already see the issue — no public bucket, no new anonymous surface.
//
// ORDERING. Jira requires the file to exist on the issue BEFORE the ADF can
// point at it, because the media node keys on the attachment id. So a
// screenshot-bearing write is two-phase: attach, then write the body
// referencing what came back. CreateGBFSMTicket callers must create the ticket
// first (there is no issue to attach to otherwise), then attach, then edit the
// description in.
//
// GOVERNANCE. AddAttachment is a governed write (FieldAttachment,
// jira_allowlist.go) and is registered in the write-policy AST guard
// (jira_no_issue_lifecycle_write_test.go). It is a SUB-RESOURCE write — it can
// only ever add a file to an issue that already exists. It creates no issue and
// deletes nothing, so the zero-FSM-create invariant (epic ka-0000002067) is
// untouched.

// jiraAttachmentMaxBytes caps a single attachment. Jira Cloud's default
// per-file limit is 10 MB; refusing locally produces a clear error instead of a
// 413 carrying an HTML body.
const jiraAttachmentMaxBytes = 10 << 20

// JiraAttachmentUpload is the subset of Jira's attachment response we use.
// The ID is what an ADF media node references.
type JiraAttachmentUpload struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	// Content is the authenticated download URL. Useful for logging and for a
	// text fallback; NOT usable as an <img> src (it is auth-gated, which is the
	// whole reason the media node exists).
	Content string `json:"content"`
}

// AddAttachment uploads a file to an existing issue and returns its attachment
// record. The returned ID is what adfMediaSingle needs to render the image
// inline in a Description or comment.
//
// Jira requires the X-Atlassian-Token: no-check header on this endpoint (its
// XSRF guard); without it the request is rejected regardless of auth.
//
// The response is an ARRAY (Jira accepts multiple files per request); this
// method uploads exactly one and returns the single element.
func (c *JiraClient) AddAttachment(ctx context.Context, issueKey, filename string, data []byte) (*JiraAttachmentUpload, error) {
	if c == nil {
		return nil, fmt.Errorf("jira attachment: nil client")
	}
	if err := c.guardWrite("add attachment", issueKey); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(issueKey)
	if key == "" {
		return nil, fmt.Errorf("jira attachment: empty issue key")
	}
	filename = sanitizeAttachmentFilename(filename)
	if len(data) == 0 {
		return nil, fmt.Errorf("jira attachment: %s is empty", filename)
	}
	if len(data) > jiraAttachmentMaxBytes {
		return nil, fmt.Errorf("jira attachment: %s is %d bytes, over the %d-byte limit",
			filename, len(data), jiraAttachmentMaxBytes)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", uploadContentType(filename))
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("jira attachment: building multipart body: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("jira attachment: writing multipart body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("jira attachment: closing multipart body: %w", err)
	}

	reqURL := c.baseURL + "/rest/api/3/issue/" + url.PathEscape(key) + "/attachments"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &body)
	if err != nil {
		return nil, fmt.Errorf("jira attachment: creating request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	// Jira's XSRF guard on this endpoint. Omitting it fails the request even
	// with valid credentials.
	req.Header.Set("X-Atlassian-Token", "no-check")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira attachment: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("jira attachment: reading response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &JiraAPIError{
			Method:     http.MethodPost,
			Path:       "/rest/api/3/issue/" + key + "/attachments",
			StatusCode: resp.StatusCode,
			Body:       truncate(string(respBody), 512),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var out []JiraAttachmentUpload
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("jira attachment: decoding response: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jira attachment: %s upload returned no attachment record", filename)
	}
	if out[0].ID == "" {
		return nil, fmt.Errorf("jira attachment: %s upload returned an empty attachment id (nothing for a media node to reference)", filename)
	}
	return &out[0], nil
}

// sanitizeAttachmentFilename reduces an arbitrary path to a safe base
// filename, so a caller passing a full local path does not leak the agent's
// directory layout onto a customer-adjacent ticket.
func sanitizeAttachmentFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) || name == ".." {
		return "attachment.png"
	}
	return name
}
