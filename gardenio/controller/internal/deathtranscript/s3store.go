package deathtranscript

// s3store.go — the production ObjectStore backed by aws-sdk-go-v2, plus the
// env-var helpers that locate the recording bucket/prefix. Kept separate from
// deathtranscript.go so the core mapping+parsing logic carries no AWS
// dependency and stays trivially unit-testable against the in-memory mock.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3API is the subset of the S3 client s3Store uses (list + get), narrowed to
// an interface so the adapter itself is exercisable with a fake SDK client.
type s3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// s3Store adapts an S3 client to the ObjectStore interface.
type s3Store struct {
	client s3API
	bucket string
}

// NewS3Store builds an ObjectStore over an aws-sdk-go-v2 S3 client for bucket.
func NewS3Store(client *s3.Client, bucket string) ObjectStore {
	return &s3Store{client: client, bucket: bucket}
}

// List returns every object key under prefix, following pagination.
func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}

// Get returns the full object bytes for an exact key.
func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("getting s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()
	// Bound the read so a corrupt/huge object cannot exhaust memory; the parser
	// enforces the same cap on the returned bytes.
	data, err := io.ReadAll(io.LimitReader(out.Body, maxTranscriptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading s3://%s/%s: %w", s.bucket, key, err)
	}
	return data, nil
}

// BucketFromEnv returns the recording bucket from COOP_S3_BUCKET ("" when unset).
func BucketFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvBucket))
}

// PrefixFromEnv returns the recording key prefix from COOP_S3_PREFIX, defaulting
// to DefaultPrefix. Mirrors gb audit.go's s3Prefix().
func PrefixFromEnv() string {
	if p := strings.TrimSpace(os.Getenv(EnvPrefix)); p != "" {
		return p
	}
	return DefaultPrefix
}
