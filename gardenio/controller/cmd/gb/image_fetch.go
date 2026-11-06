package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

// image_fetch.go — the missing READ half of `gb image upload` (ka-0000004777).
//
// WHY THIS EXISTS. `gb image upload` prints a viewer-proxied URL and calls it
// the human-shareable link. That is true only for a human with a live OIDC
// session in a browser. Every non-interactive consumer is refused twice over:
//
//   - AWS Verified Access answers an anonymous GET with a 303 into GitLab
//     OAuth, before any gardenio code runs;
//   - with AVA bypassed, beads-viewer's own oidcauth middleware answers 401
//     "missing authorization token".
//
// Both are deliberate (see docs/research/beads-viewer-image-rendering.md and
// commit 85a77b75c, which names edge authn as the security boundary). Nothing
// regressed. But the consequence is that an agent cannot read back an image it
// uploaded thirty seconds earlier — not to re-host it into a GitLab MR, not to
// attach it to a Jira ticket, not to verify what it captured.
//
// Agents papered over this by hand (forge MR !47, ka-0000007806: eight broken
// PNGs pulled back out of S3 and re-hosted). This is that operation, supported.
//
// NO NEW ACCESS SURFACE. This reads S3 with the SAME credentials `gb image
// upload` already uses to write, and enforces the SAME key-prefix restriction
// the viewer proxy enforces. Anyone who can run `gb image upload` can already
// read these objects with the aws CLI; this just spares them the URL surgery.
// It deliberately does NOT presign, publish, or widen the bucket policy —
// making the bucket public was formally rejected pending a publishability
// policy (beads-viewer-image-rendering.md:186-198).

// s3Downloader is the read counterpart of s3Uploader, kept as a narrow
// interface so tests inject an in-memory fake instead of standing up S3.
type s3Downloader interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// newS3Downloader is the production hook; tests override it.
var newS3Downloader = func(ctx context.Context) (s3Downloader, error) {
	return newS3Client(ctx)
}

var imageFetchCmd = &cobra.Command{
	Use:   "fetch <url-or-key> [more...]",
	Short: "Download an uploaded image back out of S3 (works without a browser session)",
	Long: `Download one or more previously-uploaded images from S3 to local files.

WHY YOU NEED THIS. The URL 'gb image upload' prints is browser-only: an
anonymous GET gets a 303 into GitLab OAuth (AWS Verified Access), and even with
that bypassed the viewer's OIDC middleware answers 401. So curl, an <img> tag in
a GitLab MR, Jira's renderer, Slack's unfurl bot, and an agent re-reading its own
evidence ALL fail on that link. This command reads the bytes straight from S3
with the same credentials the upload used, so no browser session is involved.

Accepts whatever you have on hand — the viewer-proxied URL, the raw S3 URL, or
a bare key:

  https://gardenio.example.com/beads/image/coop/sessions/images/<date>/<sha8>-shot.png
  https://gardenio-sessions-prod.s3.amazonaws.com/coop/sessions/images/<date>/<sha8>-shot.png
  coop/sessions/images/<date>/<sha8>-shot.png

To EMBED an image somewhere rather than inspect it, prefer the destination-native
command — it uploads the bytes to where the reader already is, so the image
actually renders:

  GitLab MR  ->  gb mr embed <mr> <file>...
  Jira issue ->  gb attachments upload <issue> <file>...
  Slack      ->  gb slack upload <file>

Examples:
  gb image fetch https://gardenio.example.com/beads/image/coop/sessions/images/2026-07-27/abc123-shot.png
  gb image fetch coop/sessions/images/2026-07-27/abc123-shot.png -o /tmp
  gb image fetch <url> --stdout | file -`,
	Args: cobra.MinimumNArgs(1),
	RunE: runImageFetch,
}

func init() {
	imageCmd.AddCommand(imageFetchCmd)
	imageFetchCmd.Flags().StringP("output-dir", "o", ".", "directory to write downloaded files into")
	imageFetchCmd.Flags().Bool("stdout", false, "stream a single image to stdout instead of writing a file")
}

func runImageFetch(cmd *cobra.Command, args []string) error {
	buckets := imageReadBuckets()
	if len(buckets) == 0 {
		return fmt.Errorf("COOP_S3_BUCKET is not set")
	}
	toStdout, _ := cmd.Flags().GetBool("stdout")
	if toStdout && len(args) != 1 {
		return fmt.Errorf("--stdout takes exactly one image, got %d", len(args))
	}
	outDir, _ := cmd.Flags().GetString("output-dir")

	client, err := newS3Downloader(cmd.Context())
	if err != nil {
		return fmt.Errorf("creating S3 client: %w", err)
	}

	for _, arg := range args {
		key, err := imageKeyFromRef(arg, buckets...)
		if err != nil {
			return err
		}

		out, err := getImageObject(cmd.Context(), client, key, buckets)
		if err != nil {
			return err
		}

		if toStdout {
			_, copyErr := io.Copy(os.Stdout, out.Body)
			_ = out.Body.Close()
			if copyErr != nil {
				return fmt.Errorf("streaming %s: %w", key, copyErr)
			}
			return nil
		}

		dest := filepath.Join(outDir, filepath.Base(key))
		n, err := writeImageFile(dest, out.Body)
		_ = out.Body.Close()
		if err != nil {
			return err
		}
		fmt.Printf("%s\t%d bytes\n", dest, n)
	}
	return nil
}

// imageReadBuckets lists the buckets an image is read from, in order: the
// images bucket, then the session bucket when they differ, where every image
// from before the switch lives (gd-CmjVAU3vgd). Empty when neither is set.
func imageReadBuckets() []string {
	var out []string
	for _, b := range []string{s3ImagesBucket(), s3Bucket()} {
		if b != "" && (len(out) == 0 || out[0] != b) {
			out = append(out, b)
		}
	}
	return out
}

// getImageObject reads key from the first of buckets that has it. Only a
// not-found moves on to the next bucket; any other error is returned as is.
func getImageObject(ctx context.Context, client s3Downloader, key string, buckets []string) (*s3.GetObjectOutput, error) {
	var err error
	for _, bucket := range buckets {
		var out *s3.GetObjectOutput
		out, err = client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err == nil {
			return out, nil
		}
		err = fmt.Errorf("fetching s3://%s/%s: %w", bucket, key, err)
		if !isS3NotFound(err) {
			return nil, err
		}
	}
	return nil, err
}

// writeImageFile streams body to dest, returning the byte count. Split out so
// the copy-then-close ordering is testable and so a partial write surfaces as
// an error rather than a silently truncated file.
func writeImageFile(dest string, body io.Reader) (int64, error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", dest, err)
	}
	n, err := io.Copy(f, body)
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return n, fmt.Errorf("writing %s: %w", dest, err)
	}
	return n, nil
}

// errImageRefOutOfPrefix is returned for a key outside the images subtree.
var errImageRefOutOfPrefix = errors.New("key is outside the uploadable images prefix")

// imageKeyFromRef normalizes any of the three shapes a caller might have — the
// viewer-proxied URL, the raw S3 URL, or a bare key — into an S3 key.
//
// Enforces the same prefix restriction the viewer proxy enforces
// (imageProxyAllowedPrefix in beads-viewer): only `<prefix>/images/` is
// readable. This command must not become a general-purpose reader for the whole
// sessions bucket, which holds transcripts and other material that is not
// image-evidence.
func imageKeyFromRef(ref string, buckets ...string) (string, error) {
	key := strings.TrimSpace(ref)
	if key == "" {
		return "", fmt.Errorf("empty image reference")
	}

	if u, err := url.Parse(key); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		p := u.Path
		// Viewer-proxied form: [<basePath>]/image/<key>. Split on the LAST
		// occurrence so a basePath that itself contains "/image/" cannot
		// truncate the real key.
		if i := strings.LastIndex(p, "/image/"); i >= 0 {
			p = p[i+len("/image/"):]
		}
		// Path-style raw S3 form: /<bucket>/<key>, for either read bucket.
		p = strings.TrimPrefix(p, "/")
		for _, bucket := range buckets {
			if bucket != "" && strings.HasPrefix(p, bucket+"/") {
				p = strings.TrimPrefix(p, bucket+"/")
				break
			}
		}
		// The URL path is percent-encoded (gb image upload encodes each
		// segment); decode back to the literal key S3 stores.
		decoded, decErr := url.PathUnescape(p)
		if decErr != nil {
			return "", fmt.Errorf("decoding image URL path %q: %w", p, decErr)
		}
		key = decoded
	}

	key = strings.TrimPrefix(key, "/")

	// Reject `..` segments before the prefix check. S3 treats keys literally —
	// `images/../transcripts/x` is a distinct (nonexistent) key, not an escape,
	// so this is not exploitable against S3 today. But the prefix check below
	// would PASS such a key, meaning the only thing standing between it and a
	// transcript is S3's refusal to normalize. That is a property of the
	// storage backend, not of this guard. Reject explicitly so the containment
	// holds on its own terms, and so a future caller that does normalize (a
	// proxy, a local cache, a different backend) cannot turn it into a real
	// escape. Also blocks the local-write side: a `..` in the key would put
	// filepath.Base's output somewhere the caller did not choose.
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q contains a %q path segment", errImageRefOutOfPrefix, key, "..")
		}
	}

	if !strings.HasPrefix(key, imageFetchAllowedPrefix()) {
		return "", fmt.Errorf("%w: %q is not under %q — this command reads uploaded images only",
			errImageRefOutOfPrefix, key, imageFetchAllowedPrefix())
	}
	return key, nil
}

// imageFetchAllowedPrefix mirrors the viewer's imageProxyAllowedPrefix and
// gb image upload's own key layout: `<COOP_S3_PREFIX>/images/`.
func imageFetchAllowedPrefix() string {
	p := strings.TrimRight(s3Prefix(), "/")
	if p == "" {
		p = "coop/sessions"
	}
	return p + "/images/"
}
