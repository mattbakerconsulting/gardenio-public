package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

// RemotesConfig holds all named remotes and tracks which one is active.
type RemotesConfig struct {
	Active  string            `toml:"active"`
	Remotes map[string]Remote `toml:"remotes"`
}

// Remote is a named server profile.
type Remote struct {
	URL         string `toml:"url"`
	Token       string `toml:"token,omitempty"`
	NATSURL     string `toml:"nats_url,omitempty"`
	Description string `toml:"description,omitempty"`
}

func remoteConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "state", "beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "remotes.toml"), nil
}

func loadRemotesConfig() (RemotesConfig, error) {
	path, err := remoteConfigPath()
	if err != nil {
		return RemotesConfig{}, err
	}
	var cfg RemotesConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return RemotesConfig{Remotes: map[string]Remote{}}, nil
		}
		return RemotesConfig{}, err
	}
	if cfg.Remotes == nil {
		cfg.Remotes = map[string]Remote{}
	}
	return cfg, nil
}

func saveRemotesConfig(cfg RemotesConfig) error {
	path, err := remoteConfigPath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

// Cached active remote values, loaded once per process.
var (
	remoteOnce      sync.Once
	cachedRemoteURL string
	cachedNATSURL   string
	cachedToken     string
)

func loadActiveRemoteOnce() {
	remoteOnce.Do(func() {
		cfg, err := loadRemotesConfig()
		if err != nil || cfg.Active == "" {
			return
		}
		r, ok := cfg.Remotes[cfg.Active]
		if !ok {
			return
		}
		cachedRemoteURL = r.URL
		cachedNATSURL = r.NATSURL
		cachedToken = r.Token
	})
}

func activeRemoteURL() string     { loadActiveRemoteOnce(); return cachedRemoteURL }
func activeRemoteNATSURL() string { loadActiveRemoteOnce(); return cachedNATSURL }
func activeRemoteToken() string   { loadActiveRemoteOnce(); return cachedToken }

var remoteCmd = &cobra.Command{
	Use:     "remote",
	Short:   "Manage named server remotes",
	GroupID: "system",
	// Skip the gRPC dial — all remote subcommands are local file operations.
	// Still silence the usage dump on RunE failures (this override would
	// otherwise skip the root's SilenceUsage set); arg-parse errors fire
	// before this hook and keep their usage block. See ka-0000001023.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { cmd.SilenceUsage = true; return nil },
}

var remoteAddCmd = &cobra.Command{
	Use:   "add <name> <url>",
	Short: "Add or update a named remote",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, url := args[0], args[1]
		token, _ := cmd.Flags().GetString("token")
		natsURL, _ := cmd.Flags().GetString("nats")
		desc, _ := cmd.Flags().GetString("description")

		cfg, err := loadRemotesConfig()
		if err != nil {
			return err
		}
		cfg.Remotes[name] = Remote{URL: url, Token: token, NATSURL: natsURL, Description: desc}
		if err := saveRemotesConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("remote %q added (%s)\n", name, url)
		return nil
	},
}

var remoteRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a named remote",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cfg, err := loadRemotesConfig()
		if err != nil {
			return err
		}
		if _, ok := cfg.Remotes[name]; !ok {
			return fmt.Errorf("remote %q not found", name)
		}
		delete(cfg.Remotes, name)
		if cfg.Active == name {
			cfg.Active = ""
		}
		if err := saveRemotesConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("remote %q removed\n", name)
		return nil
	},
}

var remoteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all remotes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadRemotesConfig()
		if err != nil {
			return err
		}
		if len(cfg.Remotes) == 0 {
			fmt.Println("no remotes configured")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tURL\tTOKEN\tDESCRIPTION")
		for name, r := range cfg.Remotes {
			marker := "  "
			if name == cfg.Active {
				marker = "* "
			}
			token := ""
			if r.Token != "" {
				if len(r.Token) > 8 {
					token = r.Token[:8] + "..."
				} else {
					token = r.Token
				}
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n", marker, name, r.URL, token, r.Description)
		}
		return w.Flush()
	},
}

var remoteUseCmd = &cobra.Command{
	Use:   "use [name]",
	Short: "Set the active remote (no args clears it)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadRemotesConfig()
		if err != nil {
			return err
		}
		if len(args) == 0 {
			cfg.Active = ""
			if err := saveRemotesConfig(cfg); err != nil {
				return err
			}
			fmt.Println("active remote cleared")
			return nil
		}
		name := args[0]
		if _, ok := cfg.Remotes[name]; !ok {
			return fmt.Errorf("remote %q not found", name)
		}
		cfg.Active = name
		if err := saveRemotesConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("active remote set to %q\n", name)
		return nil
	},
}

var remoteShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Show details for a remote (defaults to active)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadRemotesConfig()
		if err != nil {
			return err
		}

		name := cfg.Active
		if len(args) == 1 {
			name = args[0]
		}
		if name == "" {
			return fmt.Errorf("no active remote; specify a name or run 'kd remote use <name>'")
		}

		r, ok := cfg.Remotes[name]
		if !ok {
			return fmt.Errorf("remote %q not found", name)
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		active := ""
		if name == cfg.Active {
			active = " (active)"
		}
		fmt.Fprintf(w, "name:\t%s%s\n", name, active)
		if r.Description != "" {
			fmt.Fprintf(w, "description:\t%s\n", r.Description)
		}
		fmt.Fprintf(w, "url:\t%s\n", r.URL)
		if r.Token != "" {
			masked := r.Token
			if len(masked) > 8 {
				masked = masked[:8] + strings.Repeat("*", len(masked)-8)
			}
			fmt.Fprintf(w, "token:\t%s\n", masked)
		}
		if r.NATSURL != "" {
			fmt.Fprintf(w, "nats_url:\t%s\n", r.NATSURL)
		}
		return w.Flush()
	},
}

func init() {
	remoteAddCmd.Flags().String("token", "", "bearer token for authentication")
	remoteAddCmd.Flags().String("nats", "", "NATS URL for event streaming")
	remoteAddCmd.Flags().String("description", "", "human-readable description of the remote")

	remoteCmd.AddCommand(remoteAddCmd)
	remoteCmd.AddCommand(remoteRemoveCmd)
	remoteCmd.AddCommand(remoteListCmd)
	remoteCmd.AddCommand(remoteUseCmd)
	remoteCmd.AddCommand(remoteShowCmd)
}
