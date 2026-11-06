// Package buildinfo exposes the build-time version/commit/branch/date of the
// kbeads binaries (the kd CLI and the daemon it embeds via `kd serve`).
//
// Values are injected at link time with:
//
//	-ldflags "-X gardenio/kbeads/internal/buildinfo.Version=<calver>
//	          -X gardenio/kbeads/internal/buildinfo.Commit=<sha>
//	          -X gardenio/kbeads/internal/buildinfo.Branch=<branch>
//	          -X gardenio/kbeads/internal/buildinfo.Date=<rfc3339>"
//
// Anything not set at link time is backfilled from the Go toolchain's embedded
// VCS stamps (runtime/debug.ReadBuildInfo), so even a plain `go build ./cmd/kd`
// reports the commit + build time. This is the single source of truth for
// `kd version`/`kd --version` and for the X-Kbeads-Version response header the
// daemon stamps so stale clients can detect version skew (ka-0000002965).
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// SkewThresholdDays is how far behind — in calendar days, per the calver
// YYYY.DDD day-of-year scheme — the local binary must be before the CLI warns
// about version skew. Set to two weeks: below this, a client re-discovering a
// fixed bug is unlikely; above it, the friction report's "stale laptop binary"
// failure mode (ka-0000002965) becomes real.
const SkewThresholdDays = 14

// These are overwritten at build time via -ldflags -X. Leaving them empty is
// the "developer build" default; Get() fills the gaps from ReadBuildInfo.
var (
	Version = ""
	Commit  = ""
	Branch  = ""
	Date    = ""
)

// Seed backfills the package ldflag vars from a caller's own build-time vars,
// filling only the fields still empty. It exists so the CLIs can keep their
// pre-existing `-X main.Version=… -X main.Commit=…` ldflags (kd) and the
// shared `-X main.version=… -X main.commit=…` LDFLAGS (gb, shared with the
// controller + bridges) working without rewiring every build file: main
// declares those vars, hands them here, and buildinfo becomes the single
// source of truth. Values injected directly into buildinfo.* win over Seed.
func Seed(version, commit string) {
	if Version == "" {
		Version = version
	}
	if Commit == "" {
		Commit = commit
	}
}

// Info is a resolved snapshot of the build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Branch    string `json:"branch,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
	Modified  bool   `json:"modified,omitempty"`
}

// Get resolves the effective build info: ldflag-injected values win, with the
// commit/date/dirty flag backfilled from the Go toolchain's VCS stamps when a
// value was not injected. Version falls back to "dev" and Commit to "unknown"
// so the output never prints an empty field.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Branch:    Branch,
		Date:      Date,
		GoVersion: runtime.Version(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	// A full git SHA is noise on a status line; 12 chars is unambiguous.
	if len(info.Commit) > 12 {
		info.Commit = info.Commit[:12]
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	return info
}

// calverDays converts a calver version (YYYY.DDD[.N]) to an absolute day
// count (year*366 + day-of-year) for coarse ordering. Returns (0, false) for
// any value that is not a parseable calver — a "dev" build, a git-describe
// tag, or an empty string — so callers can skip the skew check rather than
// warn on noise.
func calverDays(v string) (int, bool) {
	v = strings.TrimSpace(v)
	// Tolerate a leading "v" and a "-dirty"/"-rc" suffix from git-describe.
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, false
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 2000 || year > 3000 {
		return 0, false
	}
	doy, err := strconv.Atoi(parts[1])
	if err != nil || doy < 1 || doy > 366 {
		return 0, false
	}
	return year*366 + doy, true
}

// SkewCheck compares this binary's version against a daemon's reported version
// and returns a one-line, human-readable warning when the local binary is
// materially behind (more than SkewThresholdDays calver-days older). The
// second return is false when there is nothing to warn about — versions match,
// either side is not a parseable calver (dev build / unknown daemon), or the
// gap is within threshold. Callers print the message to stderr, once.
func SkewCheck(daemonVersion string) (string, bool) {
	local := Get().Version
	return skewCheck(local, daemonVersion)
}

// skewCheck is the pure comparison, split out for testing.
func skewCheck(localVersion, daemonVersion string) (string, bool) {
	localDays, lok := calverDays(localVersion)
	daemonDays, dok := calverDays(daemonVersion)
	if !lok || !dok {
		return "", false
	}
	gap := daemonDays - localDays
	if gap <= SkewThresholdDays {
		return "", false
	}
	return fmt.Sprintf(
		"warning: your kd/gb CLI (%s) is ~%d days behind the daemon (%s); "+
			"you may be re-discovering already-fixed bugs — update your local binary",
		localVersion, gap, daemonVersion,
	), true
}

// String renders the multi-line human form used by `kd version`. Branch and
// date lines are omitted when unknown so a developer build stays terse.
func (i Info) String() string {
	out := "kd " + i.Version + "\n"
	commit := i.Commit
	if i.Modified {
		commit += " (dirty)"
	}
	out += "  commit:  " + commit + "\n"
	if i.Branch != "" {
		out += "  branch:  " + i.Branch + "\n"
	}
	if i.Date != "" {
		out += "  built:   " + i.Date + "\n"
	}
	out += "  go:      " + i.GoVersion + "\n"
	return out
}
