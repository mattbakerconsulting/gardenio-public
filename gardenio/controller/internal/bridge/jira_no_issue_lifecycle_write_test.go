package bridge

// Regression guard: the jira-bridge exposes NO create-issue or delete-issue
// write path on any CUSTOMER-FACING project (ka-0000002073, refining E2E gate
// finding gd-6kEE6Ht5bg). The ONE create path — CreateOutboxIntent — is
// project-hardcoded to the internal GBFSM outbox and can never target FSM
// (epic ka-0000002067); its GBFSM-only guarantee is pinned in jira_outbox_test.go.
//
// # Why this test exists
//
// gardenio2's Jira-native FSM permissions are PERMISSIVE — the public JSM desk
// has CREATE_ISSUES=true and DELETE_ISSUES=true — so the Jira permission floor
// does NOT block creating or deleting issues. The real control is the BRIDGE,
// and it is a control BY CONSTRUCTION: jira.go's JiraClient surface has no
// CreateIssue / DeleteIssue method and reaches no issue-create or issue-delete
// REST endpoint at all, and the ONLY create anywhere in the bridge
// (CreateOutboxIntent, in jira_outbox.go) hardcodes the GBFSM project id — a
// crafted target key cannot redirect it onto a customer ticket.
//
// That "by construction" guarantee is invisible and therefore fragile: a future
// change could add a CreateIssue method (or a raw POST to /rest/api/3/issue, or
// a DELETE verb) without any test noticing, silently opening a write surface the
// permissive Jira floor would happily accept. This file makes the absence a
// pinned, enforced invariant across four independent layers, each of which fails
// closed if the create/delete surface ever appears:
//
//	1. METHOD-SET DRIFT — every exported *JiraClient method is classified as a
//	   read or a governed write; a NEW method trips the test until a human
//	   classifies it, so CreateIssue/DeleteIssue cannot be added unnoticed.
//	2. LIFECYCLE-VERB — no *JiraClient method name denotes creating or deleting
//	   an issue (CreateIssue, DeleteIssue, NewIssue, RemoveIssue, …).
//	3. FIELD-ALLOWLIST — "create" and "delete" are UNKNOWN governed-write fields
//	   that fail closed through the real WritePolicyStore for every role, so even
//	   if such a verb were wired it could not pass the op allowlist.
//	4. REST-SURFACE SOURCE SCAN — jira.go uses no http.MethodDelete and contains
//	   no bare issue-collection ("create issue") endpoint literal.
//
// A companion teeth meta-test proves each detector actually fires on a synthetic
// violation, so none of the guards is vacuous.
//
// This is a pure unit/interface test: it makes no live-Jira call and does not
// require the sandbox bridge (intentionally at replicas=0 pending gd-Ajyxfefw3W).
// It is CI-runnable offline.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── Method classification: the closed universe of the outbound Jira surface ──

// jiraClientMethodClass labels one exported *JiraClient method as either a
// read/query method or a governed write. This is the single source of truth for
// "what the outbound Jira surface consists of"; the drift guard below asserts it
// stays exactly in sync with the real type, so a method added to jira.go — most
// importantly a CreateIssue/DeleteIssue — cannot appear without a human
// consciously classifying it here (and, for a create/delete verb, tripping the
// lifecycle-verb guard on the way).
type jiraClientMethodClass int

const (
	classRead          jiraClientMethodClass = iota // GET/query surface — never mutates an issue
	classGovernedWrite                              // one of the gated mutation verbs
)

// jiraClientMethods pins the COMPLETE exported method set of *JiraClient as it
// exists today, each classified. The seven governed writes match
// jiraWriteMethodNames (jira_field_gate_nobypass_test.go) 1:1; the reads are the
// GET/query/download surface. Crucially, NONE is a create-issue or delete-issue
// verb — that is the invariant TestNoIssueLifecycleWriteMethod enforces.
//
// SetAffectsVersions is present on main; it is a governed write (the additive
// Affects-Version PUT), not an issue-lifecycle verb.
var jiraClientMethods = map[string]jiraClientMethodClass{
	// Reads / queries — no mutation.
	"SearchIssues":     classRead,
	"SearchIssuesPage": classRead,
	"ApproximateCount": classRead,
	"GetIssue":         classRead,
	"GetRemoteLinks":   classRead,
	// GetComments fetches recent issue comments for spawn/nudge prompt history
	// (F4, plan ka-0000007401) — a pure GET, no mutation.
	"GetComments":        classRead,
	"DownloadAttachment": classRead,
	"ListFields":         classRead,
	// SearchGBFSM is the read half of the GBFSM upsert (a scoped JQL GET — the read
	// that decides create-vs-edit; 1:1 reshape ka-0000001491). It never mutates.
	"SearchGBFSM": classRead,
	// BrowseURL builds the human-facing /browse/<key> URL string for display in a
	// GBFSM ticket Description (the GBFSM→FSM clickable link, Phase 3 ka-0000001070).
	// It is a pure string builder — it issues NO HTTP request and mutates nothing —
	// so it is a read, and cannot be an issue-lifecycle write surface.
	"BrowseURL": classRead,
	// ReadOnly reports whether the client fails writes closed (the dark-Jira
	// write-token fence, client layer — ka-0000005242). A pure bool getter: it
	// issues NO HTTP request and mutates nothing, so it is a read.
	"ReadOnly": classRead,
	// Governed writes — each gated by the field allowlist (see the no-bypass
	// scan in jira_field_gate_nobypass_test.go). None creates or deletes an issue.
	"TransitionIssue":          classGovernedWrite, // status change
	"AddComment":               classGovernedWrite, // comment (default visibility)
	"AddCommentWithVisibility": classGovernedWrite, // comment (internal/public)
	// The source-linked reply comment (G4b quote-link fallback, ka-0000002864):
	// the same visibility-stamped comment POST with an optional anchor paragraph
	// prepended — a comment sub-resource write, not an issue-lifecycle verb.
	"AddCommentWithVisibilityAndSourceLink": classGovernedWrite,
	// The markdown-rendering reply variant (H1, ka-0000004233): the same
	// visibility-stamped, optionally source-linked comment POST with the body
	// typeset from markdown to rich ADF (plain-text fallback on unmappable
	// input) — a comment sub-resource write, not an issue-lifecycle verb.
	"AddMarkdownCommentWithVisibilityAndSourceLink": classGovernedWrite,
	"AddRemoteLink":      classGovernedWrite, // e.g. GitLab MR link
	"AddLabels":          classGovernedWrite, // additive label PUT
	"SetAffectsVersions": classGovernedWrite, // additive Affects-Version PUT
	"AssignIssue":        classGovernedWrite, // assignee (self-only in practice)
	// AddAttachment (ka-0000001730) uploads screenshot bytes to an EXISTING
	// issue so an ADF media node can render them inline — the only way to embed
	// an image in Jira, since the viewer's /beads/image/ proxy is OIDC-gated and
	// Jira's renderer fetches anonymously. It is a SUB-RESOURCE write (POST
	// .../issue/{key}/attachments) in the same family as AddComment/AddLabels:
	// it requires the issue to already exist, creates nothing, and deletes
	// nothing, so the zero-FSM-create invariant (epic ka-0000002067) is
	// untouched. Gated on its own FieldAttachment rather than folded into
	// FieldComment, because attaching durable downloadable bytes is a materially
	// different act from writing text.
	"AddAttachment": classGovernedWrite,
	// The GBFSM writes in the whole bridge — the create is hardcoded to the internal
	// GBFSM outbox project, the upsert-edit targets only a SearchGBFSM-verified GBFSM
	// key, and the dormant intent create is likewise GBFSM-hardcoded (epic
	// ka-0000002067; jira_outbox_test.go pins the GBFSM-only guarantee for all). Each
	// is a governed, gated write that can NEVER target FSM or a customer-facing
	// project, and none is an "*Issue" lifecycle verb. CreateGBFSMTicket /
	// EditGBFSMTicket are the ONE-ticket-per-FSM-ticket writer (1:1 reshape,
	// ka-0000001491); CreateOutboxIntent is retained dormant until Phase 4.
	"CreateOutboxIntent": classGovernedWrite,
	"CreateGBFSMTicket":  classGovernedWrite,
	"EditGBFSMTicket":    classGovernedWrite,
}

// issueLifecycleVerbRE matches a method (or op) name that denotes CREATING or
// DELETING the issue itself — the surface the permissive Jira floor would accept
// and that the bridge must never expose. It deliberately targets issue-lifecycle
// verbs (create/delete/new/remove/destroy/purge/archive + "issue") and NOT the
// sub-resource writes the bridge legitimately makes: AddComment/AddLabels/
// AddRemoteLink (an "add", but of a sub-resource, no "issue" token), AssignIssue
// and TransitionIssue ("issue" present but the verb is assign/transition, not
// create/delete), GetIssue/SearchIssues (read verbs). Case-insensitive and
// substring (not anchored) so BulkCreateIssues / DeleteIssueByKey also match.
var issueLifecycleVerbRE = regexp.MustCompile(`(?i)(create|delete|destroy|purge|remove|archive|new)issue`)

// TestJiraClientMethodSet_NoDrift asserts jiraClientMethods stays byte-for-byte
// in sync with the real exported method set of *JiraClient. A method ADDED to
// jira.go (e.g. a future CreateIssue/DeleteIssue) that is not classified here
// fails this test — forcing a human to classify it, at which point the
// lifecycle-verb guard rejects a create/delete verb. A method REMOVED that is
// still listed here also fails, keeping the pin honest.
func TestJiraClientMethodSet_NoDrift(t *testing.T) {
	ct := reflect.TypeOf(&JiraClient{})

	live := make(map[string]bool, ct.NumMethod())
	for i := 0; i < ct.NumMethod(); i++ {
		live[ct.Method(i).Name] = true
	}

	// (a) Every live exported method must be classified.
	for name := range live {
		if _, ok := jiraClientMethods[name]; !ok {
			t.Errorf("*JiraClient has exported method %q that is not classified in "+
				"jiraClientMethods — classify it as a read or governed write. If it is a "+
				"create-issue or delete-issue write path, it violates the read-only-mirror "+
				"contract (advice ka-0000003234) and must not exist (ka-0000002073).", name)
		}
	}
	// (b) Every classified method must still exist on the type (no stale pins).
	for name := range jiraClientMethods {
		if !live[name] {
			t.Errorf("jiraClientMethods pins %q but *JiraClient has no such exported "+
				"method — remove the stale entry so the drift guard stays honest.", name)
		}
	}

	// Surface the classified surface for debugging drift (-v), sorted for stability.
	names := make([]string, 0, len(jiraClientMethods))
	for n := range jiraClientMethods {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("classified *JiraClient surface (%d methods): %s", len(names), strings.Join(names, ", "))
}

// TestNoIssueLifecycleWriteMethod is the core assertion: the outbound Jira
// surface exposes NO create-issue or delete-issue method. It checks BOTH the
// live reflected method set AND the classification pin, so the guarantee holds
// even if the two were ever briefly out of sync. Because the permissive Jira FSM
// floor would accept a create/delete, the ONLY thing preventing it is this
// absence — this test makes the absence enforced rather than incidental.
func TestNoIssueLifecycleWriteMethod(t *testing.T) {
	ct := reflect.TypeOf(&JiraClient{})
	for i := 0; i < ct.NumMethod(); i++ {
		name := ct.Method(i).Name
		if issueLifecycleVerbRE.MatchString(name) {
			t.Errorf("*JiraClient exposes method %q which denotes creating/deleting an "+
				"ISSUE — the bridge must expose no issue-create/delete write path; the "+
				"permissive Jira floor (CREATE_ISSUES/DELETE_ISSUES=true) makes the bridge "+
				"the only control (ka-0000002073 / gd-6kEE6Ht5bg).", name)
		}
	}
	// Belt-and-suspenders: the classification pin must contain no such verb either,
	// and no classified GOVERNED WRITE may be an issue-lifecycle verb.
	for name, class := range jiraClientMethods {
		if issueLifecycleVerbRE.MatchString(name) {
			t.Errorf("jiraClientMethods classifies %q, an issue-create/delete verb — such a "+
				"method must never exist on the outbound surface (ka-0000002073).", name)
		}
		if class == classGovernedWrite && issueLifecycleVerbRE.MatchString(name) {
			t.Errorf("governed write %q is an issue-lifecycle verb — create/delete of an "+
				"issue is not a governed write, it is a forbidden surface (ka-0000002073).", name)
		}
	}
}

// TestGovernedWritesMatchNoBypassSet ties this file's classification to the
// existing no-bypass write set (jiraWriteMethodNames) so the two cannot drift:
// every method classified here as a governed write must be in jiraWriteMethodNames
// (and thus subject to the no-bypass gate scan), and every name in
// jiraWriteMethodNames that is implemented on *JiraClient must be classified here
// as a governed write. This makes the "write surface" a single closed universe
// across both test files.
func TestGovernedWritesMatchNoBypassSet(t *testing.T) {
	ct := reflect.TypeOf(&JiraClient{})
	implemented := func(name string) bool {
		_, ok := ct.MethodByName(name)
		return ok
	}

	// Every governed write here is in the no-bypass set.
	for name, class := range jiraClientMethods {
		if class != classGovernedWrite {
			continue
		}
		if !jiraWriteMethodNames[name] {
			t.Errorf("governed write %q is missing from jiraWriteMethodNames — the "+
				"no-bypass gate scan would not treat it as a governed write.", name)
		}
	}
	// Every IMPLEMENTED no-bypass write name is classified here as a governed
	// write (unimplemented names in that set are intentionally listed ahead of
	// their implementation and are skipped).
	for name := range jiraWriteMethodNames {
		if !implemented(name) {
			continue
		}
		if jiraClientMethods[name] != classGovernedWrite {
			t.Errorf("jiraWriteMethodNames lists implemented method %q but it is not "+
				"classified as a governed write in jiraClientMethods.", name)
		}
	}
}

// ── Field/op allowlist cross-check: create/delete are not reachable ops ──

// TestCreateDeleteAreNotGovernedFields proves "create" and "delete" are UNKNOWN
// fields in the governed-write universe (knownJiraWriteFields), and that the
// universe still enumerates only the sub-resource write ops — comment,
// transition, labels, assignee, remotelink (+ versions, added by ka-0000003103).
// It also drives the REAL WritePolicyStore to show a "create"/"delete" op fails
// closed for every role on a writable project: even if such a verb were wired,
// the op allowlist would reject it. This is the "create/delete are not reachable
// through the allowlist" cross-check the task calls for.
func TestCreateDeleteAreNotGovernedFields(t *testing.T) {
	// (a) create/delete are not in the closed field universe.
	for _, forbidden := range []JiraWriteField{"create", "delete", "createissue", "deleteissue"} {
		if knownJiraWriteFields[forbidden] {
			t.Errorf("knownJiraWriteFields must NOT contain %q — issue create/delete is not "+
				"a governed write op (ka-0000002073).", forbidden)
		}
	}

	// (b) the universe is a subset of the allowed sub-resource ops. The always-
	// present five, the versions op (ka-0000003103), and the attachment op
	// (ka-0000001730) — nothing else, and specifically nothing resembling a
	// lifecycle verb.
	//
	// FieldAttachment admitted under EXPLICIT REVIEW, which is what this guard
	// demands of any new op. It backs AddAttachment: a multipart POST to
	// .../issue/{key}/attachments, the only way to embed an image in Jira (the
	// viewer's /beads/image/ proxy is OIDC-gated, so a pasted URL renders
	// broken). It is a SUB-RESOURCE write in the same family as
	// comment/labels/remotelink — it requires the issue to already exist, and
	// creates and deletes nothing. The zero-FSM-create invariant (epic
	// ka-0000002067) is untouched: with no create path, an attachment op cannot
	// bring an issue into being. Gated separately from FieldComment because
	// attaching durable, downloadable bytes to a customer-adjacent ticket is a
	// materially different act from writing text, and a role cleared for one is
	// not automatically cleared for the other.
	allowed := map[JiraWriteField]bool{
		FieldComment: true, FieldTransition: true, FieldLabels: true,
		FieldAssignee: true, FieldRemoteLink: true, FieldVersions: true,
		FieldAttachment: true,
	}
	for f := range knownJiraWriteFields {
		if !allowed[f] {
			t.Errorf("unexpected governed field %q in knownJiraWriteFields — the op universe "+
				"must stay {comment,transition,labels,assignee,remotelink,versions,attachment}; "+
				"a new op (especially a create/delete) needs explicit review (ka-0000002073).", f)
		}
		if issueLifecycleVerbRE.MatchString(string(f)) {
			t.Errorf("governed field %q looks like an issue-lifecycle verb — must not be a "+
				"governed op (ka-0000002073).", f)
		}
	}

	// (c) fail-closed through the REAL store: a "create"/"delete" op is denied for
	// every known role on an always-writable project (PE), with fsm toggle on so
	// the store is maximally permissive on the project axis.
	var buf bytes.Buffer
	store := NewWritePolicyStore(DefaultWritePolicy(true), strongTestLogger(&buf))
	roles := append(DefaultAllowlistGate(nil).Roles(), "totally-unknown-role")
	for _, op := range []JiraWriteField{"create", "delete", "createissue", "deleteissue"} {
		for _, role := range roles {
			if store.AllowField("PE-1", role, op) {
				t.Errorf("op %q must fail closed for role %q on writable PE — a create/delete "+
					"op must never pass the field allowlist (ka-0000002073).", op, role)
			}
		}
	}
	if !strings.Contains(buf.String(), `reason="unknown field"`) {
		t.Errorf("expected 'unknown field' denial reason for the create/delete ops, got: %s", buf.String())
	}
}

// ── REST-surface source scan: no DELETE verb, no create-issue endpoint ──

// jiraClientSourceFile is the single file that defines the outbound JiraClient
// REST surface. The scan below parses it (not a mock) so a raw create/delete
// path added via doJSON — bypassing the named-method guards above — is still
// caught by the HTTP verb + endpoint-literal it must use.
const jiraClientSourceFile = "jira.go"

// issueCreateEndpointRE matches the Jira issue-COLLECTION endpoint literal used
// to CREATE an issue (POST /rest/api/{v}/issue) or bulk-create
// (/rest/api/{v}/issue/bulk). It deliberately requires the closing quote (or
// /bulk) immediately after "issue" so it does NOT match the per-issue paths the
// real methods build — those are the literal "/rest/api/3/issue/" WITH a
// trailing slash, then + url.PathEscape(key). Only the create-collection form
// ends the literal right at "issue".
var issueCreateEndpointRE = regexp.MustCompile(`^/rest/api/\d+/issue(/bulk)?$`)

// TestJiraClientHasNoDeleteVerbOrCreateEndpoint parses jira.go and asserts the
// outbound surface uses NO http.MethodDelete and contains NO issue-create
// endpoint string literal. This catches a create/delete path wired directly
// through doJSON/http even if its Go method were innocuously named (so the
// name-based guards above would miss it). Comments are ignored (they are not
// string literals), so the doc-comment prose mentioning "delete" is not a false
// positive. Fails closed: an unparseable file errors.
func TestJiraClientHasNoDeleteVerbOrCreateEndpoint(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, jiraClientSourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", jiraClientSourceFile, err)
	}

	var (
		sawGet, sawPost, sawPut bool // sanity: the scan is actually seeing verbs
		deleteVerbs             []string
		createEndpoints         []string
	)

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			// Match http.Method<Verb> usages via the selector name; robust to the
			// import alias since we key on the selector, not the package ident.
			switch node.Sel.Name {
			case "MethodGet":
				sawGet = true
			case "MethodPost":
				sawPost = true
			case "MethodPut":
				sawPut = true
			case "MethodDelete":
				pos := fset.Position(node.Pos())
				deleteVerbs = append(deleteVerbs, node.Sel.Name+" @ "+pos.String())
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(node.Value)
			if uerr != nil {
				return true
			}
			if issueCreateEndpointRE.MatchString(val) {
				pos := fset.Position(node.Pos())
				createEndpoints = append(createEndpoints, val+" @ "+pos.String())
			}
		}
		return true
	})

	if len(deleteVerbs) != 0 {
		t.Errorf("%s uses http.MethodDelete (%v) — the outbound Jira surface must expose no "+
			"delete path; the permissive Jira floor (DELETE_ISSUES=true) makes the bridge the "+
			"only control (ka-0000002073).", jiraClientSourceFile, deleteVerbs)
	}
	if len(createEndpoints) != 0 {
		t.Errorf("%s contains an issue-create endpoint literal (%v) — the bridge must not POST "+
			"to the issue-collection endpoint; there is no create-issue path by construction "+
			"(ka-0000002073).", jiraClientSourceFile, createEndpoints)
	}

	// Sanity floor: if the scan saw none of GET/POST/PUT, the matcher has gone
	// blind (e.g. the http verb idiom changed) and the delete/create checks would
	// vacuously pass. Require evidence the scan actually inspected the surface.
	if !sawGet || !sawPost || !sawPut {
		t.Fatalf("%s scan saw GET=%v POST=%v PUT=%v — expected all three; the http-verb "+
			"matcher likely went blind and this guard is no longer checking anything.",
			jiraClientSourceFile, sawGet, sawPost, sawPut)
	}
}

// ── Teeth: prove each detector fires on a synthetic violation ──

// TestNoIssueLifecycle_DetectorsHaveTeeth proves the guards above are not
// vacuous by running their exact detection logic over synthetic inputs known to
// violate (must be flagged) and known to be clean (must pass). Without this, a
// bug that broke a regex or an AST matcher would let the real guards pass while
// checking nothing.
func TestNoIssueLifecycle_DetectorsHaveTeeth(t *testing.T) {
	// (1) Lifecycle-verb regex: flags create/delete-issue names, clears the real
	// governed writes and reads.
	mustMatch := []string{"CreateIssue", "DeleteIssue", "NewIssue", "RemoveIssue",
		"BulkCreateIssues", "DeleteIssueByKey", "PurgeIssue", "ArchiveIssue"}
	for _, name := range mustMatch {
		if !issueLifecycleVerbRE.MatchString(name) {
			t.Errorf("lifecycle-verb regex FAILED to flag %q — the guard has no teeth", name)
		}
	}
	mustNotMatch := []string{
		"AddComment", "AddCommentWithVisibility", "AddLabels", "AddRemoteLink",
		"SetAffectsVersions", "AssignIssue", "TransitionIssue",
		"GetIssue", "GetRemoteLinks", "SearchIssues", "SearchIssuesPage",
		"ApproximateCount", "ListFields", "DownloadAttachment",
	}
	for _, name := range mustNotMatch {
		if issueLifecycleVerbRE.MatchString(name) {
			t.Errorf("lifecycle-verb regex FALSE-POSITIVED on legitimate method %q", name)
		}
	}

	// (2) Create-endpoint regex: flags the collection/bulk endpoints, clears the
	// per-issue paths the real methods build (trailing slash) and reads.
	for _, ep := range []string{"/rest/api/3/issue", "/rest/api/2/issue", "/rest/api/3/issue/bulk"} {
		if !issueCreateEndpointRE.MatchString(ep) {
			t.Errorf("create-endpoint regex FAILED to flag %q — the guard has no teeth", ep)
		}
	}
	for _, ep := range []string{
		"/rest/api/3/issue/", "/rest/api/3/issue/PE-1", "/rest/api/3/issue/PE-1/comment",
		"/rest/api/3/issue/PE-1/transitions", "/rest/api/3/search/jql", "/rest/api/3/field",
	} {
		if issueCreateEndpointRE.MatchString(ep) {
			t.Errorf("create-endpoint regex FALSE-POSITIVED on legitimate path %q", ep)
		}
	}

	// (3) AST source-scan detectors: run the same verb/endpoint matching over
	// synthetic source with a DELETE verb + create endpoint, and over clean
	// source, asserting the former is flagged and the latter is not.
	detect := func(src string) (delFound, createFound bool) {
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, "synthetic.go", src, 0)
		if perr != nil {
			t.Fatalf("parse synthetic: %v", perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == "MethodDelete" {
					delFound = true
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				if val, uerr := strconv.Unquote(node.Value); uerr == nil && issueCreateEndpointRE.MatchString(val) {
					createFound = true
				}
			}
			return true
		})
		return delFound, createFound
	}

	bad := `package p
import "net/http"
func (c *C) DeleteIssue(key string) {
	_ = c.doJSON(http.MethodDelete, "/rest/api/3/issue/"+key)
}
func (c *C) CreateIssue() {
	_ = c.doJSON(http.MethodPost, "/rest/api/3/issue")
}`
	clean := `package p
import "net/http"
func (c *C) GetIssue(key string) {
	_ = c.doJSON(http.MethodGet, "/rest/api/3/issue/"+key)
}
func (c *C) AddLabels(key string) {
	_ = c.doJSON(http.MethodPut, "/rest/api/3/issue/"+key)
}`

	if del, cre := detect(bad); !del || !cre {
		t.Errorf("source-scan detectors have no teeth: on violating source got deleteVerb=%v createEndpoint=%v, want both true", del, cre)
	}
	if del, cre := detect(clean); del || cre {
		t.Errorf("source-scan detectors FALSE-POSITIVED on clean source: deleteVerb=%v createEndpoint=%v, want both false", del, cre)
	}
}
