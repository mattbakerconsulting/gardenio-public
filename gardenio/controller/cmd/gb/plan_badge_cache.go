package main

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// plan_badge_cache.go — the S3-backed BadgeStore that turns `gb badge`
// into a content-addressed, reproducible cache (Phase 2 task ka-0000005930).
//
// The pure cache LOGIC (key derivation, read-through, retitle invalidation,
// advisory-error discipline) lives in internal/badge/cache.go and is
// storage-agnostic. This file is only the S3 adapter: it satisfies
// badge.BadgeStore against the SAME bucket/prefix/credentials
// `gb image upload` already uses, so a cached badge lands under
// coop/sessions/images/badges/ and is served by the existing viewer image
// proxy with no new access surface.

// s3BadgeStore implements badge.BadgeStore over the images bucket
// (s3ImagesBucket); a miss there just regenerates.
type s3BadgeStore struct {
	client s3BadgeClient
	bucket string
}

// s3BadgeClient is the minimal S3 surface the badge cache needs, kept as an
// interface so tests inject an in-memory fake (mirrors s3Uploader/s3Downloader
// in image.go / image_fetch.go).
type s3BadgeClient interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// newBadgeStore builds an S3-backed badge store, or returns (nil, nil) when no
// bucket is configured — a nil store is a valid argument to
// badge.LoadOrGenerate, which then always generates. This keeps the caller
// free of "is caching available?" branching.
func newBadgeStore(ctx context.Context) (*s3BadgeStore, error) {
	bucket := s3ImagesBucket()
	if bucket == "" {
		return nil, nil
	}
	client, err := newBadgeS3Client(ctx)
	if err != nil {
		return nil, err
	}
	return &s3BadgeStore{client: client, bucket: bucket}, nil
}

// newBadgeS3Client is the production hook; tests override it.
var newBadgeS3Client = func(ctx context.Context) (s3BadgeClient, error) {
	return newS3Client(ctx)
}

// Get returns the stored badge SVG for key. A missing object is a clean miss
// (found=false, nil error), NOT an error — that is what lets LoadOrGenerate
// treat "not cached yet" and "backend hiccup" distinctly.
func (s *s3BadgeStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Put stores an SVG badge at key with an image/svg+xml content type so the
// viewer proxy serves it renderably.
func (s *s3BadgeStore) Put(ctx context.Context, key string, svg []byte) error {
	_, err := s.client.PutObject(ctx, putImageInput(s.bucket, key, svg, "image/svg+xml"))
	return err
}

// isS3NotFound reports whether an S3 GetObject error is a plain "object does not
// exist" (NoSuchKey / 404 NotFound), which the cache treats as a miss rather
// than a failure. Anything else — permission denied, throttling, network — is a
// real error the cache surfaces (then falls through to generate).
func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	return errors.As(err, &nf)
}
