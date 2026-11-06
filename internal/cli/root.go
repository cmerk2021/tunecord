// Package cli implements the tunecord command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"tunecord/internal/config"
	"tunecord/internal/version"
)

// ExitError carries a specific exit code. An empty Msg prints nothing.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

type globals struct {
	verbose    bool
	configPath string
}

func (g *globals) path() string {
	if g.configPath != "" {
		return g.configPath
	}
	return config.ConfigPath()
}

// newLogger builds the stderr logger. The timestamp is omitted under
// systemd (journald adds its own).
func newLogger(w io.Writer, verbose bool) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	if verbose {
		lv.Set(slog.LevelDebug)
	}
	opts := &slog.HandlerOptions{Level: lv}
	if os.Getenv("JOURNAL_STREAM") != "" || os.Getenv("INVOCATION_ID") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(w, opts)), lv
}

// NewRoot builds the root command.
func NewRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "tunecord",
		Short: "MPRIS to Discord Rich Presence with a reliable progress bar",
		Long: "tunecord watches MPRIS media players on the D-Bus session bus and shows the current\n" +
			"track as a Discord \"Listening\" Rich Presence with album art and a progress bar.\n\n" +
			"Quick start:\n" +
			"  tunecord list              # see player names\n" +
			"  tunecord config            # choose players and options\n" +
			"  tunecord install --user    # run it in the background via systemd",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.String(),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "enable debug logging (overrides general.log_level)")
	root.PersistentFlags().StringVar(&g.configPath, "config", "", "config file path (default "+config.ConfigPath()+")")
	root.AddCommand(
		newRunCmd(g),
		newConfigCmd(g),
		newListCmd(g),
		newDebugCmd(g),
		newInstallCmd(g),
		newUninstallCmd(g),
		newStatusCmd(g),
		newVersionCmd(),
	)
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context) int {
	root := NewRoot()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Msg != "" {
			fmt.Fprintln(os.Stderr, ee.Msg)
		}
		return ee.Code
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	return 1
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
