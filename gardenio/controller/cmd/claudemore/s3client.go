package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gardenio/controller/internal/claudemore"
)

// awsConfigTimeout caps AWS credential resolution at startup so a broken
// IRSA/IMDS environment surfaces as a warn-level "recorder disabled" rather than
// wedging the proxy at boot. Mirrors the gb/beads-viewer timeout discipline.
const awsConfigTimeout = 8 * time.Second

// buildCassetteStore constructs the S3-backed cassette store from the process
// config. It runs on claudemore's IRSA credentials via the standard AWS config
// chain — the same env vars (COOP_S3_REGION / COOP_S3_ENDPOINT) the rest of the
// controller honors. Shared by the LIVE record path (Put) and the REPLAY serve
// path (Get) so the two agree on bucket/prefix/endpoint and key layout.
func buildCassetteStore(ctx context.Context, cfg config) (claudemore.CassetteStore, error) {
	// CLAUDEMORE_CASSETTE_DIR selects a filesystem store (Gasbox/BYO with no S3,
	// and the hermetic Phase-2 hooks e2e). It takes precedence over S3 so a run
	// that sets both a bucket and a dir uses the local dir deterministically.
	if cfg.cassetteDir != "" {
		store, err := claudemore.NewFSCassetteStore(cfg.cassetteDir)
		if err != nil {
			return nil, fmt.Errorf("build filesystem cassette store: %w", err)
		}
		return store, nil
	}
	client, err := newCassetteS3Client(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build S3 client: %w", err)
	}
	store, err := claudemore.NewS3CassetteStore(client, cfg.s3Bucket, cfg.s3Prefix)
	if err != nil {
		return nil, fmt.Errorf("build cassette store: %w", err)
	}
	return store, nil
}

// buildRecorder constructs the cassette store and wraps it in the async LIVE
// recorder. Returns an error (recorder left off) rather than blocking the proxy
// if S3 config resolution fails.
func buildRecorder(ctx context.Context, cfg config, logger *slog.Logger) (*cassetteRecorder, error) {
	store, err := buildCassetteStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newCassetteRecorder(ctx, store, logger), nil
}

// newCassetteS3Client builds an *s3.Client using the standard AWS config chain,
// honoring COOP_S3_REGION and COOP_S3_ENDPOINT (in-box MinIO / path-style) the
// same way cmd/gb and cmd/beads-viewer do.
func newCassetteS3Client(ctx context.Context, cfg config) (*s3.Client, error) {
	loadCtx, cancel := context.WithTimeout(ctx, awsConfigTimeout)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error
	if cfg.s3Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.s3Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(loadCtx, opts...)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsCfg, cassetteS3EndpointOpts(cfg)...), nil
}

// cassetteS3EndpointOpts mirrors s3EndpointOpts elsewhere in the controller: a
// non-empty COOP_S3_ENDPOINT targets a custom endpoint with path-style
// addressing (MinIO); empty leaves the default AWS S3 endpoint untouched.
func cassetteS3EndpointOpts(cfg config) []func(*s3.Options) {
	if cfg.s3Endpoint == "" {
		return nil
	}
	return []func(*s3.Options){
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.s3Endpoint)
			o.UsePathStyle = true
		},
	}
}
