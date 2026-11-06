package podmanager

import (
	"encoding/base64"
	"strings"
	"testing"
)

// maxArgStrLen is the Linux kernel cap on a SINGLE argv/envp string passed to
// execve (MAX_ARG_STRLEN = 32 pages = 131072 bytes on a 4 KiB-page system),
// independent of the ARG_MAX total. The old transport handed the entire
// rendered thaw node script to the probe as one `bash -c <script>` argv
// element; once the script crossed this limit the chroot exec fail-closed with
// `exec /usr/sbin/chroot: argument list too long` (E2BIG) BEFORE any restore
// logic ran (bug ka-0000004900, gVisor canary #9 on prod 2026.181.9). The fix
// streams the program over stdin via `bash -s`, which has no such ceiling.
const maxArgStrLen = 131072

// TestThawExecOptions_ScriptNeverInArgv is the core regression guard for the
// E2BIG bug. It asserts that, regardless of script size, thawExecOptions
//   - issues `chroot <path> bash -s` (program read from stdin), NOT `bash -c`,
//   - sets Stdin=true so the caller streams the body in, and
//   - never embeds the script in any argv element (so no single argv string can
//     cross MAX_ARG_STRLEN and trip execve E2BIG).
func TestThawExecOptions_ScriptNeverInArgv(t *testing.T) {
	// A script comfortably larger than MAX_ARG_STRLEN — the multi-container
	// create-then-restore thaw body that broke canary #9 was ~150 KiB.
	bigScript := "set -uo pipefail\n" + strings.Repeat("x", maxArgStrLen+50_000)
	if len(bigScript) <= maxArgStrLen {
		t.Fatalf("test setup: script is %d bytes, expected > MAX_ARG_STRLEN (%d)", len(bigScript), maxArgStrLen)
	}

	opts := thawExecOptions("probe", "/host", bigScript)

	// Transport must be `chroot /host bash -s` exactly — `bash -s` reads the
	// program from stdin. Any `-c` here would reintroduce the argv-size bug.
	wantCmd := []string{"chroot", "/host", "bash", "-s"}
	if len(opts.Command) != len(wantCmd) {
		t.Fatalf("Command = %v, want %v", opts.Command, wantCmd)
	}
	for i := range wantCmd {
		if opts.Command[i] != wantCmd[i] {
			t.Fatalf("Command = %v, want %v", opts.Command, wantCmd)
		}
	}
	if !opts.Stdin {
		t.Error("Stdin must be true so the script streams over stdin (bash -s), not as an argv element")
	}
	for _, c := range opts.Command {
		if c == "-c" {
			t.Error("Command must not use `bash -c` — that passes the script as a single argv element subject to MAX_ARG_STRLEN")
		}
		if strings.Contains(c, bigScript) {
			t.Errorf("argv element %q embeds the script body — this is exactly the E2BIG transport bug", c)
		}
		if len(c) > maxArgStrLen {
			t.Errorf("argv element of %d bytes exceeds MAX_ARG_STRLEN (%d); execve would fail E2BIG", len(c), maxArgStrLen)
		}
	}
}

// TestBuildThawScript_OversizeBodyStreamsViaStdin reproduces the canary-#9
// shape end to end through the transport helper: it renders a thaw script whose
// base64-inlined OCI spec files push the body well past MAX_ARG_STRLEN (the
// real-world cause — freezethaw_node.go write_spec inlines each snapshot
// config.json, scaling with container count), then asserts the rendered body is
// genuinely oversize AND that thawExecOptions keeps every argv element bounded
// (the script rides stdin, not argv). Under the old `bash -c` transport this
// script's exec would have died E2BIG before the restore logic.
func TestBuildThawScript_OversizeBodyStreamsViaStdin(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())

	// Two ~80 KiB base64 spec blobs (sandbox + agent config.json), mirroring a
	// 2-container snapshot — the create-then-restore variant that first crossed
	// the limit on prod. base64 inflates by ~4/3, so ~60 KiB raw each.
	blob := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("S", 60_000)))
	specs := []thawSpecFile{
		{Path: "sandbox/config.json", Base64: blob},
		{Path: "agent/config.json", Base64: blob},
	}

	script := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		SpecFiles:     specs,
		SubContainers: []thawSubContainer{{Name: "agent", ACID: "a"}},
	})

	if len(script) <= maxArgStrLen {
		t.Fatalf("rendered thaw script is %d bytes; expected the inlined specs to push it past MAX_ARG_STRLEN (%d) to exercise the regression", len(script), maxArgStrLen)
	}

	opts := thawExecOptions("probe", "/host", script)
	if !opts.Stdin {
		t.Fatal("oversize thaw script must be delivered via stdin (Stdin=true)")
	}
	for _, c := range opts.Command {
		if len(c) > maxArgStrLen {
			t.Fatalf("argv element of %d bytes exceeds MAX_ARG_STRLEN (%d) — the oversize thaw body leaked into argv (E2BIG)", len(c), maxArgStrLen)
		}
		if strings.Contains(c, "config.json") && strings.Contains(c, blob) {
			t.Fatal("inlined OCI spec leaked into an argv element — must ride stdin")
		}
	}
}
