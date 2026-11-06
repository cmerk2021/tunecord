// Package tui implements the interactive `tunecord config` walkthrough.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"tunecord/internal/config"
)

// ErrAborted is returned when the user cancels the walkthrough.
var ErrAborted = errors.New("configuration cancelled; nothing was saved")

// Detected describes a player currently on the bus.
type Detected struct {
	Name     string // allowlist form (base name), e.g. "vlc"
	Instance string // full suffix, e.g. "vlc.instance1234"
	Identity string
	Status   string
}

// Params configures Run.
type Params struct {
	Path     string
	Config   config.Config
	Exists   bool
	Detected []Detected
	Out      io.Writer
	// Reload is called after a successful save; it returns a message for the user.
	Reload func(ctx context.Context) (string, error)
}

// IsTerminal reports whether stdin and stdout are terminals.
func IsTerminal() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

func run(ctx context.Context, groups ...*huh.Group) error {
	err := huh.NewForm(groups...).WithTheme(huh.ThemeCharm()).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
		return ErrAborted
	}
	return err
}

// ---------------------------------------------------------------- helpers

type option struct {
	Label    string
	Value    string
	Selected bool
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// allowlistOptions builds the multi-select options: detected players first
// (deduplicated by base name), then current allowlist entries not running.
func allowlistOptions(current []string, detected []Detected) []option {
	var opts []option
	seen := map[string]bool{}
	for _, d := range detected {
		k := strings.ToLower(d.Name)
		if d.Name == "" || seen[k] {
			continue
		}
		seen[k] = true
		label := d.Name
		var extra []string
		if d.Identity != "" && !strings.EqualFold(d.Identity, d.Name) {
			extra = append(extra, d.Identity)
		}
		if d.Instance != "" && d.Instance != d.Name {
			extra = append(extra, d.Instance)
		}
		if d.Status != "" {
			extra = append(extra, strings.ToLower(d.Status))
		}
		if len(extra) > 0 {
			label += "  (" + strings.Join(extra, ", ") + ")"
		}
		sel := containsFold(current, d.Name) || (d.Identity != "" && containsFold(current, d.Identity))
		opts = append(opts, option{Label: label, Value: d.Name, Selected: sel})
	}
	for _, c := range current {
		matched := false
		for _, d := range detected {
			if strings.EqualFold(c, d.Name) || (d.Identity != "" && strings.EqualFold(c, d.Identity)) {
				matched = true
				break
			}
		}
		if matched || seen[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		opts = append(opts, option{Label: c + "  (not running)", Value: c, Selected: true})
	}
	return opts
}

// splitManual parses a comma/whitespace separated list of player names.
func splitManual(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

// mergeAllowlist keeps the existing order for entries still selected, then
// appends newly selected and manual entries. Duplicates are dropped
// case-insensitively; an existing entry matching a player's Identity counts
// as that player.
func mergeAllowlist(current, selected []string, manual string, detected []Detected) []string {
	identityOf := map[string]string{}
	for _, d := range detected {
		identityOf[strings.ToLower(d.Name)] = d.Identity
	}
	isSelected := func(entry string) bool {
		if containsFold(selected, entry) {
			return true
		}
		for _, s := range selected {
			if id := identityOf[strings.ToLower(s)]; id != "" && strings.EqualFold(id, entry) {
				return true
			}
		}
		return false
	}
	var out []string
	has := func(v string) bool {
		if containsFold(out, v) {
			return true
		}
		if id := identityOf[strings.ToLower(v)]; id != "" && containsFold(out, id) {
			return true
		}
		return false
	}
	for _, c := range current {
		if isSelected(c) && !has(c) {
			out = append(out, c)
		}
	}
	for _, s := range selected {
		if !has(s) {
			out = append(out, s)
		}
	}
	for _, m := range splitManual(manual) {
		m = strings.TrimSpace(m)
		m = strings.TrimPrefix(m, "org.mpris.MediaPlayer2.")
		if m != "" && !has(m) {
			out = append(out, m)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func validateDuration(min, max time.Duration) func(string) error {
	return func(s string) error {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("not a duration (examples: 1s, 500ms, 2.5s)")
		}
		if d < min || d > max {
			return fmt.Errorf("must be between %s and %s", min, max)
		}
		return nil
	}
}

func fmtDur(d time.Duration) string {
	b, _ := config.D(d).MarshalText()
	return string(b)
}

func mask(s string) string {
	if s == "" {
		return "(not set)"
	}
	return "•••••••• (set)"
}

func onOff(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Summary renders a human-readable summary of cfg (secrets masked).
func Summary(c config.Config) string {
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "  %-22s %s\n", k, v) }
	allow := "(empty: nothing will be shown!)"
	if len(c.Players.Allowlist) > 0 {
		var parts []string
		for i, p := range c.Players.Allowlist {
			parts = append(parts, fmt.Sprintf("%d. %s", i+1, p))
		}
		allow = strings.Join(parts, "  ")
	}
	w("Allowlist (priority)", allow)
	w("Idle poll interval", fmtDur(c.Polling.IdleInterval.Duration))
	w("Playing poll interval", fmtDur(c.Polling.PlayingInterval.Duration))
	w("Status display", c.Presence.StatusDisplay)
	w("Show album", onOff(c.Presence.ShowAlbum))
	w("Show when paused", onOff(c.Presence.ShowWhenPaused))
	if c.Presence.ShowWhenPaused {
		pa := "(none: ' · Paused' suffix)"
		if c.Presence.PausedAsset != "" {
			pa = c.Presence.PausedAsset
		}
		w("Paused small image", pa)
	}
	w("Progress fallback", c.Progress.Fallback)
	w("Seek threshold", fmtDur(c.Progress.SeekThreshold.Duration))
	srcs := "(none)"
	if len(c.Art.Sources) > 0 {
		srcs = strings.Join(c.Art.Sources, " → ")
	}
	w("Art sources", srcs)
	w("Spotify client ID", orNotSet(c.Art.Spotify.ClientID))
	w("Spotify client secret", mask(c.Art.Spotify.ClientSecret))
	w("Default art asset", c.Presence.DefaultArtAsset)
	cid := c.Discord.ClientID
	if cid == config.DefaultClientID {
		cid += " (default)"
	}
	w("Discord client ID", cid)
	w("Log level", c.General.LogLevel)
	return b.String()
}

func orNotSet(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

// ---------------------------------------------------------------- walkthrough

// Run executes the walkthrough. It returns ErrAborted if the user cancels.
func Run(ctx context.Context, p Params) error {
	out := p.Out
	if out == nil {
		out = os.Stdout
	}
	if !IsTerminal() {
		return errors.New("tunecord config needs an interactive terminal; edit " + p.Path + " by hand instead")
	}
	cfg := p.Config
	cfg.Normalize()

	intro := "This walkthrough covers every option. Current values are pre-filled; press enter to keep them.\n" +
		"Config file: " + p.Path
	if !p.Exists {
		intro += " (will be created)"
	}
	if err := run(ctx, huh.NewGroup(huh.NewNote().Title("tunecord setup").Description(intro).Next(true).NextLabel("Start"))); err != nil {
		return err
	}

	// 1. Allowlist ------------------------------------------------------------
	opts := allowlistOptions(cfg.Players.Allowlist, p.Detected)
	var selected []string
	var manual string
	var fields []huh.Field
	if len(opts) > 0 {
		hopts := make([]huh.Option[string], 0, len(opts))
		for _, o := range opts {
			hopts = append(hopts, huh.NewOption(o.Label, o.Value).Selected(o.Selected))
		}
		fields = append(fields, huh.NewMultiSelect[string]().
			Title("1/8 · Players to show (allowlist)").
			Description("Only these players will drive your Discord presence. Detected players are listed;\n"+
				"entries marked 'not running' come from your current config. space = toggle, enter = next.").
			Options(hopts...).
			Value(&selected))
	} else {
		fields = append(fields, huh.NewNote().
			Title("1/8 · Players to show (allowlist)").
			Description("No MPRIS players are running right now. Start your player and run 'tunecord list',\nor type names below."))
	}
	fields = append(fields, huh.NewInput().
		Title("Add players that are not running").
		Description("Comma-separated names as shown by 'tunecord list' (e.g. vlc, spotify, elisa) or their Identity.\nMatching is case-insensitive; '.instanceNNN' suffixes are handled automatically.").
		Placeholder("vlc, spotify").
		Value(&manual))
	if err := run(ctx, huh.NewGroup(fields...)); err != nil {
		return err
	}
	allow := mergeAllowlist(cfg.Players.Allowlist, selected, manual, p.Detected)
	if len(allow) > 1 {
		labels := map[string]string{}
		for _, d := range p.Detected {
			if d.Status != "" {
				labels[d.Name] = "(" + strings.ToLower(d.Status) + ")"
			}
		}
		m := NewReorder("1/8 · Player priority",
			"Higher = higher priority. The highest playing player wins; if none is playing,\nthe highest paused one is shown.", allow)
		m.Labels = labels
		m.AllowRemove = true
		ordered, err := RunReorder(m)
		if err != nil {
			return err
		}
		allow = ordered
	}
	cfg.Players.Allowlist = allow

	// 2. Polling --------------------------------------------------------------
	idle := fmtDur(cfg.Polling.IdleInterval.Duration)
	playing := fmtDur(cfg.Polling.PlayingInterval.Duration)
	vd := validateDuration(config.MinPollInterval, config.MaxPollInterval)
	if err := run(ctx, huh.NewGroup(
		huh.NewNote().Title("2/8 · Polling").
			Description("Player signals (track change, play/pause, seek, players appearing) are handled instantly.\n"+
				"Polling only re-reads the position to catch players that seek silently. Range 500ms – 60s."),
		huh.NewInput().Title("Idle interval").Description("How often to check when nothing is playing.").Value(&idle).Validate(vd),
		huh.NewInput().Title("Playing interval").Description("How often to verify the position while playing.").Value(&playing).Validate(vd),
	)); err != nil {
		return err
	}
	cfg.Polling.IdleInterval = config.D(mustDur(idle))
	cfg.Polling.PlayingInterval = config.D(mustDur(playing))

	// 3-5. Presence -----------------------------------------------------------
	statusDisplay := cfg.Presence.StatusDisplay
	showAlbum := cfg.Presence.ShowAlbum
	if err := run(ctx, huh.NewGroup(
		huh.NewSelect[string]().
			Title("3/8 · Status display").
			Description("What appears under your name in the member list, next to the music icon.").
			Options(
				huh.NewOption("artist: \"Listening to Daft Punk\" (default)", "artist"),
				huh.NewOption("song: \"Listening to Get Lucky\"", "song"),
				huh.NewOption("name: \"Listening to tunecord\" (the app name)", "name"),
			).Value(&statusDisplay),
		huh.NewConfirm().
			Title("4/8 · Show the album name?").
			Description("Adds the album as hover text on the album art. Off by default to keep the card compact;\nwhen off, the album is not sent to Discord at all.").
			Affirmative("Show album").Negative("Hide album").
			Value(&showAlbum),
	)); err != nil {
		return err
	}
	cfg.Presence.StatusDisplay = statusDisplay
	cfg.Presence.ShowAlbum = showAlbum

	showPaused := cfg.Presence.ShowWhenPaused
	if err := run(ctx, huh.NewGroup(
		huh.NewConfirm().
			Title("5/8 · When playback is paused").
			Description("Keep showing the track (without a progress bar, marked as paused), or clear the presence.").
			Affirmative("Keep, marked paused").Negative("Clear").
			Value(&showPaused),
	)); err != nil {
		return err
	}
	cfg.Presence.ShowWhenPaused = showPaused
	if showPaused {
		pausedAsset := cfg.Presence.PausedAsset
		if err := run(ctx, huh.NewGroup(
			huh.NewInput().
				Title("Paused indicator image (optional)").
				Description("A Discord asset key or https URL shown as a small image while paused.\nLeave empty to append \" · Paused\" to the artist line instead.").
				Value(&pausedAsset).
				Validate(func(s string) error {
					if len(strings.TrimSpace(s)) > 256 {
						return errors.New("must be at most 256 characters")
					}
					return nil
				}),
		)); err != nil {
			return err
		}
		cfg.Presence.PausedAsset = strings.TrimSpace(pausedAsset)
	}

	// 6. Progress -------------------------------------------------------------
	fallback := cfg.Progress.Fallback
	seek := fmtDur(cfg.Progress.SeekThreshold.Duration)
	if err := run(ctx, huh.NewGroup(
		huh.NewSelect[string]().
			Title("6/8 · Progress bar when a player reports a broken position").
			Description("Some players report a position stuck at 0, garbage, or nothing at all.\n'tunecord debug' shows which mode a player ends up in and why.").
			Options(
				huh.NewOption("estimate: run our own timer from the track start (default)", "estimate"),
				huh.NewOption("skip: show no progress bar for that track", "skip"),
			).Value(&fallback),
		huh.NewInput().
			Title("Seek threshold").
			Description("If the reported position differs from the expected one by more than this, treat it as a seek\nand re-sync the bar. Default 2.5s (range 500ms – 1m).").
			Value(&seek).
			Validate(validateDuration(500*time.Millisecond, time.Minute)),
	)); err != nil {
		return err
	}
	cfg.Progress.Fallback = fallback
	cfg.Progress.SeekThreshold = config.D(mustDur(seek))

	// 7. Art ------------------------------------------------------------------
	descr := map[string]string{
		"mpris":       "mpris: the player's own art URL, if it is a public http(s) link (Spotify, browsers)",
		"spotify":     "spotify: Spotify Web API search (needs your own API credentials)",
		"itunes":      "itunes: iTunes Search API (free, no key)",
		"musicbrainz": "musicbrainz: MusicBrainz + Cover Art Archive (free, no key)",
	}
	var srcOpts []huh.Option[string]
	order := append([]string(nil), cfg.Art.Sources...)
	for _, s := range config.KnownArtSources {
		if !containsFold(order, s) {
			order = append(order, s)
		}
	}
	for _, s := range order {
		srcOpts = append(srcOpts, huh.NewOption(descr[s], s).Selected(containsFold(cfg.Art.Sources, s)))
	}
	var srcs []string
	if err := run(ctx, huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("7/8 · Album art sources").
			Description("Tried in order until one finds art; results are cached in ~/.cache/tunecord/art.json.\nLookups never delay your presence: it is shown immediately and the image is filled in later.").
			Options(srcOpts...).
			Value(&srcs),
	)); err != nil {
		return err
	}
	if len(srcs) > 1 {
		ordered, err := RunReorder(NewReorder("7/8 · Album art lookup order", "Top is tried first.", srcs))
		if err != nil {
			return err
		}
		srcs = ordered
	}
	cfg.Art.Sources = srcs
	if containsFold(srcs, "spotify") {
		id, secret := cfg.Art.Spotify.ClientID, ""
		group := []huh.Field{
			huh.NewNote().Title("Spotify credentials (optional)").
				Description("Create an app at https://developer.spotify.com/dashboard (any redirect URI), then copy its\n" +
					"Client ID and Client Secret. They are stored in your config file with 0600 permissions.\n" +
					"Leave both empty to skip Spotify lookups."),
			huh.NewInput().Title("Spotify Client ID").Value(&id),
			huh.NewInput().Title("Spotify Client Secret").
				Description(secretHint(cfg.Art.Spotify.ClientSecret)).
				EchoMode(huh.EchoModePassword).Value(&secret),
		}
		if err := run(ctx, huh.NewGroup(group...)); err != nil {
			return err
		}
		id = strings.TrimSpace(id)
		secret = strings.TrimSpace(secret)
		if secret == "" && id != "" && id == cfg.Art.Spotify.ClientID {
			secret = cfg.Art.Spotify.ClientSecret
		}
		if id == "" || secret == "" {
			if id != "" || secret != "" {
				fmt.Fprintln(out, "Spotify: both Client ID and Client Secret are required; Spotify lookups disabled.")
			}
			id, secret = "", ""
		}
		cfg.Art.Spotify.ClientID, cfg.Art.Spotify.ClientSecret = id, secret
	}

	// 8. Advanced -------------------------------------------------------------
	advanced := false
	if err := run(ctx, huh.NewGroup(
		huh.NewConfirm().Title("8/8 · Advanced settings").
			Description("Custom Discord application ID, default art asset and log level. Most people can skip this.").
			Affirmative("Edit").Negative("Skip").Value(&advanced),
	)); err != nil {
		return err
	}
	if advanced {
		clientID := cfg.Discord.ClientID
		defArt := cfg.Presence.DefaultArtAsset
		logLevel := cfg.General.LogLevel
		if err := run(ctx, huh.NewGroup(
			huh.NewInput().Title("Discord client ID").
				Description("Your own Discord application's ID (its name is shown as \"Listening to <name>\").\nDefault: "+config.DefaultClientID).
				Value(&clientID).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return nil
					}
					for _, r := range s {
						if r < '0' || r > '9' {
							return errors.New("must be numeric")
						}
					}
					return nil
				}),
			huh.NewInput().Title("Default art asset").
				Description("Discord asset key (uploaded to your application) or https URL, used when no art is found.").
				Value(&defArt).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("must not be empty")
					}
					if len(s) > 256 {
						return errors.New("must be at most 256 characters")
					}
					return nil
				}),
			huh.NewSelect[string]().Title("Log level").
				Options(huh.NewOptions(config.LogLevels...)...).
				Value(&logLevel),
		)); err != nil {
			return err
		}
		cfg.Discord.ClientID = strings.TrimSpace(clientID)
		cfg.Presence.DefaultArtAsset = strings.TrimSpace(defArt)
		cfg.General.LogLevel = logLevel
	}

	// Summary -----------------------------------------------------------------
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	summary := Summary(cfg)
	if len(cfg.Players.Allowlist) == 0 {
		summary += "\nWarning: the allowlist is empty, so tunecord will not show anything."
	}
	save := true
	if err := run(ctx, huh.NewGroup(
		huh.NewNote().Title("Summary").Description(summary),
		huh.NewConfirm().Title("Save and apply?").Affirmative("Save").Negative("Cancel").Value(&save),
	)); err != nil {
		return err
	}
	if !save {
		return ErrAborted
	}
	if err := config.Save(p.Path, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "Saved %s\n", p.Path)
	if p.Reload != nil {
		msg, err := p.Reload(ctx)
		if err != nil {
			fmt.Fprintf(out, "Could not reload the service: %v\n", err)
		} else if msg != "" {
			fmt.Fprintln(out, msg)
		}
	}
	return nil
}

func secretHint(existing string) string {
	if existing != "" {
		return "Input is hidden. Leave empty to keep the saved secret (if the Client ID is unchanged)."
	}
	return "Input is hidden."
}

func mustDur(s string) time.Duration {
	d, _ := time.ParseDuration(strings.TrimSpace(s))
	return d
}
