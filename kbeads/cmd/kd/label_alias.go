package main

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// aliasLabelFlags installs a flag-name normalizer on cmd so the pluralized
// spellings `--labels` / `--labels-any` are accepted as aliases of the
// canonical `--label` / `--label-any` flags.
//
// Why: cobra rejects an unknown flag with a bare `Error: unknown flag:
// --labels` and offers NO "did you mean --label?" suggestion (cobra only
// suggests unknown SUBCOMMANDS, not flags). Agents naturally pluralize to
// `--labels` and commonly pair the call with `2>/dev/null`, so the rejection
// is swallowed and the empty stdout reads as a false all-clear — a real
// session (ka-0000008053) hit this twice, where the correct `-l` form
// returned 7 epics and `--labels` returned 0 (bug ka-0000000987).
//
// The normalizer remaps the alias name to its canonical flag during pflag's
// parse, so `--labels a,b` is indistinguishable from `-l a,b` / `--label a,b`
// and merges with any other `--label` occurrences. It is attached PER COMMAND
// (never global/persistent) and only to commands that register `--label`, so
// `kd jack up`'s genuine, unrelated `--labels` flag is never shadowed.
//
// The `labels-any`→`label-any` and `exclude-labels`→`exclude-label` mappings
// only take effect on commands that actually register the canonical flag
// (currently `list`, and `--label-any` also on `search`). On the other
// commands the alias normalizes to a flag that does not exist and stays an
// "unknown flag" error — identical to what the genuine flag already produces
// there, so attaching the helper everywhere is harmless.
//
// Call this in a command's init() AFTER registering the `--label`
// (and optionally `--label-any`) flags.
func aliasLabelFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "labels":
			name = "label"
		case "labels-any":
			name = "label-any"
		case "exclude-labels":
			name = "exclude-label"
		}
		return pflag.NormalizedName(name)
	})
}
