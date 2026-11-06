package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// preflightExtensions scans embedded migration up-files for CREATE EXTENSION
// directives and verifies that each referenced extension is present in
// pg_available_extensions before migrate.Up() runs. Fails early with a
// specific error message (listing the extension and the migrations that
// need it) rather than letting golang-migrate leave schema_migrations in
// a dirty state.
//
// Only scans migrations with version > currentVersion, so that extensions
// used by already-applied migrations don't trigger false positives on
// later-version databases whose postgres image has dropped the extension.
//
// Filed as ka-0000005888 after the 2026-04-22 incident, where migration
// 005_bead_embeddings silently failed on vanilla postgres (no pgvector)
// and converted that into a 12h CrashLoopBackOff.
func preflightExtensions(db *sql.DB, sourceFS fs.FS, currentVersion int) error {
	required, err := scanRequiredExtensions(sourceFS, currentVersion)
	if err != nil {
		return fmt.Errorf("preflight: scan migrations: %w", err)
	}
	if len(required) == 0 {
		return nil
	}

	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)

	var missing []string
	for _, name := range names {
		var defaultVersion sql.NullString
		err := db.QueryRow(
			"SELECT default_version FROM pg_available_extensions WHERE name = $1",
			name,
		).Scan(&defaultVersion)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !defaultVersion.Valid) {
			missing = append(missing, fmt.Sprintf("%s (needed by %s)", name, strings.Join(required[name], ", ")))
			continue
		}
		if err != nil {
			return fmt.Errorf("preflight: query pg_available_extensions for %q: %w", name, err)
		}
		slog.Debug("preflight: extension available",
			"name", name,
			"default_version", defaultVersion.String,
			"migrations", required[name])
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"preflight: postgres is missing required extension(s): %s — install them (e.g. switch postgres image to pgvector/pgvector:pg17 for pgvector-backed migrations) and retry",
			strings.Join(missing, "; "),
		)
	}
	return nil
}

// createExtensionRe matches `CREATE EXTENSION [IF NOT EXISTS] name` with
// either a bare identifier or a double-quoted name. Quoted names allow
// hyphens (e.g. "uuid-ossp") which are legal Postgres identifiers but not
// legal in bare form. Case-insensitive per (?i).
var createExtensionRe = regexp.MustCompile(`(?i)\bCREATE\s+EXTENSION\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"([A-Za-z_][A-Za-z0-9_\-]*)"|([A-Za-z_][A-Za-z0-9_]*))`)

// migrationFileRe parses `NNN_name.up.sql` filenames and captures the version.
var migrationFileRe = regexp.MustCompile(`^(\d+)_.+\.up\.sql$`)

// scanRequiredExtensions walks `migrations/*.up.sql` in sourceFS and returns
// a map of extension-name → migration-files that reference it. Only files
// with version > currentVersion are included.
func scanRequiredExtensions(sourceFS fs.FS, currentVersion int) (map[string][]string, error) {
	entries, err := fs.ReadDir(sourceFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	required := make(map[string][]string)
	for _, e := range entries {
		name := e.Name()
		m := migrationFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[1])
		if err != nil || version <= currentVersion {
			continue
		}
		content, err := fs.ReadFile(sourceFS, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		for _, ext := range findCreateExtensions(content) {
			required[ext] = append(required[ext], name)
		}
	}
	return required, nil
}

// findCreateExtensions scans SQL for CREATE EXTENSION directives, skipping
// lines that are fully within single-line -- comments. Returns lowercased
// extension names (Postgres treats them case-insensitively).
func findCreateExtensions(sqlBytes []byte) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(string(sqlBytes), "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		for _, match := range createExtensionRe.FindAllStringSubmatch(line, -1) {
			// Submatch 1 = quoted-name capture (may contain hyphens);
			// submatch 2 = bare identifier. Exactly one is non-empty per match.
			name := match[1]
			if name == "" {
				name = match[2]
			}
			name = strings.ToLower(name)
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}
