package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"tunecord/internal/config"
	"tunecord/internal/install"
)

const userFlagHelp = `tunecord only supports a systemd *user* service (it needs your desktop session's
D-Bus and Discord's IPC socket in $XDG_RUNTIME_DIR, which a system service cannot see).
Re-run with --user:

    tunecord %s --user`

func newInstallCmd(g *globals) *cobra.Command {
	var user bool
	cmd := &cobra.Command{
		Use:   "install --user",
		Short: "Install and enable the systemd user service",
		Long: "Write ~/.config/systemd/user/tunecord.service with the absolute path of this binary,\n" +
			"then run 'systemctl --user daemon-reload' and 'systemctl --user enable --now tunecord.service'.\n" +
			"Safe to re-run (e.g. after moving the binary).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !user {
				return &ExitError{Code: 2, Msg: fmt.Sprintf(userFlagHelp, "install")}
			}
			out := cmd.OutOrStdout()
			if hint := install.PathHint(os.Getenv("PATH")); hint != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), hint)
				fmt.Fprintln(cmd.ErrOrStderr())
			}
			if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" && os.Getenv("XDG_RUNTIME_DIR") == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: neither DBUS_SESSION_BUS_ADDRESS nor XDG_RUNTIME_DIR is set; the service needs your desktop session's bus.")
			}
			if err := install.Install(cmd.Context(), install.Options{Out: out}); err != nil {
				return err
			}
			cfg, _, exists, err := config.Load(g.path())
			switch {
			case err != nil:
				fmt.Fprintf(out, "\nWarning: your config is invalid (%v). Fix it with 'tunecord config'.\n", err)
			case !exists || len(cfg.Players.Allowlist) == 0:
				fmt.Fprintln(out, "\nNext step: no players are allowlisted yet, so nothing will be shown.")
				fmt.Fprintln(out, "Run 'tunecord config' to pick your players (it restarts the service on save).")
			default:
				fmt.Fprintln(out, "\nCheck it with 'tunecord status'. Logs: journalctl --user -u tunecord -f")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&user, "user", false, "install as a systemd user service (required)")
	return cmd
}

func newUninstallCmd(_ *globals) *cobra.Command {
	var user bool
	cmd := &cobra.Command{
		Use:   "uninstall --user",
		Short: "Stop, disable and remove the systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !user {
				return &ExitError{Code: 2, Msg: fmt.Sprintf(userFlagHelp, "uninstall")}
			}
			if err := install.Uninstall(cmd.Context(), install.Options{Out: cmd.OutOrStdout()}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Your config (%s) and art cache (%s) were kept.\n", config.ConfigDir(), config.CacheDir())
			return nil
		},
	}
	cmd.Flags().BoolVar(&user, "user", false, "remove the systemd user service (required)")
	return cmd
}
