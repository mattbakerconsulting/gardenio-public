package bridge

// Gap-2 closure for ka-0000004831: prove that no agent-attributable governed
// Jira write can bypass the per-role field allowlist via the poller back-off
// path or the @mention handler.
//
// Two facts are pinned here:
//
//  1. The poller back-off comment (jira_poller.go) is a roleless/system note
//     and is posted as an INTERNAL note, so — like every gated comment write —
//     it can never surface as a customer-facing JSM reply. It is intentionally
//     NOT routed through the AllowlistGate (there is no acting-agent role to
//     resolve); pinning the internal-visibility property is the equivalent
//     guarantee for this one fixed bridge-internal string.
//
//  2. The @mention handler reaches NO JiraClient write method at all — it only
//     spawns agents and writes gardenio-side bead rows. It is a VERIFIED non-gap,
//     asserted structurally so a future field added to the handler that wires in
//     a *JiraClient (or a Jira write verb added to its bead-client interface)
//     trips the test.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// backoffCommentBody captures the JSON body of a back-off comment POST so the
// test can assert the JSM internal-visibility property is stamped.
type backoffCommentBody struct {
	Properties []struct {
		Key   string `json:"key"`
		Value struct {
			Internal bool `json:"internal"`
		} `json:"value"`
	} `json:"properties"`
}

// TestCheckAssigneeBatch_BackoffComment_IsInternalNote proves the poller's
// human-assigned back-off comment is posted with sd.public.comment internal:true
// — i.e. an internal note, never a customer-facing reply — even though the
// comment is roleless/system and so not routed through the field allowlist
// (ka-0000004831 gap 2). PE is allow-listed, so the comment IS sent; the
// assertion is on its visibility, not its presence.
func TestCheckAssigneeBatch_BackoffComment_IsInternalNote(t *testing.T) {
	const jiraKey = "PE-4242"
	commentPath := "/rest/api/3/issue/" + jiraKey + "/comment"

	var (
		mu       sync.Mutex
		gotBody  *backoffCommentBody
		comments int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql"):
			resp := map[string]any{
				"issues": []map[string]any{{
					"key": jiraKey,
					"fields": map[string]any{
						"assignee": map[string]any{
							"displayName": "Cris Gale",
							"accountId":   "human-123",
						},
					},
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == commentPath:
			var body backoffCommentBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = &body
			comments++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	daemon := newPollerMock()
	beadID := "bd-backoff-" + jiraKey
	daemon.beads[beadID] = &beadsapi.BeadDetail{
		ID:     beadID,
		Title:  "[" + jiraKey + "] Test issue",
		Status: "open",
		Labels: []string{"source:jira", "jira:" + jiraKey},
		Fields: map[string]string{"jira_key": jiraKey},
	}

	p := &JiraPoller{
		jira:   newTestJiraClient(srv.URL),
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   map[string]string{jiraKey: beadID},
		wiredDeps: make(map[string]bool),
	}

	p.checkAssigneeBatch(context.Background(), []string{jiraKey})

	mu.Lock()
	defer mu.Unlock()
	if comments != 1 {
		t.Fatalf("expected exactly 1 back-off comment for allow-listed PE, got %d", comments)
	}
	if gotBody == nil {
		t.Fatal("back-off comment body was not captured")
	}
	var internalProp *bool
	for _, p := range gotBody.Properties {
		if p.Key == "sd.public.comment" {
			v := p.Value.Internal
			internalProp = &v
		}
	}
	if internalProp == nil {
		t.Fatal("back-off comment did not carry the sd.public.comment visibility property — it could surface as a customer-facing JSM reply")
	}
	if !*internalProp {
		t.Error("back-off comment stamped sd.public.comment internal:false (public) — must be an internal note")
	}
}

// jiraWriteMethodNames is the closed set of JiraClient mutation methods. The
// @mention path must reach NONE of them, and the no-bypass source scan
// (TestNoBypass_EveryJiraWriteIsGated) treats every one of them as a governed
// write that must sit behind a gate. Kept in sync with the governed writes
// enumerated in jira_allowlist.go (JiraWriteField) plus AddComment.
//
// SetAffectsVersions is listed AHEAD of its implementation (ka-0000003103 adds
// it to jira.go for the fsm-triage Affects-Version write): including it here now
// means the very first live caller of a versions write must be gate-preceded or
// the source scan fails — the write cannot land ungated even for one commit.
var jiraWriteMethodNames = map[string]bool{
	"AddComment":               true,
	"AddCommentWithVisibility": true,
	// The source-linked reply variant (G4b quote-link fallback, ka-0000002864):
	// same comment POST, optional anchor paragraph prepended.
	"AddCommentWithVisibilityAndSourceLink": true,
	// The markdown-rendering reply variant (H1, ka-0000004233): same comment
	// POST, body typeset from markdown with plain-text fallback.
	"AddMarkdownCommentWithVisibilityAndSourceLink": true,
	"TransitionIssue":    true,
	"AssignIssue":        true,
	"AddRemoteLink":      true,
	"AddLabels":          true,
	"SetAffectsVersions": true,
	// The GBFSM outbox intent create (epic ka-0000002067). It is a governed write
	// that can only ever create in the internal outbox — never FSM. Kept dormant
	// (no live caller) after the 1:1 reshape collapsed the writer (Phase 2
	// ka-0000001491); Phase 4 retires it. Listing it here keeps it a governed write
	// so a future re-introduction cannot land ungated.
	"CreateOutboxIntent": true,
	// The GBFSM ticket create + upsert-edit (1:1 reshape, Phase 2 ka-0000001491).
	// Both are governed writes that can only ever touch the internal GBFSM outbox
	// project (create hardcoded to it, edit targets the SearchGBFSM-verified GBFSM
	// key — jira_outbox_test.go pins both), never FSM. Their call site
	// (syncGBFSMTicket) consults the project + field gates, so the no-bypass scan
	// holds. SearchGBFSM is a READ (a scoped JQL GET), so it is deliberately NOT a
	// governed write.
	"CreateGBFSMTicket": true,
	"EditGBFSMTicket":   true,
	// The issue-attachment upload (ka-0000001730) — a governed sub-resource
	// write gated on FieldAttachment. It can only ever add a file to an issue
	// that already exists; it creates and deletes nothing.
	"AddAttachment": true,
}

// TestJiraMention_NoJiraWriteSurface is the VERIFIED-non-gap assertion for the
// @mention handler (ka-0000004831 gap 2). The mention handler performs only
// SpawnAgent + gardenio-side bead writes; it holds no *JiraClient and its bead
// client exposes no Jira mutation. This is asserted structurally so that any
// future change which wires an outbound Jira write into the mention path — a
// *JiraClient field on the handler, or a Jira write verb added to
// JiraMentionBeadClient — fails this test and forces the author to route the
// new write through the field allowlist (as jira_sync / jira_squawk_forward do).
func TestJiraMention_NoJiraWriteSurface(t *testing.T) {
	// (a) The handler struct must hold no *JiraClient (directly or as a named
	// field whose type mentions JiraClient).
	ht := reflect.TypeFor[JiraMentionHandler]()
	for i := 0; i < ht.NumField(); i++ {
		f := ht.Field(i)
		if typeMentionsJiraClient(f.Type) {
			t.Errorf("JiraMentionHandler.%s has type %s which reaches *JiraClient — "+
				"an outbound Jira write surface must not exist on the mention path "+
				"without field-allowlist gating (ka-0000004831)", f.Name, f.Type)
		}
	}

	// (b) The mention handler's only I/O collaborator interface
	// (JiraMentionBeadClient) must expose no Jira mutation method.
	it := reflect.TypeFor[JiraMentionBeadClient]()
	for i := 0; i < it.NumMethod(); i++ {
		name := it.Method(i).Name
		if jiraWriteMethodNames[name] {
			t.Errorf("JiraMentionBeadClient exposes Jira write method %q — the mention "+
				"path could bypass the field allowlist (ka-0000004831)", name)
		}
	}

	// (c) The handler config likewise carries no *JiraClient field, so the
	// wiring in buildMentionHandler (main.go) cannot inject one.
	ct := reflect.TypeFor[JiraMentionHandlerConfig]()
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if typeMentionsJiraClient(f.Type) {
			t.Errorf("JiraMentionHandlerConfig.%s has type %s which reaches *JiraClient — "+
				"the mention path must not be wired with an outbound Jira write surface "+
				"(ka-0000004831)", f.Name, f.Type)
		}
	}
}

// typeMentionsJiraClient reports whether t is, points at, or is a slice/map of a
// JiraClient — a cheap structural check that catches the obvious ways a Jira
// write surface could be smuggled onto the mention handler.
func typeMentionsJiraClient(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeMentionsJiraClient(t.Elem())
	case reflect.Map:
		return typeMentionsJiraClient(t.Elem()) || typeMentionsJiraClient(t.Key())
	default:
		return t.Name() == "JiraClient"
	}
}

// ── Source-level no-bypass scan (gd-yOllH26pFY coverage area 2) ──
//
// The reflection tests above prove the mention path holds no write surface. The
// scan below proves the COMPLEMENT for the three agent-driven write paths plus
// the poller: every JiraClient mutation call in jira_sync.go /
// jira_squawk_forward.go / jira_mentions.go / jira_poller.go sits inside a
// function whose body ALSO consults a write gate. It parses the real source
// (not a mock), so a future edit that adds an ungated write — including the
// first live AddLabels / SetAffectsVersions caller landed by ka-0000003103 —
// fails this test rather than silently shipping a bypass before the prod enforce
// flip.
//
// Why "same enclosing function" rather than "immediately preceded": the four
// handlers gate ONCE per event (project gate + scope + role resolve) then make
// several writes under nested field-allowlist guards; requiring the gate token
// in the same func body is the tightest check that is robust to that shape and
// to the pending call-site swap (gd-pvF70DCDTS) — it matches the legacy
// AllowField/AllowComment/AllowProject AND the WritePolicyStore's identically
// named methods, so it stays green across the swap.

// noBypassFiles are the outbound-write source files whose every JiraClient
// mutation must be gate-guarded. jira_mentions.go is included even though it
// reaches no write today (the reflection test proves that) — if a write is ever
// added there, the scan demands a gate alongside it. jira_reply.go is the
// agent-initiated reply endpoint (feature F2, plan ka-0000007401), added to the
// scan by the F5 guardrails spine (gd-LY6qQ2vEhQ).
var noBypassFiles = []string{
	"jira_sync.go",
	"jira_squawk_forward.go",
	"jira_mentions.go",
	"jira_poller.go",
	"jira_reply.go",
	// jira_spawn_ack.go is the mention spawn-acknowledgment path (Slack
	// agent-card parity, plan ka-0000007401): its AddCommentWithVisibility call
	// sits inside Acknowledge, which consults AllowReplyProject + AllowReplyComment
	// on the shared write policy. Included here so a future edit that adds an
	// ungated write to the ack path fails the scan.
	"jira_spawn_ack.go",
	// jira_completion_ack.go is the mention completion/failure-acknowledgment
	// path (Slack reaction-lifecycle back-half, GAP 4 / ka-0000004899): its
	// AddCommentWithVisibility call sits inside handleClosed, which consults
	// AllowReplyProject + AllowReplyComment on the shared write policy. Included
	// so a future edit that adds an ungated write to the completion path fails
	// the scan.
	"jira_completion_ack.go",
}

// gateTokens are the identifiers that count as "a write gate was consulted" in a
// function body. Deliberately broad across BOTH the legacy gates and the unified
// WritePolicyStore (ka-0000007252 / gd-pvF70DCDTS) so the scan is invariant to
// the call-site swap, plus the sync path's thin wrappers and the scope guard:
//
//	AllowField / AllowComment / AllowProject — legacy AllowlistGate + ProjectSyncGate
//	                                            AND WritePolicyStore (same names)
//	allowField / allowComment / allowProject — JiraSync's nil-safe wrappers
//	inScope / jiraWriteInScope               — the bead→ticket scope guard
//	resolveReplyTarget                       — the reply endpoint's gate-consulting
//	                                            resolver (jira_reply.go)
//
// allowProject is the true gate on the GBFSM outbox write path: syncGBFSMTicket
// guards its create/edit on s.allowProject(outboxProjectKey) (the outbox toggle),
// having delegated ticket assembly + the field-level allowField gating to the
// pure builder buildGBFSMTicket (no write there). A function that makes a governed
// write but references NONE of these is an ungated write path — the bypass this
// test forbids.
//
// resolveReplyTarget is the reply-path analog of the sync wrappers: the write in
// handlePostReply is reachable only after resolveReplyTarget returns success,
// and that helper's own body holds the bound-ticket scope check plus the
// AllowReplyProject + AllowReplyComment verdicts (the spawn-predicate variants of
// the project/comment gates, so an internal reply is not coupled to the outbox
// toggle — ka-0000007628 — while FSM/unknown and public replies stay denied).
// Counting it as a gate token would be hollow if the helper stopped gating, so
// TestNoBypass_ReplyResolverActuallyGates pins that its body references both
// store verdicts.
var gateTokens = []string{
	"AllowField", "AllowComment", "AllowProject",
	"AllowReplyComment", "AllowReplyProject",
	"allowField", "allowComment", "allowProject",
	"inScope", "jiraWriteInScope",
	"resolveReplyTarget",
}

// enclosingWriteCall records one JiraClient mutation call found in the source.
type enclosingWriteCall struct {
	file   string
	fn     string
	method string
	line   int
}

// TestNoBypass_EveryJiraWriteIsGated parses the four outbound source files and
// asserts every JiraClient mutation call sits in a function whose body also
// consults a write gate. Fails closed: if a file cannot be parsed, or a write is
// found in a gate-less function, the test errors.
func TestNoBypass_EveryJiraWriteIsGated(t *testing.T) {
	fset := token.NewFileSet()

	var (
		writes    []enclosingWriteCall
		anyWrite  bool
		anyGated  = map[string]bool{} // file → saw at least one gate token (sanity)
		perFileFn = map[string]int{}  // "file:fn" → 1 (dedup fns with writes)
	)

	for _, file := range noBypassFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		// Whole-function gate presence is cheap to compute per FuncDecl via a
		// subtree walk, so we resolve each write's enclosing function and check
		// that function's body for a gate token.
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fnName := fd.Name.Name
			bodyHasGate := funcBodyReferencesAny(fd.Body, gateTokens)
			if bodyHasGate {
				anyGated[file] = true
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				method := sel.Sel.Name
				if !jiraWriteMethodNames[method] {
					return true
				}
				// The receiver must be the JiraClient field on the handler
				// (s.jira / f.jira / p.jira), not some same-named method on
				// another type. Match "<recv>.jira.<Method>".
				recv, ok := sel.X.(*ast.SelectorExpr)
				if !ok || recv.Sel.Name != "jira" {
					return true
				}
				anyWrite = true
				key := file + ":" + fnName
				if perFileFn[key] == 0 {
					perFileFn[key] = 1
				}
				pos := fset.Position(call.Pos())
				w := enclosingWriteCall{file: file, fn: fnName, method: method, line: pos.Line}
				writes = append(writes, w)
				if !bodyHasGate {
					t.Errorf("UNGATED WRITE: %s:%d %s.jira.%s(...) is inside %s() which "+
						"consults no write gate (%v) — every governed Jira write must be "+
						"gate-guarded before the prod enforce flip (gd-yOllH26pFY)",
						file, pos.Line, "<recv>", method, fnName, gateTokens)
				}
				return true
			})
		}
	}

	// Sanity floor: the scan must have SEEN writes and gates, else a refactor
	// that renamed the JiraClient field to something other than "jira" would
	// make this test vacuously pass while silently no longer checking anything.
	if !anyWrite {
		t.Fatal("no JiraClient .jira.<Write>() calls found in any of the four files — " +
			"the scan matched nothing; the receiver-field convention likely changed " +
			"and this guard has gone blind")
	}
	// Every file that CONTAINS a write must also contain a gate token (proven
	// per-write above, but assert the aggregate too for a clear failure).
	seenWriteFiles := map[string]bool{}
	for _, w := range writes {
		seenWriteFiles[w.file] = true
	}
	for file := range seenWriteFiles {
		if !anyGated[file] {
			t.Errorf("%s makes JiraClient writes but references no gate token anywhere — "+
				"the whole file is an ungated write path", file)
		}
	}

	// Surface the coverage for debugging drift (-v). Sorted for stable output.
	sort.Slice(writes, func(i, j int) bool {
		if writes[i].file != writes[j].file {
			return writes[i].file < writes[j].file
		}
		return writes[i].line < writes[j].line
	})
	for _, w := range writes {
		t.Logf("gated write: %s:%d %s() -> %s", w.file, w.line, w.fn, w.method)
	}
}

// funcBodyReferencesAny reports whether any identifier or selector in body
// matches one of tokens. Used to decide whether a function consults a gate.
func funcBodyReferencesAny(body *ast.BlockStmt, tokens []string) bool {
	want := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		want[tok] = true
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch id := n.(type) {
		case *ast.Ident:
			if want[id.Name] {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if want[id.Sel.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// TestNoBypass_ReplyResolverActuallyGates keeps the resolveReplyTarget gate
// token honest: counting a helper as "a gate was consulted" is only sound while
// the helper itself consults the write-policy store. This parses jira_reply.go
// and asserts resolveReplyTarget's body references BOTH reply-scoped verdicts,
// AllowReplyProject and AllowReplyComment — if a refactor hollowed the resolver
// out, the token would go with it and the main scan would flag handlePostReply's
// write as ungated. (The reply path uses the spawn-predicate variants of the
// project/comment gates so an internal reply is not coupled to the outbox
// toggle — ka-0000007628 — but it is no less gated: FSM/unknown projects and
// public replies stay denied.)
func TestNoBypass_ReplyResolverActuallyGates(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "jira_reply.go", nil, 0)
	if err != nil {
		t.Fatalf("parse jira_reply.go: %v", err)
	}
	found := false
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "resolveReplyTarget" || fd.Body == nil {
			continue
		}
		found = true
		for _, gate := range []string{"AllowReplyProject", "AllowReplyComment"} {
			if !funcBodyReferencesAny(fd.Body, []string{gate}) {
				t.Errorf("resolveReplyTarget no longer consults %s — the reply write in "+
					"handlePostReply would be ungated (F5 spine, gd-LY6qQ2vEhQ)", gate)
			}
		}
	}
	if !found {
		t.Fatal("resolveReplyTarget not found in jira_reply.go — the gate token in " +
			"gateTokens is stale; re-point the no-bypass scan at the reply path's real gate")
	}
}

// TestNoBypass_WriteMethodSetMatchesClient guards the jiraWriteMethodNames set
// against drift from the real JiraClient: every name in the set that is ALREADY
// implemented must exist as a *JiraClient method, and every mutating *JiraClient
// method (POST/PUT/DELETE surface) should be represented in the set. This keeps
// the no-bypass scan honest — a new write method added to jira.go without being
// added here would otherwise slip past the scan unseen.
//
// SetAffectsVersions is exempt from the "must exist" half until ka-0000003103
// implements it (it is intentionally listed ahead of its implementation); the
// exemption is removed automatically once the method exists.
func TestNoBypass_WriteMethodSetMatchesClient(t *testing.T) {
	ct := reflect.TypeOf(&JiraClient{})
	has := func(name string) bool {
		_, ok := ct.MethodByName(name)
		return ok
	}

	// Half 1: every implemented name in the set is a real *JiraClient method.
	pendingUnimplemented := map[string]string{
		"SetAffectsVersions": "ka-0000003103",
	}
	for name := range jiraWriteMethodNames {
		if has(name) {
			continue
		}
		if bead, pending := pendingUnimplemented[name]; pending {
			t.Logf("PENDING: %s not yet on *JiraClient (arrives with %s); "+
				"listed ahead so its first caller cannot be ungated", name, bead)
			continue
		}
		t.Errorf("jiraWriteMethodNames lists %q but *JiraClient has no such method "+
			"(drift: either implement it or remove it from the set)", name)
	}

	// Half 2: the known mutating methods on *JiraClient are all represented in
	// the set, so the scan cannot miss one. This is the closed-universe anchor
	// for the write surface — mirrors knownJiraWriteFields for the fields.
	mustBeGoverned := []string{
		"AddComment", "AddCommentWithVisibility", "TransitionIssue",
		"AssignIssue", "AddRemoteLink", "AddLabels", "CreateOutboxIntent",
		"CreateGBFSMTicket", "EditGBFSMTicket",
	}
	for _, name := range mustBeGoverned {
		if !has(name) {
			t.Errorf("expected *JiraClient to implement mutating method %q", name)
		}
		if !jiraWriteMethodNames[name] {
			t.Errorf("mutating method %q is missing from jiraWriteMethodNames — "+
				"the no-bypass scan would not treat it as a governed write", name)
		}
	}
}

// TestNoBypass_ScannerHasTeeth is the meta-test that proves the scan above is
// not vacuous: it runs the same detection logic over two synthetic source
// snippets — one with an ungated write, one with a gated write — and asserts the
// detector flags the former and clears the latter. Without this, a bug that made
// funcBodyReferencesAny always-true (or the write-matcher always-false) would
// let TestNoBypass_EveryJiraWriteIsGated pass while checking nothing.
func TestNoBypass_ScannerHasTeeth(t *testing.T) {
	// detect returns true if src contains a .jira.<Write>() call inside a func
	// whose body has NO gate token — i.e. the bypass condition the real scan
	// errors on. Mirrors the real scan's matching logic exactly.
	detect := func(src string) bool {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
		if err != nil {
			t.Fatalf("parse synthetic: %v", err)
		}
		bypass := false
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			hasGate := funcBodyReferencesAny(fd.Body, gateTokens)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !jiraWriteMethodNames[sel.Sel.Name] {
					return true
				}
				recv, ok := sel.X.(*ast.SelectorExpr)
				if !ok || recv.Sel.Name != "jira" {
					return true
				}
				if !hasGate {
					bypass = true
				}
				return true
			})
		}
		return bypass
	}

	ungated := `package p
func (s *T) bad(ctx C) {
	_ = s.jira.AddLabels(ctx, "FSM-1", []string{"gardenio-triaged"})
}`
	gated := `package p
func (s *T) good(ctx C) {
	if s.gate.AllowField("FSM-1", role, FieldLabels) {
		_ = s.jira.AddLabels(ctx, "FSM-1", []string{"gardenio-triaged"})
	}
}`
	noWrite := `package p
func (s *T) neutral(ctx C) {
	_ = s.jira.GetIssue(ctx, "FSM-1")
}`

	if !detect(ungated) {
		t.Error("scanner failed to flag an UNGATED .jira.AddLabels() write — the guard has no teeth")
	}
	if detect(gated) {
		t.Error("scanner false-positived on a GATED write (AllowField present in body)")
	}
	if detect(noWrite) {
		t.Error("scanner false-positived on a non-mutating .jira.GetIssue() read")
	}
}
