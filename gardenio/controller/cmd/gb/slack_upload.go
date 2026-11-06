package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// defaultMaxSlackUploadBytes bounds the file `gb slack upload` will send
// (50MB), matching the bridge's maxUploadSize. Slack accepts up to 1GB, but
// agent screenshots/diagrams are far smaller; a tight cap fails fast on a
// fat-fingered path before streaming a giant body to the bridge.
const defaultMaxSlackUploadBytes int64 = 50 * 1024 * 1024

// ── slack upload ────────────────────────────────────────────────────

// newSlackUploadCmd builds the `gb slack upload` command with a fresh flag
// set. A constructor (rather than a package-level literal) lets tests obtain
// isolated command instances whose flags don't leak across cases.
func newSlackUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <path>",
		Short: "Upload a local file to the thread so it renders inline (alias of gb say --file)",
		Long: `A permanent alias of 'gb say --file <path>' into this agent's home
thread: uploads a local file (screenshot, diagram, log) into the originating
thread via the bridge, so it appears INLINE in the conversation rather than as
a link the reader has to click.

Reads SLACK_THREAD_CHANNEL and SLACK_THREAD_TS from the environment (with the
same agent-bead fallback as 'gb slack reply'). The file goes to the bridge's
POST /api/conversation/say, which records it as a message bead and shares it
into the thread (Slack files.upload v2); a bridge that predates it gets the old
POST /api/slack/threads/upload.

Use --comment to post a message alongside the file, --title to override the
displayed file title (defaults to the basename), and --alt-text to set image
accessibility alt text.

Requires the Slack bot to hold the 'files:write' OAuth scope. If the scope is
missing, the bridge returns an error mentioning missing_scope — ask a Slack
workspace admin to add files:write and reinstall the app.

Against an old bridge, gb records a best-effort type=message bead (label
slack-upload) with triggered-by lineage itself, mirroring 'gb slack reply'.
Bead failures never fail the upload.

Examples:
  gb slack upload screenshot.png
  gb slack upload screenshot.png --comment "here's the login page"
  gb slack upload graph.png --title "Latency p95" --alt-text "p95 latency chart"`,
		Args: cobra.ExactArgs(1),
		RunE: runSlackUpload,
	}
	cmd.Flags().StringP("comment", "c", "", "message text posted alongside the file (initial_comment)")
	cmd.Flags().String("title", "", "file title shown in Slack (defaults to the basename)")
	cmd.Flags().String("alt-text", "", "accessibility alt text for images")
	cmd.Flags().Int64("max-bytes", defaultMaxSlackUploadBytes, "maximum file size to upload, in bytes")
	return cmd
}

var slackUploadCmd = newSlackUploadCmd()

// slackUploadResult mirrors the bridge uploadResponse.
type slackUploadResult struct {
	OK     bool   `json:"ok"`
	FileID string `json:"file_id"`
	Title  string `json:"title,omitempty"`
}

func runSlackUpload(cmd *cobra.Command, args []string) error {
	path := args[0]
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	bridgeURL, channel, threadTS, err := resolveSlackEnv()
	if err != nil {
		return err
	}

	comment, _ := cmd.Flags().GetString("comment")
	title, _ := cmd.Flags().GetString("title")
	altText, _ := cmd.Flags().GetString("alt-text")
	maxBytes, _ := cmd.Flags().GetInt64("max-bytes")
	if maxBytes <= 0 {
		maxBytes = defaultMaxSlackUploadBytes
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory; pass a file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("file %s is %d bytes (>--max-bytes=%d); raise --max-bytes if you really mean to upload it", path, info.Size(), maxBytes)
	}

	files, err := readSayFiles([]string{path}, maxBytes)
	if err != nil {
		return err
	}
	files[0].title, files[0].altText = title, altText
	req := newSayParams(comment)
	req.Channel, req.ThreadTS = channel, threadTS
	said, err := conversationSay(ctx, bridgeURL, req, files)
	if errors.Is(err, errBridgeWithoutConversation) {
		return runSlackUploadLegacy(ctx, bridgeURL, channel, threadTS, path, comment, title, altText)
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(said)
	} else {
		fmt.Printf("Uploaded %s to thread.\n", filepath.Base(path))
	}
	return nil
}

// runSlackUploadLegacy uploads through /api/slack/threads/upload, for a bridge
// with no /api/conversation/say, and records the upload's bead itself.
func runSlackUploadLegacy(ctx context.Context, bridgeURL, channel, threadTS, path, comment, title, altText string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	// Build the multipart body in memory. Files are bounded by maxBytes
	// (default 50MB), so buffering is acceptable and keeps the request simple.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("channel", channel); err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if err := mw.WriteField("thread_ts", threadTS); err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if comment != "" {
		if err := mw.WriteField("initial_comment", comment); err != nil {
			return fmt.Errorf("building request: %w", err)
		}
	}
	if title != "" {
		if err := mw.WriteField("title", title); err != nil {
			return fmt.Errorf("building request: %w", err)
		}
	}
	if altText != "" {
		if err := mw.WriteField("alt_txt", altText); err != nil {
			return fmt.Errorf("building request: %w", err)
		}
	}
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/threads/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("uploading to bridge: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(readLimited(resp.Body, 1024)))
		return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, msg)
	}

	var result slackUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	// Best-effort: record the upload as a type=message bead so it is reachable
	// from the agent / slack-thread graph in beads-viewer, mirroring
	// recordSlackReplyBead. Bead failures never fail the upload itself.
	recordSlackUploadBead(ctx, filepath.Base(path), comment, channel, threadTS, result.FileID)

	if jsonOutput {
		printJSON(result)
	} else {
		fmt.Printf("Uploaded %s to thread (file %s).\n", filepath.Base(path), result.FileID)
	}
	return nil
}

// readLimited reads up to n bytes from r, ignoring errors. Used to surface a
// short snippet of a non-200 bridge response body in error messages.
func readLimited(r io.Reader, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}

// buildSlackUploadFields builds the type=message bead Fields map for an
// outbound `gb slack upload`, using ONLY field keys registered for the message
// type (source_agent, slack_thread_channel, slack_thread_ts, text) — matching
// recordSlackReplyBead in slack.go. kbeads rejects the entire CreateBead with
// HTTP 400 if any unregistered key is present, so the filename lives in the
// bead title ("Uploaded <filename>") and the Slack file id is folded into the
// `text` field rather than carried in their own keys (ka-0000005057).
//
// text combines the comment and file id so the lineage is preserved:
//   - comment + fileID → "<comment> (file <id>)"
//   - comment only     → "<comment>"
//   - fileID only      → "file <id>"
//   - neither          → text omitted
func buildSlackUploadFields(actor, comment, channel, threadTS, fileID string) map[string]string {
	fields := map[string]string{
		"source_agent":         actor,
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
	}
	text := comment
	if fileID != "" {
		if text != "" {
			text += fmt.Sprintf(" (file %s)", fileID)
		} else {
			text = fmt.Sprintf("file %s", fileID)
		}
	}
	if text != "" {
		fields["text"] = text
	}
	return fields
}

// recordSlackUploadBead creates a type=message bead recording an outbound
// `gb slack upload`, stamping the same triggered-by lineage as
// recordSlackReplyBead (via writeSlackReplyLineage). The bead carries label
// `slack-upload`. Failures are reported on stderr but never fail the caller —
// the Slack upload has already succeeded.
func recordSlackUploadBead(ctx context.Context, filename, comment, channel, threadTS, fileID string) {
	if daemon == nil {
		return
	}

	title := "Uploaded " + filename
	if comment != "" {
		title += ": " + comment
	}
	if len(title) > 500 {
		title = title[:497] + "..."
	}

	fields := buildSlackUploadFields(actor, comment, channel, threadTS, fileID)
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack upload: warn: failed to marshal bead fields: %v\n", err)
		return
	}

	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"slack-upload", fmt.Sprintf("from:%s", actor)},
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: actor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack upload: warn: failed to create message bead: %v\n", err)
		return
	}

	writeSlackReplyLineage(ctx, beadID)

	if err := daemon.CloseBead(ctx, beadID, nil); err != nil {
		fmt.Fprintf(os.Stderr, "slack upload: warn: failed to close message bead %s: %v\n", beadID, err)
	}
}

func init() {
	slackCmd.AddCommand(slackUploadCmd)
}
