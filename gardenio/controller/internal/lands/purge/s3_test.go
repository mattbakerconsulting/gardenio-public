package purge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is a versioned bucket: key → version ids, listed two per page.
type fakeS3 struct {
	versions map[string][]string
	markers  map[string]string
	deleted  []string
	failKey  string
}

func (f *fakeS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	type kv struct{ key, ver string }
	var all []kv
	for k, vs := range f.versions {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			for _, v := range vs {
				all = append(all, kv{k, v})
			}
		}
	}
	// Deterministic order, then page after the marker.
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[j].key+all[j].ver < all[i].key+all[i].ver {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	start := 0
	if in.KeyMarker != nil {
		for i, e := range all {
			if e.key == *in.KeyMarker && e.ver == aws.ToString(in.VersionIdMarker) {
				start = i + 1
			}
		}
	}
	out := &s3.ListObjectVersionsOutput{}
	end := min(start+2, len(all))
	for _, e := range all[start:end] {
		out.Versions = append(out.Versions, s3types.ObjectVersion{Key: aws.String(e.key), VersionId: aws.String(e.ver)})
	}
	if end < len(all) {
		out.IsTruncated = aws.Bool(true)
		out.NextKeyMarker, out.NextVersionIdMarker = aws.String(all[end-1].key), aws.String(all[end-1].ver)
	} else {
		for k, v := range f.markers {
			if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
				out.DeleteMarkers = append(out.DeleteMarkers, s3types.DeleteMarkerEntry{Key: aws.String(k), VersionId: aws.String(v)})
			}
		}
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	out := &s3.DeleteObjectsOutput{}
	for _, o := range in.Delete.Objects {
		if aws.ToString(o.Key) == f.failKey {
			out.Errors = append(out.Errors, s3types.Error{Key: o.Key, Code: aws.String("AccessDenied"), Message: aws.String("no")})
			continue
		}
		f.deleted = append(f.deleted, fmt.Sprintf("%s@%s", aws.ToString(o.Key), aws.ToString(o.VersionId)))
	}
	return out, nil
}

func TestS3Sessions_DeletesEveryVersionUnderThePrefix(t *testing.T) {
	f := &fakeS3{
		versions: map[string][]string{
			"coop/sessions/a-1/sp/meta.json":           {"v1", "v2"},
			"coop/sessions/a-1/sp/transcripts/0.jsonl": {"v1"},
			"coop/sessions/a-1/sp/recording.cast":      {"v1"},
			"coop/sessions/a-10/sp/meta.json":          {"v1"},
			"coop/sessions/b-2/sp/transcripts/0.jsonl": {"v1"},
		},
		markers: map[string]string{"coop/sessions/a-1/sp/old.log": "m1"},
	}
	s := &S3Sessions{Client: f, Bucket: "b"}
	n, err := s.DeletePrefix(context.Background(), "coop/sessions/a-1/")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || len(f.deleted) != 5 {
		t.Fatalf("deleted %d: %v", n, f.deleted)
	}
	for _, d := range f.deleted {
		if !strings.HasPrefix(d, "coop/sessions/a-1/") {
			t.Fatalf("deleted outside the prefix: %s", d)
		}
	}
}

func TestS3Sessions_CountPrefixDeletesNothing(t *testing.T) {
	f := &fakeS3{
		versions: map[string][]string{
			"coop/sessions/a-1/sp/meta.json":  {"v1", "v2"},
			"coop/sessions/a-1/sp/x.jsonl":    {"v1"},
			"coop/sessions/a-10/sp/meta.json": {"v1"},
		},
		markers: map[string]string{"coop/sessions/a-1/sp/old.log": "m1"},
	}
	s := &S3Sessions{Client: f, Bucket: "b"}
	n, err := s.CountPrefix(context.Background(), "coop/sessions/a-1/")
	if err != nil || n != 4 || len(f.deleted) != 0 {
		t.Fatalf("count = %d, %v; deleted %v", n, err, f.deleted)
	}
	if _, err := s.CountPrefix(context.Background(), "coop/"); err == nil {
		t.Fatal("want the broad prefix refused")
	}
}

func TestS3Sessions_RefusesBroadPrefixesAndReportsErrors(t *testing.T) {
	s := &S3Sessions{Client: &fakeS3{}, Bucket: "b"}
	for _, p := range []string{"", "coop/sessions", "coop/", "coop/sessions//", "a-1"} {
		if _, err := s.DeletePrefix(context.Background(), p); err == nil {
			t.Fatalf("prefix %q: want a refusal", p)
		}
	}
	f := &fakeS3{versions: map[string][]string{"coop/sessions/a-1/x": {"v1"}}, failKey: "coop/sessions/a-1/x"}
	s = &S3Sessions{Client: f, Bucket: "b"}
	if _, err := s.DeletePrefix(context.Background(), "coop/sessions/a-1/"); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want the per-key error back, got %v", err)
	}
}
