package gardenhub

import (
	"testing"

	"gardenio/controller/internal/conversation"
)

// The bridge's Commons mirror posts a Glass nudge's copy into the agent's Slack
// thread by this origin (gd-Ik3rTCqfxp), and kbeads declares it.
func TestGlassOriginIsTheMirrorsOrigin(t *testing.T) {
	if GlassOrigin != conversation.OriginGlass {
		t.Fatalf("GlassOrigin = %q, conversation.OriginGlass = %q", GlassOrigin, conversation.OriginGlass)
	}
}
