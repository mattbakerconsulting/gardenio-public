package main

// salvage_restore.go — `gb salvage restore`: pull a salvaged worktree bundle
// back out of S3 and fetch its salvage ref into a local repo, so a successor
// agent or operator can recover an involuntarily-killed agent's uncommitted
// work (companion to salvage.go / bug ka-0000000146).
//
// Given an agent bead ID (and optional session key), it downloads
// salvage-manifest.json + every <target>.bundle under the salvage prefix,
// writes them to an output dir, and — when run inside a git repo — fetches each
// bundle's refs/salvage/* into that repo. The salvaged WIP is then recoverable
// with `git checkout refs/salvage/<name>` (or `git stash`/cherry-pick from it).
// Never mutates any branch; only writes under refs/salvage/*.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

var salvageRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Download a salvaged worktree bundle from S3 and fetch its refs",
	Long: `Restore pulls the git bundles + manifest a previous 'gb salvage' run wrote to
S3 for an agent, and (inside a git repo) fetches each bundle's refs/salvage/*
into the current repo.

  gb salvage restore --agent-bead gd-… [--session <key>] [--output <dir>] [--no-fetch]

The salvaged work is then recoverable with:
  git log --oneline refs/salvage/<name>
  git checkout refs/salvage/<name>        # detached, inspect / copy files out`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		agentBead, _ := cmd.Flags().GetString("agent-bead")
		session, _ := cmd.Flags().GetString("session")
		outDir, _ := cmd.Flags().GetString("output")
		noFetch, _ := cmd.Flags().GetBool("no-fetch")
		timeout, _ := cmd.Flags().GetDuration("timeout")

		if agentBead == "" {
			agentBead = agentS3SessionKey()
		}
		if agentBead == "" {
			return fmt.Errorf("--agent-bead is required (no KD_AGENT_ID in env to infer it)")
		}
		if session == "" {
			session = agentBead
		}
		if outDir == "" {
			outDir = filepath.Join(".", "salvage-"+agentBead)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		return runSalvageRestore(ctx, agentBead, session, outDir, noFetch)
	},
}

func init() {
	salvageRestoreCmd.Flags().String("agent-bead", "", "agent bead ID whose salvage to restore (default: KD_AGENT_ID)")
	salvageRestoreCmd.Flags().String("session", "", "session key under the agent bead (default: same as agent-bead)")
	salvageRestoreCmd.Flags().String("output", "", "directory to download bundles into (default: ./salvage-<agent-bead>)")
	salvageRestoreCmd.Flags().Bool("no-fetch", false, "download bundles only; do not fetch refs into the current repo")
	salvageRestoreCmd.Flags().Duration("timeout", 2*time.Minute, "hard deadline for the restore")
}

func runSalvageRestore(ctx context.Context, agentBead, session, outDir string, noFetch bool) error {
	bucket := s3Bucket()
	if bucket == "" {
		return fmt.Errorf("COOP_S3_BUCKET is not set")
	}
	client, err := newS3Client(ctx)
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}

	prefix := salvagePrefix(s3Prefix(), agentBead, session)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// List every object under the salvage prefix.
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix + "/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing salvage objects: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("no salvage found at s3://%s/%s/", bucket, prefix)
	}

	var bundlePaths []string
	for _, key := range keys {
		local := filepath.Join(outDir, filepath.Base(key))
		if err := salvageDownload(ctx, client, bucket, key, local); err != nil {
			fmt.Fprintf(os.Stderr, "warning: download %s: %v\n", key, err)
			continue
		}
		fmt.Printf("downloaded s3://%s/%s -> %s\n", bucket, key, local)
		if strings.HasSuffix(local, ".bundle") {
			bundlePaths = append(bundlePaths, local)
		}
	}

	// Pretty-print the manifest recovery recipe when present.
	if mf := filepath.Join(outDir, "salvage-manifest.json"); fileExists(mf) {
		if data, err := os.ReadFile(mf); err == nil {
			var m salvageManifest
			if json.Unmarshal(data, &m) == nil {
				fmt.Printf("\nmanifest: %d target(s), reason=%q, agent=%s\n", len(m.Targets), m.Reason, m.Agent)
			}
		}
	}

	if noFetch || len(bundlePaths) == 0 {
		fmt.Printf("\n%d bundle(s) in %s. Fetch with: git fetch <bundle> 'refs/salvage/*:refs/salvage/*'\n", len(bundlePaths), outDir)
		return nil
	}

	// Fetch each bundle's salvage refs into the current repo (if we are in one).
	if _, err := gitCtx(ctx, ".", "rev-parse", "--git-dir"); err != nil {
		fmt.Printf("\nnot inside a git repo — skipping fetch. Bundles are in %s.\n", outDir)
		return nil
	}
	for _, bp := range bundlePaths {
		if _, err := gitCtx(ctx, ".", "fetch", bp, "refs/salvage/*:refs/salvage/*"); err != nil {
			// The most common failure is a missing prerequisite: the bundle was
			// anchored on a remote sha (e.g. origin/main) this clone lacks. Point
			// the operator at the fix rather than surfacing a bare git error.
			if missing := bundleMissingPrereq(ctx, bp); missing != "" {
				fmt.Fprintf(os.Stderr,
					"warning: fetch %s failed — bundle needs commit %s which this clone lacks.\n"+
						"  Re-run restore from a clone that contains it, or fetch that sha first:\n"+
						"    git fetch origin %s\n", bp, missing, missing)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: fetch %s: %v\n", bp, err)
			continue
		}
	}
	fmt.Printf("\nFetched salvage refs. Inspect with: git log --oneline --all | grep salvage\n" +
		"Recover a worktree:  git checkout refs/salvage/<name>\n")
	return nil
}

// (fileExists is defined in session.go.)

// bundleMissingPrereq returns the first prerequisite sha `git bundle verify`
// reports as missing from the current repo, or "" if the bundle verifies (or
// verify itself errors). Used to turn an opaque fetch failure into an
// actionable "this clone lacks <sha>" message.
func bundleMissingPrereq(ctx context.Context, bundlePath string) string {
	out, err := gitCtx(ctx, ".", "bundle", "verify", bundlePath)
	if err == nil {
		return "" // verifies cleanly — the fetch failed for some other reason.
	}
	// `git bundle verify` prints "error: Repository lacks these prerequisite
	// commits:" followed by "<sha> <subject>" lines. Pull the first sha out of
	// the combined output (the error text is embedded in err via gitEnv).
	for line := range strings.SplitSeq(err.Error()+"\n"+out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 1 && len(f[0]) >= 7 && isHexSHA(f[0]) {
			return f[0]
		}
	}
	return ""
}

// isHexSHA reports whether s looks like a git object id (all lowercase hex).
func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// salvageDownload GETs one object to a local path.
func salvageDownload(ctx context.Context, client *s3.Client, bucket, key, local string) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, out.Body)
	return err
}
