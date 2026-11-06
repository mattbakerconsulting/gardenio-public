package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newTestBouncer creates a Bouncer backed by a fake K8s dynamic client with
// a pre-populated Traefik middleware resource containing the given sourceRange.
func newTestBouncer(sourceRange []string) *Bouncer {
	mw := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      "test-mw",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"ipWhiteList": map[string]interface{}{
					"sourceRange": toInterfaceSlice(sourceRange),
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	gvr := schema.GroupVersionResource{
		Group:    "traefik.io",
		Version:  "v1alpha1",
		Resource: "middlewares",
	}
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "Middleware"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "MiddlewareList"},
		&unstructured.UnstructuredList{},
	)

	client := dynamicfake.NewSimpleDynamicClient(scheme, mw)
	return NewBouncer(BouncerConfig{
		Client:          client,
		Namespace:       "default",
		MiddlewareNames: []string{"test-mw"},
		Logger:          slog.Default(),
	})
}

func toInterfaceSlice(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// captureEphemeral creates a fake Slack server that captures the text of
// ephemeral messages posted.
func captureEphemeral(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			_ = r.ParseForm()
			texts = append(texts, r.FormValue("text"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	return srv, &texts
}

func TestHandleBouncerCommand_NoBouncer(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// bouncer is nil by default.

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "list",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "not configured") {
		t.Errorf("unexpected message: %s", (*texts)[0])
	}
}

func TestHandleBouncerCommand_NoArgs(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Usage") {
		t.Errorf("expected usage message, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerCommand_UnknownSubcommand(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "unknown",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Usage") {
		t.Errorf("expected usage message, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerList(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32", "192.168.1.0/24"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "list",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	msg := (*texts)[0]
	if !strings.Contains(msg, "IP Whitelist") {
		t.Errorf("expected whitelist header, got: %s", msg)
	}
	if !strings.Contains(msg, "10.0.0.1/32") {
		t.Errorf("expected 10.0.0.1/32 in list, got: %s", msg)
	}
	if !strings.Contains(msg, "192.168.1.0/24") {
		t.Errorf("expected 192.168.1.0/24 in list, got: %s", msg)
	}
	if !strings.Contains(msg, "2 entries") {
		t.Errorf("expected '2 entries' in list, got: %s", msg)
	}
}

func TestHandleBouncerList_Ls(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"1.2.3.4/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "ls",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "1.2.3.4/32") {
		t.Errorf("expected IP in list, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerAdd(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "add 192.168.1.1",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	msg := (*texts)[0]
	if !strings.Contains(msg, "Added") {
		t.Errorf("expected 'Added' confirmation, got: %s", msg)
	}
	if !strings.Contains(msg, "192.168.1.1/32") {
		t.Errorf("expected normalized CIDR in response, got: %s", msg)
	}
}

func TestHandleBouncerAdd_NoIP(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "add",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Usage") {
		t.Errorf("expected usage message, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerAdd_InvalidIP(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "add not-an-ip",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "invalid") {
		t.Errorf("expected error about invalid IP, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerRemove(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32", "192.168.1.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "remove 192.168.1.1",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	msg := (*texts)[0]
	if !strings.Contains(msg, "Removed") {
		t.Errorf("expected 'Removed' confirmation, got: %s", msg)
	}
	if !strings.Contains(msg, "192.168.1.1/32") {
		t.Errorf("expected CIDR in response, got: %s", msg)
	}
}

func TestHandleBouncerRemove_Rm(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32", "192.168.1.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "rm 192.168.1.1",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Removed") {
		t.Errorf("expected 'Removed' confirmation, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerRemove_NoIP(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "remove",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Usage") {
		t.Errorf("expected usage message, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerRemove_NotInList(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "remove 99.99.99.99",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	if !strings.Contains((*texts)[0], "Failed") {
		t.Errorf("expected failure message, got: %s", (*texts)[0])
	}
}

func TestHandleBouncerAdd_WithCIDR(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv, texts := captureEphemeral(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.bouncer = newTestBouncer([]string{"10.0.0.1/32"})

	bot.handleBouncerCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
		Text:      "add 172.16.0.0/16",
	})

	if len(*texts) != 1 {
		t.Fatalf("expected 1 ephemeral message, got %d", len(*texts))
	}
	msg := (*texts)[0]
	if !strings.Contains(msg, "Added") {
		t.Errorf("expected 'Added' confirmation, got: %s", msg)
	}
	if !strings.Contains(msg, "172.16.0.0/16") {
		t.Errorf("expected CIDR notation in response, got: %s", msg)
	}
}
