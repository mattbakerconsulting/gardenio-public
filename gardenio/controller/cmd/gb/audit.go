package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

// envS3AccessKeyIDFile / envS3SecretAccessKeyFile are the env vars the
// controller wires onto agent pods (podspec.go) pointing at the
// tmpfs-projected object-store credential files under
// podmanager.MountObjectStoreCreds — endpoint mode only, no IRSA
// (gd-W8OwRoHLLn). coop's Rust S3Client reads the same two files; gb needs
// the same support since it builds its own S3 client for the resume check
// and salvage upload/restore, or it silently falls back to an
// unauthenticated default credential chain in endpoint mode.
const (
	envS3AccessKeyIDFile     = "COOP_S3_ACCESS_KEY_ID_FILE"
	envS3SecretAccessKeyFile = "COOP_S3_SECRET_ACCESS_KEY_FILE"
)

// awsConfigTimeout bounds AWS credential/region resolution so that a missing
// IMDS endpoint can't hang the gb CLI. Var so tests can shorten it.
var awsConfigTimeout = 5 * time.Second

// awsConfigLoader is the LoadDefaultConfig hook (overridable for tests).
var awsConfigLoader = awsconfig.LoadDefaultConfig

// auditListDeadline bounds the whole listSessions/fetchMetas pipeline.
// listSessions does a full layout-agnostic recursive scan of the session
// keyspace (tens of thousands of prefixes / hundreds of thousands of objects
// on the prod bucket), so this is generous; the per-fetch timeout still bounds
// individual meta.json GETs. If the scan can't finish in time it returns
// partial results rather than failing the command.
var auditListDeadline = 180 * time.Second

// sessionMeta mirrors coop's SessionMeta struct (s3.rs).
type sessionMeta struct {
	SessionID       string   `json:"session_id"`
	AgentType       string   `json:"agent_type"`
	StartedAt       uint64   `json:"started_at"`
	EndedAt         *uint64  `json:"ended_at,omitempty"`
	ExitCode        *int32   `json:"exit_code,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	AgentName       *string  `json:"agent_name,omitempty"`
	Project         *string  `json:"project,omitempty"`
	Role            *string  `json:"role,omitempty"`
	BeadID          *string  `json:"bead_id,omitempty"`
	ExitStatus      *string  `json:"exit_status,omitempty"`
	TranscriptCount *uint32  `json:"transcript_count,omitempty"`
}

func (m *sessionMeta) duration() time.Duration {
	if m.EndedAt == nil {
		return 0
	}
	return time.Duration(*m.EndedAt-m.StartedAt) * time.Second
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// auditStatus is the single source of truth for a session's displayed STATUS,
// used by both the table column and the --status filter so they always agree.
// It prefers coop's semantic exit_status (success / stopped / suspended / switch /
// timeout / error), falling back to "exit:<code>" when only a numeric code is
// present.
//
// Note: "stopped" is the clean gb-stop / pod-teardown status coop now stamps for
// a signal-termination exit on a requested shutdown (ka-0000001328); it is benign
// and should NOT be read as a failure. "suspended" is the idle-suspend status coop
// stamps when a session tears down because the agent sat idle past
// COOP_IDLE_TIMEOUT_MS (Phase 1 snapshot-suspend, ka-0000002921) — also benign: the
// snapshot is intact and the pod is reclaimable onto the warm pool. Use `--status
// error` to isolate genuine failures for triage.
func auditStatus(m *sessionMeta) string {
	if s := ptrStr(m.ExitStatus); s != "" {
		return s
	}
	if m.ExitCode != nil {
		return fmt.Sprintf("exit:%d", *m.ExitCode)
	}
	return ""
}

func ptrUint32(u *uint32) string {
	if u == nil {
		return "?"
	}
	return fmt.Sprintf("%d", *u)
}

func s3Bucket() string {
	return os.Getenv("COOP_S3_BUCKET")
}

// s3ImagesBucket is the bucket for <prefix>/images/... objects:
// COOP_S3_IMAGES_BUCKET (chart objectStore.images.bucket, gd-CmjVAU3vgd), else
// the session bucket. A separate bucket keeps archive growth from filling the
// quota image uploads need.
func s3ImagesBucket() string {
	if b := strings.TrimSpace(os.Getenv("COOP_S3_IMAGES_BUCKET")); b != "" {
		return b
	}
	return s3Bucket()
}

func s3Prefix() string {
	if p := os.Getenv("COOP_S3_PREFIX"); p != "" {
		return p
	}
	return "coop/sessions"
}

// staticCredentialsFromFiles reads the object-store static credential files
// (gd-W8OwRoHLLn) into an explicit credentials provider, mirroring coop's
// S3Client::static_credentials_from_files (coop/crates/cli/src/s3.rs): read
// fresh via os.ReadFile and hold the trimmed result only as an in-memory
// value — never os.Setenv, so the keys never land in /proc/self/environ.
// Returns (nil, false) when either env var is unset or either file is
// missing/unreadable/empty, so the caller falls back to the default AWS
// credential chain (e.g. IRSA) rather than failing the client build.
func staticCredentialsFromFiles() (aws.CredentialsProvider, bool) {
	accessKeyIDFile := os.Getenv(envS3AccessKeyIDFile)
	secretAccessKeyFile := os.Getenv(envS3SecretAccessKeyFile)
	if accessKeyIDFile == "" || secretAccessKeyFile == "" {
		return nil, false
	}

	accessKeyID, err := os.ReadFile(accessKeyIDFile)
	if err != nil {
		slog.Warn("s3: failed to read access key id file, falling back to default credential chain", "path", accessKeyIDFile, "err", err)
		return nil, false
	}
	secretAccessKey, err := os.ReadFile(secretAccessKeyFile)
	if err != nil {
		slog.Warn("s3: failed to read secret access key file, falling back to default credential chain", "path", secretAccessKeyFile, "err", err)
		return nil, false
	}

	id := strings.TrimSpace(string(accessKeyID))
	secret := strings.TrimSpace(string(secretAccessKey))
	if id == "" || secret == "" {
		slog.Warn("s3: access key id or secret access key file is empty, falling back to default credential chain")
		return nil, false
	}

	return credentials.NewStaticCredentialsProvider(id, secret, ""), true
}

func newS3Client(ctx context.Context) (*s3.Client, error) {
	loadCtx, cancel := context.WithTimeout(ctx, awsConfigTimeout)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error

	// gb is a developer CLI, not an EC2 workload — IMDS lookups should never
	// block credential resolution here. Honor the standard escape hatch
	// AWS_EC2_METADATA_DISABLED so users running on EC2 can opt back in by
	// leaving it unset and explicitly forcing IMDS via that env var… but
	// otherwise default IMDS off.
	if os.Getenv("AWS_EC2_METADATA_DISABLED") == "" {
		opts = append(opts, awsconfig.WithEC2IMDSClientEnableState(imds.ClientDisabled))
	}
	if region := os.Getenv("COOP_S3_REGION"); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if provider, ok := staticCredentialsFromFiles(); ok {
		opts = append(opts, awsconfig.WithCredentialsProvider(provider))
	}

	cfg, err := awsConfigLoader(loadCtx, opts...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf(
				"AWS credential resolution timed out after %s — set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or AWS_PROFILE, or run on a host with an IAM role",
				awsConfigTimeout,
			)
		}
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return s3.NewFromConfig(cfg, s3EndpointOpts()...), nil
}

// s3EndpointOpts returns S3 client options that point at a custom endpoint with
// path-style addressing when COOP_S3_ENDPOINT is set (e.g. an in-box MinIO URL).
// When the env var is empty it returns no options, so the client keeps the
// default AWS S3 endpoint and virtual-hosted-style addressing — unchanged.
func s3EndpointOpts() []func(*s3.Options) {
	endpoint := strings.TrimSpace(os.Getenv("COOP_S3_ENDPOINT"))
	if endpoint == "" {
		return nil
	}
	return []func(*s3.Options){
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		},
	}
}

// ── audit (parent command) ──────────────────────────────────────────────

var auditCmd = &cobra.Command{
	Use:     "audit",
	Short:   "Audit agent session logs from S3",
	Long:    `List and fetch archived agent session logs for analysis.`,
	GroupID: "orchestration",
}

// ── audit list ──────────────────────────────────────────────────────────

var auditListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available session logs",
	Long: `List session logs archived to S3 by coop.

Requires COOP_S3_BUCKET to be set. Uses COOP_S3_PREFIX (default: coop/sessions).

Examples:
  gb audit list --since 24h
  gb audit list --since 7d --project gardenio --role crew
  gb audit list --since 48h --agent fix-auth-bug --limit 20
  gb audit list --since 24h --has-transcript        # only inspectable sessions
  gb audit list --since 24h --status error          # genuine failures only (excludes clean stops)
  gb audit list --session-id d8fa1a0e-0ff2-4307-96c8-b945568c30a7`,
	RunE: func(cmd *cobra.Command, args []string) error {
		bucket := s3Bucket()
		if bucket == "" {
			return fmt.Errorf("COOP_S3_BUCKET is not set")
		}

		since, _ := cmd.Flags().GetString("since")
		before, _ := cmd.Flags().GetString("before")
		project, _ := cmd.Flags().GetString("project")
		agent, _ := cmd.Flags().GetString("agent")
		role, _ := cmd.Flags().GetString("role")
		status, _ := cmd.Flags().GetString("status")
		limit, _ := cmd.Flags().GetInt("limit")
		maxSessions, _ := cmd.Flags().GetInt("max-sessions")
		// Reconcile the two independently-defaulted caps (--limit on RESULTS,
		// --max-sessions on the FETCH phase) so raising only --max-sessions is not
		// silently clamped by the default --limit. See resolveResultLimit.
		limit = resolveResultLimit(limit,
			cmd.Flags().Changed("max-sessions"), cmd.Flags().Changed("limit"))
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		fetchTimeout, _ := cmd.Flags().GetDuration("fetch-timeout")
		verbose, _ := cmd.Flags().GetBool("verbose")
		sessionID, _ := cmd.Flags().GetString("session-id")
		hasTranscript, _ := cmd.Flags().GetBool("has-transcript")

		var sinceTime, beforeTime time.Time
		if since != "" {
			d, err := parseDuration(since)
			if err != nil {
				return fmt.Errorf("invalid --since: %w", err)
			}
			sinceTime = time.Now().Add(-d)
		}
		if before != "" {
			d, err := parseDuration(before)
			if err != nil {
				return fmt.Errorf("invalid --before: %w", err)
			}
			beforeTime = time.Now().Add(-d)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), auditListDeadline)
		defer cancel()

		fmt.Fprintf(os.Stderr, "Resolving AWS credentials (timeout=%s)...\n", awsConfigTimeout)
		client, err := newS3Client(ctx)
		if err != nil {
			return err
		}

		prefix := s3Prefix()

		// Direct session-id lookup short-circuits the list.
		var sessions []string
		if sessionID != "" {
			sessions = []string{sessionID}
			fmt.Fprintf(os.Stderr, "Looking up session %s in s3://%s/%s/...\n", sessionID, bucket, prefix)
		} else {
			fmt.Fprintf(os.Stderr, "Listing sessions under s3://%s/%s/ (cap=%d)...\n", bucket, prefix, maxSessions)
			t0 := time.Now()
			sessions, err = listSessions(ctx, client, bucket, prefix, maxSessions, sinceTime)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Found %d session(s) in %s; fetching metadata (concurrency=%d, per-call timeout=%s)...\n",
				len(sessions), time.Since(t0).Truncate(time.Millisecond), concurrency, fetchTimeout)
		}

		metas := fetchMetasConcurrent(ctx, client, bucket, prefix, sessions, concurrency, fetchTimeout, verbose)

		// Apply filters.
		var filtered []sessionMeta
		for _, meta := range metas {
			startTime := time.Unix(int64(meta.StartedAt), 0)
			if !sinceTime.IsZero() && startTime.Before(sinceTime) {
				continue
			}
			if !beforeTime.IsZero() && startTime.After(beforeTime) {
				continue
			}
			if project != "" && ptrStr(meta.Project) != project {
				continue
			}
			if agent != "" && ptrStr(meta.AgentName) != agent {
				continue
			}
			if role != "" && ptrStr(meta.Role) != role {
				continue
			}
			if status != "" && !strings.EqualFold(auditStatus(&meta), status) {
				continue
			}
			if hasTranscript && (meta.TranscriptCount == nil || *meta.TranscriptCount == 0) {
				continue
			}
			filtered = append(filtered, meta)
		}

		// Sort by start time descending (newest first).
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].StartedAt > filtered[j].StartedAt
		})

		// Apply limit. When --limit actually drops results, surface a hint to
		// stderr so the cap is discoverable (mirrors the --max-sessions hint in
		// listSessions). Only fires when rows were truncated (len > limit), not
		// when the result set merely happens to land at exactly limit.
		if limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
			fmt.Fprintf(os.Stderr,
				"note: showing %d result(s) capped by --limit; raise --limit (or --limit 0) to widen\n",
				limit)
		}

		if jsonOutput {
			printJSON(filtered)
			return nil
		}

		if len(filtered) == 0 {
			fmt.Println("No sessions found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SESSION\tAGENT\tPROJECT\tROLE\tSTARTED\tDURATION\tSTATUS\tTRANSCRIPTS")
		for _, m := range filtered {
			started := time.Unix(int64(m.StartedAt), 0).Format("2006-01-02 15:04")
			dur := "-"
			if d := m.duration(); d > 0 {
				dur = d.Truncate(time.Second).String()
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.SessionID,
				ptrStr(m.AgentName),
				ptrStr(m.Project),
				ptrStr(m.Role),
				started,
				dur,
				auditStatus(&m),
				ptrUint32(m.TranscriptCount),
			)
		}
		w.Flush()
		fmt.Printf("\n%d sessions\n", len(filtered))
		return nil
	},
}

// resolveResultLimit reconciles the two independently-defaulted caps on
// `gb audit list`: --limit bounds the RESULTS returned after filtering, while
// --max-sessions bounds the metadata-FETCH phase. Each has its own default
// (50 and 500), so a caller who raises only --max-sessions would otherwise be
// silently clamped back to 50 by the default --limit.
//
// Rule: when the user explicitly set --max-sessions but left --limit at its
// default, treat --limit as uncapped (0) so "I asked for N sessions" returns up
// to N. An explicit --limit (even alongside --max-sessions) always wins, and a
// run that sets neither keeps the default --limit.
func resolveResultLimit(limit int, maxSessionsChanged, limitChanged bool) int {
	if maxSessionsChanged && !limitChanged {
		return 0
	}
	return limit
}

// ── audit fetch ─────────────────────────────────────────────────────────

var auditFetchCmd = &cobra.Command{
	Use:   "fetch <session-id>",
	Short: "Download session transcripts to local directory",
	Long: `Download session transcript files from S3 to a local temp directory.

The id may be a full session id ("<bead>/<coop-uuid>" or a legacy flat
"<coop-uuid>") or a bare bead id, in which case every session archived under
that bead is fetched.

Examples:
  gb audit fetch abc-123-def
  gb audit fetch ka-0000002289                    # every session under the bead
  gb audit fetch ka-0000002289/abc-123-def        # one specific session
  gb audit fetch --batch sessions.txt
  gb audit fetch --batch sessions.txt --output /tmp/audit`,
	RunE: func(cmd *cobra.Command, args []string) error {
		bucket := s3Bucket()
		if bucket == "" {
			return fmt.Errorf("COOP_S3_BUCKET is not set")
		}

		batchFile, _ := cmd.Flags().GetString("batch")
		outputDir, _ := cmd.Flags().GetString("output")

		var sessionIDs []string
		if batchFile != "" {
			ids, err := readBatchFile(batchFile)
			if err != nil {
				return fmt.Errorf("reading batch file: %w", err)
			}
			sessionIDs = ids
		} else if len(args) > 0 {
			sessionIDs = args
		} else {
			return fmt.Errorf("provide a session ID or use --batch <file>")
		}

		if outputDir == "" {
			var err error
			outputDir, err = os.MkdirTemp("", "gb-audit-*")
			if err != nil {
				return fmt.Errorf("creating temp directory: %w", err)
			}
		} else {
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("creating output directory: %w", err)
			}
		}

		ctx := cmd.Context()
		client, err := newS3Client(ctx)
		if err != nil {
			return err
		}

		prefix := s3Prefix()
		var allPaths []string
		var fetched []string

		for _, sid := range sessionIDs {
			targets, err := resolveFetchTargets(ctx, client, bucket, prefix, sid)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %v\n", err)
				continue
			}
			// Say which sessions a bare bead id expanded to, so the local
			// layout is never a surprise.
			if len(targets) > 1 || targets[0] != sid {
				fmt.Fprintf(os.Stderr, "note: %s resolved to %d session(s): %s\n",
					sid, len(targets), strings.Join(targets, ", "))
			}
			for _, target := range targets {
				paths, err := fetchSession(ctx, client, bucket, prefix, target, outputDir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to fetch %s: %v\n", target, err)
					continue
				}
				fetched = append(fetched, target)
				allPaths = append(allPaths, paths...)
			}
		}

		// A fetch that downloaded nothing must not exit 0 — the files ARE the
		// output, so a silent empty result reads as "this session has no
		// transcripts" to both humans and scripted callers (ka-0000002531).
		if len(allPaths) == 0 {
			return fmt.Errorf("no files downloaded for %d requested id(s) (see warnings above)", len(sessionIDs))
		}

		if jsonOutput {
			printJSON(map[string]any{
				"output_dir": outputDir,
				"sessions":   fetched,
				"files":      allPaths,
				"count":      len(allPaths),
			})
		} else {
			for _, p := range allPaths {
				fmt.Println(p)
			}
			fmt.Fprintf(os.Stderr, "\n%d files downloaded to %s\n", len(allPaths), outputDir)
		}
		return nil
	},
}

// ── S3 helpers ──────────────────────────────────────────────────────────

// listSessions discovers session IDs by scanning the keyspace for meta.json
// objects, the marker coop writes for every session. This is layout-agnostic
// and handles both:
//
//   - legacy flat layout: <prefix>/<coop-uuid>/meta.json     → id "<coop-uuid>"
//   - bead-namespaced layout: <prefix>/<bead>/<coop-uuid>/meta.json
//     → composite id "<bead>/<coop-uuid>"
//
// Coop switched to the bead-namespaced layout (~2026-05-03, ka-0000003664) so
// concurrent/sequential spawns sharing a bead stay distinguishable. The returned
// id is formatted straight back into "<prefix>/<id>/meta.json" by fetchMeta and
// "<prefix>/<id>/transcripts/" by fetchSession, so both layouts resolve with no
// downstream changes.
//
// Filtering on the "/meta.json" suffix (mirroring coop's own list_spawns in
// coop/crates/cli/src/s3.rs) avoids false positives from layout sub-directories
// like <bead>/transcripts/ that share a first-level common prefix — a naive
// one-level delimiter descent would emit those as phantom sessions.
//
// Discovery is not bounded by maxSessions: with lexicographic object ordering
// the legacy flat hex-UUID prefixes (which sort before the gd-/pod-/monorepo
// bead prefixes) would otherwise dominate the cap and starve the recent window
// entirely — the original bug. Instead we collect every session's id + its
// meta.json LastModified, then the caller sorts by recency and applies
// maxSessions to bound the (expensive) metadata-fetch phase. When sinceTime is
// non-zero we drop sessions whose meta.json predates it: a session's meta is
// written at/after the session starts, so meta older than --since cannot match
// the --since filter — a lossless prune that keeps the fetch phase small for
// the common recent-window query.
//
// If the context deadline trips mid-scan, the partial result collected so far
// is returned (newest-biased after the caller's sort) rather than failing.
func listSessions(ctx context.Context, client *s3.Client, bucket, prefix string, maxSessions int, sinceTime time.Time) ([]string, error) {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type candidate struct {
		id       string
		modified time.Time
	}
	var candidates []candidate

	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			// On deadline/cancel, return what we have so far instead of
			// failing the whole command — the caller sorts by recency and
			// surfaces the newest sessions it managed to discover.
			if ctx.Err() != nil && len(candidates) > 0 {
				fmt.Fprintf(os.Stderr,
					"warning: session listing interrupted (%v); returning %d discovered so far\n",
					ctx.Err(), len(candidates))
				break
			}
			return nil, fmt.Errorf("listing S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			rest, ok := strings.CutPrefix(*obj.Key, prefix)
			if !ok {
				continue
			}
			id, ok := strings.CutSuffix(rest, "/meta.json")
			if !ok || id == "" {
				continue
			}
			var modified time.Time
			if obj.LastModified != nil {
				modified = *obj.LastModified
			}
			// Lossless prune: a session started within --since cannot have a
			// meta.json written before sinceTime.
			if !sinceTime.IsZero() && modified.Before(sinceTime) {
				continue
			}
			candidates = append(candidates, candidate{id: id, modified: modified})
		}
	}

	// Newest first so maxSessions bounds the fetch phase to the most recent
	// sessions rather than the lexicographically-first (oldest, frozen) ones.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modified.After(candidates[j].modified)
	})

	if maxSessions > 0 && len(candidates) > maxSessions {
		fmt.Fprintf(os.Stderr,
			"note: discovered %d session(s); fetching metadata for the %d most recent (raise --max-sessions to widen)\n",
			len(candidates), maxSessions)
		candidates = candidates[:maxSessions]
	}

	sessions := make([]string, len(candidates))
	for i, c := range candidates {
		sessions[i] = c.id
	}
	return sessions, nil
}

// fetchMetasConcurrent fetches meta.json for a slice of session IDs using a
// bounded worker pool. Each fetch is wrapped in its own timeout; failures are
// logged (when verbose) and skipped rather than aborting the run.
func fetchMetasConcurrent(
	ctx context.Context,
	client *s3.Client,
	bucket, prefix string,
	sessions []string,
	concurrency int,
	perCallTimeout time.Duration,
	verbose bool,
) []sessionMeta {
	if concurrency < 1 {
		concurrency = 1
	}

	in := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var (
		out      []sessionMeta
		fetched  int
		failed   int
		progress = 50 // emit a heartbeat every N fetches
	)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sid := range in {
				fctx, cancel := context.WithTimeout(ctx, perCallTimeout)
				meta, err := fetchMeta(fctx, client, bucket, prefix, sid)
				cancel()

				mu.Lock()
				if err != nil {
					failed++
					if verbose {
						fmt.Fprintf(os.Stderr, "  skip %s: %v\n", sid, err)
					}
				} else {
					out = append(out, *meta)
				}
				fetched++
				if fetched%progress == 0 {
					fmt.Fprintf(os.Stderr, "  ...fetched meta for %d/%d sessions (%d failed)\n",
						fetched, len(sessions), failed)
				}
				mu.Unlock()
			}
		}()
	}

	for _, sid := range sessions {
		select {
		case <-ctx.Done():
			close(in)
			wg.Wait()
			return out
		case in <- sid:
		}
	}
	close(in)
	wg.Wait()

	fmt.Fprintf(os.Stderr, "Fetched metadata for %d session(s) (%d skipped due to errors).\n",
		len(out), failed)
	return out
}

// fetchMeta downloads and parses meta.json for a session.
func fetchMeta(ctx context.Context, client *s3.Client, bucket, prefix, sessionID string) (*sessionMeta, error) {
	key := fmt.Sprintf("%s/%s/meta.json", prefix, sessionID)
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("getting meta.json for %s: %w", sessionID, err)
	}
	defer out.Body.Close()

	var meta sessionMeta
	if err := json.NewDecoder(out.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding meta.json for %s: %w", sessionID, err)
	}
	if meta.SessionID == "" {
		meta.SessionID = sessionID
	}
	return &meta, nil
}

// resolveFetchTargets maps a user-supplied id onto the concrete session id(s)
// fetchSession can resolve against the bead-namespaced S3 layout.
//
// `gb audit list` emits ids of the form "<bead>/<coop-uuid>", but the `bead_id`
// column it also prints is the natural first thing a forensic caller feeds to
// fetch. A bare bead id builds every key one level too shallow, so before this
// resolution every GET/LIST missed and fetch reported "0 files" with exit 0 — a
// silent false-empty indistinguishable from a session that archived nothing
// (ka-0000002531).
//
// A single LIST of "<prefix>/<id>/" classifies the id, using the same
// "/meta.json"-suffix discovery as listSessions so both paths agree on the
// layout:
//
//   - "meta.json" at the root            → id is already a session (any layout)
//   - "<coop-uuid>/meta.json" children   → id is a bead; expand to every session
//   - only "transcripts/..." at the root → legacy session whose meta.json is
//     missing; fetch it directly (fetchSession warns about the absent meta)
//
// Sessions are returned newest-first by meta.json LastModified, matching the
// recency ordering of `gb audit list`. A bead root that also holds a stray
// transcripts/ dir resolves to its sub-sessions, not the stray dir — the same
// phantom-prefix trap listSessions already sidesteps.
func resolveFetchTargets(ctx context.Context, client *s3.Client, bucket, prefix, id string) ([]string, error) {
	idPrefix := fmt.Sprintf("%s/%s/", prefix, id)

	type candidate struct {
		id       string
		modified time.Time
	}
	var subSessions []candidate
	var directMeta, directTranscripts bool

	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(idPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", id, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			rest, ok := strings.CutPrefix(*obj.Key, idPrefix)
			if !ok {
				continue
			}
			if rest == "meta.json" {
				directMeta = true
				continue
			}
			if strings.HasPrefix(rest, "transcripts/") {
				directTranscripts = true
				continue
			}
			child, ok := strings.CutSuffix(rest, "/meta.json")
			if !ok || child == "" || strings.Contains(child, "/") {
				continue
			}
			var modified time.Time
			if obj.LastModified != nil {
				modified = *obj.LastModified
			}
			subSessions = append(subSessions, candidate{id: id + "/" + child, modified: modified})
		}
	}

	if directMeta {
		return []string{id}, nil
	}
	if len(subSessions) > 0 {
		sort.Slice(subSessions, func(i, j int) bool {
			return subSessions[i].modified.After(subSessions[j].modified)
		})
		targets := make([]string, len(subSessions))
		for i, c := range subSessions {
			targets[i] = c.id
		}
		return targets, nil
	}
	if directTranscripts {
		return []string{id}, nil
	}

	return nil, fmt.Errorf("no session transcripts found under s3://%s/%s — "+
		"pass a session id from `gb audit list` (\"<bead>/<coop-uuid>\") or a bead id that has archived sessions",
		bucket, idPrefix)
}

// fetchSession downloads all transcript files for a session.
func fetchSession(ctx context.Context, client *s3.Client, bucket, prefix, sessionID, outputDir string) ([]string, error) {
	sessionPrefix := fmt.Sprintf("%s/%s/transcripts/", prefix, sessionID)
	sessionDir := filepath.Join(outputDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}

	// Also download meta.json.
	metaKey := fmt.Sprintf("%s/%s/meta.json", prefix, sessionID)
	metaPath := filepath.Join(sessionDir, "meta.json")
	if err := downloadFile(ctx, client, bucket, metaKey, metaPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: no meta.json for %s\n", sessionID)
	}

	// List and download transcripts.
	var paths []string
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(sessionPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing transcripts for %s: %w", sessionID, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			filename := filepath.Base(*obj.Key)
			localPath := filepath.Join(sessionDir, filename)
			if err := downloadFile(ctx, client, bucket, *obj.Key, localPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to download %s: %v\n", *obj.Key, err)
				continue
			}
			paths = append(paths, localPath)
		}
	}

	if metaPath != "" {
		if _, err := os.Stat(metaPath); err == nil {
			paths = append([]string{metaPath}, paths...)
		}
	}

	return paths, nil
}

// downloadFile downloads a single S3 object to a local file.
func downloadFile(ctx context.Context, client *s3.Client, bucket, key, localPath string) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, out.Body)
	return err
}

// parseDuration parses a human-friendly duration like "24h", "7d", "48h", "30m".
func parseDuration(s string) (time.Duration, error) {
	// Handle "d" suffix for days.
	if strings.HasSuffix(s, "d") {
		s = strings.TrimSuffix(s, "d")
		var days int
		if _, err := fmt.Sscanf(s, "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	// Fall back to standard Go duration parsing.
	return time.ParseDuration(s)
}

// readBatchFile reads session IDs from a file, one per line.
func readBatchFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ids []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			ids = append(ids, line)
		}
	}
	return ids, scanner.Err()
}

func init() {
	auditCmd.AddCommand(auditListCmd)
	auditCmd.AddCommand(auditFetchCmd)

	auditListCmd.Flags().String("since", "", "show sessions started within this duration (e.g., 24h, 7d)")
	auditListCmd.Flags().String("before", "", "show sessions started before this duration ago")
	auditListCmd.Flags().String("project", "", "filter by project")
	auditListCmd.Flags().String("agent", "", "filter by agent name")
	auditListCmd.Flags().String("role", "", "filter by agent role")
	auditListCmd.Flags().String("status", "", "filter by exit status (success, stopped, suspended, switch, timeout, error); case-insensitive")
	auditListCmd.Flags().Int("limit", 50, "max RESULTS to return after filtering (0 = no cap); ignored when only --max-sessions is set explicitly")
	auditListCmd.Flags().Int("max-sessions", 500, "cap on the N most-recent sessions to FETCH metadata for after discovery (0 = no cap)")
	auditListCmd.Flags().Int("concurrency", 16, "parallel meta.json fetches")
	auditListCmd.Flags().Duration("fetch-timeout", 10*time.Second, "per-fetch timeout for meta.json")
	auditListCmd.Flags().Bool("verbose", false, "log per-session fetch failures")
	auditListCmd.Flags().String("session-id", "", "look up a single session by ID (skips listing)")
	auditListCmd.Flags().Bool("has-transcript", false, "only show sessions with at least one archived transcript (transcript_count > 0)")

	auditFetchCmd.Flags().String("batch", "", "file containing session IDs (one per line)")
	auditFetchCmd.Flags().String("output", "", "output directory (default: auto-created temp dir)")
}
