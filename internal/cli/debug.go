package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"tunecord/internal/art"
	"tunecord/internal/config"
	"tunecord/internal/mpris"
	"tunecord/internal/progress"
	"tunecord/internal/service"
)

type debugPlayer struct {
	tracker  *progress.Tracker
	samples  int
	first    time.Time
	lastSeek string
}

func fmtRaw(v any, ok bool) string {
	if v == nil {
		return "<missing>"
	}
	s := fmt.Sprintf("%v", v)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	if !ok {
		return fmt.Sprintf("%s  (%T, NOT usable)", s, v)
	}
	return fmt.Sprintf("%s  (%T)", s, v)
}

func fmtUs(us int64) string {
	return service.Describe(progress.Micros(us))
}

func artVerdict(u string) string {
	switch {
	case u == "":
		return "no art URL; tunecord will look art up online"
	case art.PublicURL(u):
		return "public URL, sent to Discord as-is"
	}
	if pu, err := url.Parse(u); err == nil && pu.Scheme != "" {
		switch pu.Scheme {
		case "file":
			return "local file (Discord cannot load it); tunecord will look art up online"
		case "data":
			return "inline data URL (Discord cannot load it); tunecord will look art up online"
		}
	}
	return "not a public http(s) URL (private host, too long, or unknown scheme); tunecord will look art up online"
}

// renderDebug writes the diagnostics block for one player.
func renderDebug(w io.Writer, snap mpris.Snapshot, dp *debugPlayer, cfg config.Config, now time.Time) {
	id := snap.ID
	allow := "NOT in allowlist (ignored by tunecord)"
	if i := service.AllowlistIndex(id, snap.Identity, cfg.Players.Allowlist); i >= 0 {
		allow = fmt.Sprintf("allowlisted, priority #%d", i+1)
	}
	fmt.Fprintf(w, "━━ %s  [%s]\n", id.Suffix, allow)
	fmt.Fprintf(w, "  allowlist name : %s\n", id.Base)
	fmt.Fprintf(w, "  bus name       : %s\n", id.BusName)
	fmt.Fprintf(w, "  Identity       : %s\n", orDash(snap.Identity))
	m := snap.Metadata

	fmt.Fprintf(w, "  PlaybackStatus : %s\n", fmtRaw(snap.StatusRaw, snap.StatusRaw != nil))
	if snap.PositionErr != nil {
		fmt.Fprintf(w, "  Position       : read failed: %v\n", snap.PositionErr)
	} else {
		fmt.Fprintf(w, "  Position       : %s", fmtRaw(snap.PositionRaw, snap.PositionOK))
		if snap.PositionOK {
			fmt.Fprintf(w, " = %s", fmtUs(snap.Position))
		}
		fmt.Fprintln(w)
	}
	lengthRaw := m.Raw["mpris:length"]
	fmt.Fprintf(w, "  mpris:length   : %s", fmtRaw(lengthRaw, m.LengthOK))
	if m.LengthOK {
		fmt.Fprintf(w, " = %s", fmtUs(m.Length))
	}
	fmt.Fprintln(w)
	rate := fmtRaw(snap.RateRaw, snap.RateOK)
	if snap.RateRaw == nil {
		rate += " (assuming 1.0)"
	}
	fmt.Fprintf(w, "  Rate           : %s\n", rate)
	fmt.Fprintf(w, "  mpris:artUrl   : %s\n", fmtRaw(m.Raw["mpris:artUrl"], m.ArtURL != ""))
	fmt.Fprintf(w, "                   → %s\n", artVerdict(m.ArtURL))
	if dp.lastSeek != "" {
		fmt.Fprintf(w, "  last Seeked    : %s\n", dp.lastSeek)
	}
	fmt.Fprintf(w, "  title / artist : %q / %q\n", m.Title, strings.Join(m.Artists, ", "))
	fmt.Fprintf(w, "  album / trackid: %q / %q\n", m.Album, m.TrackID)

	keys := make([]string, 0, len(m.RawTypes))
	for k := range m.RawTypes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(w, "  metadata keys  :")
	for _, k := range keys {
		fmt.Fprintf(w, "      %-24s %s\n", k, m.RawTypes[k])
	}

	v := dp.tracker.View(now)
	fmt.Fprintf(w, "  ── progress bar verdict (%d samples over %s)\n", dp.samples, now.Sub(dp.first).Round(100*time.Millisecond))
	switch v.Mode {
	case progress.ModeReal:
		fmt.Fprintln(w, "     RELIABLE: real progress bar from the player's position")
	case progress.ModeEstimated:
		fmt.Fprintln(w, "     ESTIMATED: tunecord's own timer is used (fallback = estimate)")
	default:
		fmt.Fprintln(w, "     NO BAR")
	}
	fmt.Fprintf(w, "     why: %s\n", v.Reason)
	if v.HasTimestamps {
		fmt.Fprintf(w, "     predicted position %s / %s at rate %.2g → start %s, end %s\n",
			service.Describe(v.Position), service.Describe(v.Length), v.Rate,
			v.Start.Format("15:04:05"), v.End.Format("15:04:05"))
	}
	var notes []string
	if snap.PositionOK {
		if _, isInt64 := snap.PositionRaw.(int64); !isInt64 {
			notes = append(notes, fmt.Sprintf("Position is %T instead of int64 (non-standard; tunecord normalizes it)", snap.PositionRaw))
		}
	}
	if m.LengthOK {
		if _, isInt64 := lengthRaw.(int64); !isInt64 {
			notes = append(notes, fmt.Sprintf("mpris:length is %T instead of int64 (non-standard; tunecord normalizes it)", lengthRaw))
		}
	}
	if !m.LengthOK && lengthRaw == nil {
		notes = append(notes, "mpris:length is missing: Discord cannot show a bar without a length")
	}
	if dp.samples < 3 && snap.Status == mpris.Playing {
		notes = append(notes, "few samples so far; stuck-position detection needs about 3 seconds of playback")
	}
	for _, n := range notes {
		fmt.Fprintf(w, "     note: %s\n", n)
	}
	fmt.Fprintln(w)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newDebugCmd(g *globals) *cobra.Command {
	var once bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "debug [player...]",
		Short: "Live per-player diagnostics of raw MPRIS values and the progress-bar verdict",
		Long: "Print the raw D-Bus values (with their Go types) of Position, mpris:length, Rate,\n" +
			"PlaybackStatus, mpris:artUrl and all metadata keys for every MPRIS player (or only\n" +
			"the named ones), plus tunecord's verdict on progress-bar reliability and why.\n\n" +
			"Refreshes every second until Ctrl+C. --once observes for about 3 seconds (enough to\n" +
			"detect a stuck position) and prints a single report.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, _, err := config.Load(g.path())
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			bus, err := connectBus(cmd.Context())
			if err != nil {
				return err
			}
			defer bus.Close()
			return runDebug(cmd.Context(), cmd.OutOrStdout(), bus, cfg, args, once, interval)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "observe briefly, print one report and exit")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "refresh interval")
	return cmd
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runDebug(ctx context.Context, w io.Writer, bus mpris.Bus, cfg config.Config, filter []string, once bool, interval time.Duration) error {
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	pcfg := progress.DefaultConfig()
	pcfg.SeekThreshold = cfg.Progress.SeekThreshold.Duration
	pcfg.Fallback = progress.Fallback(cfg.Progress.Fallback)
	players := map[string]*debugPlayer{}
	tty := isTTY(w) && !once
	const onceSamples = 4

	match := func(id mpris.PlayerID, identity string) bool {
		if len(filter) == 0 {
			return true
		}
		for _, f := range filter {
			if mpris.MatchesEntry(id, identity, f) {
				return true
			}
		}
		return false
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for round := 1; ; round++ {
		now := time.Now()
		ids, err := bus.ListPlayers(ctx)
		if err != nil {
			return err
		}
		type shown struct {
			snap mpris.Snapshot
			dp   *debugPlayer
		}
		var list []shown
		seen := map[string]bool{}
		var errs []string
		for _, id := range ids {
			snap, err := bus.Snapshot(ctx, id)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", id.Suffix, err))
				continue
			}
			if !match(id, snap.Identity) {
				continue
			}
			seen[id.BusName] = true
			dp := players[id.BusName]
			if dp == nil {
				dp = &debugPlayer{tracker: progress.New(pcfg), first: snap.ReadAt}
				players[id.BusName] = dp
			}
			dp.tracker.Update(service.SampleFrom(snap))
			dp.samples++
			list = append(list, shown{snap, dp})
		}
		for k := range players {
			if !seen[k] {
				delete(players, k)
			}
		}
		if !once || round >= onceSamples {
			var b strings.Builder
			fmt.Fprintf(&b, "tunecord debug · %s · allowlist: %s\n\n", now.Format("15:04:05"), allowSummary(cfg))
			if len(list) == 0 {
				if len(filter) > 0 {
					fmt.Fprintf(&b, "No MPRIS player matching %s is running.\n", strings.Join(filter, ", "))
				} else {
					b.WriteString("No MPRIS players are running.\n")
				}
			}
			for _, s := range list {
				renderDebug(&b, s.snap, s.dp, cfg, time.Now())
			}
			for _, e := range errs {
				fmt.Fprintf(&b, "error reading %s\n", e)
			}
			if !once {
				b.WriteString("Ctrl+C to quit.\n")
			}
			if tty {
				io.WriteString(w, "\033[H\033[2J")
			}
			io.WriteString(w, b.String())
			if once {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-bus.Events():
			if !ok {
				return fmt.Errorf("lost the D-Bus session bus connection")
			}
			if ev.Kind == mpris.Seeked {
				if dp := players[ev.BusName]; dp != nil {
					dp.tracker.Seek(ev.At, progress.Micros(ev.Position), ev.PositionOK)
					dp.lastSeek = fmt.Sprintf("%s at %s", fmtUs(ev.Position), ev.At.Format("15:04:05.000"))
				}
			}
			// Drain the ticker so a burst of signals doesn't print faster than once per interval.
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return nil
			}
		case <-ticker.C:
		}
	}
}

func allowSummary(cfg config.Config) string {
	if len(cfg.Players.Allowlist) == 0 {
		return "(empty: tunecord shows nothing)"
	}
	return strings.Join(cfg.Players.Allowlist, " > ")
}
