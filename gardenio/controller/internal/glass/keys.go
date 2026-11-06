package glass

import (
	"strings"
)

// Named keys (spec §3.2 GL-9): what an attached person may send as keys, and
// which of them ask first.

// destructiveKeys are the keys that can stop or suspend what the agent is
// doing: a batch carrying one is sent only once the person confirms it
// (beads-viewer's agent page asks the same, agent_phase3.go). Esc is one:
// Claude Code reads it as "interrupt" (its own status line says "esc to
// interrupt"), so it asks first like Ctrl-C (rubric C8, gd-bWtNrk4snt).
// Each is a key coop types (IsKeyName): Ctrl-\ is not one, coop names Ctrl and
// a letter only, so it is not listed and cannot be sent (gd-HCcFZAc9Rj).
// Lowercase.
var destructiveKeys = map[string]bool{"ctrl-c": true, "ctrl-d": true, "ctrl-z": true, "esc": true, "escape": true}

// IsDestructiveKey reports whether any key in keys asks to be confirmed. One
// in a batch holds the whole batch: it is confirmed whole or not sent.
func IsDestructiveKey(keys []string) bool {
	for _, k := range keys {
		if destructiveKeys[strings.ToLower(strings.TrimSpace(k))] {
			return true
		}
	}
	return false
}

// namedKeys are the key names coop's encode_key types into the PTY
// (coop/crates/cli/src/transport/mod.rs), Ctrl-<letter> aside. Lowercase.
var namedKeys = map[string]bool{
	"enter": true, "return": true, "tab": true, "escape": true, "esc": true, "backspace": true, "delete": true, "del": true,
	"up": true, "down": true, "right": true, "left": true, "home": true, "end": true, "pageup": true, "page_up": true,
	"pagedown": true, "page_down": true, "insert": true, "space": true,
	"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true, "f7": true, "f8": true, "f9": true, "f10": true, "f11": true, "f12": true,
}

// IsKeyName reports whether coop can type the key named k: one of its named
// keys, or Ctrl and a letter ("Ctrl-C"), in any case.
func IsKeyName(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	if namedKeys[k] {
		return true
	}
	c, ok := strings.CutPrefix(k, "ctrl-")
	return ok && len(c) == 1 && c[0] >= 'a' && c[0] <= 'z'
}
