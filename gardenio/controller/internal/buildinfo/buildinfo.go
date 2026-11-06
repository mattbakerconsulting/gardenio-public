// Package buildinfo exposes the build-time version/commit/branch/date of the
// gardenio binaries (the gb CLI, controller, and bridges).
//
// Values are injected at link time. The gardenio build files already set
// `-X main.version=<calver> -X main.commit=<sha>` (see gardenio/Makefile,
// controller/Makefile, Dockerfiles); main seeds those into this package via
// Seed() so buildinfo is the single source of truth for `gb version`/
// `gb --version` and the version-skew signal (ka-0000002965). Branch/date may
// additionally be injected directly:
//
//	-ldflags "-X gardenio/controller/internal/buildinfo.Branch=<branch>
//	          -X gardenio/controller/internal/buildinfo.Date=<rfc3339>"
//
// Anything not set at link time is backfilled from the Go toolchain's embedded
// VCS stamps (runtime/debug.ReadBuildInfo), so even a plain `go build` reports
// the commit + build time.
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
// about version skew. Two weeks: below this a client re-discovering a fixed
// bug is unlikely; above it, the "stale laptop binary" failure mode
// (ka-0000002965) becomes real.
const SkewThresholdDays = 14

// These are overwritten at build time via Seed() (from main.version/commit) or
// directly via -ldflags -X. Empty is the developer-build default; Get() fills
// the gaps from ReadBuildInfo.
var (
	Version = ""
	Commit  = ""
	Branch  = ""
	Date    = ""
)

// Seed backfills Version/Commit from a caller's own build-time vars, filling
// only fields still empty. It lets the gardenio CLIs keep their pre-existing
// `-X main.version=… -X main.commit=…` LDFLAGS (shared across gb, controller,
// and every bridge) working without rewiring the build files.
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

// Get resolves the effective build info: ldflag/seeded values win, with the
// commit/date/dirty flag backfilled from the Go toolchain's VCS stamps when a
// value was not injected. Version falls back to "dev" and Commit to "unknown".
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

// String renders the multi-line human form used by `gb version`. Branch and
// date lines are omitted when unknown so a developer build stays terse.
func (i Info) String() string {
	out := "gb " + i.Version + "\n"
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

// calverDays converts a calver version (YYYY.DDD[.N]) to an absolute day count
// (year*366 + day-of-year) for coarse ordering. Returns (0, false) for any
// value that is not a parseable calver so callers can skip the skew check.
func calverDays(v string) (int, bool) {
	v = strings.TrimSpace(v)
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
// and returns a one-line warning when the local binary is materially behind
// (more than SkewThresholdDays calver-days older). The second return is false
// when there is nothing to warn about. Callers print it to stderr, once.
func SkewCheck(daemonVersion string) (string, bool) {
	return skewCheck(Get().Version, daemonVersion)
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
