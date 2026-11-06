package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestNewS3Client_TimesOutOnHangingConfig verifies that when AWS credential
// resolution blocks (e.g. IMDS unreachable on a laptop), newS3Client returns a
// clear actionable error within a short bound — not an indefinite hang.
func TestNewS3Client_TimesOutOnHangingConfig(t *testing.T) {
	origLoader := awsConfigLoader
	origTimeout := awsConfigTimeout
	t.Cleanup(func() {
		awsConfigLoader = origLoader
		awsConfigTimeout = origTimeout
	})

	awsConfigTimeout = 100 * time.Millisecond
	awsConfigLoader = func(ctx context.Context, _ ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		<-ctx.Done()
		return aws.Config{}, ctx.Err()
	}

	parent, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	start := time.Now()
	_, err := newS3Client(parent)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from hanging credential resolution, got nil")
	}
	if elapsed > 6*time.Second {
		t.Fatalf("newS3Client took %s, expected to bail within 6s", elapsed)
	}
	if !strings.Contains(err.Error(), "AWS credential resolution timed out") {
		t.Errorf("expected actionable timeout error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Errorf("expected error to mention how to fix (AWS_ACCESS_KEY_ID), got: %v", err)
	}
}

// TestNewS3Client_HonorsCOOP_S3_REGION verifies the COOP_S3_REGION env var is
// passed through to LoadDefaultConfig as an explicit region override.
func TestNewS3Client_HonorsCOOP_S3_REGION(t *testing.T) {
	origLoader := awsConfigLoader
	t.Cleanup(func() { awsConfigLoader = origLoader })

	t.Setenv("COOP_S3_REGION", "us-west-2")

	awsConfigLoader = func(ctx context.Context, optFns ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		var lo awsconfig.LoadOptions
		for _, f := range optFns {
			if err := f(&lo); err != nil {
				return aws.Config{}, err
			}
		}
		if lo.Region != "us-west-2" {
			t.Errorf("expected region us-west-2, got %q", lo.Region)
		}
		return aws.Config{Region: lo.Region}, nil
	}

	if _, err := newS3Client(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestStaticCredentialsFromFiles_BothPresent verifies that when both
// credential-file env vars point at readable, non-empty files, the static
// provider wins with the trimmed file contents.
func TestStaticCredentialsFromFiles_BothPresent(t *testing.T) {
	dir := t.TempDir()
	idPath := dir + "/access_key_id"
	secretPath := dir + "/secret_access_key"
	if err := os.WriteFile(idPath, []byte("AKIAEXAMPLE\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("  s3cr3t  \n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envS3AccessKeyIDFile, idPath)
	t.Setenv(envS3SecretAccessKeyFile, secretPath)

	provider, ok := staticCredentialsFromFiles()
	if !ok {
		t.Fatal("expected staticCredentialsFromFiles to succeed with both files present")
	}
	creds, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error retrieving credentials: %v", err)
	}
	if creds.AccessKeyID != "AKIAEXAMPLE" {
		t.Errorf("expected trimmed access key id, got %q", creds.AccessKeyID)
	}
	if creds.SecretAccessKey != "s3cr3t" {
		t.Errorf("expected trimmed secret access key, got %q", creds.SecretAccessKey)
	}
}

// TestStaticCredentialsFromFiles_NeitherSet verifies the default (AWS/IRSA)
// path is untouched when neither credential-file env var is set — the
// regression the bead's acceptance criteria cares about.
func TestStaticCredentialsFromFiles_NeitherSet(t *testing.T) {
	t.Setenv(envS3AccessKeyIDFile, "")
	t.Setenv(envS3SecretAccessKeyFile, "")

	if provider, ok := staticCredentialsFromFiles(); ok {
		t.Fatalf("expected no provider when neither env var is set, got %v", provider)
	}
}

// TestStaticCredentialsFromFiles_UnreadableFallsBack verifies a missing or
// unreadable file degrades to the default credential chain instead of
// failing the client build.
func TestStaticCredentialsFromFiles_UnreadableFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envS3AccessKeyIDFile, dir+"/does-not-exist")
	t.Setenv(envS3SecretAccessKeyFile, dir+"/also-missing")

	if provider, ok := staticCredentialsFromFiles(); ok {
		t.Fatalf("expected fallback when files are unreadable, got %v", provider)
	}
}

// TestNewS3Client_StaticCredentialsFromFilesTakesPrecedence verifies
// newS3Client wires the static file-based provider into LoadDefaultConfig
// when the credential files are present (gd-W8OwRoHLLn merge-desk
// follow-up): gb builds its own S3 client for the resume check and salvage
// upload/restore, so it needs the same file-based credentials coop reads in
// endpoint mode or it silently falls back to an unauthenticated default
// chain.
func TestNewS3Client_StaticCredentialsFromFilesTakesPrecedence(t *testing.T) {
	origLoader := awsConfigLoader
	t.Cleanup(func() { awsConfigLoader = origLoader })

	dir := t.TempDir()
	idPath := dir + "/access_key_id"
	secretPath := dir + "/secret_access_key"
	if err := os.WriteFile(idPath, []byte("AKIAEXAMPLE"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("s3cr3t"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envS3AccessKeyIDFile, idPath)
	t.Setenv(envS3SecretAccessKeyFile, secretPath)

	var gotProvider aws.CredentialsProvider
	awsConfigLoader = func(ctx context.Context, optFns ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		var lo awsconfig.LoadOptions
		for _, f := range optFns {
			if err := f(&lo); err != nil {
				return aws.Config{}, err
			}
		}
		gotProvider = lo.Credentials
		return aws.Config{Credentials: lo.Credentials}, nil
	}

	if _, err := newS3Client(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotProvider == nil {
		t.Fatal("expected newS3Client to pass a static credentials provider, got nil")
	}
	creds, err := gotProvider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error retrieving credentials: %v", err)
	}
	if creds.AccessKeyID != "AKIAEXAMPLE" || creds.SecretAccessKey != "s3cr3t" {
		t.Errorf("expected file-based static credentials, got %+v", creds)
	}
}

// TestS3EndpointOpts_Unset verifies that with COOP_S3_ENDPOINT unset, the helper
// returns no options so the S3 client keeps default AWS endpoint + addressing.
func TestS3EndpointOpts_Unset(t *testing.T) {
	t.Setenv("COOP_S3_ENDPOINT", "")

	if opts := s3EndpointOpts(); opts != nil {
		t.Fatalf("expected nil opts when COOP_S3_ENDPOINT unset, got %d opt(s)", len(opts))
	}

	// Applying the (nil) opts must leave a fresh s3.Options untouched.
	var o s3.Options
	for _, f := range s3EndpointOpts() {
		f(&o)
	}
	if o.BaseEndpoint != nil {
		t.Errorf("expected nil BaseEndpoint, got %q", *o.BaseEndpoint)
	}
	if o.UsePathStyle {
		t.Error("expected UsePathStyle=false when endpoint unset")
	}
}

// TestS3EndpointOpts_Set verifies that COOP_S3_ENDPOINT sets BaseEndpoint and
// enables path-style addressing for in-box MinIO.
func TestS3EndpointOpts_Set(t *testing.T) {
	t.Setenv("COOP_S3_ENDPOINT", "  http://minio.gardenio.svc:9000  ")

	var o s3.Options
	opts := s3EndpointOpts()
	if len(opts) != 1 {
		t.Fatalf("expected 1 opt when endpoint set, got %d", len(opts))
	}
	for _, f := range opts {
		f(&o)
	}
	if o.BaseEndpoint == nil || *o.BaseEndpoint != "http://minio.gardenio.svc:9000" {
		t.Errorf("expected trimmed BaseEndpoint, got %v", o.BaseEndpoint)
	}
	if !o.UsePathStyle {
		t.Error("expected UsePathStyle=true when endpoint set")
	}
}

// ── fake S3 harness ───────────────────────────────────────────────────────
//
// A minimal in-memory S3 stand-in that serves the two ListObjectsV2 + GetObject
// calls the audit reader makes. It deliberately models the real prod bucket's
// mixed layout (flat hex-UUID prefixes that froze in 2026-05 + bead-namespaced
// gd-* / pod-crew-* prefixes written today), so the test reproduces the original
// "No sessions found" bug and proves the fix.

type fakeS3Object struct {
	body         []byte
	lastModified time.Time
}

// newFakeS3 returns an httptest server plus an s3.Client wired to it with
// path-style addressing and static creds, mirroring the COOP_S3_ENDPOINT
// in-box MinIO path exercised by s3EndpointOpts.
func newFakeS3(t *testing.T, objects map[string]fakeS3Object) *s3.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path-style requests look like /<bucket>/<key...>. Strip the leading
		// "/<bucket>" segment to recover the object key / prefix.
		p := strings.TrimPrefix(r.URL.Path, "/")
		bucket, key, _ := strings.Cut(p, "/")
		_ = bucket

		q := r.URL.Query()
		if q.Get("list-type") == "2" {
			serveFakeList(w, q, objects)
			return
		}
		serveFakeGet(w, key, objects)
	}))
	t.Cleanup(srv.Close)

	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	return client
}

// serveFakeList implements ListObjectsV2 with prefix filtering and continuation
// tokens. It only emits <Contents> (the audit reader's listSessions lists by
// the /meta.json object suffix, not by CommonPrefixes), with a small page size
// to exercise pagination.
func serveFakeList(w http.ResponseWriter, q url.Values, objects map[string]fakeS3Object) {
	prefix := q.Get("prefix")
	const pageSize = 2

	keys := make([]string, 0, len(objects))
	for k := range objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // S3 returns keys in lexicographic order.

	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil {
			start = n
		}
	}
	end := start + pageSize
	truncated := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	if truncated {
		fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	for _, k := range keys[start:end] {
		obj := objects[k]
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>%d</Size></Contents>",
			k, obj.lastModified.UTC().Format(time.RFC3339), len(obj.body))
	}
	b.WriteString("</ListBucketResult>")

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func serveFakeGet(w http.ResponseWriter, key string, objects map[string]fakeS3Object) {
	obj, ok := objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	_, _ = w.Write(obj.body)
}

// metaJSON builds a minimal coop SessionMeta document. session_id is the
// composite the reader is expected to round-trip back into keys.
func metaJSON(sessionID string, startedAt int64, transcripts int) []byte {
	return fmt.Appendf(nil,
		`{"session_id":%q,"agent_type":"claude","started_at":%d,"transcript_count":%d}`,
		sessionID, startedAt, transcripts)
}

// TestListSessions_MixedLayout is the regression test for ka-0000002331: the
// audit reader must discover sessions under BOTH the legacy flat layout and the
// bead-namespaced layout coop switched to ~2026-05-03, must NOT emit phantom
// sessions from layout sub-directories, and fetchMeta must decode a nested
// meta.json that the old one-level reader 404'd on.
func TestListSessions_MixedLayout(t *testing.T) {
	prefix := "coop/sessions"
	day := func(d int) time.Time { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC) }

	objects := map[string]fakeS3Object{
		// Legacy flat layout (frozen): <prefix>/<uuid>/meta.json
		prefix + "/00aaflat-1111/meta.json":           {metaJSON("00aaflat-1111", day(1).Unix(), 1), day(1)},
		prefix + "/00aaflat-1111/transcripts/0.jsonl": {[]byte("{}"), day(1)},
		// Bead-namespaced layout (live): <prefix>/<bead>/<uuid>/meta.json
		prefix + "/gd-Beadddd1/uuid-aaaa/meta.json":           {metaJSON("gd-Beadddd1/uuid-aaaa", day(20).Unix(), 2), day(20)},
		prefix + "/gd-Beadddd1/uuid-aaaa/transcripts/0.jsonl": {[]byte("{}"), day(20)},
		prefix + "/gd-Beadddd1/uuid-aaaa/transcripts/1.jsonl": {[]byte("{}"), day(20)},
		// Bead IDs are not always gd-* — pod-crew-* roots also use nesting.
		prefix + "/pod-crew-xyz/uuid-bbbb/meta.json": {metaJSON("pod-crew-xyz/uuid-bbbb", day(25).Unix(), 0), day(25)},
		// A bead with a sibling transcripts/ dir at the bead root (phantom-prefix
		// trap): must NOT be reported as a session of its own.
		prefix + "/gd-Phantom2/uuid-cccc/meta.json": {metaJSON("gd-Phantom2/uuid-cccc", day(10).Unix(), 0), day(10)},
		prefix + "/gd-Phantom2/transcripts/9.jsonl": {[]byte("{}"), day(10)},
	}
	client := newFakeS3(t, objects)
	ctx := context.Background()

	got, err := listSessions(ctx, client, "gardenio-sessions", prefix, 0, time.Time{})
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}

	want := []string{
		"pod-crew-xyz/uuid-bbbb", // day 25 — newest first
		"gd-Beadddd1/uuid-aaaa",  // day 20
		"gd-Phantom2/uuid-cccc",  // day 10
		"00aaflat-1111",          // day 1 — oldest last
	}
	if len(got) != len(want) {
		t.Fatalf("listSessions returned %d sessions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// Explicitly assert no phantom "gd-Phantom2/transcripts" style entry slipped in.
	for _, s := range got {
		if strings.HasSuffix(s, "/transcripts") || strings.Contains(s, "transcripts/") {
			t.Errorf("phantom transcripts prefix leaked as a session: %q", s)
		}
	}

	// fetchMeta must now decode the NESTED meta.json the old reader 404'd on.
	meta, err := fetchMeta(ctx, client, "gardenio-sessions", prefix, "gd-Beadddd1/uuid-aaaa")
	if err != nil {
		t.Fatalf("fetchMeta(nested composite key): %v — this is the original bug", err)
	}
	if meta.SessionID != "gd-Beadddd1/uuid-aaaa" {
		t.Errorf("fetchMeta SessionID = %q, want composite key", meta.SessionID)
	}
	if meta.TranscriptCount == nil || *meta.TranscriptCount != 2 {
		t.Errorf("fetchMeta TranscriptCount = %v, want 2", meta.TranscriptCount)
	}
}

// TestListSessions_RecencyCapBeatsLexStarvation proves the fix for the second
// half of the bug: the max-sessions cap must select the most RECENT sessions,
// not the lexicographically-first ones. Old flat hex UUIDs sort before gd-*, so
// a lexicographic cap of N would surface only frozen sessions and starve the
// recent window entirely.
func TestListSessions_RecencyCapBeatsLexStarvation(t *testing.T) {
	prefix := "coop/sessions"
	objects := map[string]fakeS3Object{}
	// 5 old flat sessions that sort first lexicographically (0…).
	for i := range 5 {
		key := fmt.Sprintf("%s/0%dflat-old/meta.json", prefix, i)
		ts := time.Date(2026, 4, 29, 0, i, 0, 0, time.UTC)
		objects[key] = fakeS3Object{metaJSON(fmt.Sprintf("0%dflat-old", i), ts.Unix(), 1), ts}
	}
	// 2 recent bead sessions that sort LAST lexicographically (gd-…).
	recent := []string{"gd-recentAA/uuid-1", "gd-recentBB/uuid-2"}
	for i, sid := range recent {
		key := fmt.Sprintf("%s/%s/meta.json", prefix, sid)
		ts := time.Date(2026, 6, 24+i, 0, 0, 0, 0, time.UTC)
		objects[key] = fakeS3Object{metaJSON(sid, ts.Unix(), 1), ts}
	}

	client := newFakeS3(t, objects)

	// Cap of 2 must return the 2 newest (the gd-* ones), NOT the lex-first flats.
	got, err := listSessions(context.Background(), client, "gardenio-sessions", prefix, 2, time.Time{})
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("with cap=2 expected 2 sessions, got %d: %v", len(got), got)
	}
	wantSet := map[string]bool{"gd-recentBB/uuid-2": true, "gd-recentAA/uuid-1": true}
	for _, s := range got {
		if !wantSet[s] {
			t.Errorf("capped result %q is not one of the 2 most-recent sessions (lex starvation regression)", s)
		}
	}
}

// TestListSessions_SincePrune verifies the lossless --since pre-filter: sessions
// whose meta.json predates sinceTime are dropped before the metadata-fetch phase
// (a session's meta is written at/after it starts, so older meta cannot match).
func TestListSessions_SincePrune(t *testing.T) {
	prefix := "coop/sessions"
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	objects := map[string]fakeS3Object{
		prefix + "/gd-old111/uuid-a/meta.json":  {metaJSON("gd-old111/uuid-a", old.Unix(), 1), old},
		prefix + "/gd-fresh22/uuid-b/meta.json": {metaJSON("gd-fresh22/uuid-b", fresh.Unix(), 1), fresh},
	}
	client := newFakeS3(t, objects)

	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got, err := listSessions(context.Background(), client, "gardenio-sessions", prefix, 0, since)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-fresh22/uuid-b" {
		t.Fatalf("since-prune: got %v, want only [gd-fresh22/uuid-b]", got)
	}
}

// TestResolveFetchTargets is the regression test for ka-0000002531: `gb audit
// fetch` was reachable only with a compound "<bead>/<coop-uuid>" id. Fed the
// bare bead id that `gb audit list` prints as its bead_id column, every key was
// built one level too shallow, so the command downloaded nothing and still
// exited 0 — a silent false-empty. resolveFetchTargets is what closes that gap:
// a bead id expands to its sessions, a session id passes through untouched, and
// an id with nothing under it errors instead of resolving to a no-op fetch.
func TestResolveFetchTargets(t *testing.T) {
	prefix := "coop/sessions"
	day := func(d int) time.Time { return time.Date(2026, 8, d, 12, 0, 0, 0, time.UTC) }

	objects := map[string]fakeS3Object{
		// Bead with TWO archived sessions (the real ka-0000002289 shape): a
		// bare bead id must expand to both, newest meta.json first.
		prefix + "/gd-Twosess1/uuid-old/meta.json":           {metaJSON("gd-Twosess1/uuid-old", day(1).Unix(), 1), day(1)},
		prefix + "/gd-Twosess1/uuid-old/transcripts/1.jsonl": {[]byte("{}"), day(1)},
		prefix + "/gd-Twosess1/uuid-new/meta.json":           {metaJSON("gd-Twosess1/uuid-new", day(4).Unix(), 2), day(4)},
		prefix + "/gd-Twosess1/uuid-new/transcripts/1.jsonl": {[]byte("{}"), day(4)},
		// A bead root that ALSO holds a stray transcripts/ dir: resolution must
		// pick the real sub-session, not the phantom sibling prefix.
		prefix + "/gd-Phantom3/uuid-real/meta.json": {metaJSON("gd-Phantom3/uuid-real", day(2).Unix(), 0), day(2)},
		prefix + "/gd-Phantom3/transcripts/9.jsonl": {[]byte("{}"), day(2)},
		// Legacy flat session: meta.json sits at the id root, so the id is
		// already a session and must pass through unchanged.
		prefix + "/00aaflat-2222/meta.json":           {metaJSON("00aaflat-2222", day(1).Unix(), 1), day(1)},
		prefix + "/00aaflat-2222/transcripts/0.jsonl": {[]byte("{}"), day(1)},
		// Transcripts but no meta.json — still a fetchable session.
		prefix + "/00nometa-3333/transcripts/0.jsonl": {[]byte("{}"), day(1)},
	}
	client := newFakeS3(t, objects)
	ctx := context.Background()

	cases := []struct {
		name string
		id   string
		want []string
	}{{
		name: "bare bead id expands to every session, newest first",
		id:   "gd-Twosess1",
		want: []string{"gd-Twosess1/uuid-new", "gd-Twosess1/uuid-old"},
	}, {
		name: "compound session id passes through (no regression)",
		id:   "gd-Twosess1/uuid-old",
		want: []string{"gd-Twosess1/uuid-old"},
	}, {
		name: "stray transcripts/ at bead root does not mask the session",
		id:   "gd-Phantom3",
		want: []string{"gd-Phantom3/uuid-real"},
	}, {
		name: "legacy flat session id passes through",
		id:   "00aaflat-2222",
		want: []string{"00aaflat-2222"},
	}, {
		name: "session with transcripts but no meta.json is still fetchable",
		id:   "00nometa-3333",
		want: []string{"00nometa-3333"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveFetchTargets(ctx, client, "gardenio-sessions", prefix, tc.id)
			if err != nil {
				t.Fatalf("resolveFetchTargets(%q): %v", tc.id, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("resolveFetchTargets(%q) = %v, want %v", tc.id, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("target[%d] = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}

	// The core of the bug: an id with nothing archived under it must ERROR, so
	// the command can exit non-zero instead of reporting "0 files" with exit 0.
	// Both a typo'd bead and a real-but-empty session prefix take this path.
	for _, id := range []string{"gd-JJGaaGEzew", "gd-Twosess1/uuid-missing"} {
		if got, err := resolveFetchTargets(ctx, client, "gardenio-sessions", prefix, id); err == nil {
			t.Errorf("resolveFetchTargets(%q) = %v, want error (silent false-empty)", id, got)
		}
	}
}

func strptr(s string) *string { return &s }
func i32ptr(i int32) *int32   { return &i }

// TestAuditStatus is the gb-side half of the ka-0000001328 fix: the STATUS the
// audit reader renders and filters on must come from coop's semantic exit_status.
// A clean gb-stop / pod teardown now arrives as exit_status="stopped" (benign);
// a genuine non-graceful 143 arrives as exit_status="error" and must stay "error"
// so real crashes still surface. The numeric fallback only applies when coop
// wrote no exit_status at all.
func TestAuditStatus(t *testing.T) {
	cases := []struct {
		name string
		meta sessionMeta
		want string
	}{
		{
			name: "clean stop after gb stop renders benign 'stopped'",
			meta: sessionMeta{ExitStatus: strptr("stopped"), ExitCode: i32ptr(143)},
			want: "stopped",
		},
		{
			name: "genuine non-graceful 143 stays 'error'",
			meta: sessionMeta{ExitStatus: strptr("error"), ExitCode: i32ptr(143)},
			want: "error",
		},
		{
			name: "success passes through",
			meta: sessionMeta{ExitStatus: strptr("success"), ExitCode: i32ptr(0)},
			want: "success",
		},
		{
			name: "credential switch passes through",
			meta: sessionMeta{ExitStatus: strptr("switch"), ExitCode: i32ptr(10)},
			want: "switch",
		},
		{
			name: "missing exit_status falls back to numeric exit code",
			meta: sessionMeta{ExitCode: i32ptr(143)},
			want: "exit:143",
		},
		{
			name: "no exit info at all yields empty",
			meta: sessionMeta{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditStatus(&tc.meta); got != tc.want {
				t.Errorf("auditStatus(%+v) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

// TestAuditListStatusFilter proves the --status filter isolates genuine failures
// from clean stops — the operator triage workflow the ka-0000001328 bug exists to
// restore. The filter is case-insensitive and matches the displayed status.
func TestAuditListStatusFilter(t *testing.T) {
	metas := []sessionMeta{
		{SessionID: "a", ExitStatus: strptr("stopped")},
		{SessionID: "b", ExitStatus: strptr("error")},
		{SessionID: "c", ExitStatus: strptr("success")},
		{SessionID: "d", ExitStatus: strptr("error")},
		{SessionID: "e", ExitCode: i32ptr(143)}, // no exit_status → "exit:143"
	}

	filterByStatus := func(status string) []string {
		var ids []string
		for i := range metas {
			if status != "" && !strings.EqualFold(auditStatus(&metas[i]), status) {
				continue
			}
			ids = append(ids, metas[i].SessionID)
		}
		return ids
	}

	if got := filterByStatus("error"); len(got) != 2 || got[0] != "b" || got[1] != "d" {
		t.Errorf(`--status error = %v, want [b d] (clean stops excluded)`, got)
	}
	if got := filterByStatus("ERROR"); len(got) != 2 {
		t.Errorf("--status filter should be case-insensitive, got %v", got)
	}
	if got := filterByStatus("stopped"); len(got) != 1 || got[0] != "a" {
		t.Errorf(`--status stopped = %v, want [a]`, got)
	}
	if got := filterByStatus(""); len(got) != len(metas) {
		t.Errorf("empty --status should not filter; got %d of %d", len(got), len(metas))
	}
}

// TestResolveResultLimit covers the gd-J9kJNNDjoC fix: --limit caps RESULTS and
// --max-sessions caps the FETCH phase, each with its own default. Raising only
// --max-sessions must not be silently clamped by the default --limit, while an
// explicit --limit always wins.
func TestResolveResultLimit(t *testing.T) {
	cases := []struct {
		name               string
		limit              int
		maxSessionsChanged bool
		limitChanged       bool
		want               int
	}{
		{
			name:               "neither flag set keeps default limit",
			limit:              50,
			maxSessionsChanged: false,
			limitChanged:       false,
			want:               50,
		},
		{
			name:               "only --max-sessions set uncaps results",
			limit:              50, // the untouched default
			maxSessionsChanged: true,
			limitChanged:       false,
			want:               0,
		},
		{
			name:               "explicit --limit wins even with --max-sessions",
			limit:              10,
			maxSessionsChanged: true,
			limitChanged:       true,
			want:               10,
		},
		{
			name:               "only --limit set is respected",
			limit:              25,
			maxSessionsChanged: false,
			limitChanged:       true,
			want:               25,
		},
		{
			name:               "explicit --limit 0 alongside --max-sessions stays uncapped",
			limit:              0,
			maxSessionsChanged: true,
			limitChanged:       true,
			want:               0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveResultLimit(tc.limit, tc.maxSessionsChanged, tc.limitChanged); got != tc.want {
				t.Errorf("resolveResultLimit(%d, max=%v, limit=%v) = %d, want %d",
					tc.limit, tc.maxSessionsChanged, tc.limitChanged, got, tc.want)
			}
		})
	}
}
