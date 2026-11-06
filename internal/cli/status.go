package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"tunecord/internal/config"
	"tunecord/internal/control"
	"tunecord/internal/install"
	"tunecord/internal/service"
)

type statusReport struct {
	Running       bool            `json:"running"`
	PID           int             `json:"pid,omitempty"`
	UnitState     string          `json:"unit_state,omitempty"`
	UnitInstalled bool            `json:"unit_installed"`
	ConfigPath    string          `json:"config_path"`
	ConfigExists  bool            `json:"config_exists"`
	ConfigError   string          `json:"config_error,omitempty"`
	Allowlist     []string        `json:"allowlist"`
	Service       *service.Status `json:"service,omitempty"`
}

func newStatusCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show service, Discord, player, track and progress-bar state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := statusReport{ConfigPath: g.path(), Allowlist: []string{}}
			cfg, _, exists, err := config.Load(r.ConfigPath)
			r.ConfigExists = exists
			if err != nil {
				r.ConfigError = err.Error()
			} else {
				r.Allowlist = cfg.Players.Allowlist
			}
			r.UnitState = control.UnitState(cmd.Context())
			r.UnitInstalled = fileExists(install.UnitPath())
			r.PID = control.RunningPID()
			r.Running = r.PID != 0 || r.UnitState == "active"
			if st, err := service.ReadStatus(); err == nil && st.PID != 0 && control.IsTunecordProcess(st.PID) {
				r.Service = &st
				r.Running = true
				if r.PID == 0 {
					r.PID = st.PID
				}
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			printStatus(cmd.OutOrStdout(), r)
			if !r.Running {
				return &ExitError{Code: 3}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func printStatus(w io.Writer, r statusReport) {
	line := func(k, f string, a ...any) { fmt.Fprintf(w, "%-16s %s\n", k+":", fmt.Sprintf(f, a...)) }
	switch {
	case r.Running && r.UnitState == "active":
		line("Service", "running (systemd user unit, pid %d)", r.PID)
	case r.Running:
		line("Service", "running in the foreground (pid %d)", r.PID)
	case r.UnitInstalled:
		line("Service", "not running (unit %s: %s). Start: systemctl --user start tunecord", control.UnitName, orDash(r.UnitState))
	default:
		line("Service", "not running and not installed. Run 'tunecord install --user' or 'tunecord run'")
	}
	cfgDesc := r.ConfigPath
	if !r.ConfigExists {
		cfgDesc += " (missing: defaults in use, run 'tunecord config')"
	}
	if r.ConfigError != "" {
		cfgDesc += " (INVALID: " + r.ConfigError + ")"
	}
	line("Config", "%s", cfgDesc)
	allow := r.Allowlist
	if s := r.Service; s != nil {
		allow = s.Allowlist
	}
	if len(allow) == 0 {
		line("Allowlist", "EMPTY: tunecord shows nothing until you add players ('tunecord config')")
	} else {
		line("Allowlist", "%s", strings.Join(allow, " > "))
	}
	s := r.Service
	if s == nil {
		if r.Running {
			line("Details", "the service has not published its state yet")
		}
		return
	}
	if s.BusConnected {
		line("D-Bus", "connected")
	} else {
		line("D-Bus", "NOT connected: %s", orDash(s.BusError))
	}
	if s.DiscordConnected {
		line("Discord", "connected (%s)", s.DiscordSocket)
	} else {
		msg := "not connected (is Discord running? retrying automatically)"
		if s.DiscordError != "" {
			msg += ": " + s.DiscordError
		}
		line("Discord", "%s", msg)
	}
	if s.Player == "" {
		line("Player", "none (no allowlisted player is playing)")
	} else {
		p := s.Player
		if s.PlayerIdentity != "" {
			p += " (" + s.PlayerIdentity + ")"
		}
		line("Player", "%s, %s", p, s.PlaybackStatus)
		track := s.Title
		if s.Artist != "" {
			track += " · " + s.Artist
		}
		if s.Album != "" {
			track += " · " + s.Album
		}
		line("Track", "%s", track)
		mode := orDash(s.ProgressMode)
		if s.ProgressReason != "" {
			mode += " (" + s.ProgressReason + ")"
		}
		line("Progress bar", "%s", mode)
		art := s.ArtURL
		if art == "" {
			art = "default asset"
		}
		line("Album art", "%s", art)
	}
	shown := "cleared"
	if s.PresenceShown {
		shown = "shown"
	}
	line("Presence", "%s", shown)
	line("Version", "%s, up %s", s.Version, time.Since(s.StartedAt).Round(time.Second))
	for _, wn := range s.Warnings {
		line("Warning", "%s", wn)
	}
}
