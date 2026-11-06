package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"gardenio/controller/internal/commons"
)

// The Commons attachment store (gd-S8ufEc3k6s): the files people attach to a
// message, kept in the session object store the /image proxy reads (S3, or
// MinIO in-box, on the same COOP_S3_* settings and credentials) but outside
// its images subtree, under COMMONS_FILES_PREFIX (default commons/files). The
// /image proxy never serves them: the Commons' own file route does, after it
// has read the channel on the viewer's bearer (internal/commons/files.go).
//
// COMMONS_FILES_BUCKET puts them in a bucket of their own instead. Writing
// needs PutObject and DeleteObject there; the in-box viewer's MinIO
// credentials have both.

// defaultCommonsFilesPrefix is where attachments live when
// COMMONS_FILES_PREFIX is unset: beside coop/, not under it, so a lifecycle
// rule scoped to the session archive does not expire them.
const defaultCommonsFilesPrefix = "commons/files"

// s3CommonsObjects is the slice of *s3.Client the attachment store uses.
type s3CommonsObjects interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// s3CommonsFiles is a commons.FileStore over one bucket and prefix.
type s3CommonsFiles struct {
	client s3CommonsObjects
	bucket string
	prefix string
}

var _ commons.FileStore = (*s3CommonsFiles)(nil)

func (s *s3CommonsFiles) key(k string) *string { return aws.String(s.prefix + "/" + k) }

func (s *s3CommonsFiles) PutFile(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           s.key(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
	})
	return err
}

func (s *s3CommonsFiles) GetFile(ctx context.Context, key string) (commons.StoredFile, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	if err != nil {
		var noKey *s3types.NoSuchKey
		var notFound *s3types.NotFound
		var apiErr smithy.APIError
		if errors.As(err, &noKey) || errors.As(err, &notFound) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey") {
			return commons.StoredFile{}, commons.ErrNoFile
		}
		return commons.StoredFile{}, err
	}
	return commons.StoredFile{Body: out.Body, ContentType: aws.ToString(out.ContentType), ETag: aws.ToString(out.ETag), Size: aws.ToInt64(out.ContentLength)}, nil
}

func (s *s3CommonsFiles) DeleteFile(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	return err
}

// newCommonsFilesFromEnv builds the attachment store from COMMONS_FILES_BUCKET
// (else COOP_S3_BUCKET) and COMMONS_FILES_PREFIX. It returns (nil, nil) when
// no bucket is configured: the Commons then offers no attaching.
func newCommonsFilesFromEnv(ctx context.Context) (*s3CommonsFiles, error) {
	bucket := strings.TrimSpace(os.Getenv("COMMONS_FILES_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("COOP_S3_BUCKET"))
	}
	if bucket == "" {
		return nil, nil
	}
	prefix := strings.Trim(strings.TrimSpace(os.Getenv("COMMONS_FILES_PREFIX")), "/")
	if prefix == "" {
		prefix = defaultCommonsFilesPrefix
	}
	client, err := newCassetteS3Client(ctx) // the same AWS chain and endpoint as every S3 reader here
	if err != nil {
		return nil, err
	}
	return &s3CommonsFiles{client: client, bucket: bucket, prefix: prefix}, nil
}
