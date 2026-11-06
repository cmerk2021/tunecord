package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"tunecord/internal/control"
	"tunecord/internal/mpris"
	"tunecord/internal/service"
)

func newRunCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the service in the foreground (this is what systemd runs)",
		Long: "Run the presence service in the foreground.\n\n" +
			"The config is read once at startup. SIGHUP re-reads it and re-initializes the\n" +
			"connections without exiting; SIGINT/SIGTERM clear the presence and exit.\n" +
			"A pidfile is written to $XDG_RUNTIME_DIR/tunecord.pid.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, lv := newLogger(os.Stderr, g.verbose)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			reload := make(chan struct{}, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-hup:
						select {
						case reload <- struct{}{}:
						default:
						}
					}
				}
			}()

			err := service.Run(ctx, service.Options{
				ConfigPath:   g.path(),
				Logger:       log,
				LevelVar:     lv,
				Verbose:      g.verbose,
				Reload:       reload,
				WriteStatus:  true,
				WritePidfile: true,
			})
			switch {
			case err == nil:
				return nil
			case errors.Is(err, mpris.ErrNoSessionBus):
				return &ExitError{Code: 1, Msg: "Error: " + err.Error()}
			case errors.Is(err, control.ErrAlreadyRunning):
				return &ExitError{Code: 1, Msg: "Error: " + err.Error() + " (see 'tunecord status')"}
			}
			return err
		},
	}
}

func newConfigCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Interactive setup; saves the config and reloads the running service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfig(cmd.Context(), g)
		},
	}
}

func connectBus(ctx context.Context) (*mpris.DBusClient, error) {
	return mpris.Connect(ctx)
}
