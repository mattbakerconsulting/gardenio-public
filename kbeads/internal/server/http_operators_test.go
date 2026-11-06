package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/store"
)

// TestIsBotOperatorRow_ScreensHumanKindBots is the unit-level pin for
// ka-0000004034: kind='oidc' is stamped onto ANY email-shaped subject, so a
// bot/service/fixture identity that carries an email passes the {oidc,slack}
// human-kind gate. isBotOperatorRow must additionally screen those by their
// objective identifier markers. Each junk row here was observed VERBATIM on the
// live /v1/operators feed; each human row is on a real, deliverable domain and
// must survive. The markers mirror the Users-CAMP harness BOT_ROW_MARKER_RE
// exactly so the page render, this feed, and the harness bot_rows_rendered
// assertion all reconcile at zero.
func TestIsBotOperatorRow_ScreensHumanKindBots(t *testing.T) {
	tests := []struct {
		name string
		row  store.OperatorDirectoryEntry
		want bool
	}{
		// --- human-kind bot/fixture rows that MUST be screened (want true) ---
		{
			name: "oidc gitlab bot service account (email-shaped subject, noreply.gitlab.com)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "group_14176856_bot_1dd8a88e50156d4a6dcf9d03e75d8d34@noreply.gitlab.com", Email: "group_14176856_bot_1dd8a88e50156d4a6dcf9d03e75d8d34@noreply.gitlab.com"},
			want: true,
		},
		{
			name: "slack pre-resync sentinel email (noreply.kbeads.local in Email, U-id in Subject)",
			row:  store.OperatorDirectoryEntry{Kind: "slack", Subject: "U0PERSON002", Email: "slack-U0PERSON002@noreply.kbeads.local"},
			want: true,
		},
		{
			name: "oidc e2e fixture (RFC 2606 example.com)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "e2e-fixture@example.com", Email: "e2e-fixture@example.com"},
			want: true,
		},
		{
			name: "github noreply sub-label (.noreply.github.com)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "12345+octocat@users.noreply.github.com", Email: "12345+octocat@users.noreply.github.com"},
			want: true,
		},
		{
			name: "bot token marker even off a noreply domain (_bot_)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "project_79511414_bot_45c427d@somewhere.example.org", Email: "project_79511414_bot_45c427d@somewhere.example.org"},
			want: true,
		},
		{
			// Email domains are case-insensitive (RFC 1035): an uppercased
			// noreply domain is the same bot domain and must screen. Pins the
			// (?i) flag so it stays in lockstep with the harness's re.I.
			name: "uppercased noreply domain still screens (case-insensitive)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "Group_Bot@NOREPLY.GitLab.com", Email: "Group_Bot@NOREPLY.GitLab.com"},
			want: true,
		},

		// --- real humans on real domains that MUST survive (want false) ---
		{
			name: "oidc human on acme.example",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "matthew.baker@acme.example", Email: "Matthew.Baker@acme.example", Display: "Matt Baker"},
			want: false,
		},
		{
			name: "oidc human on a vendor domain (contractor-c.example)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "devin.hale@contractor-c.example", Email: "devin.hale@contractor-c.example", Display: "Devin Hale"},
			want: false,
		},
		{
			name: "slack human with a resolved real email (bare U-id subject, no marker)",
			row:  store.OperatorDirectoryEntry{Kind: "slack", Subject: "U0OPERATOR1", Email: "Matthew.Baker@acme.example", Display: "Matt Baker"},
			want: false,
		},
		{
			// DELIBERATELY NOT screened: a service account on a REAL domain
			// carries no objective non-human address marker. The harness makes
			// the same exclusion (its BOT_ROW_MARKER_RE); flagging it here is a
			// judge curation call, not a feed-level correctness assertion.
			name: "service account on a real domain is NOT an objective bot marker",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "info-density-audit@acme.example", Email: "info-density-audit@acme.example"},
			want: false,
		},

		// --- sentinel KINDS are out of scope: never screened here (want false) ---
		// /fleet renders these on purpose and they legitimately carry
		// @noreply.kbeads.local sentinel emails. The store query already excludes
		// them from /people via the slug filter; the marker screen must not also
		// claim them, or a future consumer of a sentinel kind loses its rows.
		{
			name: "system sentinel with noreply email is NOT screened (wrong kind)",
			row:  store.OperatorDirectoryEntry{Kind: "system", Subject: "scheduler", Email: "system-scheduler@noreply.kbeads.local"},
			want: false,
		},
		{
			name: "fleet sentinel with noreply email is NOT screened (wrong kind)",
			row:  store.OperatorDirectoryEntry{Kind: "fleet", Subject: "slack-bridge", Email: "fleet-slack-bridge@noreply.kbeads.local"},
			want: false,
		},
		{
			name: "gitlab bot sentinel with _bot_ token is NOT screened (wrong kind)",
			row:  store.OperatorDirectoryEntry{Kind: "gitlab", Subject: "group_14176856_bot_259bb18", Email: "gitlab-group_14176856_bot_259bb18@noreply.kbeads.local"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBotOperatorRow(tt.row); got != tt.want {
				t.Errorf("isBotOperatorRow(%+v) = %v, want %v", tt.row, got, tt.want)
			}
		})
	}
}

// TestIsServiceOperatorRow_DenyListsServiceIdentities is the unit-level pin for
// ka-0000004673: four service/CI accounts authenticate on a REAL corporate
// domain (kind='oidc', ordinary email-shaped subject), so botOperatorMarkerRe
// provably cannot reach them — the marker approach scoped this class out
// (ka-0000004034). isServiceOperatorRow deny-lists exactly those four subjects.
// The failure mode to guard against is a false positive on a real employee, so
// every real-human case here shares a domain (and, for joel.*, a local-part
// prefix) with a denied service subject and MUST survive.
func TestIsServiceOperatorRow_DenyListsServiceIdentities(t *testing.T) {
	tests := []struct {
		name string
		row  store.OperatorDirectoryEntry
		want bool
	}{
		// --- the 4 service/CI subjects that MUST be denied (want true) ---
		{
			name: "gardenio fleet identity (subject and email)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "gardenio@acme.example", Email: "gardenio@acme.example"},
			want: true,
		},
		{
			name: "gardenio2 second Jira API-token identity",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "gardenio2@acme.example", Email: "gardenio2@acme.example"},
			want: true,
		},
		{
			name: "info-density-audit OIDC claim (acme.example)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "info-density-audit@acme.example", Email: "info-density-audit@acme.example"},
			want: true,
		},
		{
			name: "joel.approver CI approval fixture",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "joel.approver@acme.example", Email: "joel.approver@acme.example"},
			want: true,
		},
		{
			// Email domains are case-insensitive (RFC 1035): an uppercased
			// service address is the same identity and must still deny.
			name: "uppercased service subject still denies (case-insensitive)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "Gardenio2@acme.example", Email: "Gardenio2@acme.example"},
			want: true,
		},
		{
			// The service address may sit in Email while Subject is something
			// else; screening both fields catches it either way.
			name: "service address in Email only still denies",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "some-old-subject", Email: "gardenio@acme.example"},
			want: true,
		},

		// --- real humans that share a domain/prefix and MUST survive (want false) ---
		{
			// jordan.lake@ is a DISTINCT real human from the denied joel.approver@
			// fixture — the deny-list is exact-subject, never a prefix pattern.
			name: "jordan.lake is a real human on the same domain (not the approver fixture)",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "jordan.lake@acme.example", Email: "jordan.lake@acme.example", Display: "Jordan Lake"},
			want: false,
		},
		{
			name: "real employee on acme.example survives",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "matthew.baker@acme.example", Email: "Matthew.Baker@acme.example", Display: "Matt Baker"},
			want: false,
		},
		{
			name: "real employee on acme.example (info-density-audit's domain) survives",
			row:  store.OperatorDirectoryEntry{Kind: "oidc", Subject: "val.reyes@acme.example", Email: "val.reyes@acme.example", Display: "Val Reyes"},
			want: false,
		},

		// --- sentinel KINDS are out of scope: never screened here (want false) ---
		{
			name: "system sentinel is not screened (wrong kind) even if subject matched",
			row:  store.OperatorDirectoryEntry{Kind: "system", Subject: "gardenio@acme.example", Email: "gardenio@acme.example"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isServiceOperatorRow(tt.row); got != tt.want {
				t.Errorf("isServiceOperatorRow(%+v) = %v, want %v", tt.row, got, tt.want)
			}
		})
	}
}

// TestHandleListOperators_ScreensServiceIdentities is the handler-level pin for
// ka-0000004673: the GET /v1/operators feed itself must drop the four
// marker-invisible service/CI identities so /people AND its harness ground-truth
// query (both read this feed) stay reconciled and humans-only. A real employee
// on the same domain, and the distinct human jordan.lake@ that shares a prefix
// with the joel.approver@ fixture, must both survive.
func TestHandleListOperators_ScreensServiceIdentities(t *testing.T) {
	_, ms, h := newTestServer()
	// Real humans on the same domains as the service accounts.
	seedOperator(ms, 1, "oidc", "matthew.baker@acme.example", "Matt Baker", "matthew.baker@acme.example", "matthew.baker", false)
	seedOperator(ms, 2, "oidc", "jordan.lake@acme.example", "Jordan Lake", "jordan.lake@acme.example", "jordan.lake", false)
	// The four service/CI identities — kind=oidc, real domain, slugged, so ONLY
	// the deny-list keeps them off the feed (the slug filter and marker regex
	// both let them through).
	seedOperator(ms, 3, "oidc", "gardenio@acme.example", "", "gardenio@acme.example", "gardenio", false)
	seedOperator(ms, 4, "oidc", "gardenio2@acme.example", "", "gardenio2@acme.example", "gardenio2", false)
	seedOperator(ms, 5, "oidc", "info-density-audit@acme.example", "", "info-density-audit@acme.example", "info-density-audit", false)
	seedOperator(ms, 6, "oidc", "joel.approver@acme.example", "", "joel.approver@acme.example", "joel.approver", false)

	rec := doJSON(t, h, http.MethodGet, "/v1/operators?limit=500", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ListOperatorsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	gotSlugs := map[string]bool{}
	for _, op := range resp.Operators {
		gotSlugs[op.Slug] = true
	}
	for _, want := range []string{"matthew.baker", "jordan.lake"} {
		if !gotSlugs[want] {
			t.Errorf("expected human slug %q on the feed; got %+v", want, resp.Operators)
		}
	}
	for _, screened := range []string{"gardenio", "gardenio2", "info-density-audit", "joel.approver"} {
		if gotSlugs[screened] {
			t.Errorf("service slug %q must NOT be on the feed; got %+v", screened, resp.Operators)
		}
	}
	if len(resp.Operators) != 2 {
		t.Errorf("feed len = %d, want 2 (humans only); got %+v", len(resp.Operators), resp.Operators)
	}
}

// TestHandleListOperators_ScreensBotRows is the handler-level pin: the
// GET /v1/operators feed itself must drop human-kind bot/fixture rows so the
// gardenio /people directory AND its harness ground-truth query (which both read
// this feed) stay consistent — a viewer-only screen would make the two diverge
// and false-fail Coverage's row_vs_truth_delta (ka-0000004034).
func TestHandleListOperators_ScreensBotRows(t *testing.T) {
	_, ms, h := newTestServer()
	// Two real humans.
	seedOperator(ms, 1, "oidc", "matthew.baker@acme.example", "Matt Baker", "matthew.baker@acme.example", "matthew.baker", false)
	seedOperator(ms, 2, "slack", "U0OPERATOR1", "Devin Hale", "devin.hale@contractor-c.example", "devin.hale", false)
	// Bot/fixture rows that are kind=oidc/slack and slugged, so only the marker
	// screen keeps them off the feed (the slug filter alone lets them through).
	seedOperator(ms, 3, "oidc", "group_14176856_bot_1dd8@noreply.gitlab.com", "", "group_14176856_bot_1dd8@noreply.gitlab.com", "op-3", false)
	seedOperator(ms, 4, "oidc", "e2e-fixture@example.com", "", "e2e-fixture@example.com", "op-4", false)
	seedOperator(ms, 5, "slack", "U0PERSON002", "", "slack-U0PERSON002@noreply.kbeads.local", "op-5", false)

	rec := doJSON(t, h, http.MethodGet, "/v1/operators?limit=500", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ListOperatorsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	gotSlugs := map[string]bool{}
	for _, op := range resp.Operators {
		gotSlugs[op.Slug] = true
	}
	for _, want := range []string{"matthew.baker", "devin.hale"} {
		if !gotSlugs[want] {
			t.Errorf("expected human slug %q on the feed; got %+v", want, resp.Operators)
		}
	}
	for _, screened := range []string{"op-3", "op-4", "op-5"} {
		if gotSlugs[screened] {
			t.Errorf("bot/fixture slug %q must NOT be on the feed; got %+v", screened, resp.Operators)
		}
	}
	if len(resp.Operators) != 2 {
		t.Errorf("feed len = %d, want 2 (humans only); got %+v", len(resp.Operators), resp.Operators)
	}
}

// TestHandleListOperators_LimitClampedAtStore is the regression pin for the
// /v1/operators OOM-clamp sibling class (ka-0000006889 / ka-0000004753).
// handleListOperators is RoleViewer-gated (http.go: GET /v1/operators) and
// powers the public /people directory. Before the fix it parsed ?limit= with
// only an n<0 lower guard and passed the value straight into the store's
// `LIMIT $1`, so a low-privilege caller could ask for limit=2000000000 and
// drive the single shared kbeads daemon toward OOM — exactly the class fixed
// for gRPC ListBeads (ka-0000003195 / !7128), the graph family
// (ka-0000007023 / !7150), and audit/decisions/escalations
// (ka-0000001252 / !7164).
//
// The load-bearing assertion is on the limit the STORE receives
// (ms.operatorLimits) — that is what bounds the SQL, not the handler's local
// variable. Fault injection: revert the handler to `limit = n` (drop the
// clampListBeadsLimit call) and the over-cap cases fail.
func TestHandleListOperators_LimitClampedAtStore(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"omitted uses store default", "", defaultOperatorsLimit},
		{"under cap preserved", "?limit=42", 42},
		{"at cap preserved", "?limit=10000", maxListBeadsLimit},
		{"over cap clamped", "?limit=10001", maxListBeadsLimit},
		{"huge over cap clamped", "?limit=2000000000", maxListBeadsLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			rec := doJSON(t, h, http.MethodGet, "/v1/operators"+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if len(ms.operatorLimits) == 0 {
				t.Fatalf("ListOperatorsWithSlugs was not called")
			}
			if got := ms.operatorLimits[len(ms.operatorLimits)-1]; got != tc.want {
				t.Fatalf("store received limit=%d, want clamped %d", got, tc.want)
			}
		})
	}
}

// TestHandleListOperators_NegativeLimitRejected pins that a negative ?limit=
// stays a 400 (unchanged by the clamp) — the clamp only bounds the upper end,
// it does not turn an invalid input into a silent default.
func TestHandleListOperators_NegativeLimitRejected(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/operators?limit=-5", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for negative limit; body=%s", rec.Code, rec.Body.String())
	}
}
