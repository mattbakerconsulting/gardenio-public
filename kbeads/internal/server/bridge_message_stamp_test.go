package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// The Slack bridge stamps handled_at and its Slack ref on a message a person or
// an agent wrote, on its own fleet token (bridge_message_stamp.go,
// gd-2IEp8qx7bq). The commons_origin tests made these writes on the static
// token, which passes ownership for everything, so they never saw the 403 the
// bridge got on live.

const (
	bmsBridge      = "bms-slack-bridge-tok"   // "<token>:slack-bridge", as deployed
	bmsBridgeRot   = "bms-slack-bridge-rot"   // rotator form, subject bridge:slack-bridge
	bmsJira        = "bms-jira-bridge-tok"    // another bridge
	bmsAgentNamed  = "bms-agent-named-bridge" // an agent token whose subject is "slack-bridge"
	bmsStampedTime = "2026-10-20T12:00:00Z"
)

func newBridgeStampFixture(t *testing.T) *commonsOriginFixture {
	t.Helper()
	f := newCommonsOriginFixture(t)
	f.fc.RegisterBridgeToken(bmsBridge, slackBridgeFleet, "")
	f.fc.RegisterBridgeToken(bmsBridgeRot, slackBridgeFleet, "bridge:slack-bridge")
	f.fc.RegisterBridgeToken(bmsJira, "jira-bridge", "")
	f.fc.RegisterTokenFull(bmsAgentNamed, "agent-gd-agentx", "gardenio", nil, nil, time.Time{}, slackBridgeFleet, false)
	return f
}

// seedAgentMessage seeds a message agent gardenio-crew-x posted to the
// Commons, not yet projected into Slack.
func (f *commonsOriginFixture) seedAgentMessage(id string) {
	f.seedAdaMessage(id, map[string]any{"origin": nil, "author": nil, "slack_channel": nil, "slack_ts": nil})
	f.ms.beads[id].CreatedBy = "gardenio-crew-x"
}

func TestBridgeMessageStamp_HTTP(t *testing.T) {
	f := newBridgeStampFixture(t)
	noRef := map[string]any{"slack_channel": nil, "slack_ts": nil}
	handled := map[string]any{"handled_at": bmsStampedTime}
	ref := map[string]any{"slack_channel": "C0OUT", "slack_ts": "1690000000.000200"}
	for _, c := range []struct {
		name, token string
		agent       bool           // seed an agent's message, else ada's Commons message
		seed        map[string]any // over the seed's fields; nil value drops one
		fields      map[string]any
		extra       map[string]any // non-field columns in the patch
		want        int
	}{
		// The bridge's own stamps (the three live 403s).
		{"bridge stamps handled_at on a person's reply", bmsBridge, false, nil, handled, nil, http.StatusOK},
		{"bridge stamps the Slack ref on a person's message", bmsBridge, false, noRef, ref, nil, http.StatusOK},
		{"bridge stamps the Slack ref on an agent's message", bmsBridge, true, nil, ref, nil, http.StatusOK},
		{"bridge stamps handled_at on an agent's message", bmsBridge, true, nil, handled, nil, http.StatusOK},
		{"bridge echoes the Slack ref and stamps handled_at", bmsBridge, false, nil,
			map[string]any{"slack_channel": "C0OUT", "slack_ts": "1690000000.000100", "handled_at": bmsStampedTime}, nil, http.StatusOK},
		{"bridge on its rotator token", bmsBridgeRot, true, nil, ref, nil, http.StatusOK},
		{"bridge fills a handled_at left empty", bmsBridge, true, map[string]any{"handled_at": ""}, handled, nil, http.StatusOK},

		// Set once: no moving, clearing or re-dating what is there.
		{"bridge re-dates handled_at", bmsBridge, true, handled, map[string]any{"handled_at": "2026-10-21T00:00:00Z"}, nil, http.StatusForbidden},
		{"bridge clears handled_at", bmsBridge, true, handled, map[string]any{"handled_at": ""}, nil, http.StatusForbidden},
		{"bridge moves an agent's Slack ref", bmsBridge, true, map[string]any{"slack_channel": "C0OUT", "slack_ts": "1.1"},
			map[string]any{"slack_ts": "2.2"}, nil, http.StatusForbidden},
		{"bridge clears an agent's Slack ref", bmsBridge, true, map[string]any{"slack_channel": "C0OUT", "slack_ts": "1.1"},
			map[string]any{"slack_ts": ""}, nil, http.StatusForbidden},
		{"bridge moves a person's Slack ref", bmsBridge, false, nil, map[string]any{"slack_ts": "2.2"}, nil, http.StatusForbidden},
		{"bridge stamps an empty value", bmsBridge, true, nil, map[string]any{"handled_at": ""}, nil, http.StatusForbidden},
		{"bridge stamps a non-string", bmsBridge, true, nil, map[string]any{"handled_at": 1}, nil, http.StatusForbidden},

		// Those fields only, and no column.
		{"bridge rewrites a person's words", bmsBridge, false, nil, map[string]any{"text": "forged"}, nil, http.StatusForbidden},
		{"bridge rewrites an agent's words", bmsBridge, true, nil, map[string]any{"text": "forged"}, nil, http.StatusForbidden},
		{"bridge stamps a field of its own", bmsBridge, true, nil, map[string]any{"slack_attachment_count": "0"}, nil, http.StatusForbidden},
		{"bridge stamps and renames", bmsBridge, true, nil, handled, map[string]any{"title": "forged"}, http.StatusForbidden},
		{"bridge stamps and reopens", bmsBridge, true, nil, handled, map[string]any{"status": "open"}, http.StatusForbidden},

		// Nobody else gets the carve-out.
		{"agent stamps handled_at on a person's reply", coAgent, false, nil, handled, nil, http.StatusForbidden},
		// An agent's own message passes ownership, not the carve-out.
		{"agent stamps the Slack ref on its own message", coAgent, true, nil, ref, nil, http.StatusOK},
		{"agent token named slack-bridge", bmsAgentNamed, false, nil, handled, nil, http.StatusForbidden},
		{"jira bridge stamps handled_at", bmsJira, false, nil, handled, nil, http.StatusForbidden},
		{"jira bridge stamps the Slack ref", bmsJira, true, nil, ref, nil, http.StatusForbidden},
		{"land stamps handled_at", coLand, false, nil, handled, nil, http.StatusForbidden},
	} {
		id := "gd-bms-" + strings.ReplaceAll(c.name, " ", "-")
		if c.agent {
			f.seedAgentMessage(id)
			f.applySeed(id, c.seed)
		} else {
			f.seedAdaMessage(id, c.seed)
		}
		body := map[string]any{"fields": c.fields}
		for k, v := range c.extra {
			body[k] = v
		}
		before := string(f.ms.beads[id].Fields)
		code, out := f.do(t, c.token, "", http.MethodPatch, "/v1/beads/"+id, body)
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, out, c.want)
			continue
		}
		after := f.fieldsOf(id)
		if code == http.StatusForbidden {
			if string(f.ms.beads[id].Fields) != before {
				t.Errorf("%s: refused, but the message changed", c.name)
			}
			continue
		}
		for k, v := range c.fields {
			if after[k] != v {
				t.Errorf("%s: %s = %v after the stamp, want %v", c.name, k, after[k], v)
			}
		}
	}
}

// applySeed overlays seed on a seeded message's fields; a nil value drops one.
func (f *commonsOriginFixture) applySeed(id string, seed map[string]any) {
	if len(seed) == 0 {
		return
	}
	fields := f.fieldsOf(id)
	for k, v := range seed {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	f.ms.beads[id].Fields, _ = json.Marshal(fields)
}

func (f *commonsOriginFixture) fieldsOf(id string) map[string]any {
	var fields map[string]any
	_ = json.Unmarshal(f.ms.beads[id].Fields, &fields)
	if fields == nil {
		fields = map[string]any{}
	}
	return fields
}

// The bridge's daemon client is HTTP, but the gRPC update path runs the same
// updateBead and must agree.
func TestBridgeMessageStamp_GRPC(t *testing.T) {
	f := newBridgeStampFixture(t)
	update := func(token, id, fields string) error {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
		_, err := AuthInterceptor(withTestToken(f.fc, coStatic), f.verifier)(ctx,
			&beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/UpdateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.UpdateBead(ctx, req.(*beadsv1.UpdateBeadRequest))
			})
		return err
	}
	f.seedAdaMessage("gd-bmsg-handled", nil)
	if err := update(bmsBridge, "gd-bmsg-handled", `{"handled_at":"`+bmsStampedTime+`"}`); err != nil {
		t.Errorf("bridge stamps handled_at: %v", err)
	}
	f.seedAgentMessage("gd-bmsg-ref")
	if err := update(bmsBridge, "gd-bmsg-ref", `{"slack_channel":"C0OUT","slack_ts":"1.2"}`); err != nil {
		t.Errorf("bridge stamps the Slack ref on an agent's message: %v", err)
	}
	f.seedAgentMessage("gd-bmsg-text")
	if err := update(bmsBridge, "gd-bmsg-text", `{"text":"forged"}`); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bridge rewrites an agent's words: err = %v, want PermissionDenied", err)
	}
	f.seedAdaMessage("gd-bmsg-jira", nil)
	if err := update(bmsJira, "gd-bmsg-jira", `{"handled_at":"`+bmsStampedTime+`"}`); status.Code(err) != codes.PermissionDenied {
		t.Errorf("jira bridge stamps handled_at: err = %v, want PermissionDenied", err)
	}
}

// The carve-out is for messages only: the same stamp on a task the bridge did
// not create is ownership-gated.
func TestBridgeMessageStamp_MessagesOnly(t *testing.T) {
	ctx := fleetCtxWithFleet(slackBridgeFleet, slackBridgeFleet)
	patch := &updateBeadInput{Fields: json.RawMessage(`{"handled_at":"` + bmsStampedTime + `"}`)}
	task := &model.Bead{ID: "gd-t", Type: model.TypeTask, CreatedBy: "ada@example.com", Fields: json.RawMessage(`{}`)}
	if authorizeBridgeMessageStamp(ctx, task, patch) {
		t.Error("the bridge may stamp handled_at on a task")
	}
	msg := &model.Bead{ID: "gd-m", Type: typeMessage, CreatedBy: "ada@example.com", Fields: json.RawMessage(`{}`)}
	if !authorizeBridgeMessageStamp(ctx, msg, patch) {
		t.Error("the bridge may not stamp handled_at on a message")
	}
	if authorizeBridgeMessageStamp(fleetCtxWithFleet("agent-gd-x", slackBridgeFleet), msg, patch) {
		t.Error("an agent token whose subject is slack-bridge passed as the bridge")
	}
	if authorizeBridgeMessageStamp(slackBridgeMachineCtx(), msg, patch) {
		t.Error("the slack-bridge machine principal passed as the bridge fleet token")
	}
	if authorizeBridgeMessageStamp(context.Background(), msg, patch) {
		t.Error("no identity passed as the bridge")
	}
	// A no-op stamp buys nothing.
	stamped := &model.Bead{ID: "gd-m2", Type: typeMessage, Fields: json.RawMessage(`{"handled_at":"` + bmsStampedTime + `"}`)}
	if authorizeBridgeMessageStamp(ctx, stamped, patch) {
		t.Error("a no-op patch passed the carve-out")
	}
}
