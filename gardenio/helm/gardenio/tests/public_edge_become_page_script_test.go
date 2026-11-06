package tests

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Become page's script (public-edge-become become.js, gd-VIm9kMDswf),
// run under node with a fake browser: location, history, sessionStorage, a
// minimal DOM and a scripted fetch standing in for Keycloak. Each case reads
// back where the page navigated, what it showed and which calls it made.
// Skips without node, as gardenhub's node tests do.

const becomeJSHarness = `
const vm = require('vm');
const fs = require('fs');
const { webcrypto } = require('crypto');
const src = fs.readFileSync(process.argv[1], 'utf8');
const c = JSON.parse(process.argv[2]);

const out = { navigated: null, replaced: null, title: '', text: '', links: [], calls: [], storage: null };
const store = new Map(Object.entries(c.storage || {}));
const el = (id) => ({ set textContent(v) { out[id] = v; }, replaceChildren(...kids) { out.links = kids.map((k) => [k.textContent, k.href]); } });
const nodes = { title: el('title'), text: el('text'), links: el('links') };
const replies = c.replies || {};

function reply(method, url) {
  const key = Object.keys(replies).find((k) => (method + ' ' + url).startsWith(k));
  const r = key ? replies[key] : { status: 404, body: null };
  if (r.throws) return Promise.reject(new TypeError(r.throws));
  return Promise.resolve({ ok: r.status >= 200 && r.status < 300, status: r.status, json: async () => r.body });
}

const sandbox = {
  location: {
    origin: 'https://auth.clonk.tv',
    search: c.search || '',
    hash: c.hash || '',
    replace(u) { out.navigated = u; },
  },
  history: { replaceState(_s, _t, u) { out.replaced = u; } },
  sessionStorage: {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  },
  document: {
    getElementById: (id) => nodes[id],
    createElement: () => ({ href: '', textContent: '' }),
  },
  fetch: (url, opts) => {
    const method = (opts && opts.method) || 'GET';
    out.calls.push({ method, url, auth: (opts.headers || {}).Authorization || '', body: opts.body ? String(opts.body) : '', credentials: opts.credentials || '' });
    return reply(method, url);
  },
  crypto: webcrypto, TextEncoder, URLSearchParams, btoa, atob, Uint8Array, JSON, Array, String, Error, TypeError, Promise,
};
vm.runInNewContext(src, sandbox);

const t0 = Date.now();
(function wait() {
  if (out.navigated || out.title !== '' && !out.title.startsWith('Becoming ') || Date.now() - t0 > 3000) {
    out.storage = Object.fromEntries(store);
    process.stdout.write(JSON.stringify(out));
    return;
  }
  setTimeout(wait, 5);
})();
`

type becomeRun struct {
	Navigated string      `json:"navigated"`
	Replaced  string      `json:"replaced"`
	Title     string      `json:"title"`
	Text      string      `json:"text"`
	Links     [][2]string `json:"links"`
	Calls     []struct {
		Method, URL, Auth, Body, Credentials string
	} `json:"calls"`
	Storage map[string]string `json:"storage"`
}

type becomeReply struct {
	Status int    `json:"status"`
	Body   any    `json:"body"`
	Throws string `json:"throws,omitempty"`
}

func runBecomeJS(t *testing.T, search, hash string, storage map[string]string, replies map[string]becomeReply) becomeRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js := filepath.Join(t.TempDir(), "become.js")
	if err := os.WriteFile(js, []byte(becomePageFiles(t, readEdgeFile(t, "public-edge.yaml"))["become.js"]), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"search": search, "hash": hash, "storage": storage, "replies": replies})
	raw, err := exec.Command(node, "-e", becomeJSHarness, js, string(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var run becomeRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("node output: %v\n%s", err, raw)
	}
	return run
}

const (
	becomeHere    = "https://auth.clonk.tv/gardenio/become/"
	becomeAdminID = "admin-id"
	becomeTenant  = "tenant@example.com"
	becomeUsers   = "GET /auth/admin/realms/gardenio/users?email=tenant%40example.com&exact=true"
	becomeGroups  = "GET /auth/admin/realms/gardenio/users/tenant-id/groups?"
	becomeImp     = "POST /auth/admin/realms/gardenio/users/tenant-id/impersonation"
	becomeToken   = "POST /auth/realms/gardenio/protocol/openid-connect/token"
)

// A token whose sub is the admin; only its payload is read.
var becomeAccessToken = "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+becomeAdminID+`"}`)) + ".y"

func becomeSaved() map[string]string {
	return map[string]string{"gardenio-become": `{"state":"st","verifier":"ver","email":"` + becomeTenant + `"}`}
}

func becomeKeycloak(groups any) map[string]becomeReply {
	return map[string]becomeReply{
		becomeToken:  {Status: 200, Body: map[string]string{"access_token": becomeAccessToken}},
		becomeUsers:  {Status: 200, Body: []map[string]string{{"id": "tenant-id", "email": becomeTenant}}},
		becomeGroups: {Status: 200, Body: groups},
		becomeImp:    {Status: 200, Body: map[string]any{"redirect": "x", "sameRealm": true}},
	}
}

func TestBecomePageJS_StartsASilentPKCESignIn(t *testing.T) {
	t.Parallel()
	run := runBecomeJS(t, "?email=tenant%40example.com", "", nil, nil)
	if len(run.Calls) != 0 {
		t.Errorf("start made %d calls", len(run.Calls))
	}
	prefix := "/auth/realms/gardenio/protocol/openid-connect/auth?"
	if !strings.HasPrefix(run.Navigated, prefix) {
		t.Fatalf("navigated to %q, want the realm's auth endpoint", run.Navigated)
	}
	q := parseQuery(t, strings.TrimPrefix(run.Navigated, prefix))
	for k, v := range map[string]string{
		"client_id": "gardenio-become", "redirect_uri": becomeHere, "response_type": "code",
		"response_mode": "fragment", "prompt": "none", "code_challenge_method": "S256",
	} {
		if q[k] != v {
			t.Errorf("auth request %s=%q, want %q", k, q[k], v)
		}
	}
	var saved struct{ State, Verifier, Email string }
	if err := json.Unmarshal([]byte(run.Storage["gardenio-become"]), &saved); err != nil {
		t.Fatalf("saved state: %v", err)
	}
	if saved.Email != becomeTenant || saved.State == "" || saved.State != q["state"] || len(saved.Verifier) < 43 {
		t.Errorf("saved %+v, state %q", saved, q["state"])
	}
	if q["code_challenge"] == "" || q["code_challenge"] == saved.Verifier {
		t.Errorf("code_challenge %q must be the S256 of the verifier", q["code_challenge"])
	}

	// The link offered after login_required asks for a real sign-in.
	run = runBecomeJS(t, "?email=tenant%40example.com&login=1", "", nil, nil)
	if strings.Contains(run.Navigated, "prompt=") {
		t.Errorf("login=1 still asks for prompt: %s", run.Navigated)
	}
}

func TestBecomePageJS_BecomesTheTenant(t *testing.T) {
	t.Parallel()
	run := runBecomeJS(t, "", "#state=st&session_state=s&code=the-code", becomeSaved(),
		becomeKeycloak([]map[string]string{{"path": "/lands-tenants"}, {"path": "/project-x/owners"}}))
	if run.Navigated != "https://gardenio.clonk.tv/oauth2/sign_out?rd=%2Fgarden" {
		t.Fatalf("navigated to %q (title %q: %q), want the gardenio sign-out", run.Navigated, run.Title, run.Text)
	}
	if run.Replaced != becomeHere {
		t.Errorf("history not cleared of the code: %q", run.Replaced)
	}
	if len(run.Storage) != 0 {
		t.Errorf("saved state left behind: %v", run.Storage)
	}
	want := []string{becomeToken, becomeUsers, becomeGroups, becomeImp}
	if len(run.Calls) != len(want) {
		t.Fatalf("calls %+v, want %v", run.Calls, want)
	}
	for i, c := range run.Calls {
		if !strings.HasPrefix(c.Method+" "+c.URL, want[i]) {
			t.Errorf("call %d: %s %s, want %s", i, c.Method, c.URL, want[i])
		}
		if c.Credentials != "same-origin" {
			t.Errorf("call %d credentials %q", i, c.Credentials)
		}
		if i > 0 && c.Auth != "Bearer "+becomeAccessToken {
			t.Errorf("call %d Authorization %q", i, c.Auth)
		}
	}
	form := parseQuery(t, run.Calls[0].Body)
	for k, v := range map[string]string{"grant_type": "authorization_code", "code": "the-code", "redirect_uri": becomeHere, "client_id": "gardenio-become", "code_verifier": "ver"} {
		if form[k] != v {
			t.Errorf("token request %s=%q, want %q", k, form[k], v)
		}
	}
}

func TestBecomePageJS_RefusesAndShowsWhy(t *testing.T) {
	t.Parallel()
	keycloak := func(edit func(map[string]becomeReply)) map[string]becomeReply {
		r := becomeKeycloak([]map[string]string{{"path": "/lands-tenants"}})
		edit(r)
		return r
	}
	for _, tc := range []struct {
		name, search, hash string
		storage            map[string]string
		replies            map[string]becomeReply
		title, link        string
	}{
		{name: "no email", search: "", title: "No one to become"},
		{name: "bad email", search: "?email=nope", title: "No one to become"},
		{name: "state from elsewhere", hash: "#state=other&code=c", storage: becomeSaved(), title: "This sign-in did not start here"},
		{name: "no saved state", hash: "#state=st&code=c", title: "This sign-in did not start here"},
		{name: "silent sign-in needs a login", hash: "#error=login_required&state=st", storage: becomeSaved(),
			title: "You need to sign in first", link: becomeHere + "?email=tenant%40example.com&login=1"},
		{name: "sign-in error", hash: "#error=access_denied&state=st", storage: becomeSaved(), title: "Sign-in failed"},
		{name: "code refused", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeToken] = becomeReply{Status: 400} }), title: "Sign-in failed"},
		{name: "unknown email", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeUsers] = becomeReply{Status: 200, Body: []any{}} }), title: "No such tenant"},
		{name: "fuzzy match is not a match", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) {
				r[becomeUsers] = becomeReply{Status: 200, Body: []map[string]string{{"id": "tenant-id", "email": "xtenant@example.com"}}}
			}), title: "No such tenant"},
		{name: "yourself", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) {
				r[becomeUsers] = becomeReply{Status: 200, Body: []map[string]string{{"id": becomeAdminID, "email": becomeTenant}}}
			}), title: "That is you"},
		{name: "an admin", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) {
				r[becomeGroups] = becomeReply{Status: 200, Body: []map[string]string{{"path": "/lands-tenants"}, {"path": "/gardenio-admins"}}}
			}), title: "That is an admin"},
		{name: "groups unreadable", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeGroups] = becomeReply{Status: 200, Body: map[string]any{}} }), title: "Could not check tenant@example.com"},
		{name: "keycloak 403", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeUsers] = becomeReply{Status: 403} }), title: "Keycloak refused"},
		{name: "impersonation 403", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeImp] = becomeReply{Status: 403} }), title: "Keycloak refused"},
		{name: "rate limited", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeGroups] = becomeReply{Status: 429} }), title: "Too many requests"},
		{name: "edge sign-in ran out", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeUsers] = becomeReply{Status: 401} }), title: "Your sign-in ran out"},
		{name: "network", hash: "#state=st&code=c", storage: becomeSaved(),
			replies: keycloak(func(r map[string]becomeReply) { r[becomeImp] = becomeReply{Throws: "Failed to fetch"} }), title: "Could not reach Keycloak"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runBecomeJS(t, tc.search, tc.hash, tc.storage, tc.replies)
			if run.Title != tc.title {
				t.Fatalf("title %q (%q), want %q", run.Title, run.Text, tc.title)
			}
			if run.Text == "" {
				t.Error("no explanation shown")
			}
			if strings.Contains(run.Navigated, "sign_out") {
				t.Error("navigated to the sign-out after a refusal")
			}
			for _, c := range run.Calls {
				if c.Method == "POST" && strings.HasSuffix(c.URL, "/impersonation") && tc.name != "impersonation 403" && tc.name != "network" {
					t.Errorf("impersonated after a refusal: %s", c.URL)
				}
			}
			last := run.Links[len(run.Links)-1]
			if last != [2]string{"Back to /garden", "https://gardenio.clonk.tv/garden"} {
				t.Errorf("links %v, want the last to go back to /garden", run.Links)
			}
			if tc.link != "" && (len(run.Links) < 2 || run.Links[0][1] != tc.link) {
				t.Errorf("links %v, want first %s", run.Links, tc.link)
			}
		})
	}
}

func parseQuery(t *testing.T, raw string) map[string]string {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("bad query %q: %v", raw, err)
	}
	out := map[string]string{}
	for k := range v {
		out[k] = v.Get(k)
	}
	return out
}
