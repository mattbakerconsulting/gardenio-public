package main

import (
	"strings"
	"testing"
)

// TestCreateCmd_DocWithoutParent_Warns is the end-to-end half of
// ka-0000000755: the lint must actually be REACHED from `kd create
// --type=doc`. A correct lintOrphanDoc that nothing calls closes nothing.
func TestCreateCmd_DocWithoutParent_Warns(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "doc")
	setFlag(t, "project", "gardenio")
	defer resetCreateFlags(t)

	var stderr string
	_ = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := createCmd.RunE(createCmd, []string{"a deliverable doc"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "gb bundle add") {
		t.Errorf("expected the orphan-doc warning on stderr, got %q", stderr)
	}
	// The warning is ADVISORY: the bead must still be created. Turning this
	// into a hard error would break every legitimate standalone doc.
	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Errorf("expected the doc to still be created, got %d CreateBead calls", got)
	}
}

// TestCreateCmd_DocWithExplicitParent_Quiet guards the false-positive side:
// once the author has said where the doc goes, kd must not nag. Critically
// this asserts on the flag being CHANGED, which is the distinction that makes
// the lint useful in an agent pod at all — --parent-task defaults to
// $BOAT_TASK_ID, so a value-based check would be silently disabled there,
// which is exactly the environment the bug was found in.
func TestCreateCmd_DocWithExplicitParent_Quiet(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "doc")
	setFlag(t, "project", "gardenio")
	setFlag(t, "parent-task", "gd-L3JsKCdw1i")
	defer resetCreateFlags(t)
	defer func() { _ = createCmd.Flags().Set("parent-task", "") }()

	var stderr string
	_ = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := createCmd.RunE(createCmd, []string{"a bundle doc"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.Contains(stderr, "gb bundle add") {
		t.Errorf("explicit --parent-task should silence the warning, got %q", stderr)
	}
}

// TestCreateCmd_NonDoc_Quiet keeps the lint scoped: a task create is the
// overwhelmingly common path and must stay silent.
func TestCreateCmd_NonDoc_Quiet(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "task")
	setFlag(t, "project", "gardenio")
	defer resetCreateFlags(t)

	var stderr string
	_ = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := createCmd.RunE(createCmd, []string{"a normal task"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.Contains(stderr, "gb bundle add") {
		t.Errorf("non-doc create should not warn, got %q", stderr)
	}
}
