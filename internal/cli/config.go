package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"tunecord/internal/config"
	"tunecord/internal/control"
	"tunecord/internal/tui"
)

func runConfig(ctx context.Context, g *globals) error {
	path := g.path()
	cfg, warnings, exists, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: the existing config is invalid (%v); starting from defaults.\n", err)
		cfg = config.Default()
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s (will be dropped on save)\n", w)
	}
	var detected []tui.Detected
	if bus, err := connectBus(ctx); err == nil {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		for _, e := range listEntries(lctx, bus, cfg) {
			detected = append(detected, tui.Detected{Name: e.Name, Instance: e.Instance, Identity: e.Identity, Status: e.Status})
		}
		cancel()
		bus.Close()
	} else {
		fmt.Fprintf(os.Stderr, "Note: cannot list running players (%v); you can still type names manually.\n", err)
	}
	err = tui.Run(ctx, tui.Params{
		Path:     path,
		Config:   cfg,
		Exists:   exists,
		Detected: detected,
		Out:      os.Stdout,
		Reload:   control.Reload,
	})
	if errors.Is(err, tui.ErrAborted) {
		return &ExitError{Code: 1, Msg: err.Error()}
	}
	return err
}
