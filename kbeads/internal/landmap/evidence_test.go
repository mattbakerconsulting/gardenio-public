package landmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gardenio/lands/contract/landsv1"
)

// fakeBucket is an in-memory picture bucket.
type fakeBucket struct {
	objects map[string]fakeObject
	// err, when set, is what every read answers.
	err  error
	gets []string
}

type fakeObject struct {
	contentType string
	body        []byte
	// length overrides the reported ContentLength when set.
	length int64
}

func (b *fakeBucket) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if aws.ToString(in.Bucket) != "sessions" {
		return nil, errors.New("wrong bucket " + aws.ToString(in.Bucket))
	}
	b.gets = append(b.gets, aws.ToString(in.Key))
	if b.err != nil {
		return nil, b.err
	}
	o, ok := b.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	n := int64(len(o.body))
	if o.length != 0 {
		n = o.length
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(o.body)),
		ContentType:   aws.String(o.contentType),
		ContentLength: aws.Int64(n),
	}, nil
}

const (
	viewer   = "https://viewer.example/beads/image/"
	keyFront = "coop/sessions/images/ac-tE00000001/1a2b3c4d-front.png"
	keyBack  = "coop/sessions/images/ac-tE00000001/5e6f7a8b-back.png"
	keyDesc  = "coop/sessions/images/ac-tE00000001/9c0d1e2f-plan.jpg"
	keyFleet = "coop/sessions/images/gd-Fleet00001/0a0b0c0d-fleet.png"
	keyZeta  = "coop/sessions/images/zz-t1/0e0f0a0b-zeta.png"
	keyDate  = "coop/sessions/images/2026-10-31/0c0d0e0f-adhoc.png"
)

// newEvidenceFixture is acme with a decision about one of its tickets, whose
// comments carry the viewer's picture links. bucket nil gives the map no
// picture bucket.
func newEvidenceFixture(t *testing.T, bucket *fakeBucket) (*fakeDaemon, http.Handler) {
	t.Helper()
	d := newFakeDaemon()
	acme := []any{"project:acme"}
	d.add(map[string]any{"id": "ac-tE00000001", "type": "task", "title": "Shed photos, after gd-Abc1234567", "labels": acme,
		"description": "The plan: ![plan](" + viewer + keyDesc + ")"})
	d.comments["ac-tE00000001"] = []map[string]any{
		{"id": 1, "author": "oidc:al@example.com", "created_at": t0,
			"text": "Before: ![front](" + viewer + keyFront + ") and " + viewer + keyBack + ". Also https://cdn.example/banner.png"},
	}
	// A fleet record, readable by the fake daemon, with pictures of its own.
	d.add(map[string]any{"id": "gd-Fleet00001", "type": "task", "title": "Fleet work", "labels": []any{"project:gardenio"}})
	d.comments["gd-Fleet00001"] = []map[string]any{{"id": 2, "author": "x", "created_at": t0, "text": viewer + keyFleet}}
	d.add(map[string]any{"id": "zz-t1", "type": "task", "title": "Other land's task", "labels": []any{"project:zeta"}})
	d.add(map[string]any{"id": "ac-dE00000001", "type": "decision", "title": "Which side?", "labels": acme, "kind": "data",
		"fields": map[string]any{"prompt": "Which side?", "evidence": "gd-Fleet00001", "requesting_task": "ac-tE00000001",
			"options": []any{map[string]any{"id": "front", "label": "Front"}}}})
	d.add(map[string]any{"id": "zz-d1", "type": "decision", "title": "Zeta's", "labels": []any{"project:zeta"}, "kind": "data",
		"fields": map[string]any{"prompt": "Zeta's?", "requesting_task": "ac-tE00000001",
			"options": []any{map[string]any{"id": "a", "label": "A"}}}})
	m := New(d.mux, fakeLands{}, nil)
	if bucket != nil {
		m.SetPictures(newPictures(bucket, "sessions", "coop/sessions/"))
	}
	mux := http.NewServeMux()
	Register(mux, m)
	return d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", "v-test")
		mux.ServeHTTP(w, r)
	})
}

func evidenceURL(decision, key string) string {
	return "https://example.com/land/acme/v1/lands/v1/lands/acme/decisions/" + decision + "/evidence/" + pictureIDOf(key)
}

func noLeaks(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{"viewer.example", "coop/sessions", "/image/", "cdn.example", "gd-Fleet00001", "gd-Abc1234567"} {
		if strings.Contains(body, leak) {
			t.Errorf("response carries %q:\n%s", leak, body)
		}
	}
}

// The ticket's comment carries two viewer links and a third-party picture:
// two items, linked through the door, then the description's. The fleet
// record named in fields.evidence gives nothing, and the source title's
// foreign id reads [ref].
func TestEvidence_ProjectsTheLandsTicketPicturesThroughTheDoor(t *testing.T) {
	_, h := newEvidenceFixture(t, &fakeBucket{})
	r := do(t, h, "GET", "/acme/decisions/ac-dE00000001", "")
	dec := decodeAs[landsv1.Decision](t, r, http.StatusOK)
	if dec.Evidence == nil {
		t.Fatalf("evidence absent:\n%s", r.body)
	}
	title := "Shed photos, after [ref]"
	want := []landsv1.DecisionEvidence{
		{SourceId: "ac-tE00000001", SourceTitle: &title, Url: evidenceURL("ac-dE00000001", keyFront), Alt: ptr("front")},
		{SourceId: "ac-tE00000001", SourceTitle: &title, Url: evidenceURL("ac-dE00000001", keyBack), Alt: ptr("5e6f7a8b-back.png")},
		{SourceId: "ac-tE00000001", SourceTitle: &title, Url: evidenceURL("ac-dE00000001", keyDesc), Alt: ptr("plan")},
	}
	got := *dec.Evidence
	if len(got) != len(want) {
		t.Fatalf("evidence = %d items, want %d:\n%s", len(got), len(want), r.body)
	}
	for i := range want {
		if got[i].SourceId != want[i].SourceId || *got[i].SourceTitle != *want[i].SourceTitle || got[i].Url != want[i].Url || *got[i].Alt != *want[i].Alt {
			t.Errorf("evidence[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	noLeaks(t, r.body)
}

// A decision about a fleet record only, or about another land's, shows no
// evidence; nor does a link whose upload scope is outside the land.
func TestEvidence_NoItemFromARecordOrScopeOutsideTheLand(t *testing.T) {
	d, h := newEvidenceFixture(t, &fakeBucket{})
	acme := []any{"project:acme"}
	d.add(map[string]any{"id": "ac-tS00000001", "type": "task", "title": "Borrowed pictures", "labels": acme})
	d.comments["ac-tS00000001"] = []map[string]any{{"id": 3, "author": "x", "created_at": t0,
		"text": viewer + keyFleet + " " + viewer + keyZeta + " " + viewer + keyDate}}
	for id, fields := range map[string]map[string]any{
		"ac-dF00000001": {"evidence": "gd-Fleet00001"},
		"ac-dZ00000001": {"evidence": []any{"zz-t1"}, "requesting_task": "gd-Fleet00001"},
		"ac-dS00000001": {"requesting_task": "ac-tS00000001"},
		"ac-dC00000001": {"requesting_task": "ac-c1"},
	} {
		fields["prompt"] = "Which?"
		fields["options"] = []any{map[string]any{"id": "a", "label": "A"}}
		d.add(map[string]any{"id": id, "type": "decision", "title": "Which?", "labels": acme, "kind": "data", "fields": fields})
	}
	d.add(map[string]any{"id": "ac-c1", "type": "chore", "title": "hidden chore", "labels": acme, "description": viewer + keyFront})
	for _, id := range []string{"ac-dF00000001", "ac-dZ00000001", "ac-dS00000001", "ac-dC00000001"} {
		r := do(t, h, "GET", "/acme/decisions/"+id, "")
		if dec := decodeAs[landsv1.Decision](t, r, http.StatusOK); dec.Evidence != nil {
			t.Errorf("%s: evidence = %+v, want absent", id, *dec.Evidence)
		}
		noLeaks(t, r.body)
	}
}

// A third-party picture is never shown, on its own either.
func TestEvidence_NoItemFromAThirdPartyHost(t *testing.T) {
	d, h := newEvidenceFixture(t, &fakeBucket{})
	d.comments["ac-tE00000001"] = []map[string]any{{"id": 1, "author": "x", "created_at": t0,
		"text": "![banner](https://cdn.example/banner.png) https://images.example/shot.webp"}}
	d.beads["ac-tE00000001"]["description"] = ""
	r := do(t, h, "GET", "/acme/decisions/ac-dE00000001", "")
	if dec := decodeAs[landsv1.Decision](t, r, http.StatusOK); dec.Evidence != nil {
		t.Errorf("evidence = %+v, want absent", *dec.Evidence)
	}
	noLeaks(t, r.body)
}

// Newest comment first, the description last, each picture once, at most 20.
func TestEvidence_NewestFirstDedupedCapped(t *testing.T) {
	d, h := newEvidenceFixture(t, &fakeBucket{})
	var many []string
	for i := range 25 {
		many = append(many, viewer+"coop/sessions/images/ac-tE00000001/"+strconv.Itoa(i)+"-many.png")
	}
	d.comments["ac-tE00000001"] = []map[string]any{
		{"id": 1, "author": "x", "created_at": t0, "text": strings.Join(many, " ")},
		{"id": 2, "author": "x", "created_at": t0.Add(time.Hour), "text": viewer + keyBack + " " + viewer + keyBack + " " + viewer + keyDesc},
	}
	dec := decodeAs[landsv1.Decision](t, do(t, h, "GET", "/acme/decisions/ac-dE00000001", ""), http.StatusOK)
	if dec.Evidence == nil || len(*dec.Evidence) != maxEvidence {
		t.Fatalf("evidence = %v, want %d items", dec.Evidence, maxEvidence)
	}
	ev := *dec.Evidence
	if ev[0].Url != evidenceURL("ac-dE00000001", keyBack) || ev[1].Url != evidenceURL("ac-dE00000001", keyDesc) {
		t.Errorf("first two = %s, %s; want the newest comment's back then plan", ev[0].Url, ev[1].Url)
	}
	if ev[2].Url != evidenceURL("ac-dE00000001", "coop/sessions/images/ac-tE00000001/0-many.png") {
		t.Errorf("third = %s, want the older comment's first", ev[2].Url)
	}
}

// With no picture bucket the decision shows no evidence and the route is
// upstream_unavailable.
func TestEvidence_NoBucket(t *testing.T) {
	_, h := newEvidenceFixture(t, nil)
	r := do(t, h, "GET", "/acme/decisions/ac-dE00000001", "")
	if dec := decodeAs[landsv1.Decision](t, r, http.StatusOK); dec.Evidence != nil {
		t.Errorf("evidence = %+v, want absent", *dec.Evidence)
	}
	noLeaks(t, r.body)
	problemOf(t, do(t, h, "GET", "/acme/decisions/ac-dE00000001/evidence/"+pictureIDOf(keyFront), ""), landsv1.UpstreamUnavailable)
	// A land the caller is not in is still not_found.
	problemOf(t, do(t, h, "GET", "/zeta/decisions/zz-d1/evidence/"+pictureIDOf(keyFront), ""), landsv1.NotFound)
}

// pictureEnv points the AWS SDK at a bucket with no credential source at
// all: empty shared files, no env keys, no web identity, container or
// instance metadata.
func pictureEnv(t *testing.T) {
	t.Helper()
	empty := t.TempDir() + "/empty"
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"COOP_S3_BUCKET":                         "pictures",
		"COOP_S3_IMAGES_BUCKET":                  "",
		"COOP_S3_REGION":                         "us-east-1",
		"COOP_S3_ENDPOINT":                       "",
		"AWS_CONFIG_FILE":                        empty,
		"AWS_SHARED_CREDENTIALS_FILE":            empty,
		"AWS_EC2_METADATA_DISABLED":              "true",
		"AWS_PROFILE":                            "",
		"AWS_ACCESS_KEY_ID":                      "",
		"AWS_SECRET_ACCESS_KEY":                  "",
		"AWS_SESSION_TOKEN":                      "",
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "",
		"AWS_ROLE_ARN":                           "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "",
	} {
		t.Setenv(k, v)
	}
}

// A bucket named with no credentials to read it gives no pictures, so no
// decision lists evidence links that would all fail (gd-ash4SzfR8U).
func TestPicturesFromEnv_NoCredentials(t *testing.T) {
	pictureEnv(t)
	p, err := PicturesFromEnv(context.Background())
	if err == nil || p != nil {
		t.Fatalf("PicturesFromEnv = %v, %v; want nil and an error", p, err)
	}
}

func TestPicturesFromEnv_StaticCredentials(t *testing.T) {
	pictureEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	p, err := PicturesFromEnv(context.Background())
	if err != nil || p == nil {
		t.Fatalf("PicturesFromEnv = %v, %v; want pictures", p, err)
	}
	if p.bucket != "pictures" || p.allowed != "coop/sessions/images/" {
		t.Errorf("pictures = %q %q", p.bucket, p.allowed)
	}
}

func TestEvidenceRoute_ServesOnlyTheDecisionsPictures(t *testing.T) {
	png := []byte("\x89PNG fake picture")
	bucket := &fakeBucket{objects: map[string]fakeObject{
		keyFront: {contentType: "image/png", body: png},
		keyBack:  {contentType: "image/svg+xml", body: []byte("<svg/>")},
		keyDesc:  {contentType: "image/jpeg", body: []byte("jpeg"), length: maxPictureBytes + 1},
		keyFleet: {contentType: "image/png", body: png},
	}}
	_, h := newEvidenceFixture(t, bucket)
	r := do(t, h, "GET", "/acme/decisions/ac-dE00000001/evidence/"+pictureIDOf(keyFront), "")
	if r.status != http.StatusOK || r.body != string(png) {
		t.Fatalf("picture = %d %q", r.status, r.body)
	}
	for k, want := range map[string]string{
		"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Cache-Control": "private, max-age=300",
		landsv1.HeaderContract: landsv1.ContractVersion,
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if r.header.Get("X-Kbeads-Version") != "" {
		t.Error("the daemon's version header went out")
	}
	if !slices.Equal(bucket.gets, []string{keyFront}) {
		t.Errorf("bucket reads = %v", bucket.gets)
	}

	for name, path := range map[string]string{
		"an svg":                          "/acme/decisions/ac-dE00000001/evidence/" + pictureIDOf(keyBack),
		"an oversized picture":            "/acme/decisions/ac-dE00000001/evidence/" + pictureIDOf(keyDesc),
		"a fleet record's picture":        "/acme/decisions/ac-dE00000001/evidence/" + pictureIDOf(keyFleet),
		"a picture not on the decision":   "/acme/decisions/ac-dE00000001/evidence/" + pictureIDOf("coop/sessions/images/ac-tE00000001/x.png"),
		"a key, not an id":                "/acme/decisions/ac-dE00000001/evidence/front.png",
		"another land's decision":         "/acme/decisions/zz-d1/evidence/" + pictureIDOf(keyFront),
		"another land":                    "/zeta/decisions/zz-d1/evidence/" + pictureIDOf(keyFront),
		"a decision that is not there":    "/acme/decisions/ac-nope/evidence/" + pictureIDOf(keyFront),
		"a ticket in the decision's spot": "/acme/decisions/ac-tE00000001/evidence/" + pictureIDOf(keyFront),
	} {
		t.Run(name, func(t *testing.T) {
			problemOf(t, do(t, h, "GET", path, ""), landsv1.NotFound)
		})
	}
	if slices.Contains(bucket.gets, keyFleet) {
		t.Error("the fleet record's picture was read from the bucket")
	}

	delete(bucket.objects, keyFront)
	problemOf(t, do(t, h, "GET", "/acme/decisions/ac-dE00000001/evidence/"+pictureIDOf(keyFront), ""), landsv1.NotFound)
	bucket.err = errors.New("s3: connection reset by bucket.internal")
	r = do(t, h, "GET", "/acme/decisions/ac-dE00000001/evidence/"+pictureIDOf(keyFront), "")
	problemOf(t, r, landsv1.UpstreamUnavailable)
	if strings.Contains(r.body, "bucket.internal") {
		t.Errorf("the bucket's words went out: %s", r.body)
	}
}

func TestEvidenceSources(t *testing.T) {
	dec := func(fields map[string]any) *beadRecord {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return &beadRecord{ID: "ac-d1", Type: modelType("decision"), Fields: raw}
	}
	for name, tc := range map[string]struct {
		fields map[string]any
		want   []string
	}{
		"task alone":           {map[string]any{"requesting_task": "ac-t1"}, []string{"ac-t1"}},
		"named then task":      {map[string]any{"evidence": "ac-t2, ac-t3", "requesting_task": "ac-t1"}, []string{"ac-t2", "ac-t3", "ac-t1"}},
		"task keeps its slot":  {map[string]any{"evidence": "ac-t2 ac-t3 ac-t4", "requesting_task": "ac-t1"}, []string{"ac-t2", "ac-t3", "ac-t1"}},
		"a JSON list":          {map[string]any{"evidence": []any{"ac-t2", "ac-t2", "ac-t1"}, "requesting_task": "ac-t1"}, []string{"ac-t2", "ac-t1"}},
		"never the decision":   {map[string]any{"evidence": "ac-d1,ac-t2", "requesting_task": "ac-d1"}, []string{"ac-t2"}},
		"three without a task": {map[string]any{"evidence": "ac-t2,ac-t3,ac-t4,ac-t5"}, []string{"ac-t2", "ac-t3", "ac-t4"}},
	} {
		if got := evidenceSources(dec(tc.fields)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestPictureKey(t *testing.T) {
	const allowed = "coop/sessions/images/"
	for raw, want := range map[string]string{
		viewer + keyFront: keyFront,
		"https://viewer.example/image/coop/sessions/images/ac-t1/a%20b.PNG":      "coop/sessions/images/ac-t1/a b.PNG",
		"https://viewer.example/image/coop/sessions/images/ac-t1/a.png?x=1#frag": "coop/sessions/images/ac-t1/a.png",
		"https://viewer.example/image/coop/sessions/transcripts/ac-t1/a.png":     "",
		"https://viewer.example/image/coop/sessions/images/ac-t1/../x/a.png":     "",
		"https://viewer.example/image/coop/sessions/images/ac-t1/deeper/a.png":   "",
		"https://viewer.example/image/coop/sessions/images/ac-t1/a.svg":          "",
		"https://viewer.example/image/coop/sessions/images/a.png":                "",
		"https://viewer.example/image/coop/sessions/images/ac-t1%2Fdeeper/a.png": "",
		"https://viewer.example/beads/shot.png":                                  "",
		"https://viewer.example/image/image/coop/sessions/images/ac-t1/a.png":    "coop/sessions/images/ac-t1/a.png",
		"https://viewer.example/image/coop/sessions/images//a.png":               "",
		"https://viewer.example/image/coop/sessions/images/ac-t1/a.png/../b.png": "",
	} {
		if got, _ := pictureKey(raw, allowed); got != want {
			t.Errorf("pictureKey(%s) = %q, want %q", raw, got, want)
		}
	}
}

// gd-CmjVAU3vgd: with an images bucket, a picture is read there first and from
// the session bucket on a miss, so evidence uploaded before the switch shows.
type splitBucket struct {
	images   map[string]fakeObject
	sessions *fakeBucket
	reads    []string
}

func (b *splitBucket) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b.reads = append(b.reads, aws.ToString(in.Bucket))
	if aws.ToString(in.Bucket) != "images" {
		return b.sessions.GetObject(ctx, in, opts...)
	}
	o, ok := b.images[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{ContentType: aws.String(o.contentType), ContentLength: aws.Int64(int64(len(o.body))), Body: io.NopCloser(bytes.NewReader(o.body))}, nil
}

func TestPictures_FallBackToSessionBucket(t *testing.T) {
	b := &splitBucket{
		images:   map[string]fakeObject{keyBack: {contentType: "image/png", body: []byte("new")}},
		sessions: &fakeBucket{objects: map[string]fakeObject{keyFront: {contentType: "image/png", body: []byte("old")}}},
	}
	p := newPictures(b, "images", "coop/sessions")
	p.fallback = "sessions"
	for key, want := range map[string]string{keyFront: "old", keyBack: "new"} {
		b.reads = nil
		rr := httptest.NewRecorder()
		p.serve(rr, httptest.NewRequest(http.MethodGet, "/x", nil), key)
		if rr.Code != http.StatusOK || rr.Body.String() != want {
			t.Errorf("%s: got %d %q, want 200 %q", key, rr.Code, rr.Body.String(), want)
		}
		if want == "new" && len(b.reads) != 1 {
			t.Errorf("an images-bucket hit must not read the fallback: %v", b.reads)
		}
	}
	rr := httptest.NewRecorder()
	p.serve(rr, httptest.NewRequest(http.MethodGet, "/x", nil), keyDesc)
	if rr.Code != http.StatusNotFound {
		t.Errorf("missing in both: got %d, want 404", rr.Code)
	}
}

func TestPicturesFromEnv_ImagesBucket(t *testing.T) {
	pictureEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "images")
	p, err := PicturesFromEnv(context.Background())
	if err != nil || p == nil {
		t.Fatalf("PicturesFromEnv = %v, %v; want pictures", p, err)
	}
	if p.bucket != "images" || p.fallback != "pictures" {
		t.Errorf("bucket, fallback = %q, %q; want images, pictures", p.bucket, p.fallback)
	}
	t.Setenv("COOP_S3_IMAGES_BUCKET", "pictures")
	if p, _ := PicturesFromEnv(context.Background()); p == nil || p.bucket != "pictures" || p.fallback != "" {
		t.Errorf("same bucket twice must read once: %+v", p)
	}
}
