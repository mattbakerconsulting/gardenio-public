package podmanager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NewS3SnapshotTransportFromClient is the exported constructor cmd/controller
// wires: it builds the presign adapter over an already-configured *s3.Client
// (region/endpoint resolved by the caller) and returns the off-node transport.
// A zero ttl uses defaultPresignTTL. This keeps the snapshotPresigner interface
// unexported (so tests inject a hermetic fake) while giving the controller a
// single clean entry point — it never has to touch the presign plumbing.
func NewS3SnapshotTransportFromClient(exec NodeExecutor, client *s3.Client, bucket, prefix string, ttl time.Duration, logger *slog.Logger) (*S3SnapshotTransport, error) {
	if client == nil {
		return nil, fmt.Errorf("s3 snapshot transport: nil s3 client")
	}
	presigner := newS3PresignAdapter(client, bucket, ttl)
	return NewS3SnapshotTransport(exec, presigner, bucket, prefix, logger)
}

// freezethaw_s3presign.go — the concrete snapshotPresigner backed by the AWS
// SDK's *s3.PresignClient. It is a thin adapter: it converts the SDK's
// *v4.PresignedHTTPRequest into the transport-local presignedRequest so the
// transport core (freezethaw_s3transport.go) carries no SDK dependency and stays
// unit-testable with a fake presigner. It runs on the CONTROLLER's IRSA
// credentials — the identity the terraform IAM grant (s3:PutObject/GetObject on
// the snapshot prefix) attaches to — because the controller is what presigns;
// the node only curls the resulting URL.

// s3PresignAdapter presigns PUT/GET URLs for a fixed bucket with a fixed TTL.
type s3PresignAdapter struct {
	pc     *s3.PresignClient
	bucket string
	ttl    time.Duration
}

// newS3PresignAdapter builds the adapter over an already-configured *s3.Client
// (endpoint/region/path-style resolved by the caller, mirroring the rest of the
// controller's S3 setup). A zero ttl falls back to defaultPresignTTL.
func newS3PresignAdapter(client *s3.Client, bucket string, ttl time.Duration) *s3PresignAdapter {
	if ttl <= 0 {
		ttl = defaultPresignTTL
	}
	return &s3PresignAdapter{
		pc:     s3.NewPresignClient(client, s3.WithPresignExpires(ttl)),
		bucket: bucket,
		ttl:    ttl,
	}
}

func (a *s3PresignAdapter) PresignPut(ctx context.Context, key string) (presignedRequest, error) {
	out, err := a.pc.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return presignedRequest{}, fmt.Errorf("presign PUT %s/%s: %w", a.bucket, key, err)
	}
	return presignedRequest{URL: out.URL, Headers: out.SignedHeader}, nil
}

func (a *s3PresignAdapter) PresignGet(ctx context.Context, key string) (presignedRequest, error) {
	out, err := a.pc.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return presignedRequest{}, fmt.Errorf("presign GET %s/%s: %w", a.bucket, key, err)
	}
	return presignedRequest{URL: out.URL, Headers: out.SignedHeader}, nil
}

var _ snapshotPresigner = (*s3PresignAdapter)(nil)
