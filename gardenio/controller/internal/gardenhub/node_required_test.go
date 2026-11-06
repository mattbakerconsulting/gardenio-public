package gardenhub

import (
	"os"
	"os/exec"
	"testing"
)

// TestNodeOnPathWhenRequired gates this package's node-script tests, each of which skips when node is not on PATH.
// ci.yml's controller test step installs node and sets GARDENIO_NODE_REQUIRED=1, so a CI job that loses node goes red
// here instead of skipping every one of them silently (gd-8V9KodoXNf).
func TestNodeOnPathWhenRequired(t *testing.T) {
	if os.Getenv("GARDENIO_NODE_REQUIRED") != "1" {
		t.Skip("GARDENIO_NODE_REQUIRED unset: the node-script tests skip without node")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("GARDENIO_NODE_REQUIRED=1 and node is not on PATH (%v): the node-script tests would all skip", err)
	}
}
