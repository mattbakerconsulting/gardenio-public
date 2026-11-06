package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/purge"
)

// The Lands purge sweep (internal/lands/purge, Track B spec §3.3, step 8.11):
// once an hour it deletes a closed tenant land's beads and coop session
// objects LANDS_RETENTION_DAYS after land_closed_at, its credit ledger
// LANDS_RECORDS_RETENTION_DAYS after, and a denied or withdrawn plot
// application LANDS_APPLICATION_RETENTION_DAYS after its decision, the
// periods the privacy policy states (gardenio/docs/lands/legal/privacy.md §5).
// It deletes data, so it is off unless LANDS_PURGE_ENABLED is true, and on
// it is a dry run unless LANDS_PURGE_DRY_RUN is exactly false: it deletes
// nothing and logs what is due ("lands purge: dry run: ..."), so the first
// sweep of a rollout is read before one acts.
// The daemon holds every purge to its own floors
// (BEADS_LANDS_*_RETENTION_DAYS); these periods may be longer, not shorter.
//
// The beads go through POST /v1/lands/purge as the controller's machine
// principal, which must hold gardenio-machine-lands-provisioner. Session
// objects are deleted from COOP_S3_BUCKET under COOP_S3_PREFIX with the
// controller's own S3 credentials (IRSA, or the MinIO keys in endpoint
// mode), which need s3:ListBucketVersions and s3:DeleteObjectVersion there.
const (
	landsPurgeEnabledEnv           = "LANDS_PURGE_ENABLED"
	landsPurgeDryRunEnv            = "LANDS_PURGE_DRY_RUN"
	landsRetentionDaysEnv          = "LANDS_RETENTION_DAYS"
	landsRecordsRetentionDaysEnv   = "LANDS_RECORDS_RETENTION_DAYS"
	landsApplicationRetentionDaysE = "LANDS_APPLICATION_RETENTION_DAYS"
)

// landsPurgeEnabled reports whether the deployment turned the sweep on.
func landsPurgeEnabled(getenv func(string) string) bool {
	return strings.EqualFold(strings.TrimSpace(getenv(landsPurgeEnabledEnv)), "true")
}

// landsPurgeDays reads a retention period in whole days, def when unset.
func landsPurgeDays(getenv func(string) string, key string, def int) (time.Duration, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return purge.Days(def), nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s=%q is not a whole number of days, at least 1", key, v)
	}
	return purge.Days(n), nil
}

// newLandsPurge builds the sweep from the environment. It returns nil and no
// error when the sweep is off. sessions is the session store, nil when the
// deployment keeps no coop sessions (COOP_S3_BUCKET unset).
func newLandsPurge(daemon purge.Daemon, sessions purge.Sessions, sessionPrefix string, getenv func(string) string, logger *slog.Logger) (*purge.Sweeper, error) {
	if !landsPurgeEnabled(getenv) {
		return nil, nil
	}
	s := &purge.Sweeper{
		Daemon: daemon, Sessions: sessions, SessionPrefix: sessionPrefix,
		DryRun: !strings.EqualFold(strings.TrimSpace(getenv(landsPurgeDryRunEnv)), "false"),
		Logger: logger.With("component", "lands-purge"),
	}
	var err error
	if s.Retention, err = landsPurgeDays(getenv, landsRetentionDaysEnv, purge.DefaultRetentionDays); err != nil {
		return nil, err
	}
	if s.RecordsRetention, err = landsPurgeDays(getenv, landsRecordsRetentionDaysEnv, purge.DefaultRecordsRetentionDays); err != nil {
		return nil, err
	}
	if s.ApplicationRetention, err = landsPurgeDays(getenv, landsApplicationRetentionDaysE, purge.DefaultApplicationRetentionDays); err != nil {
		return nil, err
	}
	if s.RecordsRetention < s.Retention {
		return nil, fmt.Errorf("%s (%s) is shorter than %s (%s)", landsRecordsRetentionDaysEnv, s.RecordsRetention, landsRetentionDaysEnv, s.Retention)
	}
	return s, nil
}

// landsPurgeSessions is the coop session store on the controller's S3
// credentials, or nil when there is no session bucket.
func landsPurgeSessions(cfg *config.Config) (purge.Sessions, error) {
	if cfg.CoopS3Bucket == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotAWSConfigTimeout)
	defer cancel()
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.CoopS3Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.CoopS3Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return purge.NewS3Sessions(s3.NewFromConfig(awsCfg, snapshotS3EndpointOpts(cfg.CoopS3Endpoint)...), cfg.CoopS3Bucket), nil
}

// startLandsPurge runs the sweep when it is on and can reach the session
// store; otherwise it logs why it is not running. A sweep that cannot delete
// sessions does not run at all: purging the beads would lose the agent list
// the sessions are found by.
func startLandsPurge(ctx context.Context, daemon *beadsapi.Client, cfg *config.Config, getenv func(string) string, logger *slog.Logger) {
	if !landsPurgeEnabled(getenv) {
		return
	}
	sessions, err := landsPurgeSessions(cfg)
	if err != nil {
		logger.Error("lands purge: not started; the session store is unreachable", "error", err)
		return
	}
	s, err := newLandsPurge(daemon, sessions, cfg.CoopS3Prefix, getenv, logger)
	if err != nil {
		logger.Error("lands purge: not started", "error", err)
		return
	}
	logger.Info("lands purge: started", "retention", s.Retention, "records_retention", s.RecordsRetention,
		"application_retention", s.ApplicationRetention, "session_bucket", cfg.CoopS3Bucket, "dry_run", s.DryRun)
	go s.Run(ctx, purge.Interval)
}
