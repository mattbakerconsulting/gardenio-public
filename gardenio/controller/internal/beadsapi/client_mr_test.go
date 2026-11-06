package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUpsertMergeRequestBead_OperatedBySetOnce pins ka-0000004473: when the MR
// bead already carries a non-empty operated_by (stamped at create time from
// the MR's creator), a later upsert whose actor is a different user (e.g. a
// merger) must NOT overwrite it. The PATCH body sent to the daemon must drop
// the operated_by key so the field is left untouched.
func TestUpsertMergeRequestBead_OperatedBySetOnce(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/88"
	var patchBody map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// findMergeRequestBead lists type=merge-request beads.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("type") != "":
			resp := listBeadsResponse{Beads: []beadJSON{{
				ID:     "mr-bead-1",
				Type:   "merge-request",
				Status: "open",
				Fields: json.RawMessage(`{"mr_url":"` + mrURL + `","operated_by":"gitlab:alice"}`),
			}}}
			_ = json.NewEncoder(w).Encode(resp)

		// UpdateBeadFields GETs the single bead, then PATCHes.
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{
				ID:     "mr-bead-1",
				Type:   "merge-request",
				Status: "open",
				Fields: json.RawMessage(`{"mr_url":"` + mrURL + `","operated_by":"gitlab:alice"}`),
			})

		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &patchBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	// Merger bob upserts; operated_by=gitlab:bob must be dropped (set-once).
	id, err := c.UpsertMergeRequestBead(context.Background(), MergeRequestBeadFields{
		MRURL:      mrURL,
		MRIID:      "88",
		Author:     "bob",
		OperatedBy: "gitlab:bob",
		MRState:    "opened",
	}, false)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != "mr-bead-1" {
		t.Fatalf("id=%q, want mr-bead-1", id)
	}

	fieldsRaw, ok := patchBody["fields"]
	if !ok {
		t.Fatal("expected 'fields' in PATCH body")
	}
	var merged map[string]string
	if err := json.Unmarshal(fieldsRaw, &merged); err != nil {
		t.Fatalf("unmarshal patch fields: %v", err)
	}
	// UpdateBeadFields now sends a PARTIAL patch (only changed keys), so the
	// set-once drop means operated_by is OMITTED from the PATCH entirely — the
	// server's additive merge leaves the creator's gitlab:alice in place. The
	// point is that the update did NOT carry gitlab:bob, i.e. the creator's
	// attribution wins.
	if _, present := merged["operated_by"]; present {
		t.Errorf("operated_by=%q present in patch, want it OMITTED so the creator's gitlab:alice is preserved (set-once)", merged["operated_by"])
	}
}

// TestUpsertMergeRequestBead_ConcurrentSameURL pins bug ka-0000007606: two
// near-simultaneous upserts for the SAME mr_url (e.g. a single `gb mr link` of
// one MR to two beads firing two bead.updated events ~2ms apart, fanned out
// concurrently by the SSE dispatcher) must converge to exactly ONE
// type=merge-request bead — not the observed double (ka-0000000654 +
// ka-0000008228).
//
// The test server emulates a realistic backing store: findMergeRequestBead's
// LIST reflects beads created by a prior POST, and CreateBead appends. An
// artificial delay between the LIST and POST widens the find-then-create
// window so an unsynchronised implementation would reliably double-insert.
// The per-mr_url lock in UpsertMergeRequestBead must serialise the two calls
// so the second finds the first's bead and PATCHes it instead of inserting.
func TestUpsertMergeRequestBead_ConcurrentSameURL(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/3656"

	var mu sync.Mutex
	created := []beadJSON{} // server-side store of MR beads
	createCount := 0
	idSeq := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// findMergeRequestBead: LIST type=merge-request beads.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("type") != "":
			mu.Lock()
			snapshot := make([]beadJSON, len(created))
			copy(snapshot, created)
			mu.Unlock()
			// Widen the find→create window: without the per-URL lock both
			// goroutines LIST an empty store here and then both POST.
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: snapshot})

		// UpdateBeadFields GETs the single bead before PATCHing.
		case r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			mu.Lock()
			var found *beadJSON
			for i := range created {
				if created[i].ID == id {
					found = &created[i]
					break
				}
			}
			mu.Unlock()
			if found == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(*found)

		// CreateBead POSTs a new MR bead.
		case r.Method == http.MethodPost:
			mu.Lock()
			idSeq++
			createCount++
			id := "mr-bead-" + strconv.Itoa(idSeq)
			created = append(created, beadJSON{
				ID:     id,
				Type:   "merge-request",
				Status: "open",
				Fields: json.RawMessage(`{"mr_url":"` + mrURL + `"}`),
			})
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		// PATCH (UpdateBeadFields) — accept and no-op.
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	// Fire two upserts for the same mr_url concurrently, gated on a common
	// start so they enter the critical section together.
	const n = 2
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		done.Add(1)
		go func(idx int) {
			defer done.Done()
			start.Wait()
			ids[idx], errs[idx] = c.UpsertMergeRequestBead(context.Background(), MergeRequestBeadFields{
				MRURL:   mrURL,
				MRIID:   "3656",
				MRState: "opened",
			}, false)
		}(i)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("upsert[%d]: %v", i, err)
		}
	}
	if createCount != 1 {
		t.Fatalf("createCount=%d, want exactly 1 MR bead created (duplicate-bead race)", createCount)
	}
	if ids[0] != ids[1] {
		t.Fatalf("concurrent upserts returned different bead ids %q vs %q, want one converged id", ids[0], ids[1])
	}
	mu.Lock()
	total := len(created)
	mu.Unlock()
	if total != 1 {
		t.Fatalf("store holds %d MR beads, want 1", total)
	}
}

// TestUpsertMergeRequestBead_OperatedByStampedOnCreate pins that a brand-new MR
// bead is created with operated_by set from the resolved creator.
func TestUpsertMergeRequestBead_OperatedByStampedOnCreate(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/99"
	var createBody map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// findMergeRequestBead returns no existing bead.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("type") != "":
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{}})

		// CreateBead POSTs to /v1/beads.
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "mr-bead-new"})
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.UpsertMergeRequestBead(context.Background(), MergeRequestBeadFields{
		MRURL:      mrURL,
		MRIID:      "99",
		Author:     "alice",
		OperatedBy: "slack:U0ALICE",
		MRState:    "opened",
	}, false)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != "mr-bead-new" {
		t.Fatalf("id=%q, want mr-bead-new", id)
	}

	fieldsRaw, ok := createBody["fields"]
	if !ok {
		t.Fatal("expected 'fields' in create body")
	}
	var fields map[string]string
	if err := json.Unmarshal(fieldsRaw, &fields); err != nil {
		t.Fatalf("unmarshal create fields: %v", err)
	}
	if fields["operated_by"] != "slack:U0ALICE" {
		t.Errorf("operated_by=%q, want slack:U0ALICE on create", fields["operated_by"])
	}
}

// TestFindMergeRequestBead_PushesMRURLSelectorServerSide is the core regression
// guard for bug ka-0000005190: findMergeRequestBead (behind MRBeadByURL and
// UpsertMergeRequestBead's find-then-create) must push the mr_url selector to
// the DAEMON via field=mr_url=<url>, and must NOT ride the old fixed Limit:500
// scan of every merge-request bead. There are 6834 merge-request beads
// fleet-wide, so the daemon capped the old fetch at 500 (created_at DESC) and
// any older MR bead was invisible — MRBeadByURL returned (nil,nil) for an MR
// whose bead genuinely exists. This mock asserts (a) the request carries the
// server-side equality selector, (b) it dropped the Limit:500 cap, and (c) the
// single matched bead the daemon returns is surfaced.
func TestFindMergeRequestBead_PushesMRURLSelectorServerSide(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		// The daemon serves the single row matching field=mr_url=<url>; the
		// point of the fix is the CLIENT no longer relies on scanning a page.
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{{
			ID: "gd-mr-old", Type: "merge-request", Status: "closed",
			Fields: json.RawMessage(`{"mr_url":"` + mrURL + `","mr_outcome":"merged"}`),
		}}, Total: 1})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.MRBeadByURL(context.Background(), mrURL)
	if err != nil {
		t.Fatalf("MRBeadByURL: %v", err)
	}
	if got == nil || got.ID != "gd-mr-old" {
		t.Fatalf("expected gd-mr-old, got %+v", got)
	}

	// field=mr_url=<url> url-encodes as field=mr_url%3D<escaped-url>. Assert the
	// key+eq prefix is present so the selector ran SERVER-SIDE.
	if !strings.Contains(gotQuery, "field=mr_url%3D") {
		t.Errorf("query must push the mr_url equality selector server-side (field=mr_url=<url>), got %q", gotQuery)
	}
	// It must NOT ride the old fixed 500 cap; the point lookup sends Limit:2
	// (duplicate detection) so the daemon returns the match directly.
	if strings.Contains(gotQuery, "limit=500") {
		t.Errorf("query still carries the old fixed limit=500 scan, got %q", gotQuery)
	}
	// All statuses (incl. closed) must remain in the filter — a merged MR's
	// canonical bead is status=closed, so closed-omission would blank it.
	if !strings.Contains(gotQuery, "closed") {
		t.Errorf("query must keep status=closed reachable (merged MR beads are closed), got %q", gotQuery)
	}
}

// TestFindMergeRequestBead_NoMatchReturnsNilNil pins that a URL with no matching
// bead returns (nil, nil) — "not found", not an error — even though the daemon
// now filters server-side. UpsertMergeRequestBead relies on this to take the
// create path.
func TestFindMergeRequestBead_NoMatchReturnsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server-side field filter matched nothing → empty page.
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.MRBeadByURL(context.Background(), "https://gitlab.com/org/repo/-/merge_requests/999")
	if err != nil {
		t.Fatalf("MRBeadByURL: %v", err)
	}
	if got != nil {
		t.Errorf("expected (nil,nil) for no match, got %+v", got)
	}
}

// TestListOpenMergeRequestBeads_PagesPastCap is the regression guard for the
// ListOpenMergeRequestBeads half of bug ka-0000005190: the old Limit:500 fetch
// silently dropped every open MR bead past the newest 500 once the population
// outgrew the cap. The fix routes through ListAllBeadsFiltered, which follows
// offset to exhaustion. This mock serves a FULL first page (exactly
// listAllPagesPageSize rows) so the walk MUST page to a second page where a
// trailing open MR bead lives, and asserts that bead is returned.
func TestListOpenMergeRequestBeads_PagesPastCap(t *testing.T) {
	const beyond = "gd-mr-beyond-cap"
	var sawStatuses string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		sawStatuses = q.Get("status")
		off, _ := strconv.Atoi(q.Get("offset"))
		total := listAllPagesPageSize + 1
		var beads []beadJSON
		switch off {
		case 0:
			beads = make([]beadJSON, listAllPagesPageSize)
			for i := range beads {
				beads[i] = beadJSON{ID: fmt.Sprintf("gd-mr-p0-%d", i), Type: "merge-request", Status: "open"}
			}
		case listAllPagesPageSize:
			beads = []beadJSON{{ID: beyond, Type: "merge-request", Status: "open"}}
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: total})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.ListOpenMergeRequestBeads(context.Background(), "")
	if err != nil {
		t.Fatalf("ListOpenMergeRequestBeads: %v", err)
	}
	if len(got) != listAllPagesPageSize+1 {
		t.Fatalf("returned %d beads, want %d (full first page + trailing bead)", len(got), listAllPagesPageSize+1)
	}
	var found bool
	for _, b := range got {
		if b.ID == beyond {
			found = true
		}
	}
	if !found {
		t.Errorf("open MR bead beyond the first-page cap (%s) was dropped — the exact ka-0000005190 defect", beyond)
	}
	// The open-MR status set (no "closed") must be preserved — this is the read
	// side of the upstream backstop poller and only reconciles active beads.
	if strings.Contains(sawStatuses, "closed") {
		t.Errorf("ListOpenMergeRequestBeads must not request closed beads, got status=%q", sawStatuses)
	}
}

// TestListOpenMergeRequestBeads_ForwardsLabel pins that a non-empty label still
// scopes the paged walk (the upstream-backstop caller passes upstream:true so
// it reconciles only the small human-owned mirror set, not the broad dark-MR
// population).
func TestListOpenMergeRequestBeads_ForwardsLabel(t *testing.T) {
	var gotLabels string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLabels = r.URL.Query().Get("labels")
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListOpenMergeRequestBeads(context.Background(), "upstream:true"); err != nil {
		t.Fatalf("ListOpenMergeRequestBeads: %v", err)
	}
	if !strings.Contains(gotLabels, "upstream:true") {
		t.Errorf("label filter must reach the daemon, got labels=%q", gotLabels)
	}
}

// Forge rides along as the `forge` field when set, and is omitted (not blanked)
// for GitLab, whose beads never carry it.
func TestMRFieldsMap_Forge(t *testing.T) {
	if got := mrFieldsMap(MergeRequestBeadFields{MRURL: "u", Forge: "github"})["forge"]; got != "github" {
		t.Errorf("forge = %q, want github", got)
	}
	if _, ok := mrFieldsMap(MergeRequestBeadFields{MRURL: "u"})["forge"]; ok {
		t.Errorf("empty Forge must not be written")
	}
}
