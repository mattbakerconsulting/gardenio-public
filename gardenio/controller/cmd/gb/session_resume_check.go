package main

// session_resume_check.go — observable-truth checks for the session bead's
// `resumed` field.
//
// The previous implementation set resumed=true whenever a resume *intent*
// existed (a non-empty resumeLog path, or COOP_S3_RESUME_SESSION env var
// set). That misled investigators on first runs of fresh pods: the agent
// bead could carry a stale last_session_id, the env var would be set, but
// no S3 archive existed and nothing was actually restored — yet the
// session bead reported resumed=true (ka-0000005950).
//
// These helpers verify ground truth: the local file exists and has
// non-trivial content, or the S3 prefix actually contains objects.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// minResumeLogBytes mirrors the size threshold used by findResumeSession
// when picking a candidate transcript. Below this we treat the file as a
// stub with no real conversation to resume.
const minResumeLogBytes = 256

// localResumeLogLoadable returns true if path points to a real file with
// at least minResumeLogBytes of content. Used by registerSessionBead to
// decide whether the local resume path actually has prior state to load.
func localResumeLogLoadable(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() >= minResumeLogBytes
}

// s3ResumeSessionExistsFn is the function used by registerSessionBead to
// verify that an S3 archive exists for a given session ID. Exposed as a
// package-level var so tests can stub it without touching real S3.
var s3ResumeSessionExistsFn = s3ResumeSessionExists

// s3ResumeOutcome enumerates the distinct outcomes of a resume-archive
// existence check. Recorded both as a structured log line and as a
// Prometheus counter so an S3 brownout (which fails closed → resumed=false)
// becomes visible instead of silent.
//
// Acceptance for ka-0000003346 (G5 in docs/research/s3-resume-e2e.md):
//   - archive-exists       — at least one object under the prefix
//   - archive-empty        — list succeeded with zero contents
//   - list-error-timeout   — context deadline exceeded / canceled
//   - list-error-other     — any other ListObjectsV2 / client error
//   - no-bucket-config     — COOP_S3_BUCKET unset or empty session ID
type s3ResumeOutcome string

const (
	s3ResumeOutcomeArchiveExists s3ResumeOutcome = "archive-exists"
	s3ResumeOutcomeArchiveEmpty  s3ResumeOutcome = "archive-empty"
	s3ResumeOutcomeListTimeout   s3ResumeOutcome = "list-error-timeout"
	s3ResumeOutcomeListOther     s3ResumeOutcome = "list-error-other"
	s3ResumeOutcomeNoBucket      s3ResumeOutcome = "no-bucket-config"
)

// s3ResumeSessionExists returns true if at least one object exists in
// s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<sessionID>/. Returns false on
// any error — we only claim resumed=true when we can prove data is there.
//
// Records a structured log line and (when VMAGENT_URL is set) increments
// gardenio_s3_resume_check_total{outcome=...} so an S3 brownout — which
// fails closed and would otherwise look like a wave of fresh sessions —
// is observable. See docs/research/s3-resume-e2e.md §5 G5.
func s3ResumeSessionExists(ctx context.Context, sessionID string) bool {
	bucket := s3Bucket()
	if bucket == "" || sessionID == "" {
		recordS3ResumeOutcome(ctx, s3ResumeOutcomeNoBucket, sessionID, bucket, nil)
		return false
	}

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client, err := newS3Client(listCtx)
	if err != nil {
		recordS3ResumeOutcome(ctx, classifyS3ResumeError(err), sessionID, bucket, err)
		return false
	}

	prefix := s3Prefix() + "/" + sessionID + "/"
	maxKeys := int32(1)
	out, err := client.ListObjectsV2(listCtx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: &maxKeys,
	})
	if err != nil {
		recordS3ResumeOutcome(ctx, classifyS3ResumeError(err), sessionID, bucket, err)
		return false
	}
	if len(out.Contents) == 0 {
		recordS3ResumeOutcome(ctx, s3ResumeOutcomeArchiveEmpty, sessionID, bucket, nil)
		return false
	}
	recordS3ResumeOutcome(ctx, s3ResumeOutcomeArchiveExists, sessionID, bucket, nil)
	return true
}

// classifyS3ResumeError distinguishes timeout from other errors so the
// metric/log can tell "S3 was slow" from "S3 said no".
func classifyS3ResumeError(err error) s3ResumeOutcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return s3ResumeOutcomeListTimeout
	}
	// AWS SDK wraps timeouts in operation-error wrappers; sniff the message
	// as a best-effort fallback when errors.Is doesn't match.
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "context deadline exceeded") ||
			strings.Contains(msg, "context canceled") ||
			strings.Contains(msg, "request canceled") ||
			strings.Contains(msg, "Client.Timeout") {
			return s3ResumeOutcomeListTimeout
		}
	}
	return s3ResumeOutcomeListOther
}

// s3ResumeOutcomeRecorder is the function used to publish an outcome.
// Exposed as a package-level var so tests can capture calls without
// touching stdout or vmagent.
var s3ResumeOutcomeRecorder = defaultRecordS3ResumeOutcome

// recordS3ResumeOutcome publishes a single resume-check outcome. Thin
// wrapper so callers don't need to know whether the recorder was stubbed.
func recordS3ResumeOutcome(ctx context.Context, outcome s3ResumeOutcome, sessionID, bucket string, err error) {
	s3ResumeOutcomeRecorder(ctx, outcome, sessionID, bucket, err)
}

// defaultRecordS3ResumeOutcome emits the structured log line and pushes a
// counter increment to vmagent (when VMAGENT_URL is configured). Failures
// to push the metric are logged at warning level only — an observability
// failure must not affect session registration.
func defaultRecordS3ResumeOutcome(ctx context.Context, outcome s3ResumeOutcome, sessionID, bucket string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"[gb agent start] s3 resume check: outcome=%s session=%q bucket=%q err=%v\n",
			outcome, sessionID, bucket, err)
	} else {
		fmt.Fprintf(os.Stderr,
			"[gb agent start] s3 resume check: outcome=%s session=%q bucket=%q\n",
			outcome, sessionID, bucket)
	}
	pushS3ResumeOutcomeMetric(ctx, outcome)
}

// pushS3ResumeOutcomeMetric increments gardenio_s3_resume_check_total for
// the given outcome via vmagent's prometheus import endpoint. Best-effort:
// no VMAGENT_URL → silently skip; any HTTP error → warn only.
func pushS3ResumeOutcomeMetric(ctx context.Context, outcome s3ResumeOutcome) {
	agent := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	body := fmt.Sprintf(
		"gardenio_s3_resume_check_total{outcome=%q,agent=%q} 1\n",
		string(outcome), agent,
	)
	pushVMAgentLines(ctx, body, 3*time.Second)
}
