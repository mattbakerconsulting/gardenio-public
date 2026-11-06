package landmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"gardenio/lands/contract/landsv1"
)

// Decision evidence (contract 1.0.0-beta.5, task gd-wRXlwYH2GI). A decision
// shows the screenshots already on the ticket it is about, as decisionview
// does for the fleet (gardenio/controller/internal/decisionview/evidence.go):
// the ids in fields.evidence, then fields.requesting_task, and on each the
// image links in its comments, newest first, then its description. Those
// links are the viewer's /image/<key> proxy, which no tenant can load, so each
// picture is linked through the land's own door instead
// (getDecisionEvidence), which reads the object from the viewer's bucket.
//
// The door serves a picture only when the decision's evidence, worked out
// again as the caller, names it. A linked record is read as the caller and
// must be a ticket of the land, so another land's record, a fleet record or
// one the caller cannot read gives nothing. The key must also be under
// <prefix>/images/ (the viewer proxy's rule) and its upload scope, the bead
// gb image upload filed it under, must be a record of the land: a link a
// tenant writes on their own ticket cannot reach a picture filed under
// someone else's record. Every other host is dropped (gd-ZwnwonBxGS item 3,
// no third-party images).

const (
	// maxEvidenceSources is how many linked records one decision reads, the
	// requesting task's slot included (decisionview.maxEvidenceBeads).
	maxEvidenceSources = 3
	// maxEvidence is the contract's Decision.evidence maxItems.
	maxEvidence = 20
	// maxEvidenceText is DecisionEvidence's source_title and alt maxLength.
	maxEvidenceText = 500
	// maxPictureBytes caps one served picture; a bigger object is not served.
	maxPictureBytes = 10 << 20
	// pictureTimeout caps one bucket read, as the viewer proxy's does.
	pictureTimeout = 30 * time.Second
	// pictureCacheControl is the viewer proxy's: private, and short enough
	// that a picture taken off the record stops being served within minutes.
	pictureCacheControl = "private, max-age=300"
	// pictureConfigTimeout caps AWS credential resolution at startup.
	pictureConfigTimeout = 8 * time.Second
)

// pictureTypes are the only types a picture is served as. SVG is never one:
// it is a document that can carry script.
var pictureTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// pictureExts are the file extensions an image link may end in.
var pictureExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}

// pictureID is the opaque id the door names a picture by: the first 32 hex
// digits of the SHA-256 of its key. The key itself never reaches the tenant:
// its upload scope is often a fleet id, and the edge matches each route
// wildcard as one path segment.
var pictureID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func pictureIDOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// s3ImageGetter is the slice of *s3.Client the door reads pictures with, so
// tests use an in-memory fake (the viewer proxy's image_proxy.go has the
// same).
type s3ImageGetter interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Pictures is the bucket evidence pictures are read from: the one gb image
// upload writes and the viewer's /image/ proxy reads (COOP_S3_*).
type Pictures struct {
	s3     s3ImageGetter
	bucket string
	// fallback is read when bucket misses: the session bucket, once images
	// have one of their own (COOP_S3_IMAGES_BUCKET, gd-CmjVAU3vgd). Empty
	// means none.
	fallback string
	// allowed is "<prefix>/images/", the only keys ever served.
	allowed string
}

func newPictures(client s3ImageGetter, bucket, prefix string) *Pictures {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = "coop/sessions"
	}
	return &Pictures{s3: client, bucket: bucket, allowed: prefix + "/images/"}
}

// PicturesFromEnv reads the bucket from the viewer proxy's environment:
// COOP_S3_IMAGES_BUCKET, then COOP_S3_BUCKET as the fallback for older
// pictures (or alone when no images bucket is set), COOP_S3_PREFIX (default coop/sessions), COOP_S3_REGION and
// COOP_S3_ENDPOINT (an S3-compatible endpoint, path-style). It returns nil,
// with no error, when COOP_S3_BUCKET is unset, and nil with an error when no
// credentials resolve for it: either way the door serves no pictures and
// shows no evidence, rather than links that would all fail. Credentials are
// checked once, here: every way they arrive (env, Secret, IRSA) rolls the pod.
func PicturesFromEnv(ctx context.Context) (*Pictures, error) {
	bucket, fallback := strings.TrimSpace(os.Getenv("COOP_S3_IMAGES_BUCKET")), strings.TrimSpace(os.Getenv("COOP_S3_BUCKET"))
	if bucket == "" || bucket == fallback {
		bucket, fallback = fallback, ""
	}
	if bucket == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, pictureConfigTimeout)
	defer cancel()
	var opts []func(*awsconfig.LoadOptions) error
	if region := strings.TrimSpace(os.Getenv("COOP_S3_REGION")); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Credentials == nil {
		return nil, errors.New("no AWS credentials provider")
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("no AWS credentials: %w", err)
	}
	var s3opts []func(*s3.Options)
	if endpoint := strings.TrimSpace(os.Getenv("COOP_S3_ENDPOINT")); endpoint != "" {
		s3opts = append(s3opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	p := newPictures(s3.NewFromConfig(cfg, s3opts...), bucket, os.Getenv("COOP_S3_PREFIX"))
	p.fallback = fallback
	return p, nil
}

// SetPictures gives the map its picture bucket; nil (the default) serves none.
func (m *Map) SetPictures(p *Pictures) { m.pictures = p }

// evidencePicture is one picture of a decision's evidence.
type evidencePicture struct {
	key    string
	alt    string
	source *beadRecord
}

// evidenceSources are the records a decision borrows pictures from, in
// order: the ids in fields.evidence (comma or space separated, as gb decision
// create --evidence stamps them, or a JSON list), then
// fields.requesting_task, without duplicates or the decision itself. The
// requesting task always keeps its slot (decisionview.EvidenceBeads).
func evidenceSources(b *beadRecord) []string {
	task := strings.TrimSpace(b.fieldString("requesting_task"))
	if task == b.ID {
		task = ""
	}
	limit := maxEvidenceSources
	if task != "" {
		limit--
	}
	var named []string
	if raw := b.field("evidence"); len(raw) > 0 && json.Unmarshal(raw, &named) != nil {
		named = strings.FieldsFunc(b.fieldString("evidence"), func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t'
		})
	}
	var out []string
	seen := map[string]bool{b.ID: true, task: true}
	for _, id := range named {
		id = strings.TrimSpace(id)
		if len(out) == limit {
			break
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if task != "" {
		out = append(out, task)
	}
	return out
}

var (
	mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?(https?://[^\s)>]+)>?(?:\s+"[^"]*")?\s*\)`)
	bareURLRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)
)

// imageLink is one image link found in text.
type imageLink struct{ url, alt string }

// imagesIn finds the image links in Markdown or plain text, in the order they
// appear, without duplicates: ![alt](url) images and bare image URLs
// (decisionview.ImagesIn). alt is the Markdown alt text, else the file name.
func imagesIn(text string) []imageLink {
	type hit struct {
		at       int
		url, alt string
	}
	var hits []hit
	for _, m := range mdImageRe.FindAllStringSubmatchIndex(text, -1) {
		hits = append(hits, hit{m[0], text[m[4]:m[5]], text[m[2]:m[3]]})
	}
	for _, m := range bareURLRe.FindAllStringIndex(text, -1) {
		hits = append(hits, hit{m[0], text[m[0]:m[1]], ""})
	}
	// A Markdown image's URL is also a bare-URL match; stable order by
	// position with the Markdown hit first keeps its alt text.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	var out []imageLink
	seen := map[string]bool{}
	for _, h := range hits {
		raw := strings.TrimRight(h.url, ".,;:!?")
		u, err := url.Parse(raw)
		if err != nil || seen[raw] || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			!slices.Contains(pictureExts, strings.ToLower(path.Ext(u.Path))) {
			continue
		}
		seen[raw] = true
		alt := strings.TrimSpace(h.alt)
		if alt == "" {
			alt = path.Base(u.Path)
		}
		out = append(out, imageLink{url: raw, alt: alt})
	}
	return out
}

// pictureKey is the bucket key a viewer /image/<key> link names, and the
// upload scope in it, or "" when the link is not one or its key is outside
// allowed (<prefix>/images/<scope>/<file>, as gb image upload writes it). The
// last /image/ wins, as in gb image fetch.
func pictureKey(raw, allowed string) (key, scope string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	p := u.EscapedPath()
	i := strings.LastIndex(p, "/image/")
	if i < 0 {
		return "", ""
	}
	key, err = url.PathUnescape(p[i+len("/image/"):])
	if err != nil || strings.Contains(key, "..") || strings.Contains(key, "//") {
		return "", ""
	}
	rest, ok := strings.CutPrefix(key, allowed)
	if !ok {
		return "", ""
	}
	scope, file, ok := strings.Cut(rest, "/")
	if !ok || scope == "" || file == "" || strings.Contains(file, "/") ||
		!slices.Contains(pictureExts, strings.ToLower(path.Ext(file))) {
		return "", ""
	}
	return key, scope
}

// readable reads one record as the caller; ok is false when the caller may
// not read it or it does not exist. Any other failure is logged and read as
// unreadable: a decision shows without its evidence rather than fail.
func (m *Map) readable(r *http.Request, id string) (*beadRecord, bool) {
	var b beadRecord
	if err := m.d.call(r, rGetBead, []string{id}, nil, nil, &b); err != nil {
		var ue *upstreamError
		if !errors.As(err, &ue) || (ue.status != http.StatusForbidden && ue.status != http.StatusNotFound) {
			slog.Info("landmap: evidence record unread", "error", err)
		}
		return nil, false
	}
	return &b, true
}

// evidence works out a decision's pictures as the caller: newest first
// within each linked ticket, tickets in order, without duplicates, at most
// maxEvidence. It is nil when the map has no picture bucket.
func (m *Map) evidence(r *http.Request, land string, decision *beadRecord) []evidencePicture {
	if m.pictures == nil {
		return nil
	}
	var out []evidencePicture
	seen := map[string]bool{}
	scopes := map[string]bool{}
	scopeInLand := func(scope string) bool {
		in, ok := scopes[scope]
		if !ok {
			b, read := m.readable(r, scope)
			in = read && b.inLand(land)
			scopes[scope] = in
		}
		return in
	}
	for _, id := range evidenceSources(decision) {
		src, ok := m.readable(r, id)
		if !ok || !src.inLand(land) || !isTicketType(src.Type) {
			continue
		}
		scopes[src.ID] = true
		var cs struct {
			Comments []commentRecord `json:"comments"`
		}
		if err := m.d.call(r, rListComments, []string{src.ID}, nil, nil, &cs); err != nil {
			slog.Info("landmap: evidence comments unread", "error", err)
		}
		sort.SliceStable(cs.Comments, func(i, j int) bool { return cs.Comments[i].CreatedAt.After(cs.Comments[j].CreatedAt) })
		texts := make([]string, 0, len(cs.Comments)+1)
		for _, c := range cs.Comments {
			texts = append(texts, c.Text)
		}
		texts = append(texts, src.Description)
		for _, text := range texts {
			for _, l := range imagesIn(text) {
				key, scope := pictureKey(l.url, m.pictures.allowed)
				if key == "" || seen[key] || !scopeInLand(scope) {
					continue
				}
				seen[key] = true
				out = append(out, evidencePicture{key: key, alt: l.alt, source: src})
				if len(out) == maxEvidence {
					return out
				}
			}
		}
	}
	return out
}

// withEvidence is d with its evidence filled in, each picture linked through
// the land's door; d is unchanged when there is none.
func (m *Map) withEvidence(r *http.Request, land string, b *beadRecord, d landsv1.Decision) landsv1.Decision {
	pics := m.evidence(r, land, b)
	if len(pics) == 0 {
		return d
	}
	base := doorURL(r, land) + BaseURL + "/lands/v1/lands/" + land + "/decisions/" + url.PathEscape(b.ID) + "/evidence/"
	ev := make([]landsv1.DecisionEvidence, 0, len(pics))
	for _, p := range pics {
		e := landsv1.DecisionEvidence{SourceId: p.source.ID, Url: base + pictureIDOf(p.key)}
		if t := clip(scrubProse(p.source.ID, p.source.Title)); t != "" {
			e.SourceTitle = &t
		}
		if a := clip(scrubProse(p.source.ID, p.alt)); a != "" {
			e.Alt = &a
		}
		ev = append(ev, e)
	}
	d.Evidence = &ev
	return d
}

// clip cuts s to maxEvidenceText characters.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxEvidenceText {
		return s
	}
	return string([]rune(s)[:maxEvidenceText])
}

func (m *Map) GetDecisionEvidence(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, decision landsv1.DecisionPath, image landsv1.EvidenceImagePath) {
	if !m.admit(w, r, land) {
		return
	}
	if m.pictures == nil {
		problem(w, landsv1.UpstreamUnavailable, "")
		return
	}
	if !pictureID.MatchString(image) {
		problem(w, landsv1.NotFound, "")
		return
	}
	b, ok := m.getBead(w, r, "getDecisionEvidence", land, decision, decisionTypes...)
	if !ok {
		return
	}
	for _, p := range m.evidence(r, land, b) {
		if pictureIDOf(p.key) == image {
			m.pictures.serve(w, r, p.key)
			return
		}
	}
	problem(w, landsv1.NotFound, "")
}

// isPictureMissing reports whether err is the bucket's "no such object".
func isPictureMissing(err error) bool {
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	var apiErr smithy.APIError
	return errors.As(err, &noKey) || errors.As(err, &notFound) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey")
}

// serve writes the picture at key. A picture that is missing, not one of
// pictureTypes or over maxPictureBytes is not_found; a bucket that cannot be
// read is upstream_unavailable. The bucket's own words go only to the log.
func (p *Pictures) serve(w http.ResponseWriter, r *http.Request, key string) {
	if !strings.HasPrefix(key, p.allowed) {
		problem(w, landsv1.NotFound, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pictureTimeout)
	defer cancel()
	out, err := p.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(key)})
	if err != nil && p.fallback != "" && isPictureMissing(err) {
		out, err = p.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(p.fallback), Key: aws.String(key)})
	}
	if err != nil {
		if isPictureMissing(err) {
			problem(w, landsv1.NotFound, "")
			return
		}
		fail(w, r, "getDecisionEvidence", err)
		return
	}
	defer out.Body.Close()
	ct, _, _ := mime.ParseMediaType(aws.ToString(out.ContentType))
	if !slices.Contains(pictureTypes, ct) || aws.ToInt64(out.ContentLength) > maxPictureBytes {
		slog.Info("landmap: evidence picture refused", "content_type", aws.ToString(out.ContentType), "bytes", aws.ToInt64(out.ContentLength))
		problem(w, landsv1.NotFound, "")
		return
	}
	var body bytes.Buffer
	if _, err := io.Copy(&body, io.LimitReader(out.Body, maxPictureBytes+1)); err != nil {
		fail(w, r, "getDecisionEvidence", err)
		return
	}
	if body.Len() > maxPictureBytes {
		problem(w, landsv1.NotFound, "")
		return
	}
	h := w.Header()
	for k := range h {
		if !slices.Contains(ResponseHeaders, k) {
			h.Del(k)
		}
	}
	h.Set("Content-Type", ct)
	h.Set(landsv1.HeaderContract, landsv1.ContractVersion)
	h.Set("Cache-Control", pictureCacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	// The bytes are a picture, not prose: only the headers are scanned.
	scanLive(tallyFromWriter(w), h, nil, "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}
