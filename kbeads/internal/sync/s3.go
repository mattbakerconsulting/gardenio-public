package sync

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Destination writes JSONL data to an S3-compatible bucket.
type S3Destination struct {
	client *s3.Client
	bucket string
	key    string
}

// NewS3Destination creates an S3 destination. If endpoint is non-empty,
// path-style addressing is enabled (for MinIO and similar).
func NewS3Destination(ctx context.Context, bucket, key, region, endpoint string) (*S3Destination, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	var s3opts []func(*s3.Options)
	if endpoint != "" {
		s3opts = append(s3opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(cfg, s3opts...)
	return &S3Destination{
		client: client,
		bucket: bucket,
		key:    key,
	}, nil
}

// Write uploads data to S3 as the configured object key.
func (d *S3Destination) Write(ctx context.Context, data []byte) error {
	contentType := "application/x-ndjson"
	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(d.bucket),
		Key:         aws.String(d.key),
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("s3 put object: %w", err)
	}
	return nil
}
