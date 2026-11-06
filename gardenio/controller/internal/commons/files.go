package commons

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif" // image.DecodeConfig reads the inline types' sizes
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "golang.org/x/image/webp"
)

// Attachments (gd-S8ufEc3k6s): a person attaches files to a message from the
// composer. The say and reply routes take them as multipart/form-data, read
// part by part once the Gate has let the viewer post (beads-viewer's root
// filesystem is read-only, so nothing is spooled to disk). Each file goes into
// the FileStore under its land and channel, and the message bead's files field
// (conversation.BeadFiles) records where it opens: the file route below, as an
// absolute URL, which the Commons links and the bridge's Slack mirror posts.
//
// The file route reads the channel on the viewer's bearer before it serves a
// byte, so whoever may read the timeline may open its files, and no one else.
// Only a raster image is served inline; anything else is a download, never
// rendered from the viewer's origin.
//
// The machine file route (gd-qnF6mJZtkQ) serves the same bytes to a machine
// principal: the slack-bridge, which uploads them into Slack in place of a
// viewer link, and an agent on its own token (`gb commons file`,
// gd-WXY7Jm5ugY), which reads only its own land's. beads-viewer admits only a
// machine principal to it (its routeMachine class) and says so through
// Machine; the channel is still read on the caller's own bearer first.

// FileStore keeps the bytes of the files people attach. beads-viewer's is the
// session object store (S3, or MinIO in-box). Keys are this package's:
// <land>/<channel id>/<file id>/<name>.
type FileStore interface {
	PutFile(ctx context.Context, key, contentType string, body []byte) error
	// GetFile opens a stored file; ErrNoFile when there is none.
	GetFile(ctx context.Context, key string) (StoredFile, error)
	DeleteFile(ctx context.Context, key string) error
}

// StoredFile is an open stored file. The caller closes Body.
type StoredFile struct {
	Body        io.ReadCloser
	ContentType string
	ETag        string
	Size        int64 // bytes; 0 when the store did not say
}

// ErrNoFile is what a FileStore answers for a key it does not hold.
var ErrNoFile = errors.New("commons: no such file")

// Attachment bounds.
const (
	// fileMaxBytes caps one file.
	fileMaxBytes = 10 << 20
	// uploadMaxBytes caps a say or reply that carries files.
	uploadMaxBytes = 25 << 20
	// fileNameMax caps a stored file name, in bytes.
	fileNameMax = 120
	// fileTimeout is the store's time per file, on top of writeTimeout.
	fileTimeout = 15 * time.Second
	// fileCache: a file never changes under its id, but access can, so a
	// browser keeps it briefly and privately (as the /image proxy does).
	fileCache = "private, max-age=300"
)

// inlineTypes are the types a file is served as: raster images, which a
// browser shows and never runs. Anything else is stored and served as
// application/octet-stream, a download.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// fileRef is one entry of a message bead's files field.
type fileRef struct {
	Name     string `json:"name"`
	MimeType string `json:"mimetype,omitempty"`
	Size     int    `json:"size,omitempty"`
	URL      string `json:"url"`
	// Width and Height: a raster image's own size in pixels, read from its
	// header at upload, so the Commons reserves its box before it loads.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// upload is one file read from a say or reply form.
type upload struct {
	name     string // safe as a key and URL segment (fileName)
	mimeType string // what it says it is, for the files field
	stored   string // what it is stored and served as (inlineTypes, else octet-stream)
	w, h     int    // a raster image's size; 0 when its header does not say
	body     []byte
}

// posted is what a say or reply form carries.
type posted struct {
	text  string
	files []upload
	snap  string // the context field as posted (snap.go)
	nonce string // the nonce field as posted (sendonce.go)
	about string // the about field as posted: the bead a message is about
}

// stored is a file put in the store: its key, and its files-field entry.
type stored struct {
	key string
	ref fileRef
}

// readPost reads a say or reply form: url-encoded text, or multipart text and
// files when the FileStore is set. It answers the error itself.
func (h *Handler) readPost(w http.ResponseWriter, r *http.Request) (posted, bool) {
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		text, ok := h.formText(w, r)
		return posted{text: text, nonce: r.PostForm.Get("nonce"), about: r.PostForm.Get("about")}, ok
	}
	if h.Files == nil {
		http.Error(w, "Attaching files is not enabled here.", http.StatusUnsupportedMediaType)
		return posted{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes)
	mr := multipart.NewReader(r.Body, params["boundary"])
	var in posted
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			bodyFail(w, err)
			return posted{}, false
		}
		switch p.FormName() {
		case "text":
			b, err := io.ReadAll(io.LimitReader(p, textMaxBytes+1))
			if err != nil {
				bodyFail(w, err)
				return posted{}, false
			}
			in.text = string(b)
		case "nonce":
			b, err := io.ReadAll(io.LimitReader(p, 65))
			if err != nil {
				bodyFail(w, err)
				return posted{}, false
			}
			in.nonce = string(b)
		case "about":
			b, err := io.ReadAll(io.LimitReader(p, 65))
			if err != nil {
				bodyFail(w, err)
				return posted{}, false
			}
			in.about = string(b)
		case "context":
			b, err := io.ReadAll(io.LimitReader(p, snapMaxBytes+1))
			if err != nil {
				bodyFail(w, err)
				return posted{}, false
			}
			in.snap = string(b)
		case "file":
			if p.FileName() == "" {
				continue // an empty file input
			}
			if len(in.files) == filesMax {
				http.Error(w, "Attach "+strconv.Itoa(filesMax)+" files at most.", http.StatusBadRequest)
				return posted{}, false
			}
			name := fileName(p.FileName())
			b, err := io.ReadAll(io.LimitReader(p, fileMaxBytes+1))
			if err != nil {
				bodyFail(w, err)
				return posted{}, false
			}
			if len(b) > fileMaxBytes {
				http.Error(w, name+" is larger than 10 MB.", http.StatusRequestEntityTooLarge)
				return posted{}, false
			}
			in.files = append(in.files, newUpload(name, p.Header.Get("Content-Type"), b))
		}
	}
	in.text = strings.TrimSpace(in.text)
	if len(in.files) > 0 && in.text == "" {
		return in, true // the files are the message
	}
	return in, checkText(w, in.text)
}

// bodyFail answers a form body that could not be read.
func bodyFail(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "Those files are too large together (25 MB at most).", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "Bad form.", http.StatusBadRequest)
}

// newUpload sniffs what the bytes are: a raster image keeps its type, and
// anything else is stored as a download whatever it claims to be.
func newUpload(name, declared string, body []byte) upload {
	u := upload{name: name, body: body, stored: "application/octet-stream"}
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(body))
	if inlineTypes[sniffed] {
		u.stored, u.mimeType = sniffed, sniffed
		if c, _, err := image.DecodeConfig(bytes.NewReader(body)); err == nil {
			u.w, u.h = c.Width, c.Height
		}
		return u
	}
	if mt, _, err := mime.ParseMediaType(declared); err == nil && !strings.HasPrefix(mt, "image/") {
		u.mimeType = mt // a claimed image that is not one shows as a file
	}
	return u
}

// fileName is a file name safe as one path segment: no directory, no control
// characters, at most fileNameMax bytes, never empty or a dot name.
func fileName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > fileNameMax {
		ext := ""
		if i := strings.LastIndexByte(s, '.'); i > 0 && len(s)-i <= 10 {
			ext = s[i:]
		}
		s = clipBytes(s[:len(s)-len(ext)], fileNameMax-len(ext)) + ext
	}
	if s == "" || strings.Trim(s, ".") == "" {
		return "file"
	}
	return s
}

// storeFiles puts each upload in the store under ch and returns where each
// opens. On a failure it takes back what it put.
func (h *Handler) storeFiles(ctx context.Context, r *http.Request, ch Channel, ups []upload) ([]stored, error) {
	out := make([]stored, 0, len(ups))
	for _, u := range ups {
		id, err := fileID()
		if err != nil {
			h.dropFiles(r, out)
			return nil, err
		}
		key := ch.Land + "/" + ch.ID + "/" + id + "/" + u.name
		if err := h.Files.PutFile(ctx, key, u.stored, u.body); err != nil {
			h.dropFiles(r, out)
			return nil, err
		}
		link := requestOrigin(r) + h.BasePath + "/garden/c/f/" + url.PathEscape(ch.Land) + "/" +
			url.PathEscape(ch.ID) + "/" + id + "/" + url.PathEscape(u.name)
		out = append(out, stored{key: key, ref: fileRef{Name: u.name, MimeType: u.mimeType, Size: len(u.body), URL: link, Width: u.w, Height: u.h}})
	}
	return out, nil
}

// dropFiles deletes stored files no message records, past the request's end.
func (h *Handler) dropFiles(r *http.Request, files []stored) {
	if len(files) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
	defer cancel()
	for _, f := range files {
		if err := h.Files.DeleteFile(ctx, f.key); err != nil {
			slog.Default().Warn("commons: an unused attachment was not deleted", "key", f.key, "error", err)
		}
	}
}

// fileID is a stored file's unguessable id: 128 random bits in hex.
func fileID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validFileID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// filesField is the files field of a message's stored files.
func filesField(files []stored) string {
	refs := make([]fileRef, len(files))
	for i, f := range files {
		refs[i] = f.ref
	}
	raw, _ := json.Marshal(refs) // a []fileRef always marshals
	return string(raw)
}

// fileTitle is the title of a message that is only files: Shared a.png, b.pdf.
func fileTitle(files []stored) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.ref.Name
	}
	return "Shared " + strings.Join(names, ", ")
}

// requestOrigin is the scheme and host the viewer sees in its address bar,
// as beads-viewer's login redirect reads them behind the ingress.
func requestOrigin(r *http.Request) string {
	scheme := "https"
	if v := r.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
		scheme = v
	} else if v == "" && r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host
}

// file serves GET /garden/c/f/{land}/{channel}/{file}/{name}: an attachment,
// to a viewer who may read the channel it was posted in.
func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	h.serveFile(w, r, fileCache)
}

// machineFile serves GET /internal/commons/f/{land}/{channel}/{file}/{name}:
// the file route for a machine principal, which keeps no copy. An agent asking
// for another land's file is refused before the channel is read.
func (h *Handler) machineFile(w http.ResponseWriter, r *http.Request) {
	if h.Machine == nil || !h.Machine(r, r.PathValue("land")) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	h.serveFile(w, r, "no-store")
}

// serveFile streams one attachment after reading its channel on the caller's
// bearer, with cache as its Cache-Control.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, cache string) {
	if !h.ready(w) {
		return
	}
	if h.Files == nil {
		http.Error(w, "attachments not enabled", http.StatusServiceUnavailable)
		return
	}
	land, chID, id, name := r.PathValue("land"), r.PathValue("channel"), r.PathValue("file"), r.PathValue("name")
	if !validFileID(id) || name != fileName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	ch, err := readChannel(ctx, h.Beads, h.scope(r), land, chID)
	cancel()
	if err == nil && ch.ID != chID {
		err = errNotFound // addressed by name: a file lives under its channel's id
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), fileTimeout)
	defer cancel()
	f, err := h.Files.GetFile(ctx, land+"/"+chID+"/"+id+"/"+name)
	if errors.Is(err, ErrNoFile) {
		http.Error(w, "That file is no longer here.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "the file store did not answer", http.StatusBadGateway)
		return
	}
	defer f.Body.Close()
	hd := w.Header()
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Cache-Control", cache)
	if f.ETag != "" {
		hd.Set("ETag", f.ETag)
	}
	if f.Size > 0 {
		hd.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	disposition := "inline"
	if ct, _, _ := mime.ParseMediaType(f.ContentType); inlineTypes[ct] {
		hd.Set("Content-Type", ct)
	} else {
		hd.Set("Content-Type", "application/octet-stream")
		disposition = "attachment"
	}
	hd.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	if _, err := io.Copy(w, f.Body); err != nil {
		slog.Default().Warn("commons: an attachment stream was cut", "key", land+"/"+chID+"/"+id, "error", err)
	}
}
