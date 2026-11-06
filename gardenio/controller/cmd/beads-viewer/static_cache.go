package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// staticCached serves /assets/static/ (fsys, under the route's StripPrefix)
// with a content-hash ETag and Cache-Control: no-cache (P1.5, gd-YU9gGOMvur).
// http.FileServer over an embed.FS sends neither: embedded files have no
// modtime, so no Last-Modified either, and a response with no validator and no
// freshness is downloaded in full on every visit. That was every viewer page's
// scripts and the Glass's xterm.bundle.js (285 KB raw) on each open. The URLs
// carry no version, so the browser still asks each time, and a deploy that
// changes a file changes its tag; an unchanged file is a bodyless 304
// (http.ServeContent compares If-None-Match against the ETag set here).
//
// The tags are hashed once, on the first request, so a Server built by a test
// that never serves a static file pays nothing.
func staticCached(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))
	tags := sync.OnceValue(func() map[string]string { return staticTags(fsys) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags()[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// staticTags is a strong ETag per regular file in fsys, by its slash path: the
// first 16 bytes of its SHA-256, hex.
func staticTags(fsys fs.FS) map[string]string {
	tags := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		tags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return tags
}

// overlayFS is base with top's root files beside its own, top's winning on a name both hold. /assets/static/ is the
// viewer's templates/static and gardenhub's xterm.js (gardenhub.XtermFS): the Glass serves xterm hashed from
// /garden/assets/, and the agent page loads it from here by its old names (gd-BC3yZxTQ65).
type overlayFS struct{ base, top fs.FS }

func (o overlayFS) Open(name string) (fs.File, error) {
	if !strings.Contains(name, "/") {
		if f, err := o.top.Open(name); err == nil {
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				return f, nil
			}
			_ = f.Close()
		}
	}
	return o.base.Open(name)
}

// ReadDir lists base's directory, and at the root top's files too, so staticTags tags them.
func (o overlayFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(o.base, name)
	if err != nil || name != "." {
		return entries, err
	}
	top, err := fs.ReadDir(o.top, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		if e.IsDir() {
			continue
		}
		entries = slices.DeleteFunc(entries, func(b fs.DirEntry) bool { return b.Name() == e.Name() })
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
