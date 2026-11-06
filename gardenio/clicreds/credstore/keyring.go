package credstore

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zalando/go-keyring"
)

// keyringService is the service name used for all gardenio secret-store
// entries. Each Context lives at (service=keyringService, account=Server)
// — keying by daemon URL so multiple environments don't clash.
const keyringService = "gardenio"

// keyringIndex is the non-sensitive sidecar persisted next to the OS secret
// store. It holds the current context name + a {name → server} mapping so
// the secret entries (which carry no name metadata of their own) can be
// enumerated. Tokens are NEVER persisted here.
type keyringIndex struct {
	CurrentContext string            `json:"current_context"`
	Contexts       map[string]string `json:"contexts"`
}

// keyringBackend stores each Context as a separate OS-secret-store entry
// (keyed by daemon URL) and persists a small non-sensitive index file
// alongside for context-name → server enumeration. The migrateFrom field
// (typically the legacy credentials.json path) is read on first load when
// the index is empty: any data found is migrated into the keyring and the
// legacy file is renamed to "<path>.migrated" so the old code path is no
// longer the source of truth.
type keyringBackend struct {
	indexPath   string
	migrateFrom string
}

func newKeyringBackend(indexPath, migrateFrom string) *keyringBackend {
	return &keyringBackend{indexPath: indexPath, migrateFrom: migrateFrom}
}

// gzipMagic is the two-byte gzip header (RFC 1952). A JSON document never
// begins with these bytes (it starts with '{' or whitespace), so the prefix
// unambiguously distinguishes gzipped payloads from legacy plaintext ones.
var gzipMagic = []byte{0x1f, 0x8b}

// encodeKeyringValue gzip-compresses a keyring payload. The keyring value is
// a string at the API boundary, but go-keyring stores the underlying bytes
// verbatim, so binary gzip output round-trips cleanly. Compression keeps the
// stored entry under the macOS Keychain ~4KB SecItemAdd cap (ka-0000008122).
func encodeKeyringValue(raw []byte) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// decodeKeyringValue transparently decompresses a keyring payload. Entries
// written before the gzip change are stored as plaintext JSON and lack the
// gzip magic prefix, so they are returned unchanged — keeping existing
// Keychain entries readable after upgrade.
func decodeKeyringValue(raw string) ([]byte, error) {
	b := []byte(raw)
	if !bytes.HasPrefix(b, gzipMagic) {
		// Legacy plaintext entry — return as-is.
		return b, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

func (b *keyringBackend) load() (storeData, error) {
	idx, err := b.readIndex()
	if err != nil {
		return storeData{}, err
	}
	// Migration: if no index yet and a legacy credentials.json exists,
	// pull its data into the keyring and write the index. The legacy file
	// is renamed (not deleted) so operators can recover if migration ever
	// goes sideways.
	if len(idx.Contexts) == 0 && b.migrateFrom != "" {
		if migrated, ok, mErr := b.migrateLegacyFile(); mErr != nil {
			return storeData{}, mErr
		} else if ok {
			return migrated, nil
		}
	}
	data := storeData{
		CurrentContext: idx.CurrentContext,
		Contexts:       make(map[string]*Context, len(idx.Contexts)),
	}
	for name, server := range idx.Contexts {
		raw, err := keyring.Get(keyringService, server)
		if errors.Is(err, keyring.ErrNotFound) {
			// Index drift: keyring entry was deleted out-of-band. Skip
			// it rather than fail; the next save will reconcile.
			continue
		}
		if err != nil {
			return storeData{}, fmt.Errorf("reading keyring entry for %s: %w", server, err)
		}
		decoded, err := decodeKeyringValue(raw)
		if err != nil {
			return storeData{}, fmt.Errorf("decoding keyring entry for %s: %w", server, err)
		}
		var c Context
		if err := json.Unmarshal(decoded, &c); err != nil {
			return storeData{}, fmt.Errorf("parsing keyring entry for %s: %w", server, err)
		}
		data.Contexts[name] = &c
	}
	return data, nil
}

func (b *keyringBackend) save(d storeData) error {
	// Snapshot existing index so we can delete keyring entries whose
	// context was dropped or whose Server changed.
	prev, _ := b.readIndex()

	idx := keyringIndex{
		CurrentContext: d.CurrentContext,
		Contexts:       make(map[string]string, len(d.Contexts)),
	}

	// Write each Context to the keyring keyed by its Server. A keyring
	// Set is atomic at the OS level — there is no half-written state to
	// observe on token refresh.
	for name, c := range d.Contexts {
		if c == nil {
			continue
		}
		account := c.Server
		if account == "" {
			// A server-less context cannot be stored by daemon URL.
			// Fall back to keying by the context name to avoid silent
			// data loss; this is the case for some unit tests.
			account = "context:" + name
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("marshaling context %s: %w", name, err)
		}
		// Gzip the JSON before storing. macOS Keychain (Security.framework
		// SecItemAdd) caps kSecValueData at ~4KB; a large id_token plus the
		// other OAuth fields blows past that uncompressed. gzip keeps a single
		// keyring entry per context while landing well under the cap. load() sniffs the gzip magic bytes so
		// pre-existing plaintext entries still decode (ka-0000008122).
		encoded, err := encodeKeyringValue(raw)
		if err != nil {
			return fmt.Errorf("compressing context %s: %w", name, err)
		}
		if err := keyring.Set(keyringService, account, encoded); err != nil {
			return fmt.Errorf("writing keyring entry for %s: %w", account, err)
		}
		idx.Contexts[name] = account
	}

	// Delete entries for contexts that no longer exist or whose Server
	// changed (so the old account isn't left dangling).
	for oldName, oldAccount := range prev.Contexts {
		newAccount, stillPresent := idx.Contexts[oldName]
		if !stillPresent || newAccount != oldAccount {
			// Only delete if no other current context still uses this
			// account (e.g. two names pointing at the same daemon URL).
			inUse := false
			for _, a := range idx.Contexts {
				if a == oldAccount {
					inUse = true
					break
				}
			}
			if !inUse {
				_ = keyring.Delete(keyringService, oldAccount)
			}
		}
	}

	return writeJSONAtomic(b.indexPath, idx)
}

func (b *keyringBackend) describe() string {
	return "OS secret store (service=" + keyringService + ") + index " + b.indexPath
}

// readIndex loads the non-sensitive index file. Missing file => empty
// index (first-time use).
func (b *keyringBackend) readIndex() (keyringIndex, error) {
	idx := keyringIndex{Contexts: map[string]string{}}
	raw, err := os.ReadFile(b.indexPath)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return idx, fmt.Errorf("reading credstore index: %w", err)
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return idx, fmt.Errorf("parsing credstore index: %w", err)
	}
	if idx.Contexts == nil {
		idx.Contexts = map[string]string{}
	}
	return idx, nil
}

// migrateLegacyFile reads the legacy credentials.json and rewrites the
// data into the keyring + index. Returns (data, true, nil) on a successful
// migration; (zero, false, nil) when there is nothing to migrate.
func (b *keyringBackend) migrateLegacyFile() (storeData, bool, error) {
	raw, err := os.ReadFile(b.migrateFrom)
	if os.IsNotExist(err) {
		return storeData{}, false, nil
	}
	if err != nil {
		return storeData{}, false, fmt.Errorf("reading legacy credentials: %w", err)
	}
	var d storeData
	if err := json.Unmarshal(raw, &d); err != nil {
		return storeData{}, false, fmt.Errorf("parsing legacy credentials: %w", err)
	}
	if len(d.Contexts) == 0 {
		// Empty legacy file — drop it on the floor; the keyring is the
		// new source of truth.
		_ = os.Rename(b.migrateFrom, b.migrateFrom+".migrated")
		return storeData{}, false, nil
	}
	if d.Contexts == nil {
		d.Contexts = map[string]*Context{}
	}
	if err := b.save(d); err != nil {
		return storeData{}, false, fmt.Errorf("migrating legacy credentials into keyring: %w", err)
	}
	// Rename rather than delete so operators can recover from a botched
	// migration. A subsequent migrateLegacyFile call sees the renamed
	// path and skips.
	if err := os.Rename(b.migrateFrom, b.migrateFrom+".migrated"); err != nil {
		// Non-fatal: data is now in the keyring and the index will
		// short-circuit future migrations. Surface a warning so the
		// operator can clean up by hand.
		fmt.Fprintf(os.Stderr, "warning: credstore: could not rename legacy %s: %v\n", b.migrateFrom, err)
	}
	return d, true, nil
}
