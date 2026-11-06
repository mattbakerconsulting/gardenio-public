package credstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// backend abstracts persistence for the credential store. Two
// implementations exist: fileBackend (a single JSON blob at 0600) and
// keyringBackend (one OS-secret-store entry per daemon URL + a small
// non-sensitive index file).
type backend interface {
	load() (storeData, error)
	save(storeData) error
	describe() string
}

// fileBackend persists the entire storeData as a single JSON file at 0600.
// This is the historical layout and the fallback when the OS secret store
// is unavailable.
type fileBackend struct {
	path string
}

func (b *fileBackend) load() (storeData, error) {
	var d storeData
	raw, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("reading credentials: %w", err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("parsing credentials: %w", err)
	}
	return d, nil
}

func (b *fileBackend) save(d storeData) error {
	return writeJSONAtomic(b.path, d)
}

func (b *fileBackend) describe() string {
	return "file " + b.path
}

// writeJSONAtomic marshals v and writes it to path via tempfile + rename
// with 0600 perms and a 0700 parent dir.
func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}
