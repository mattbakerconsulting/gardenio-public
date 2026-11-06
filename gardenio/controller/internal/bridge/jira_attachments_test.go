package bridge

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type attachCapture struct {
	path        string
	method      string
	auth        string
	xsrfToken   string
	filename    string
	partType    string
	body        []byte
	contentType string
	hits        int
}

func newAttachServer(t *testing.T, status int, respBody string) (*JiraClient, *attachCapture) {
	t.Helper()
	cap := &attachCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.hits++
		cap.path = r.URL.Path
		cap.method = r.Method
		cap.auth = r.Header.Get("Authorization")
		cap.xsrfToken = r.Header.Get("X-Atlassian-Token")
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

	return NewJiraClient(JiraClientConfig{
		BaseURL:  srv.URL,
		Email:    "bot@example.com",
		APIToken: "tok",
		Logger:   quietLogger(),
	}), cap
}

const okAttachResp = `[{
  "id": "10501",
  "filename": "before.png",
  "mimeType": "image/png",
  "size": 7,
  "content": "https://jira.example.com/rest/api/3/attachment/content/10501"
}]`

func TestJiraAddAttachment_PostsMultipartWithXSRFHeader(t *testing.T) {
	c, cap := newAttachServer(t, http.StatusOK, okAttachResp)

	got, err := c.AddAttachment(context.Background(), "GBFSM-42", "before.png", []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}

	if cap.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cap.method)
	}
	if want := "/rest/api/3/issue/GBFSM-42/attachments"; cap.path != want {
		t.Errorf("path = %q, want %q", cap.path, want)
	}
	// Jira rejects this endpoint without the XSRF opt-out regardless of auth —
	// forgetting it is the classic first-attempt failure.
	if cap.xsrfToken != "no-check" {
		t.Errorf("X-Atlassian-Token = %q, want no-check (Jira 403s the upload without it)", cap.xsrfToken)
	}
	if !strings.HasPrefix(cap.auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic auth", cap.auth)
	}
	if !strings.HasPrefix(cap.contentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", cap.contentType)
	}
	if cap.filename != "before.png" || string(cap.body) != "PNGDATA" {
		t.Errorf("part = %q / %q, want before.png / PNGDATA", cap.filename, cap.body)
	}
	if cap.partType != "image/png" {
		t.Errorf("part Content-Type = %q, want image/png", cap.partType)
	}

	// The ID is the whole point — an ADF media node keys on it.
	if got.ID != "10501" {
		t.Errorf("attachment id = %q, want 10501 (that is what adfMediaSingle references)", got.ID)
	}
	if got.Filename != "before.png" || got.MimeType != "image/png" || got.Size != 7 {
		t.Errorf("attachment record not fully parsed: %#v", got)
	}
}

func TestJiraAddAttachment_EscapesIssueKey(t *testing.T) {
	c, cap := newAttachServer(t, http.StatusOK, okAttachResp)
	if _, err := c.AddAttachment(context.Background(), "  GBFSM-42  ", "x.png", []byte("x")); err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if want := "/rest/api/3/issue/GBFSM-42/attachments"; cap.path != want {
		t.Errorf("path = %q, want %q (key should be trimmed)", cap.path, want)
	}
}

func TestJiraAddAttachment_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		data    []byte
		wantErr string
	}{
		{"empty key", "", []byte("x"), "empty issue key"},
		{"blank key", "   ", []byte("x"), "empty issue key"},
		{"empty data", "GBFSM-1", nil, "is empty"},
		{"oversize", "GBFSM-1", make([]byte, jiraAttachmentMaxBytes+1), "over the"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := newAttachServer(t, http.StatusOK, okAttachResp)
			_, err := c.AddAttachment(context.Background(), tc.key, "x.png", tc.data)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
			if cap.hits != 0 {
				t.Error("want the upload refused locally, but it hit Jira")
			}
		})
	}
}

// TestJiraAddAttachment_SanitizesFilename: an agent passing a full local path
// would leak its directory layout onto a customer-adjacent ticket.
func TestJiraAddAttachment_SanitizesFilename(t *testing.T) {
	for in, want := range map[string]string{
		"/home/agent/work/tmp/after.png": "after.png",
		"../../secrets.png":              "secrets.png",
		"":                               "attachment.png",
		"..":                             "attachment.png",
	} {
		c, cap := newAttachServer(t, http.StatusOK, okAttachResp)
		if _, err := c.AddAttachment(context.Background(), "GBFSM-1", in, []byte("x")); err != nil {
			t.Fatalf("AddAttachment(%q): %v", in, err)
		}
		if cap.filename != want {
			t.Errorf("filename %q sanitized to %q, want %q", in, cap.filename, want)
		}
	}
}

func TestJiraAddAttachment_SurfacesAPIError(t *testing.T) {
	c, _ := newAttachServer(t, http.StatusForbidden, `{"errorMessages":["no permission"]}`)
	_, err := c.AddAttachment(context.Background(), "GBFSM-1", "x.png", []byte("x"))
	if err == nil {
		t.Fatal("want an error on 403")
	}
	apiErr, ok := asJiraAPIError(err)
	if !ok {
		t.Fatalf("want a *JiraAPIError so callers can branch on the status, got %T", err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
}

func TestJiraAddAttachment_RejectsUnusableResponses(t *testing.T) {
	tests := []struct {
		name    string
		resp    string
		wantErr string
	}{
		{"empty array", `[]`, "no attachment record"},
		{"no id", `[{"filename":"x.png"}]`, "empty attachment id"},
		{"not json", `<html>nope</html>`, "decoding response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newAttachServer(t, http.StatusOK, tc.resp)
			_, err := c.AddAttachment(context.Background(), "GBFSM-1", "x.png", []byte("x"))
			if err == nil {
				t.Fatal("want an error — without an id there is nothing a media node can reference")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestJiraAddAttachment_NilClient(t *testing.T) {
	var c *JiraClient
	if _, err := c.AddAttachment(context.Background(), "GBFSM-1", "x.png", []byte("x")); err == nil {
		t.Error("want an error from a nil client, not a panic")
	}
}

// --- ADF media node. ---------------------------------------------------------

func TestAdfMediaSingle_Shape(t *testing.T) {
	raw, err := json.Marshal(adfMediaSingle("10501"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{
		`"type":"mediaSingle"`,
		`"layout":"center"`,
		`"type":"media"`,
		`"id":"10501"`,
		`"type":"file"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("media node missing %s: %s", want, got)
		}
	}
	// An empty collection attr makes Jira Cloud 400; omitting it lets Jira
	// resolve the collection from the issue.
	if strings.Contains(got, `"collection":""`) {
		t.Errorf("must not send an empty collection attr (Jira 400s it): %s", got)
	}
}
