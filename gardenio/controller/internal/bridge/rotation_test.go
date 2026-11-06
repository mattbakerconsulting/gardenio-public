package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

// fakeRotationNotifier returns a fixed error from every Notify* call so we can
// assert how rotation.handleMessage logs the failure.
type fakeRotationNotifier struct {
	err error
}

func (f *fakeRotationNotifier) NotifyAccountRotating(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyAccountRotated(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyNoFallback(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifySwitchback(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyAccountThrottled(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyAccountRecovered(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyAccountWarming(context.Context, RotationEvent) error {
	return f.err
}
func (f *fakeRotationNotifier) NotifyAccountCooled(context.Context, RotationEvent) error {
	return f.err
}
func TestRotation_HandleMessage_DemotesEmptyProjectFailureToWarn(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		wantLevel string // "WARN" or "ERROR"
	}{
		{name: "empty project demoted to warn", project: "", wantLevel: "WARN"},
		{name: "non-empty project stays error", project: "myproject", wantLevel: "ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			r := &Rotation{
				cfg: RotationConfig{
					Notifier: &fakeRotationNotifier{err: errors.New("post account warming to Slack: channel_not_found")},
					Logger:   logger,
				},
			}

			evt := RotationEvent{
				Event:       "account_warming",
				Project:     tc.project,
				FromAccount: "acct-a",
			}
			payload, err := json.Marshal(evt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			r.handleMessage(&nats.Msg{Subject: "gardenio.rotation.account_warming", Data: payload})

			out := buf.String()
			if !strings.Contains(out, `"level":"`+tc.wantLevel+`"`) {
				t.Errorf("expected %s log entry, got: %s", tc.wantLevel, out)
			}
			// Sanity: the wrong level should not be present.
			otherLevel := "ERROR"
			if tc.wantLevel == "ERROR" {
				otherLevel = "WARN"
			}
			if strings.Contains(out, `"level":"`+otherLevel+`"`) {
				t.Errorf("unexpected %s log entry in output: %s", otherLevel, out)
			}
		})
	}
}
