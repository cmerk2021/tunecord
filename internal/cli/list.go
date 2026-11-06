package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tunecord/internal/config"
	"tunecord/internal/mpris"
	"tunecord/internal/service"
)

// listEntry is one player as shown by `tunecord list`.
type listEntry struct {
	Name        string `json:"name"`     // allowlist form (base name)
	Instance    string `json:"instance"` // full bus-name suffix
	BusName     string `json:"bus_name"`
	Identity    string `json:"identity,omitempty"`
	Status      string `json:"status"`
	Allowlisted bool   `json:"allowlisted"`
	Priority    int    `json:"priority,omitempty"` // 1 = highest; 0 = not allowlisted
	Active      bool   `json:"active"`
}

// listEntries reads all players on the bus and marks allowlist membership
// and the active player. The active player is the one tunecord would show
// (allowlist priority); if no allowlisted player qualifies, the first
// Playing player is marked instead.
func listEntries(ctx context.Context, bus mpris.Bus, cfg config.Config) []listEntry {
	ids, err := bus.ListPlayers(ctx)
	if err != nil {
		return nil
	}
	var out []listEntry
	var cands []service.Candidate
	for _, id := range ids {
		e := listEntry{Name: id.Base, Instance: id.Suffix, BusName: id.BusName, Status: "Unknown"}
		snap, err := bus.Snapshot(ctx, id)
		if err == nil {
			e.Identity = snap.Identity
			e.Status = string(snap.Status)
		} else {
			e.Identity = bus.Identity(ctx, id)
		}
		if idx := service.AllowlistIndex(id, e.Identity, cfg.Players.Allowlist); idx >= 0 {
			e.Allowlisted, e.Priority = true, idx+1
		}
		out = append(out, e)
		cands = append(cands, service.Candidate{ID: id, Identity: e.Identity, Status: mpris.ParseStatus(e.Status)})
	}
	if i := service.Select(cands, cfg.Players.Allowlist, cfg.Presence.ShowWhenPaused); i >= 0 {
		out[i].Active = true
	} else {
		for i := range out {
			if out[i].Status == string(mpris.Playing) {
				out[i].Active = true
				break
			}
		}
	}
	return out
}

func activeEntry(entries []listEntry) (listEntry, bool) {
	for _, e := range entries {
		if e.Active {
			return e, true
		}
	}
	return listEntry{}, false
}

func printList(w io.Writer, entries []listEntry, cfg config.Config) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No MPRIS players are running. Start a player (VLC, Spotify, Elisa, a browser tab with media...) and try again.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tINSTANCE\tIDENTITY\tSTATUS\tALLOWLIST\tACTIVE")
		for _, e := range entries {
			allow := "-"
			if e.Allowlisted {
				allow = fmt.Sprintf("yes (#%d)", e.Priority)
			}
			active := ""
			if e.Active {
				active = "◀ active"
			}
			inst := e.Instance
			if inst == e.Name {
				inst = "-"
			}
			ident := e.Identity
			if ident == "" {
				ident = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, inst, ident, e.Status, allow, active)
		}
		tw.Flush()
		fmt.Fprintln(w, "\nUse the NAME column in [players].allowlist (or run 'tunecord config').")
	}
	if len(cfg.Players.Allowlist) == 0 {
		fmt.Fprintln(w, "Warning: your allowlist is empty, so tunecord shows nothing. Run 'tunecord config'.")
	} else {
		var missing []string
		for _, a := range cfg.Players.Allowlist {
			found := false
			for _, e := range entries {
				if e.Allowlisted && (strings.EqualFold(a, e.Name) || strings.EqualFold(a, e.Instance) || strings.EqualFold(a, e.Identity)) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, a)
			}
		}
		fmt.Fprintf(w, "Allowlist (priority order): %s\n", strings.Join(cfg.Players.Allowlist, ", "))
		if len(missing) > 0 {
			fmt.Fprintf(w, "Not running right now: %s\n", strings.Join(missing, ", "))
		}
	}
}

func newListCmd(g *globals) *cobra.Command {
	var active, asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List MPRIS players on the bus, in allowlist form",
		Long: "List MPRIS players currently on the session bus. The NAME column is exactly what\n" +
			"goes into [players].allowlist. Allowlist membership/priority and the active player\n" +
			"(the one tunecord would show) are marked.\n\n" +
			"--active prints only the active player's name and exits 1 if there is none.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, _, err := config.Load(g.path())
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			bus, err := connectBus(cmd.Context())
			if err != nil {
				return err
			}
			defer bus.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			entries := listEntries(ctx, bus, cfg)
			out := cmd.OutOrStdout()
			if active {
				e, ok := activeEntry(entries)
				if asJSON {
					var v any
					if ok {
						v = e
					}
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					if err := enc.Encode(v); err != nil {
						return err
					}
				} else if ok {
					fmt.Fprintln(out, e.Name)
				}
				if !ok {
					return &ExitError{Code: 1}
				}
				return nil
			}
			if asJSON {
				if entries == nil {
					entries = []listEntry{}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			printList(out, entries, cfg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&active, "active", false, "print only the name of the active/playing player")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}
