package commons

import (
	"net/http"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// A message shows fields.author only when the daemon vouches for it, and
// otherwise created_by (gd-PjNjRhAHPn).
func TestMessageAuthor(t *testing.T) {
	for _, c := range []struct {
		name, createdBy, author, origin, want string
	}{
		// The impersonation: an agent wrote author=oidc:<owner>.
		{"agent names the owner, no origin", "gardenio-crew-x", "oidc:owner@example.com", "", "gardenio-crew-x"},
		{"agent names the owner, origin=slack", "gardenio-crew-x", "oidc:owner@example.com", "slack", "gardenio-crew-x"},
		{"static names the owner", "static", "oidc:owner@example.com", "", "static"},
		{"pre-gate origin=commons naming another", "gardenio-crew-x", "oidc:owner@example.com", "commons", "gardenio-crew-x"},
		{"person names another, no origin", "ada@example.com", "oidc:owner@example.com", "", "ada@example.com"},
		// A person's Commons message, and an agent as itself.
		{"person's Commons message", "ada@example.com", "oidc:ada@example.com", "commons", "oidc:ada@example.com"},
		{"person's Commons message, case differs", "ada@example.com", "oidc:Ada@Example.com", "commons", "oidc:Ada@Example.com"},
		{"agent as agent:<self>", "gardenio-crew-x", "agent:gardenio-crew-x", "", "agent:gardenio-crew-x"},
		{"agent as its bare name", "gardenio-crew-x", "gardenio-crew-x", "", "gardenio-crew-x"},
		// A relay writes in others' names.
		{"slack mirror of a person's message", "slack-bridge", "oidc:ada@example.com", "slack", "oidc:ada@example.com"},
		{"rotated bridge", "bridge:slack", "oidc:ada@example.com", "slack", "oidc:ada@example.com"},
		{"bridge writes an agent's say", "slack-bridge", "agent:gardenio-crew-x", "", "agent:gardenio-crew-x"},
		{"controller notice", "machine:gardenio-controller", "agent:gardenio-purse", "", "agent:gardenio-purse"},
		// One of the two is missing.
		{"no author", "gardenio-crew-x", "", "", "gardenio-crew-x"},
		{"no created_by", "", "agent:gardenio-crew-x", "", "agent:gardenio-crew-x"},
	} {
		b := &beadsapi.BeadDetail{ID: "cl-m", Type: "message", Labels: []string{"project:claimlayer"}, CreatedBy: c.createdBy,
			Fields: map[string]string{"channel_id": "cl-chgen", "text": "hi", "author": c.author, "origin": c.origin}}
		m, ok := messageFromBead(b)
		if !ok {
			t.Fatalf("%s: not a message", c.name)
		}
		if m.Author != c.want {
			t.Errorf("%s: author = %q, want %q", c.name, m.Author, c.want)
		}
	}
}

// The forged message renders as its writer, never the owner.
func TestMessageAuthor_Rendered(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-forge1", Type: "message", Title: "trust me", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(30), CreatedBy: "claimlayer-crew-9",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "text": "trust me"}})
	h := commonsServer(t, d)
	code, body := get(t, h, "/test/m/cl-forge1", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET message = %d", code)
	}
	mustContain(t, body, `title="claimlayer-crew-9"`, `cm-author--agent`)
	mustNotContain(t, body, "oidc:ann@claimlayer.example")
}

// A message an admin wrote signed in as a tenant (one-click Become,
// gd-L8LeYDLACc) carries the daemon's created_impersonated_by stamp and shows
// a "by <admin> as <tenant>" mark beside its author. Anyone else's shows none,
// even when a later impersonated write (a close, a reaction) left
// impersonated_by on it.
func TestMessageAuthor_ImpersonatedMark(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-become1", Type: "message", Title: "hello", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(30), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "origin": "commons", "text": "hello",
			"impersonated_by": "owner", "created_impersonated_by": "owner"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-become2", Type: "message", Title: "reply", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(40), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-become1", "author": "oidc:ann@claimlayer.example", "origin": "commons",
			"text": "a reply", "impersonated_by": "owner", "created_impersonated_by": "owner"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-own1", Type: "message", Title: "mine", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(50), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "origin": "commons", "text": "mine"}})
	// Ann wrote this wish herself; owner, Become as Ann, then closed it.
	d.Put(&beadsapi.BeadDetail{ID: "cl-own2", Type: "message", Title: "my wish", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(60), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "origin": "commons", "text": "my wish",
			"impersonated_by": "owner"}})
	h := commonsServer(t, d)

	for _, id := range []string{"cl-become1", "cl-become2"} {
		code, body := get(t, h, "/test/m/"+id, "claimlayer")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", id, code)
		}
		mustContain(t, body, `<span class="cm-as" title="Written by owner while signed in as oidc:ann@claimlayer.example">by owner as ann</span>`)
		// The tenant stays the author.
		mustContain(t, body, `title="oidc:ann@claimlayer.example">ann</span>`)
	}
	for _, id := range []string{"cl-own1", "cl-own2"} {
		code, body := get(t, h, "/test/m/"+id, "claimlayer")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", id, code)
		}
		mustNotContain(t, body, `cm-as`)
	}
}

// The mark escapes what it shows.
func TestMessageAuthor_ImpersonatedMarkEscaped(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-become3", Type: "message", Title: "x", Status: "closed", Labels: []string{"project:claimlayer"},
		CreatedAt: t0.Add(30), CreatedBy: "ann@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ann@claimlayer.example", "origin": "commons", "text": "x",
			"created_impersonated_by": `<b>"x"</b>`}})
	h := commonsServer(t, d)
	_, body := get(t, h, "/test/m/cl-become3", "claimlayer")
	mustContain(t, body, `by &lt;b&gt;&#34;x&#34;&lt;/b&gt; as ann`)
	mustNotContain(t, body, `<b>"x"</b>`)
}
