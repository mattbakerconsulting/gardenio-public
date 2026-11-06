// Package localreg tracks locally-spawned `gb agent start --local` sessions
// via a small on-disk JSON registry.
//
// Each live local agent owns one file at
//
//	$GARDENIO_HOME/local/<name>.json      (default: ~/.gardenio/local/<name>.json)
//
// The registry exists so that multiple local agents can coexist on one host
// without colliding on names, and so operators can enumerate them via
// `gb agent list --local` even when the kbeads daemon is unreachable.
//
// This epic deliberately does NOT build multi-spawn UX. It only ensures the
// layout does not preclude it: entries are keyed by a per-spawn name and the
// disambiguator helper suffixes a numeric tag on collision.
package localreg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Entry is a single live local-agent registration.
type Entry struct {
	Name      string    `json:"name"`
	BeadID    string    `json:"bead_id"`
	Role      string    `json:"role"`
	Project   string    `json:"project,omitempty"`
	Dir       string    `json:"dir"`
	PID       int       `json:"pid"`
	CoopURL   string    `json:"coop_url,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// RunnerID and Owner are set when `gb runner join` started the agent
	// (spec §5): the runner bead it runs on and the operator it acts for.
	RunnerID string `json:"runner_id,omitempty"`
	Owner    string `json:"owner,omitempty"`
}

// Dir returns the directory that holds registry entries. It does not create
// the directory; callers that need it materialized should call EnsureDir.
func Dir() string {
	if d := os.Getenv("GARDENIO_HOME"); d != "" {
		return filepath.Join(d, "local")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fall back to a tmpdir rather than crashing; callers will get a
		// sensible error from the first file op if this path isn't usable.
		return filepath.Join(os.TempDir(), "gardenio-local")
	}
	return filepath.Join(home, ".gardenio", "local")
}

// EnsureDir creates the registry directory with 0700 permissions if it does
// not already exist.
func EnsureDir() (string, error) {
	d := Dir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", d, err)
	}
	return d, nil
}

// Path returns the file path for a named registry entry.
func Path(name string) string {
	return filepath.Join(Dir(), safeName(name)+".json")
}

// safeName maps an agent name to a filesystem-safe slug. Disallowed chars are
// replaced with '-' so a user-supplied name cannot escape the registry dir.
func safeName(name string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", "..", "-", string(os.PathSeparator), "-")
	s := r.Replace(name)
	s = strings.TrimSpace(s)
	if s == "" {
		s = "unnamed"
	}
	return s
}

// Register writes entry atomically to the registry. Any existing file for
// the same name is overwritten.
func Register(entry Entry) (string, error) {
	if entry.Name == "" {
		return "", fmt.Errorf("localreg.Register: Name is required")
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = time.Now().UTC()
	}
	if _, err := EnsureDir(); err != nil {
		return "", err
	}
	final := Path(entry.Name)
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal entry: %w", err)
	}
	tmp, err := os.CreateTemp(Dir(), ".localreg-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename %s: %w", final, err)
	}
	return final, nil
}

// Deregister removes the entry for name. Missing entry is not an error.
func Deregister(name string) error {
	if name == "" {
		return nil
	}
	err := os.Remove(Path(name))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// Read returns the entry for name, or an error if the entry does not exist
// or is malformed.
func Read(name string) (Entry, error) {
	var e Entry
	data, err := os.ReadFile(Path(name))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("parse %s: %w", Path(name), err)
	}
	return e, nil
}

// List returns all registry entries sorted by StartedAt ascending. Malformed
// files are skipped with their paths returned in the second slice so callers
// can surface them without failing the whole enumeration.
func List() ([]Entry, []string, error) {
	d := Dir()
	dirents, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []Entry
	var bad []string
	for _, de := range dirents {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(d, name)
		data, err := os.ReadFile(p)
		if err != nil {
			bad = append(bad, p)
			continue
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			bad = append(bad, p)
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, bad, nil
}

// Alive reports whether the PID recorded on the entry is still running.
// PID 0 or a negative PID is treated as not alive.
//
// This is a best-effort check using signal 0; it cannot distinguish a
// genuinely-live agent from a recycled PID that now belongs to another
// process. For registry display purposes that's acceptable — a stale entry
// will only mislead until the next `gb agent list --local --prune`.
func Alive(e Entry) bool {
	if e.PID <= 0 {
		return false
	}
	proc, err := os.FindProcess(e.PID)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// Prune removes registry entries whose PIDs are no longer alive, returning
// the names that were removed.
func Prune() ([]string, error) {
	entries, _, err := List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if Alive(e) {
			continue
		}
		if err := Deregister(e.Name); err == nil {
			removed = append(removed, e.Name)
		}
	}
	return removed, nil
}

// PickName returns a name derived from base that does not collide with any
// currently-registered live entry. If base itself is free (or its registered
// entry is dead), base is returned unchanged. Otherwise `-2`, `-3`, ... are
// appended until a free slot is found.
//
// The function does not reserve the returned name; callers should Register
// promptly. Best-effort — a racing second caller could still collide.
func PickName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "local"
	}
	if !nameTaken(base) {
		return base
	}
	for i := 2; i < 10_000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !nameTaken(candidate) {
			return candidate
		}
	}
	// Extremely unlikely; fall back to a time-suffixed name.
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func nameTaken(name string) bool {
	e, err := Read(name)
	if err != nil {
		return false
	}
	return Alive(e)
}
