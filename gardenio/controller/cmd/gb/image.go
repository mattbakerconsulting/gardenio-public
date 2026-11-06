package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

// Default upload cap. 10 MiB is generous for screenshots and typical doc
// attachments; larger blobs should go through a deliberate path (e.g.
// `gb audit fetch` or a direct aws-cli upload).
const defaultMaxImageBytes int64 = 10 * 1024 * 1024

// s3Uploader is the subset of the S3 client surface that `gb image upload`
// needs. Defining it as an interface lets tests inject an in-memory fake
// without spinning up an HTTP server.
type s3Uploader interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// newS3Uploader is the production hook; tests override it.
var newS3Uploader = func(ctx context.Context) (s3Uploader, error) {
	return newS3Client(ctx)
}

// ── gb image ────────────────────────────────────────────────────────────

var imageCmd = &cobra.Command{
	Use:     "image",
	Short:   "Upload and link local images to S3",
	GroupID: "orchestration",
	Long: `Upload a local image (or any small binary) to the same S3 bucket and
per-instance prefix that audit/coop already use, and return the viewer-proxied
URL — a link a logged-in human can open in the beads-viewer.

The raw S3 bucket is PRIVATE; a bare bucket URL 403s in a browser. Share the
proxied URL this command prints, not the "storage location" line.

The proxied URL is browser-session-only: it does NOT render in a GitLab MR, a
Jira ticket, or a Slack unfurl (see 'gb image upload --help'). For those, use
'gb mr embed' / 'gb attachments upload' / 'gb slack upload'. To read an
uploaded image back without a browser, use 'gb image fetch'.`,
}

// ── gb image upload ─────────────────────────────────────────────────────

var imageUploadCmd = &cobra.Command{
	Use:   "upload <path>",
	Short: "Upload a local file to S3 and print the viewer-proxied URL",
	Long: `Upload <path> to s3://<bucket>/<COOP_S3_PREFIX>/images/<scope>/<sha8>-<basename>
(<bucket> is COOP_S3_IMAGES_BUCKET when set, else COOP_S3_BUCKET) and print the viewer-proxied URL: <BEADS_VIEWER_URL>/image/<key>.

READ THIS BEFORE PASTING THE URL ANYWHERE.

The printed URL opens for a HUMAN IN A BROWSER WITH A LIVE OIDC SESSION, and
for nothing else. An anonymous GET is answered with a 303 into GitLab OAuth by
AWS Verified Access, before any gardenio code runs. So every non-interactive
consumer fails on it:

  * an <img> in a GitLab MR description  -> broken image
  * an image in a Jira ticket            -> broken image
  * Slack unfurl                         -> plain link, no preview
  * curl / an agent re-reading evidence  -> 303, no bytes

This is by design (the bucket is private and edge authn is the security
boundary), not a defect — but it means the URL is NOT a general-purpose
shareable link. To make an image actually RENDER somewhere, use the
destination-native command, which uploads the bytes to where the reader
already is:

  GitLab MR  ->  gb mr embed <mr> <file>...
  Jira issue ->  gb attachments upload <issue> <file>...
  Slack      ->  gb slack upload <file>
  read back  ->  gb image fetch <url-or-key>

Use THIS command for the durable bead-side record and for links a logged-in
human will click in the viewer. The raw S3 location is printed to stderr as a
secondary "storage location" line, marked not-human-viewable; it is even less
shareable (a bare bucket URL 403s outright).

The <scope> segment is the --bead id when provided, the --bundle id when only
that is provided, or today's UTC date (YYYY-MM-DD) otherwise. The sha8 prefix
is the first 8 hex chars of the file's SHA-256, so re-uploading the same bytes
is idempotent.

When --bundle <id> is set, a 'type=doc format=image' bead is created with
fields.content_url pointing at the upload, parented to the bundle (same shape
as 'gb bundle add --content-url').

Examples:
  gb image upload screenshot.png
  gb image upload screenshot.png --bead ka-0000005974
  gb image upload screenshot.png --bundle ka-Bundle1 --headline "Login flow"
  gb image upload payload.bin --content-type application/octet-stream`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		beadID, _ := cmd.Flags().GetString("bead")
		bundleID, _ := cmd.Flags().GetString("bundle")
		contentType, _ := cmd.Flags().GetString("content-type")
		maxBytes, _ := cmd.Flags().GetInt64("max-bytes")
		headline, _ := cmd.Flags().GetString("headline")

		bucket := s3ImagesBucket()
		if bucket == "" {
			return fmt.Errorf("COOP_S3_BUCKET is not set")
		}
		if maxBytes <= 0 {
			maxBytes = defaultMaxImageBytes
		}

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory; pass a file", path)
		}
		if info.Size() > maxBytes {
			return fmt.Errorf("file %s is %d bytes (>--max-bytes=%d); raise --max-bytes if you really mean to upload it", path, info.Size(), maxBytes)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		if contentType == "" {
			contentType = sniffContentType(path)
		}

		scope := imageScope(beadID, bundleID, time.Now().UTC())
		key := imageKey(s3Prefix(), scope, filepath.Base(path), data)
		region := os.Getenv("COOP_S3_REGION")
		publicURL := imageURL(bucket, region, key)
		proxyURL := imageProxyURL(key)

		client, err := newS3Uploader(cmd.Context())
		if err != nil {
			return err
		}

		putCtx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
		defer cancel()

		if _, err := client.PutObject(putCtx, putImageInput(bucket, key, data, contentType)); err != nil {
			return fmt.Errorf("uploading to s3://%s/%s: %w", bucket, key, err)
		}

		var docID, viewerURL string
		if bundleID != "" {
			docID, viewerURL, err = createImageDocBead(cmd.Context(), bundleID, publicURL, filepath.Base(path), headline)
			if err != nil {
				return fmt.Errorf("creating doc bead for bundle %s: %w", bundleID, err)
			}
		}

		if jsonOutput {
			out := map[string]any{
				"bucket": bucket,
				"key":    key,
				// "url" is the viewer-proxied, human-shareable link (primary).
				// "storage_url" is the private raw S3 URL (not human-viewable).
				"url":          proxyURL,
				"storage_url":  publicURL,
				"size":         info.Size(),
				"content_type": contentType,
				"scope":        scope,
			}
			if docID != "" {
				out["doc"] = docID
				out["bundle"] = bundleID
				out["viewer_url"] = viewerURL
			}
			printJSON(out)
			return nil
		}

		// Primary (stdout): the viewer-proxied URL — the ONLY link a human can
		// open. The raw bucket is private (ka-0000003520), so we keep stdout to
		// the shareable URL alone and demote the raw S3 location to stderr,
		// clearly marked not-human-viewable.
		fmt.Println(proxyURL)
		fmt.Fprintf(os.Stderr, "storage location (private S3, not human-viewable; use the URL above to share): %s\n", publicURL)
		if docID != "" {
			fmt.Fprintf(os.Stderr, "Doc %s added to bundle %s\n", docID, bundleID)
			fmt.Fprintf(os.Stderr, "  %s\n", viewerURL)
		}
		return nil
	},
}

// ── helpers ─────────────────────────────────────────────────────────────

// putImageInput builds the S3 PutObject params shared by `gb image upload` and
// `gb grafana render --upload`. Centralizing it keeps both upload paths byte-
// for-byte identical (bucket/key/body/content-type), so a rendered PNG lands in
// the viewer bucket exactly the way an explicitly-uploaded image does.
func putImageInput(bucket, key string, data []byte, contentType string) *s3.PutObjectInput {
	return &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	}
}

// imageScope picks the <bead-or-date> path segment. Bead id wins; bundle id
// is the fallback when only it is given; otherwise we group by UTC day to
// keep ad-hoc uploads bucketed.
func imageScope(beadID, bundleID string, now time.Time) string {
	if beadID != "" {
		return beadID
	}
	if bundleID != "" {
		return bundleID
	}
	return now.Format("2006-01-02")
}

// imageKey returns "<prefix>/images/<scope>/<sha8>-<basename>" with prefix
// trimmed of any trailing slash. The sha8 is the first 8 hex chars of the
// file's SHA-256 — short enough to read, long enough that name collisions
// are effectively impossible inside a scope.
func imageKey(prefix, scope, basename string, data []byte) string {
	sum := sha256.Sum256(data)
	sha8 := hex.EncodeToString(sum[:])[:8]
	prefix = strings.TrimRight(prefix, "/")
	cleanName := sanitizeBasename(basename)
	return fmt.Sprintf("%s/images/%s/%s-%s", prefix, scope, sha8, cleanName)
}

// sanitizeBasename strips characters that confuse path-style URLs or shell
// pipelines. We keep alnum, dot, dash, underscore; everything else collapses
// to '-'. Empty input becomes "file".
func sanitizeBasename(name string) string {
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "file"
	}
	return out
}

// imageProxyURL builds the viewer-proxied, human-shareable URL for an uploaded
// image: <viewer-base>/image/<encoded-key>. This is the ONLY URL a human can
// actually open — the raw S3 bucket is private, so a bare bucket URL 403s in a
// browser (ka-0000003520). The beads-viewer /image/<key> proxy (ka-0000006131)
// streams the bytes through the viewer's OIDC chain.
//
// The viewer base comes from $BEADS_VIEWER_URL (which already carries the
// /beads path prefix in gardenio deploys, e.g. https://<host>/beads), falling
// back to the built-in defaultGardenioViewerURL so the printed link is always
// absolute and clickable even when the env var is unset. The key is encoded
// segment-by-segment with encodeS3Key, identical to the renderer-side rewrite
// (encodeImageProxyKey in bundle_renderers.go), so both produce the same URL.
func imageProxyURL(key string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("BEADS_VIEWER_URL")), "/")
	if base == "" {
		base = strings.TrimRight(defaultGardenioViewerURL, "/")
	}
	return base + "/image/" + encodeS3Key(key)
}

// imageURL builds the virtual-hosted public URL. us-east-1 (or empty region)
// uses the regionless form, matching the AWS-recommended layout.
func imageURL(bucket, region, key string) string {
	encoded := encodeS3Key(key)
	if region == "" || region == "us-east-1" {
		return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", bucket, encoded)
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucket, region, encoded)
}

// encodeS3Key URL-encodes each '/'-delimited segment so a filename that
// happens to contain spaces or other URI-reserved characters still produces
// a clickable link, while leaving the '/' separators intact.
func encodeS3Key(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// sniffContentType picks a MIME type from the file extension. Returns
// application/octet-stream for unknown extensions so the upload always has
// a defined Content-Type. We do NOT sniff bytes — gb image is a thin CLI
// shim and the extension is reliable enough for the screenshots / docs
// / small attachments this command targets.
func sniffContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "application/octet-stream"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch ext {
	case ".log", ".txt":
		return "text/plain; charset=utf-8"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	}
	return "application/octet-stream"
}

// createImageDocBead mirrors the bundle.go `--content-url` path: creates a
// type=doc, format=image bead, links it parent-child under the bundle, and
// closes it (docs are static artifacts). Returns the new doc id and the
// viewer URL anchor.
func createImageDocBead(ctx context.Context, bundleID, contentURL, basename, headline string) (string, string, error) {
	bundle, err := daemon.GetBead(ctx, bundleID)
	if err != nil {
		return "", "", fmt.Errorf("fetching bundle %s: %w", bundleID, err)
	}
	if bundle.Type != "bundle" {
		return "", "", fmt.Errorf("%s is not a bundle (type=%s)", bundleID, bundle.Type)
	}

	fields := map[string]any{
		"format":      "image",
		"content_url": contentURL,
	}
	if headline != "" {
		fields["headline"] = headline
	} else {
		fields["headline"] = basename
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", "", fmt.Errorf("encoding fields: %w", err)
	}

	docID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       basename,
		Type:        "doc",
		Kind:        "data",
		Priority:    2,
		CreatedBy:   actor,
		Description: "parent(" + bundleID + ")",
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", "", fmt.Errorf("creating doc bead: %w", err)
	}

	if err := daemon.AddDependency(ctx, docID, bundleID, "parent-child", actor); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to link doc to bundle: %v\n", err)
	}
	if err := daemon.CloseBead(ctx, docID, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to close doc bead: %v\n", err)
	}

	return docID, bundleURL(bundleID) + "#doc-" + docID, nil
}

func init() {
	imageCmd.AddCommand(imageUploadCmd)

	imageUploadCmd.Flags().String("bead", "", "scope upload under this bead id (default: today's UTC date)")
	imageUploadCmd.Flags().String("bundle", "", "after upload, create a type=doc bead and link it under this bundle")
	imageUploadCmd.Flags().String("content-type", "", "override Content-Type (default: sniffed from extension)")
	imageUploadCmd.Flags().Int64("max-bytes", defaultMaxImageBytes, "reject files larger than this many bytes")
	imageUploadCmd.Flags().String("headline", "", "optional TOC headline when --bundle is given (default: basename)")
}
