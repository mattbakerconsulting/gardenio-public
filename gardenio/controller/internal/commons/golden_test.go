package commons

import (
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

var update = flag.Bool("update", false, "rewrite the fragment goldens in testdata/")

// TestFragmentGoldens pins the markup the /garden Commons pane (step 9) builds
// on: its classes, data-* hooks and links. A change you meant is
// `go test ./internal/commons -run TestFragmentGoldens -update`.
func TestFragmentGoldens(t *testing.T) {
	d := commonstest.TwoLands(t0)
	// attachments (gd-fAgk8mVsta): an image on the file route shows inline in
	// its reserved box, and a pdf, a Slack image and another channel's image
	// stay links
	const route = "https://garden.example/base/garden/c/f/claimlayer/cl-chgen/"
	d.Put(&beadsapi.BeadDetail{ID: "cl-files1", Type: "message", Title: "the farm", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0, Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "text": "the farm",
			"files": `[{"name":"farm view.png","mimetype":"image/png","size":48213,"width":1600,"height":900,"url":"` + route + `0123456789abcdef0123456789abcdef/farm%20view.png"},` +
				`{"name":"notes.pdf","mimetype":"application/pdf","size":2048,"url":"` + route + `fedcba9876543210fedcba9876543210/notes.pdf"},` +
				`{"name":"slack.png","mimetype":"image/png","size":512,"url":"https://acme.slack.com/files/U1/F1/slack.png"},` +
				`{"name":"elsewhere.png","mimetype":"image/png","url":"https://garden.example/base/garden/c/f/claimlayer/cl-chcrew/00112233445566778899aabbccddeeff/elsewhere.png"}]`}})
	withBuildThread(d)
	h := commonsServer(t, d)
	msgs := func(ids ...string) (paths []string) {
		for _, id := range ids {
			paths = append(paths, "/test/m/"+id)
		}
		return paths
	}
	for name, c := range map[string]struct {
		paths []string
		self  string
	}{
		"thread":  {paths: []string{"/garden/c/claimlayer/general/t/cl-wish1"}},
		"message": {paths: msgs("cl-card1")},
		// the agent cards' wrap-up face (the live face is cl-card1, in message)
		"crew": {paths: msgs("cl-crew1", "cl-card2", "cl-card3", "cl-storm1")},
		// decision and permission cards, open, answered and dismissed, and a
		// decision's Other and Escalate lightboxes
		"decisions":        {paths: msgs("cl-dcard1", "cl-dcard2", "cl-dcard3", "cl-pcard1", "cl-pcard2")},
		"lightbox-other":   {paths: []string{"/garden/c/m/cl-dcard1/lightbox/other"}},
		"lightbox-promote": {paths: []string{"/garden/c/m/cl-dcard1/lightbox/promote"}},
		// escalation cards waiting, approved, done (secret) and rejected
		"escalations": {paths: msgs(escalationCards...)},
		// a died agent's card thread: its crash notice and death report
		"death": {paths: []string{"/garden/c/claimlayer/crew/t/cl-card3"}},
		// a user channel's thread: Join for bo, who is not in it; the composer for ann, who is
		"join":   {paths: []string{"/garden/c/claimlayer/build/t/cl-bld1"}},
		"member": {paths: []string{"/garden/c/claimlayer/build/t/cl-bld1"}, self: "oidc:ann@claimlayer.example"},
		"files":  {paths: msgs("cl-files1")},
	} {
		t.Run(name, func(t *testing.T) {
			body := ""
			for _, path := range c.paths {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("X-Test-As", "claimlayer")
				if c.self != "" {
					r.Header.Set("X-Test-Self", c.self)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Fatalf("GET %s = %d", path, rec.Code)
				}
				body += rec.Body.String()
			}
			file := filepath.Join("testdata", name+".golden.html")
			if *update {
				if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if body != string(want) {
				t.Errorf("GET %v drifted from %s (run with -update if meant):\n%s", c.paths, file, body)
			}
		})
	}
}
