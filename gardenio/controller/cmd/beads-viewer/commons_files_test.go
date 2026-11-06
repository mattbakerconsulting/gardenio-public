package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gardenio/controller/internal/commons"
)

// fakeCommonsS3 is an in-memory bucket.
type fakeCommonsS3 struct {
	objs map[string][]byte
	cts  map[string]string
}

func (f *fakeCommonsS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, _ := io.ReadAll(in.Body)
	if int64(len(b)) != aws.ToInt64(in.ContentLength) {
		return nil, errors.New("content length does not match the body")
	}
	f.objs[aws.ToString(in.Bucket)+"|"+aws.ToString(in.Key)] = b
	f.cts[aws.ToString(in.Key)] = aws.ToString(in.ContentType)
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeCommonsS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b, ok := f.objs[aws.ToString(in.Bucket)+"|"+aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), ContentType: aws.String(f.cts[aws.ToString(in.Key)]), ETag: aws.String(`"e"`)}, nil
}

func (f *fakeCommonsS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objs, aws.ToString(in.Bucket)+"|"+aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

// TestS3CommonsFiles pins the attachment store: keys under its prefix in its
// bucket, a missing key as commons.ErrNoFile.
func TestS3CommonsFiles(t *testing.T) {
	fake := &fakeCommonsS3{objs: map[string][]byte{}, cts: map[string]string{}}
	s := &s3CommonsFiles{client: fake, bucket: "b", prefix: defaultCommonsFilesPrefix}
	ctx := context.Background()
	if err := s.PutFile(ctx, "cl/ch/id/a.png", "image/png", []byte("png")); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.objs["b|commons/files/cl/ch/id/a.png"]; !ok {
		t.Fatalf("stored %v", fake.objs)
	}
	f, err := s.GetFile(ctx, "cl/ch/id/a.png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f.Body)
	_ = f.Body.Close()
	if string(b) != "png" || f.ContentType != "image/png" || f.ETag != `"e"` {
		t.Errorf("got %q %+v", b, f)
	}
	if err := s.DeleteFile(ctx, "cl/ch/id/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFile(ctx, "cl/ch/id/a.png"); !errors.Is(err, commons.ErrNoFile) {
		t.Errorf("missing = %v, want ErrNoFile", err)
	}
}

func TestNewCommonsFilesFromEnv(t *testing.T) {
	t.Setenv("COMMONS_FILES_BUCKET", "")
	t.Setenv("COOP_S3_BUCKET", "")
	if s, err := newCommonsFilesFromEnv(context.Background()); s != nil || err != nil {
		t.Errorf("no bucket = %v, %v", s, err)
	}
	t.Setenv("COOP_S3_BUCKET", "sessions")
	t.Setenv("COOP_S3_REGION", "us-east-1")
	t.Setenv("COMMONS_FILES_PREFIX", "/att/")
	s, err := newCommonsFilesFromEnv(context.Background())
	if err != nil || s == nil || s.bucket != "sessions" || s.prefix != "att" {
		t.Fatalf("env store = %+v, %v", s, err)
	}
	t.Setenv("COMMONS_FILES_BUCKET", "attachments")
	if s, _ := newCommonsFilesFromEnv(context.Background()); s == nil || s.bucket != "attachments" {
		t.Errorf("own bucket = %+v", s)
	}
}
