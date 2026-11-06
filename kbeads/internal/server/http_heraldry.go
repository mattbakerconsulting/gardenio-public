package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
)

// heraldryLibraryMaxAge is how long a client may keep the library (spec §3.1:
// "a day's cache"). The library only moves when the artwork set is re-pinned,
// by PR, which also redeploys the daemon.
const heraldryLibraryMaxAge = 24 * 60 * 60

// heraldryLibraryCharge is one charge on GET /v1/heraldry/library.
type heraldryLibraryCharge struct {
	Emoji      string `json:"emoji"`
	Name       string `json:"name"`
	Codepoints string `json:"codepoints"`
	VS16       bool   `json:"vs16,omitempty"`
}

// heraldryLibraryResponse is the body of GET /v1/heraldry/library: everything
// a client needs to offer, check and name arms without importing kbeads (the
// bridge, the beads-viewer's picker, the Workbench).
type heraldryLibraryResponse struct {
	Artwork             string                  `json:"artwork"`
	ArtworkVersion      string                  `json:"artwork_version"`
	UnicodeEmojiVersion string                  `json:"unicode_emoji_version"`
	Help                string                  `json:"help"`
	Charges             []heraldryLibraryCharge `json:"charges"`
}

// heraldryLibraryEncoded is the library body and its ETag (a hash of the
// body, so any change to the table or the help text moves it).
type heraldryLibraryEncoded struct {
	body []byte
	etag string
}

// heraldryLibraryBody is the encoded library, built once: it is a generated
// table, the same for every caller and every request.
var heraldryLibraryBody = sync.OnceValues(func() (heraldryLibraryEncoded, error) {
	resp := heraldryLibraryResponse{
		Artwork:             "noto-emoji",
		ArtworkVersion:      heraldry.NotoEmojiTag,
		UnicodeEmojiVersion: heraldry.UnicodeEmojiVersion,
		Help:                model.HeraldryHelp,
	}
	for _, c := range heraldry.Library() {
		resp.Charges = append(resp.Charges, heraldryLibraryCharge{Emoji: c.Emoji, Name: c.Name, Codepoints: c.Codepoints, VS16: c.VS16})
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return heraldryLibraryEncoded{}, err
	}
	sum := sha256.Sum256(body)
	return heraldryLibraryEncoded{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}, nil
})

// handleHeraldryLibrary handles GET /v1/heraldry/library (heraldry spec §3.1):
// the charges arms may be made of and the help text, as JSON with a day's
// cache. It returns no bead data.
func (s *BeadsServer) handleHeraldryLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := heraldryLibraryBody()
	if err != nil {
		writeServerError(r.Context(), w, "handleHeraldryLibrary", err, "failed to encode the arms library")
		return
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", heraldryLibraryMaxAge))
	w.Header().Set("ETag", lib.etag)
	if r.Header.Get("If-None-Match") == lib.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(lib.body)
}
