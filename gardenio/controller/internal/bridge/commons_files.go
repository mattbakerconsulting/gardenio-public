package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// Commons attachments into Slack as bytes (gd-qnF6mJZtkQ, decision
// gd-nQK8UX7V0A "fetch"). A Commons message's files field records where each
// file opens in the viewer, which a Slack reader would have to sign in to. The
// bridge instead fetches the bytes from beads-viewer's machine file route
// (GET /internal/commons/f/..., routeMachine) on its own machine principal and
// hands them to the Router, which uploads them into Slack. A file it does not
// fetch (too large, not a Commons file of this channel, or a failed fetch)
// stays a viewer link, as before.

// Fetch bounds.
const (
	// commonsFileMaxBytes caps one fetched file: the viewer's own cap on an
	// attachment (commons.fileMaxBytes).
	commonsFileMaxBytes = 10 << 20
	// commonsMessageMaxBytes caps the files one message fetches together: the
	// viewer's cap on one say or reply (commons.uploadMaxBytes).
	commonsMessageMaxBytes = 25 << 20
	// commonsFileTimeout is the time one fetch has.
	commonsFileTimeout = 20 * time.Second
)

// commonsFilePath is the file route's path prefix in a files-field URL, and
// commonsMachineFilePath the machine route's.
const (
	commonsFilePath        = "/garden/c/f/"
	commonsMachineFilePath = "/internal/commons/f/"
)

// CommonsFileFetcher fetches Commons attachments from beads-viewer.
type CommonsFileFetcher struct {
	base     string // the viewer's in-cluster URL, no trailing slash
	tokens   beadsapi.TokenSource
	client   *http.Client
	maxFile  int64
	maxTotal int64
	logger   *slog.Logger
}

// NewCommonsFileFetcher returns a fetcher that reads files from the viewer at
// viewerURL on tokens' bearer, or nil when either is missing: the mirror then
// posts every file as a link.
func NewCommonsFileFetcher(viewerURL string, tokens beadsapi.TokenSource, logger *slog.Logger) *CommonsFileFetcher {
	viewerURL = strings.TrimRight(strings.TrimSpace(viewerURL), "/")
	if viewerURL == "" || tokens == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CommonsFileFetcher{
		base: viewerURL, tokens: tokens, client: &http.Client{Timeout: commonsFileTimeout},
		maxFile: commonsFileMaxBytes, maxTotal: commonsMessageMaxBytes, logger: logger,
	}
}

// machineFileURL is the machine route for ref, a file of channel chID in land,
// or false when ref is not one: another host's link (a Slack permalink), a
// malformed path, or a file of another land or channel, which a message must
// not carry into this channel's Slack.
func (f *CommonsFileFetcher) machineFileURL(land, chID string, ref fileRef) (string, bool) {
	gotLand, gotCh, path, ok := CommonsFileRoute(ref.URL)
	if !ok || gotLand != land || gotCh != chID {
		return "", false
	}
	return f.base + path, true
}

// CommonsFileRoute reads a Commons file URL, the file route's link a message's
// files field records: the land and channel id it was posted in, and the
// machine route path that serves its bytes (/internal/commons/f/...), which
// the bridge and `gb commons file` join to the viewer's in-cluster URL. It is
// false for anything else: another host's link (a Slack permalink) or a
// malformed path.
func CommonsFileRoute(raw string) (land, chID, machinePath string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false
	}
	_, rest, ok := strings.Cut(u.EscapedPath(), commonsFilePath)
	if !ok {
		return "", "", "", false
	}
	segs := strings.Split(rest, "/")
	if len(segs) != 4 {
		return "", "", "", false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", "", "", false
		}
	}
	land, err1 := url.PathUnescape(segs[0])
	chID, err2 := url.PathUnescape(segs[1])
	if err1 != nil || err2 != nil || !isFileID(segs[2]) {
		return "", "", "", false
	}
	return land, chID, commonsMachineFilePath + rest, true
}

// isFileID reports whether s is a Commons file id: 32 lowercase hex digits.
func isFileID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Fetch reads the files of a message in channel chID of land, in order. It
// returns the bytes it read, and the refs it did not, which stay links. A nil
// fetcher reads none.
func (f *CommonsFileFetcher) Fetch(ctx context.Context, land, chID string, refs []fileRef) ([]conversation.File, []fileRef) {
	if f == nil || len(refs) == 0 {
		return nil, refs
	}
	var got []conversation.File
	var linked []fileRef
	var total int64
	var token string
	for i, ref := range refs {
		u, ok := f.machineFileURL(land, chID, ref)
		if !ok || int64(ref.Size) > f.maxFile || total+int64(ref.Size) > f.maxTotal {
			linked = append(linked, ref)
			continue
		}
		if token == "" {
			t, err := f.tokens.Token(ctx)
			if err != nil || t == "" {
				f.logger.Warn("commons-files: no machine token, posting links", "error", err)
				return got, append(linked, refs[i:]...)
			}
			token = t
		}
		body, err := f.fetch(ctx, u, token, f.maxTotal-total)
		if err != nil {
			f.logger.Warn("commons-files: fetch failed, posting a link", "file", ref.Name, "error", err)
			linked = append(linked, ref)
			continue
		}
		total += int64(len(body))
		got = append(got, conversation.File{Name: ref.Name, MimeType: ref.MimeType, Data: body})
	}
	return got, linked
}

// errFileTooLarge is a fetched file over its cap.
var errFileTooLarge = errors.New("file over the size cap")

// fetch GETs one file, at most min(maxFile, room) bytes of it.
func (f *CommonsFileFetcher) fetch(ctx context.Context, u, token string, room int64) ([]byte, error) {
	limit := min(f.maxFile, room)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("viewer answered %d", resp.StatusCode)
	}
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n > limit {
		return nil, errFileTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errFileTooLarge
	}
	if len(body) == 0 {
		return nil, errors.New("empty file")
	}
	return body, nil
}
