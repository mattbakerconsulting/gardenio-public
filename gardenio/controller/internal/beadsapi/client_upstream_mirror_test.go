package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestUpsertUpstreamMirrorBead_GateNewBead is the E-PROV FW-5 GATE
// (ka-0000003146): a created upstream-mirror bead carries upstream:true +
// gardener-delivery-skip + external-mirror:true AND the external_mirror /
// upstream fields, is created OPEN, and the URL host is preserved verbatim so a
// dark !N and an upstream !N never collide.
//
// This pins the read-only-mirror invariant (ka-0000003234) at its single
// enforcement point: nothing else in the codebase decides which labels an
// upstream-mirror bead gets.
func TestUpsertUpstreamMirrorBead_GateNewBead(t *testing.T) {
	// A REAL-monorepo (upstream) host — deliberately NOT gitlab.com so a
	// host-collision regression would surface.
	const mrURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/777"

	var (
		mu          sync.Mutex
		createBody  map[string]json.RawMessage
		createLabel []string // labels sent on CreateBead
		addedLabels []string // labels sent via the AddLabel endpoint
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// findMergeRequestBead: no existing bead → forces the create path.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("type") != "":
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{}})

		// AddLabel: POST /v1/beads/{id}/labels.
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			body, _ := io.ReadAll(r.Body)
			var b map[string]string
			_ = json.Unmarshal(body, &b)
			mu.Lock()
			addedLabels = append(addedLabels, b["label"])
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		// CreateBead: POST /v1/beads.
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(body, &createBody)
			if raw, ok := createBody["labels"]; ok {
				_ = json.Unmarshal(raw, &createLabel)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "mr-mirror-1"})
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	id, err := c.UpsertUpstreamMirrorBead(context.Background(), MergeRequestBeadFields{
		MRURL:       mrURL,
		MRIID:       "777",
		ProjectPath: "Acme/Platform/monorepo",
		OperatedBy:  "oidc:matthew.baker@acme.example",
	})
	if err != nil {
		t.Fatalf("UpsertUpstreamMirrorBead: %v", err)
	}
	if id != "mr-mirror-1" {
		t.Fatalf("id=%q, want mr-mirror-1", id)
	}

	// --- GATE 1: created OPEN (status not closed). ---
	var status string
	if raw, ok := createBody["status"]; ok {
		_ = json.Unmarshal(raw, &status)
	}
	if status == "closed" {
		t.Errorf("upstream-mirror bead created with status=closed; want open (terminal state flows in later from the webhook/poller)")
	}

	// --- GATE 2: canonical labels present on creation. ---
	assertHasAllLabels(t, "CreateBead.labels", createLabel, UpstreamMirrorLabels)

	// --- GATE 3: labels idempotently re-applied via AddLabel (brings a
	// pre-existing webhook-created bead into compliance). ---
	assertHasAllLabels(t, "AddLabel calls", addedLabels, UpstreamMirrorLabels)

	// --- GATE 4: external_mirror / upstream fields stamped so the viewer
	// renders the node as external. ---
	var fields map[string]string
	if raw, ok := createBody["fields"]; ok {
		_ = json.Unmarshal(raw, &fields)
	}
	if fields["external_mirror"] != "true" {
		t.Errorf("external_mirror=%q, want \"true\"", fields["external_mirror"])
	}
	if fields["upstream"] != "true" {
		t.Errorf("upstream=%q, want \"true\"", fields["upstream"])
	}

	// --- GATE 5: URL host preserved verbatim (no canonicalisation that could
	// collide a dark !N with an upstream !N). ---
	if fields["mr_url"] != mrURL {
		t.Errorf("mr_url=%q, want %q (host must be preserved)", fields["mr_url"], mrURL)
	}

	// --- GATE 6: operator attribution preserved. ---
	if fields["operated_by"] != "oidc:matthew.baker@acme.example" {
		t.Errorf("operated_by=%q, want the human operator preserved", fields["operated_by"])
	}
}

// TestUpsertUpstreamMirrorBead_GateExistingBeadRelabel pins the second half of
// the GATE: when the upstream webhook already created the MR bead (un-labelled,
// no external_mirror field) BEFORE `gb mr link --upstream` runs, the mirror
// upsert must bring it into compliance — stamp external_mirror/upstream via the
// field PATCH and re-apply every canonical label via AddLabel.
func TestUpsertUpstreamMirrorBead_GateExistingBeadRelabel(t *testing.T) {
	const mrURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/888"

	var (
		mu          sync.Mutex
		patchBody   map[string]json.RawMessage
		addedLabels []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// findMergeRequestBead: an existing MR bead with NO marker labels/fields.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("type") != "":
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{{
				ID:     "mr-existing",
				Type:   "merge-request",
				Status: "open",
				Fields: json.RawMessage(`{"mr_url":"` + mrURL + `"}`),
			}}})

		// AddLabel.
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			body, _ := io.ReadAll(r.Body)
			var b map[string]string
			_ = json.Unmarshal(body, &b)
			mu.Lock()
			addedLabels = append(addedLabels, b["label"])
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		// UpdateBeadFields GETs the single bead then PATCHes it.
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{
				ID:     "mr-existing",
				Type:   "merge-request",
				Status: "open",
				Fields: json.RawMessage(`{"mr_url":"` + mrURL + `"}`),
			})
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(body, &patchBody)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	id, err := c.UpsertUpstreamMirrorBead(context.Background(), MergeRequestBeadFields{
		MRURL: mrURL,
		MRIID: "888",
	})
	if err != nil {
		t.Fatalf("UpsertUpstreamMirrorBead: %v", err)
	}
	if id != "mr-existing" {
		t.Fatalf("id=%q, want mr-existing", id)
	}

	// Field PATCH stamps the external markers.
	var fields map[string]string
	if raw, ok := patchBody["fields"]; ok {
		_ = json.Unmarshal(raw, &fields)
	}
	if fields["external_mirror"] != "true" {
		t.Errorf("PATCH external_mirror=%q, want \"true\" (existing bead brought into compliance)", fields["external_mirror"])
	}

	// Labels re-applied even though the create path never ran.
	assertHasAllLabels(t, "AddLabel calls (existing bead)", addedLabels, UpstreamMirrorLabels)
}

// assertHasAllLabels fails the test if want is not a subset of got.
func assertHasAllLabels(t *testing.T, what string, got, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, l := range got {
		set[l] = true
	}
	var missing []string
	for _, w := range want {
		if !set[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s missing labels %v; got %v", what, missing, got)
	}
}
