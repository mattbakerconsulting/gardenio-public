package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ka-0000006651 / gd-hfFzGxupw9: `kd show <non-bead-id>` used to die with a
// bare "bead not found" even when the arg was an agent name or title that
// `kd search` resolves as the top hit. These tests lock in the hint contract:
//   - a name-like arg (no gd- prefix) that 404s gets a `kd search` hint
//     appended to the error, on both the single-id and batch surfaces;
//   - a gd-prefixed arg keeps the pre-existing error surface byte-for-byte
//     (no hint) — the "getting bead <id>: " prefix contract is untouched;
//   - non-not-found errors (auth, transport) never grow the hint.

const searchHintMarker = "kd search"

// TestShow_SingleID_NameLikeArg_HintsSearch: an agent-name arg that 404s must
// keep the stable "getting bead <arg>: " prefix AND gain the kd search hint.
func TestShow_SingleID_NameLikeArg_HintsSearch(t *testing.T) {
	mc := newMockClient()
	arg := "gardenio-tarboat-202620010-byo-no8"
	wireShowMock(t, mc)

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{arg})
	})
	if runErr == nil {
		t.Fatalf("expected an error for a missing name-like arg; got nil")
	}
	if !strings.HasPrefix(runErr.Error(), "getting bead "+arg+": ") {
		t.Errorf("single-id error prefix contract broken; got %q", runErr.Error())
	}
	if !strings.Contains(runErr.Error(), searchHintMarker) {
		t.Errorf("name-like arg should hint kd search; got %q", runErr.Error())
	}
	if !strings.Contains(runErr.Error(), "kd search '"+arg+"'") {
		t.Errorf("hint should quote the original arg for copy-paste; got %q", runErr.Error())
	}
	if out != "" || errOut != "" {
		t.Errorf("single-id miss writes nothing itself; stdout=%q stderr=%q", out, errOut)
	}
}

// TestShow_SingleID_KdPrefixedArg_NoHint: a gd-prefixed miss keeps the
// pre-hint error surface — no suffix of any kind.
func TestShow_SingleID_KdPrefixedArg_NoHint(t *testing.T) {
	mc := newMockClient()
	id := "gd-doesnotexist00"
	wireShowMock(t, mc)

	_, _, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{id})
	})
	if runErr == nil {
		t.Fatalf("expected an error for a missing gd-prefixed id; got nil")
	}
	if strings.Contains(runErr.Error(), searchHintMarker) {
		t.Errorf("gd-prefixed args must not grow the search hint; got %q", runErr.Error())
	}
	want := fmt.Sprintf("getting bead %s: bead %s not found", id, id)
	if runErr.Error() != want {
		t.Errorf("gd-prefixed error surface changed; got %q, want %q", runErr.Error(), want)
	}
}

// TestShow_Batch_NameLikeArg_HintOnStderr: the batch stderr diagnostic for a
// name-like miss carries the same hint; a gd-prefixed sibling miss does not.
func TestShow_Batch_NameLikeArg_HintOnStderr(t *testing.T) {
	mc := newMockClient()
	found := "gd-t3ao12wu57"
	mc.Beads[found] = &model.Bead{ID: found, Title: "i exist", Type: model.TypeTask, Status: model.StatusOpen}
	nameArg := "some-agent-name"
	idArg := "gd-ghost000002"
	wireShowMock(t, mc)

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{found, nameArg, idArg})
	})
	if runErr == nil {
		t.Fatalf("expected a summary error; got nil")
	}
	if !strings.Contains(out, "i exist") {
		t.Errorf("found bead must still render; stdout:\n%s", out)
	}
	var nameLine, idLine string
	for line := range strings.SplitSeq(errOut, "\n") {
		if strings.Contains(line, nameArg) {
			nameLine = line
		}
		if strings.Contains(line, idArg) {
			idLine = line
		}
	}
	if nameLine == "" || idLine == "" {
		t.Fatalf("both misses must be reported on stderr; stderr:\n%s", errOut)
	}
	if !strings.Contains(nameLine, searchHintMarker) {
		t.Errorf("name-like batch miss should hint kd search; got %q", nameLine)
	}
	if strings.Contains(idLine, searchHintMarker) {
		t.Errorf("gd-prefixed batch miss must not grow the hint; got %q", idLine)
	}
}

// TestShow_SingleID_NameLikeArg_NonNotFoundError_NoHint: a transport/auth
// failure on a name-like arg is not a lookup miss — no hint.
func TestShow_SingleID_NameLikeArg_NonNotFoundError_NoHint(t *testing.T) {
	mc := newMockClient()
	mc.GetErr = &client.APIError{StatusCode: http.StatusUnauthorized, Message: "token expired"}
	wireShowMock(t, mc)

	_, _, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{"some-agent-name"})
	})
	if runErr == nil {
		t.Fatalf("expected the injected error; got nil")
	}
	if strings.Contains(runErr.Error(), searchHintMarker) {
		t.Errorf("non-404 errors must not grow the search hint; got %q", runErr.Error())
	}
}

// TestIsBeadNotFound covers the transport-specific signals the hint keys on.
func TestIsBeadNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"http 404", &client.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}, true},
		{"http 401", &client.APIError{StatusCode: http.StatusUnauthorized, Message: "nope"}, false},
		{"wrapped http 404", fmt.Errorf("outer: %w", &client.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}), true},
		{"grpc NotFound", status.Error(codes.NotFound, "bead not found"), true},
		{"grpc Unavailable", status.Error(codes.Unavailable, "conn refused"), false},
		{"plain not-found text", errors.New("bead xyz not found"), true},
		{"unrelated", errors.New("dial tcp: timeout"), false},
	}
	for _, tc := range cases {
		if got := isBeadNotFound(tc.err); got != tc.want {
			t.Errorf("%s: isBeadNotFound(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestShowSearchHint pins the gating: hint only for (non-gd-prefixed arg,
// not-found error) pairs.
func TestShowSearchHint(t *testing.T) {
	notFound := errors.New("bead not found")
	if h := showSearchHint("gd-pwwcs82cnv", notFound); h != "" {
		t.Errorf("gd-prefixed arg must yield no hint; got %q", h)
	}
	if h := showSearchHint("agent-name", errors.New("timeout")); h != "" {
		t.Errorf("non-not-found error must yield no hint; got %q", h)
	}
	h := showSearchHint("agent-name", notFound)
	if !strings.Contains(h, "kd search 'agent-name'") {
		t.Errorf("hint should embed a runnable kd search; got %q", h)
	}
}
