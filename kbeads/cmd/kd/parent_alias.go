package main

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// aliasParentTaskFlag installs a flag-name normalizer on cmd so the shorter
// spelling `--parent` is accepted as an alias of the canonical `--parent-task`
// flag on `kd create`.
//
// Why: the parent-child concept has two shipped spellings — `kd create` names
// its dep-target flag `--parent-task`, while `kd list` names its filter
// `--parent`. An agent that (reasonably) reaches for `kd create ... --parent
// ka-X` gets a bare `Error: unknown flag: --parent` with NO "did you mean
// --parent-task?" suggestion (cobra only suggests unknown SUBCOMMANDS, not
// flags). A real gardener session (ka-0000003191) burned a full create attempt
// plus a --help consult on exactly this trap and felt compelled to warn its
// successor in handoff notes. The normalizer remaps `--parent` to
// `--parent-task` during pflag's parse, so `--parent ka-X` is indistinguishable
// from `--parent-task ka-X`.
//
// Scope: this is attached ONLY to `kd create` — `kd list`'s genuine, unrelated
// `--parent` filter flag must never be shadowed, so the helper is deliberately
// create-specific (never global/persistent). `--parent-task` remains the
// canonical flag shown in --help; the alias is noted in its usage string.
//
// Composition: `kd create` already installs a normalize func via
// aliasLabelFlags (the --labels→--label alias). pflag allows only ONE normalize
// func per FlagSet, so this helper CHAINS: it captures the existing func and
// runs it first, then applies the parent→parent-task mapping on top. It must
// therefore be called AFTER aliasLabelFlags(cmd) in the command's init(),
// otherwise aliasLabelFlags would clobber this mapping.
func aliasParentTaskFlag(cmd *cobra.Command) {
	prev := cmd.Flags().GetNormalizeFunc()
	cmd.Flags().SetNormalizeFunc(func(fs *pflag.FlagSet, name string) pflag.NormalizedName {
		if prev != nil {
			name = string(prev(fs, name))
		}
		if name == "parent" {
			name = "parent-task"
		}
		return pflag.NormalizedName(name)
	})
}
