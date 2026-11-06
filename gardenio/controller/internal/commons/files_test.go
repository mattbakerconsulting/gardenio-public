package commons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/commons/commonstest"
)

// memFiles is an in-memory FileStore.
type memFiles struct {
	mu      sync.Mutex
	objs    map[string]memFile
	deleted []string
	failPut error
}

type memFile struct {
	ct   string
	body []byte
}

func (m *memFiles) PutFile(_ context.Context, key, ct string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut != nil {
		return m.failPut
	}
	if m.objs == nil {
		m.objs = map[string]memFile{}
	}
	m.objs[key] = memFile{ct, bytes.Clone(body)}
	return nil
}

func (m *memFiles) GetFile(_ context.Context, key string) (StoredFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.objs[key]
	if !ok {
		return StoredFile{}, ErrNoFile
	}
	return StoredFile{Body: io.NopCloser(bytes.NewReader(f.body)), ContentType: f.ct, ETag: `"e1"`, Size: int64(len(f.body))}, nil
}

func (m *memFiles) DeleteFile(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *memFiles) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// pngBytes is the start of a PNG, enough for http.DetectContentType.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")

type part struct {
	name, file, ct string
	body           []byte
}

// postParts sends a multipart say or reply as the test principal.
func postParts(t *testing.T, h http.Handler, path, as string, parts ...part) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		hd := textproto.MIMEHeader{}
		cd := `form-data; name="` + p.name + `"`
		if p.file != "" || p.name == "file" {
			cd += `; filename="` + p.file + `"`
		}
		hd.Set("Content-Disposition", cd)
		if p.ct != "" {
			hd.Set("Content-Type", p.ct)
		}
		w, err := mw.CreatePart(hd)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(p.body)
	}
	_ = mw.Close()
	r := httptest.NewRequest("POST", path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if as != "" {
		r.Header.Set("X-Test-As", as)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func withFiles(m *memFiles) func(*Handler) { return func(h *Handler) { h.Files = m } }

func filesOf(t *testing.T, f map[string]string) []fileRef {
	t.Helper()
	var refs []fileRef
	if err := json.Unmarshal([]byte(f["files"]), &refs); err != nil {
		t.Fatalf("files field %q: %v", f["files"], err)
	}
	return refs
}

func TestSayWithFilesStoresThemAndRecordsLinks(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))

	code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer",
		part{name: "text", body: []byte(" the plot ")},
		part{name: "file", file: "plot.png", ct: "image/png", body: pngBytes},
		part{name: "file", file: `C:\docs\spec.html`, ct: "text/html", body: []byte("<script>alert(1)</script>")})
	if code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	req, f := lastCreated(t, d)
	if req.Title != "the plot" || f["text"] != "the plot" || f["origin"] != "commons" {
		t.Errorf("bead = %+v fields %v", req, f)
	}
	refs := filesOf(t, f)
	if len(refs) != 2 {
		t.Fatalf("files = %+v", refs)
	}
	keys := m.keys()
	if len(keys) != 2 {
		t.Fatalf("stored %v", keys)
	}
	// The png is what it says; the html is stored as a download and shows as a file.
	if refs[0].Name != "plot.png" || refs[0].MimeType != "image/png" || refs[0].Size != len(pngBytes) {
		t.Errorf("png ref = %+v", refs[0])
	}
	if refs[1].Name != "spec.html" || refs[1].MimeType != "text/html" {
		t.Errorf("html ref = %+v", refs[1])
	}
	for _, r := range refs {
		u, err := url.Parse(r.URL)
		if err != nil || u.Scheme != "http" || u.Host != "example.com" || !strings.HasPrefix(u.Path, "/base/garden/c/f/claimlayer/cl-chgen/") {
			t.Errorf("url = %q", r.URL)
		}
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "claimlayer/cl-chgen/") {
			t.Errorf("key %q is not under its land and channel", k)
		}
		if obj := m.objs[k]; strings.HasSuffix(k, "/spec.html") && obj.ct != "application/octet-stream" {
			t.Errorf("html stored as %q", obj.ct)
		}
	}
	// The answer shows the message with its files.
	mustContain(t, body, `<p class="cm-text">the plot</p>`, `<ul class="cm-files" aria-label="Attachments">`,
		`<figure class="cm-img"><a href="/base/garden/c/f/claimlayer/cl-chgen/`, `alt="plot.png" loading="lazy" decoding="async"></a>`,
		`<figcaption class="cm-file-meta"><span class="cm-img-name">plot.png</span> · 20 B</figcaption>`,
		`>spec.html</a> <span class="cm-file-meta">file · 25 B</span>`)

	// The viewer opens each from its link: the image inline, the html as a download.
	open := func(raw, as string) *httptest.ResponseRecorder {
		u, _ := url.Parse(raw)
		r := httptest.NewRequest("GET", strings.TrimPrefix(u.EscapedPath(), "/base"), nil)
		r.Header.Set("X-Test-As", as)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec := open(refs[0].URL, "claimlayer")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), pngBytes) ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("png = %d %v", rec.Code, rec.Header())
	}
	rec = open(refs[1].URL, "claimlayer")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename=spec.html` ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("html = %d %v", rec.Code, rec.Header())
	}
}

func TestFileOnlyMessage(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))

	code, body := postParts(t, h, "/garden/c/t/cl-msg1/reply", "claimlayer",
		part{name: "text", body: []byte("  ")},
		part{name: "file", file: "a.png", body: pngBytes},
		part{name: "file", file: "", body: nil}) // an empty file input
	if code != http.StatusCreated {
		t.Fatalf("reply = %d %s", code, body)
	}
	req, f := lastCreated(t, d)
	if req.Title != "Shared a.png" || f["thread_id"] != "cl-msg1" {
		t.Errorf("bead = %+v fields %v", req, f)
	}
	if _, ok := f["text"]; ok {
		t.Errorf("a file-only message carries text: %v", f)
	}
	if refs := filesOf(t, f); len(refs) != 1 || !strings.Contains(refs[0].URL, "/garden/c/f/claimlayer/cl-chgen/") {
		t.Errorf("files = %+v", refs)
	}
	mustContain(t, body, `alt="a.png"`, `<span class="cm-img-name">a.png</span>`)
	mustNotContain(t, body, `class="cm-text"`)

	// Neither text nor a file is nothing to say.
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", part{name: "text"}); code != http.StatusBadRequest {
		t.Errorf("empty multipart say = %d", code)
	}
}

func TestUploadRefusalsStoreNothing(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))
	img := part{name: "file", file: "a.png", body: pngBytes}

	// bo is not in #build: refused before the body is read.
	if code, _ := postParts(t, h, "/garden/c/claimlayer/build/say", "claimlayer", img); code != http.StatusForbidden {
		t.Errorf("non-member = %d", code)
	}
	// Another land is not there at all.
	if code, _ := postParts(t, h, "/garden/c/gardenio/general/say", "claimlayer", img); code != http.StatusNotFound {
		t.Errorf("other land = %d", code)
	}
	// Too large a file.
	big := part{name: "file", file: "big.bin", body: make([]byte, fileMaxBytes+1)}
	if code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", big); code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "big.bin is larger than 10 MB") {
		t.Errorf("big file = %d %s", code, body)
	}
	// Too many.
	many := make([]part, filesMax+1)
	for i := range many {
		many[i] = img
	}
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", many...); code != http.StatusBadRequest {
		t.Errorf("too many = %d", code)
	}
	if k := m.keys(); len(k) != 0 || len(d.Created) != 0 {
		t.Fatalf("stored %v, created %d", k, len(d.Created))
	}

	// The daemon refuses the message: what was stored is taken back.
	d.FailWrites = errors.New("daemon down")
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", img); code != http.StatusBadGateway {
		t.Errorf("daemon down = %d", code)
	}
	if k := m.keys(); len(k) != 0 || len(m.deleted) != 1 {
		t.Errorf("after a refused write: stored %v, deleted %v", k, m.deleted)
	}
	d.FailWrites = nil

	// The store fails: no message.
	m.failPut = errors.New("quota")
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", img); code != http.StatusBadGateway || len(d.Created) != 0 {
		t.Errorf("store down = %d, created %d", code, len(d.Created))
	}
}

func TestNoFileStore(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", part{name: "text", body: []byte("hi")}); code != http.StatusUnsupportedMediaType {
		t.Errorf("multipart without a store = %d", code)
	}
	_, tl := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, tl, `name="text" rows="1" maxlength="10000" required`)
	mustNotContain(t, tl, "cm-attach", "multipart")
	if code, _ := get(t, h, "/garden/c/f/claimlayer/cl-chgen/"+strings.Repeat("a", 32)+"/a.png", "claimlayer"); code != http.StatusServiceUnavailable {
		t.Errorf("file route without a store = %d", code)
	}

	// With one, the composer offers attaching and text is not required.
	h = commonsServer(t, d, withFiles(&memFiles{}))
	_, tl = get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, tl, `enctype="multipart/form-data"`, `<input class="cm-attach-input" type="file" name="file" multiple aria-label="Attach files">`)
	mustNotContain(t, tl, "required")
}

func TestFileRouteIsCutLikeTheTimeline(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))
	id := strings.Repeat("ab", 16)
	_ = m.PutFile(context.Background(), "gardenio/gd-chgen/"+id+"/x.png", "image/png", pngBytes)
	_ = m.PutFile(context.Background(), "claimlayer/cl-chgen/"+id+"/x.png", "image/png", pngBytes)

	for _, c := range []struct {
		path, as string
		want     int
	}{
		{"/garden/c/f/claimlayer/cl-chgen/" + id + "/x.png", "claimlayer", http.StatusOK},
		{"/garden/c/f/gardenio/gd-chgen/" + id + "/x.png", "claimlayer", http.StatusNotFound},   // not the viewer's land
		{"/garden/c/f/gardenio/gd-chgen/" + id + "/x.png", "", http.StatusOK},                   // the operator's
		{"/garden/c/f/claimlayer/general/" + id + "/x.png", "claimlayer", http.StatusNotFound},  // a channel by name
		{"/garden/c/f/claimlayer/cl-chgen/" + id + "/y.png", "claimlayer", http.StatusNotFound}, // no such file
		{"/garden/c/f/claimlayer/cl-chgen/nothex/x.png", "claimlayer", http.StatusNotFound},
	} {
		if code, _ := get(t, h, c.path, c.as); code != c.want {
			t.Errorf("GET %s as %q = %d, want %d", c.path, c.as, code, c.want)
		}
	}
}

// The machine file route serves the same bytes only to a caller Machine
// admits, still cut by the channel read, and keeps no copy (gd-qnF6mJZtkQ).
func TestMachineFileRoute(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	id := strings.Repeat("ab", 16)
	_ = m.PutFile(context.Background(), "claimlayer/cl-chgen/"+id+"/x.png", "image/png", pngBytes)
	path := "/internal/commons/f/claimlayer/cl-chgen/" + id + "/x.png"

	h := commonsServer(t, d, withFiles(m))
	if code, _ := get(t, h, path, ""); code != http.StatusForbidden {
		t.Errorf("no Machine = %d, want 403", code)
	}

	// X-Test-Machine names the land the caller may read ("*": any).
	machine := func(h *Handler) {
		h.Machine = func(r *http.Request, land string) bool {
			may := r.Header.Get("X-Test-Machine")
			return may == "*" || (may != "" && may == land)
		}
	}
	h = commonsServer(t, d, withFiles(m), machine)
	if code, _ := get(t, h, path, ""); code != http.StatusForbidden {
		t.Errorf("not a machine = %d, want 403", code)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Test-Machine", "*")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("machine = %d (%d bytes), want 200 with the file", rec.Code, rec.Body.Len())
	}
	hd := rec.Result().Header
	if hd.Get("Content-Type") != "image/png" || hd.Get("Content-Length") != strconv.Itoa(len(pngBytes)) || hd.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", hd)
	}

	// An agent reads its own land's file, and gets 403 for another land's
	// (gd-WXY7Jm5ugY).
	for may, want := range map[string]int{"claimlayer": http.StatusOK, "gardenio": http.StatusForbidden} {
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Test-Machine", may)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("agent of %s = %d, want %d", may, rec.Code, want)
		}
	}

	// The channel read still cuts it: a channel addressed by name is no file.
	r = httptest.NewRequest("GET", "/internal/commons/f/claimlayer/general/"+id+"/x.png", nil)
	r.Header.Set("X-Test-Machine", "*")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("by channel name = %d, want 404", rec.Code)
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"plot.png":         "plot.png",
		"../../etc/passwd": "passwd",
		`C:\Users\a\b.pdf`: "b.pdf",
		"..":               "file",
		"  ":               "file",
		"a\x00b\nc.txt":    "abc.txt",
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
	// A long name keeps its extension and stays a fixed point.
	long := fileName(strings.Repeat("x", 300) + ".gz")
	if len(long) > fileNameMax || !strings.HasSuffix(long, "….gz") || fileName(long) != long {
		t.Errorf("long name = %q (%d bytes)", long, len(long))
	}
}

// TestUploadRecordsImageSize: a raster image's size is read from its header at
// upload and recorded in the files field, so the timeline reserves its box
// (gd-fAgk8mVsta); a file that is not an image records none.
func TestUploadRecordsImageSize(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, withFiles(&memFiles{}))
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1600, 900))); err != nil {
		t.Fatal(err)
	}
	code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer",
		part{name: "file", file: "farm.png", ct: "image/png", body: img.Bytes()},
		part{name: "file", file: "notes.txt", ct: "text/plain", body: []byte("notes")})
	if code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	_, f := lastCreated(t, d)
	refs := filesOf(t, f)
	if len(refs) != 2 || refs[0].Width != 1600 || refs[0].Height != 900 || refs[1].Width != 0 || refs[1].Height != 0 {
		t.Fatalf("refs = %+v", refs)
	}
	if !strings.Contains(f["files"], `"width":1600,"height":900`) || strings.Count(f["files"], `"width"`) != 1 {
		t.Errorf("files field = %s", f["files"])
	}
	// laid out at 320 tall, keeping the ratio, before it loads
	mustContain(t, body, `alt="farm.png" loading="lazy" decoding="async" width="569" height="320"></a>`,
		`>notes.txt</a> <span class="cm-file-meta">file · 5 B</span>`)
}

// TestInlineSrc pins which attachments show inline: a raster image on this
// Commons's file route under the message's own land and channel, and nothing
// else. A Slack permalink, an SVG, another channel's file or a path that is
// not the route's shape stays a link.
func TestInlineSrc(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	route := "https://garden.example/base/garden/c/f/my%20land/gd-ch1/" + id + "/farm%20view.png"
	for _, c := range []struct {
		mime, url, want string
	}{
		{"image/png", route, "/garden/c/f/my%20land/gd-ch1/" + id + "/farm%20view.png"},
		{"IMAGE/JPEG", "http://other.host/garden/c/f/my%20land/gd-ch1/" + id + "/a.jpg", "/garden/c/f/my%20land/gd-ch1/" + id + "/a.jpg"},
		{"image/webp", "https://h/garden/c/f/my%20land/gd-ch1/" + id + "/a.webp?x=1#y", "/garden/c/f/my%20land/gd-ch1/" + id + "/a.webp"},
		{"image/gif", "https://h/garden/c/f/my%20land/gd-ch1/" + id + "/a.gif", "/garden/c/f/my%20land/gd-ch1/" + id + "/a.gif"},
		{"image/svg+xml", route, ""},
		{"application/pdf", route, ""},
		{"", route, ""},
		{"image/png", "https://acme.slack.com/files/U1/F1/plot.png", ""},
		{"image/png", "https://h/garden/c/f/my%20land/gd-ch2/" + id + "/a.png", ""},   // another channel
		{"image/png", "https://h/garden/c/f/other/gd-ch1/" + id + "/a.png", ""},       // another land
		{"image/png", "https://h/garden/c/f/my%20land/gd-ch1/ABCDEF/a.png", ""},       // not a file id
		{"image/png", "https://h/garden/c/f/my%20land/gd-ch1/" + id + "/x/a.png", ""}, // a deeper path
		{"image/png", "https://h/garden/c/f/my%20land/gd-ch1/" + id + "/", ""},
		{"image/png", "https://h/garden/c/f/my%20land/gd-ch1/" + id + "/%2E%2E", ""},
	} {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := inlineSrc(c.mime, u, "my land", "gd-ch1"); got != c.want {
			t.Errorf("inlineSrc(%q, %s) = %q, want %q", c.mime, c.url, got, c.want)
		}
	}
}

func TestImageBox(t *testing.T) {
	for _, c := range []struct{ w, h, bw, bh int }{
		{1600, 900, 569, 320},
		{200, 100, 200, 100},
		{390, 844, 148, 320}, // a phone screenshot
		{4000, 320, 4000, 320},
		{1, 5000, 1, 320},
		{0, 100, 0, 0},
		{100, 0, 0, 0},
	} {
		f := File{W: c.w, H: c.h}
		if f.BoxW() != c.bw || f.BoxH() != c.bh {
			t.Errorf("box(%dx%d) = %dx%d, want %dx%d", c.w, c.h, f.BoxW(), f.BoxH(), c.bw, c.bh)
		}
	}
	// an implausible recorded size reads as not known
	raw := `[{"name":"a.png","mimetype":"image/png","url":"https://h/garden/c/f/l/c/0123456789abcdef0123456789abcdef/a.png","width":99999,"height":10}]`
	if fs := filesFrom(raw, "l", "c"); len(fs) != 1 || fs[0].Src == "" || fs[0].W != 0 || fs[0].H != 0 {
		t.Errorf("files = %+v", fs)
	}
}
