package purge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3API is the slice of *s3.Client S3Sessions uses.
type s3API interface {
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// deleteBatch is the most keys one DeleteObjects call takes.
const deleteBatch = 1000

// S3Sessions deletes coop session objects from the session bucket (AWS S3 or
// MinIO). It deletes every version and delete marker under a prefix, not just
// the current objects: the sessions bucket is versioned, and a delete that
// left the old versions would keep the content for the noncurrent-version
// lifecycle (90 days by default), past what the privacy policy promises.
// The caller needs s3:ListBucketVersions and s3:DeleteObjectVersion on the
// prefix.
type S3Sessions struct {
	Client s3API
	Bucket string
}

// NewS3Sessions wraps an S3 client for bucket.
func NewS3Sessions(client *s3.Client, bucket string) *S3Sessions {
	return &S3Sessions{Client: client, Bucket: bucket}
}

// DeletePrefix deletes every version under prefix. A prefix that is not
// below a session root (empty, or without a trailing slash) is refused.
func (s *S3Sessions) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	deleted := 0
	err := s.walk(ctx, prefix, func(ids []s3types.ObjectIdentifier) error {
		for len(ids) > 0 {
			n := min(len(ids), deleteBatch)
			if err := s.delete(ctx, ids[:n]); err != nil {
				return err
			}
			deleted += n
			ids = ids[n:]
		}
		return nil
	})
	return deleted, err
}

// CountPrefix counts the versions and delete markers DeletePrefix would
// delete, under the same guard.
func (s *S3Sessions) CountPrefix(ctx context.Context, prefix string) (int, error) {
	count := 0
	err := s.walk(ctx, prefix, func(ids []s3types.ObjectIdentifier) error {
		count += len(ids)
		return nil
	})
	return count, err
}

// walk hands fn every version and delete marker under prefix, one listing
// page at a time.
func (s *S3Sessions) walk(ctx context.Context, prefix string, fn func([]s3types.ObjectIdentifier) error) error {
	if s.Bucket == "" {
		return errors.New("no session bucket")
	}
	if !strings.HasSuffix(prefix, "/") || strings.Count(prefix, "/") < 2 || strings.Contains(prefix, "//") {
		return fmt.Errorf("refusing to delete under %q: not one session's prefix", prefix)
	}
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix)}
	for {
		out, err := s.Client.ListObjectVersions(ctx, in)
		if err != nil {
			return fmt.Errorf("listing s3://%s/%s: %w", s.Bucket, prefix, err)
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range out.Versions {
			ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range out.DeleteMarkers {
			ids = append(ids, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if err := fn(ids); err != nil {
			return err
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		in.KeyMarker, in.VersionIdMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

func (s *S3Sessions) delete(ctx context.Context, ids []s3types.ObjectIdentifier) error {
	out, err := s.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.Bucket),
		Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("deleting from s3://%s: %w", s.Bucket, err)
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return fmt.Errorf("deleting s3://%s/%s: %s %s (and %d more)", s.Bucket, aws.ToString(e.Key),
			aws.ToString(e.Code), aws.ToString(e.Message), len(out.Errors)-1)
	}
	return nil
}
