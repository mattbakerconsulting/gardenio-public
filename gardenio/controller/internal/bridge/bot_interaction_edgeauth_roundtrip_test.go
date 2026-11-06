package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/edgeauth"

	"github.com/slack-go/slack"
)

// TestSlackInteraction_V1EnvelopeResolvesDecision_EnforceOn is the
// regression test whose absence let MR !6364 silently revoke the Slack
// operator-unblock path (gd-9mtFh0Zqpe, epic gd-N237aQ55f2).
//
// The operator's authoritative contract (decision ka-0000001641 =
// "additive") is: the Slack bridge keeps its EXISTING authority via a
// valid SIGNED v1 envelope; v2 role enforcement is added ON TOP for the
// PiID/Entra path, NOT as a replacement. The daemon runs with
// DAEMON_HMAC_VERIFY_ENFORCE=true in live prod, so this must hold WITH
// enforcement on.
//
// This test drives the real Slack-interaction entry point through a REAL
// beadsapi.Client (the same type wired in cmd/slack-bridge/main.go),
// captures the actual wire request the bridge emits on the decision
// close, and proves the envelope it produced is a cryptographically
// VERIFIED v1 envelope (edgeauth.Outcome==OK, Trusted) under Enforce=true
// — i.e. exactly the "signed v1 keeps its authority" precondition the
// additive daemon fix (owned by ka-0000006597) keys on. It deliberately
// asserts the envelope is v1 (no X-Edge-Auth-Version) — the bridge is one
// of the paths the operator kept on v1, so a future well-meaning change
// that quietly turns it into a v2 signer should trip this test.
func TestSlackInteraction_V1EnvelopeResolvesDecision_EnforceOn(t *testing.T) {
	const (
		hmacKey  = "test-shared-key-32-bytes-padding!"
		slackUID = "U0OPERATOR1"
		decID    = "dec-edge-rt"
	)
	wantOperator := "slack:" + slackUID

	var (
		mu       sync.Mutex
		gotClose *http.Request
	)
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The only daemon mutation the dismiss interaction performs is the
		// decision close (POST /v1/beads/{id}/close). Capture it; ack
		// everything with 204 so beadsapi treats the call as success.
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+decID+"/close" {
			mu.Lock()
			gotClose = r.Clone(context.Background())
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer daemonSrv.Close()

	// Real beadsapi client, edge-auth signing enabled — mirrors the
	// slack-bridge wiring (SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY).
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	client.SetEdgeAuthHMACKey([]byte(hmacKey))

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(client, slackSrv)

	// Dismiss button = the canonical Slack decision-resolve interaction:
	// handleInteraction stamps WithEdgeAuthOperator(slack:U…) then closes
	// the decision bead (bot_decisions_modal.go handleDismiss -> CloseBead).
	callback := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: slackUID, Name: "matt"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: "dismiss_decision", Value: decID},
			},
		},
	}
	bot.handleInteraction(context.Background(), callback)

	mu.Lock()
	req := gotClose
	mu.Unlock()
	if req == nil {
		t.Fatal("bridge never issued the decision close POST to the daemon")
	}

	// The bridge must sign v1, not v2 — this is a kept-on-v1 path.
	if v := req.Header.Get(edgeauth.VerifyHeaderOperator); v != wantOperator {
		t.Errorf("X-Operator = %q, want %q", v, wantOperator)
	}
	if v := req.Header.Get("X-Edge-Auth-Version"); v != "" {
		t.Errorf("bridge stamped X-Edge-Auth-Version=%q; the Slack path must stay v1 (operator spec ka-0000001641)", v)
	}
	if req.Header.Get("X-Edge-Auth-Sig") == "" {
		t.Fatal("bridge close carried no v1 signature (X-Edge-Auth-Sig)")
	}

	// Faithful daemon-side verification with enforcement ON: a correctly
	// signed v1 envelope is Trusted (Outcome==OK) — the invariant that
	// makes the additive fix restore pre-MR authority without weakening the
	// gate. The verifier shares beadsapi's canonical HMAC byte layout
	// (operator\nauditID\npath\nts), the same bytes kbeads recomputes.
	verify := edgeauth.VerifyConfig{HMACKey: []byte(hmacKey), Enforce: true}
	res := verify.Verify(req)
	if res.Outcome != edgeauth.OutcomeOK {
		t.Fatalf("v1 envelope outcome = %q, want %q (enforce=true)", res.Outcome, edgeauth.OutcomeOK)
	}
	if !res.Trusted {
		t.Fatal("verified v1 bridge envelope is not Trusted under enforce=true — the operator-unblock path would 403")
	}
	if res.Operator != wantOperator {
		t.Errorf("verified operator = %q, want %q", res.Operator, wantOperator)
	}
}

// TestSlackInteraction_UnsignedAndTamperedEnvelopeRefused_EnforceOn is the
// negative half: enforcement is real, not a rubber stamp. Starting from the
// exact bridge-shaped request the producer emits, an envelope whose
// signature is stripped or corrupted must NOT be Trusted under enforce=true.
func TestSlackInteraction_UnsignedAndTamperedEnvelopeRefused_EnforceOn(t *testing.T) {
	const (
		hmacKey  = "test-shared-key-32-bytes-padding!"
		slackUID = "U0OPERATOR1"
		decID    = "dec-edge-neg"
	)

	var (
		mu       sync.Mutex
		gotClose *http.Request
	)
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+decID+"/close" {
			mu.Lock()
			gotClose = r.Clone(context.Background())
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer daemonSrv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	client.SetEdgeAuthHMACKey([]byte(hmacKey))

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(client, slackSrv)

	bot.handleInteraction(context.Background(), slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: slackUID, Name: "matt"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: "dismiss_decision", Value: decID},
			},
		},
	})

	mu.Lock()
	base := gotClose
	mu.Unlock()
	if base == nil {
		t.Fatal("bridge never issued the decision close POST to the daemon")
	}

	verify := edgeauth.VerifyConfig{HMACKey: []byte(hmacKey), Enforce: true}

	// Sanity: the untouched envelope is accepted (guards against a test that
	// would pass simply because verification always fails).
	if res := verify.Verify(base); res.Outcome != edgeauth.OutcomeOK || !res.Trusted {
		t.Fatalf("baseline envelope not accepted: outcome=%q trusted=%v", res.Outcome, res.Trusted)
	}

	t.Run("stripped signature", func(t *testing.T) {
		r := base.Clone(context.Background())
		r.Header.Del("X-Edge-Auth-Sig")
		res := verify.Verify(r)
		if res.Trusted {
			t.Fatalf("unsigned bridge envelope was Trusted (outcome=%q) under enforce=true", res.Outcome)
		}
		if res.Outcome != edgeauth.OutcomeNoSig {
			t.Errorf("outcome = %q, want %q", res.Outcome, edgeauth.OutcomeNoSig)
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		r := base.Clone(context.Background())
		// Flip the signature to a well-formed-but-wrong value.
		r.Header.Set("X-Edge-Auth-Sig", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
		res := verify.Verify(r)
		if res.Trusted {
			t.Fatalf("tampered bridge envelope was Trusted (outcome=%q) under enforce=true", res.Outcome)
		}
		if res.Outcome != edgeauth.OutcomeMismatch {
			t.Errorf("outcome = %q, want %q", res.Outcome, edgeauth.OutcomeMismatch)
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		// A correctly signed but far-past envelope is rejected as stale by a
		// verifier whose clock has moved well beyond the skew window.
		future := edgeauth.VerifyConfig{
			HMACKey: []byte(hmacKey),
			Enforce: true,
			Now:     func() time.Time { return time.Now().Add(1 * time.Hour) },
		}
		res := future.Verify(base)
		if res.Trusted {
			t.Fatalf("stale bridge envelope was Trusted (outcome=%q) under enforce=true", res.Outcome)
		}
		if res.Outcome != edgeauth.OutcomeStale {
			t.Errorf("outcome = %q, want %q", res.Outcome, edgeauth.OutcomeStale)
		}
	})
}
