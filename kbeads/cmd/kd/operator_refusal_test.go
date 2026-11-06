package main

import (
	"errors"
	"fmt"
	"testing"

	"gardenio/kbeads/internal/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDetectOperatorRefusal asserts both transport paths produce a resolver
// source label and a positive match: the typed HTTP error from
// internal/client and a gRPC status whose message carries the canonical JSON
// payload. A regression here would silently downgrade refusals to exit 1
// and break the gardenio harness's loud-surface contract (ka-0000006731).
func TestDetectOperatorRefusal(t *testing.T) {
	t.Run("typed HTTP error", func(t *testing.T) {
		err := &client.OperatorRefusalError{
			StatusCode:     400,
			Message:        "operator could not be resolved",
			ResolverSource: "header_non_human",
		}
		src, ok := detectOperatorRefusal(err)
		if !ok {
			t.Fatal("expected detection")
		}
		if src != "header_non_human" {
			t.Errorf("source: got %q want header_non_human", src)
		}
	})
	t.Run("typed HTTP error wrapped", func(t *testing.T) {
		err := fmt.Errorf("create bead: %w", &client.OperatorRefusalError{
			StatusCode:     400,
			ResolverSource: "header_missing",
		})
		src, ok := detectOperatorRefusal(err)
		if !ok {
			t.Fatal("expected detection through fmt.Errorf wrap")
		}
		if src != "header_missing" {
			t.Errorf("source: got %q want header_missing", src)
		}
	})
	t.Run("typed HTTP error empty source becomes unknown", func(t *testing.T) {
		err := &client.OperatorRefusalError{StatusCode: 400}
		src, ok := detectOperatorRefusal(err)
		if !ok || src != "unknown" {
			t.Errorf("got (%q,%v) want (unknown,true)", src, ok)
		}
	})
	t.Run("gRPC status with embedded JSON", func(t *testing.T) {
		err := status.Error(codes.InvalidArgument,
			`operator could not be resolved {"code":"operator_refusal","resolver_source":"fallback_agent_only"}`)
		src, ok := detectOperatorRefusal(err)
		if !ok {
			t.Fatal("expected detection")
		}
		if src != "fallback_agent_only" {
			t.Errorf("source: got %q want fallback_agent_only", src)
		}
	})
	t.Run("gRPC InvalidArgument without payload is not a refusal", func(t *testing.T) {
		err := status.Error(codes.InvalidArgument, "bead_id is required")
		if _, ok := detectOperatorRefusal(err); ok {
			t.Errorf("did not expect refusal classification for plain validation error")
		}
	})
	t.Run("plain error is not a refusal", func(t *testing.T) {
		err := errors.New("network unreachable")
		if _, ok := detectOperatorRefusal(err); ok {
			t.Errorf("plain error misclassified as refusal")
		}
	})
	t.Run("nil error is not a refusal", func(t *testing.T) {
		if _, ok := detectOperatorRefusal(nil); ok {
			t.Errorf("nil misclassified as refusal")
		}
	})
}

// TestParseGRPCRefusalMessage covers the standalone parser used to peel the
// embedded JSON payload off a gRPC status message string.
func TestParseGRPCRefusalMessage(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		wantSrc   string
		wantMatch bool
	}{
		{
			name:      "canonical embedded payload",
			msg:       `operator could not be resolved {"code":"operator_refusal","resolver_source":"header_missing"}`,
			wantSrc:   "header_missing",
			wantMatch: true,
		},
		{
			name:      "code mismatch is ignored",
			msg:       `something bad {"code":"other_error","resolver_source":"header_missing"}`,
			wantSrc:   "",
			wantMatch: false,
		},
		{
			name:      "missing JSON returns no match",
			msg:       `operator could not be resolved`,
			wantSrc:   "",
			wantMatch: false,
		},
		{
			name:      "empty resolver_source becomes unknown",
			msg:       `operator could not be resolved {"code":"operator_refusal"}`,
			wantSrc:   "unknown",
			wantMatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, ok := parseGRPCRefusalMessage(tc.msg)
			if ok != tc.wantMatch {
				t.Fatalf("match: got %v want %v", ok, tc.wantMatch)
			}
			if src != tc.wantSrc {
				t.Errorf("source: got %q want %q", src, tc.wantSrc)
			}
		})
	}
}

// TestCommandPathFromArgs ensures we emit a sanitized subcommand path with no
// flag values, since flag values may contain free-form text (e.g.
// --description) that could leak operator context.
func TestCommandPathFromArgs(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{argv: nil, want: "kd"},
		{argv: []string{"kd"}, want: "kd"},
		{argv: []string{"kd", "create", "title here", "--type=task"}, want: "kd create title here"},
		{argv: []string{"kd", "comment", "add", "gd-XYZ", "--text", "secret"}, want: "kd comment add gd-XYZ"},
		{argv: []string{"kd", "--json", "list"}, want: "kd"},
	}
	for _, tc := range cases {
		got := commandPathFromArgs(tc.argv)
		if got != tc.want {
			t.Errorf("commandPathFromArgs(%v): got %q want %q", tc.argv, got, tc.want)
		}
	}
}
