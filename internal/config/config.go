// Package config loads, validates and saves the tunecord configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// DefaultClientID is the Discord application ID used unless overridden.
const DefaultClientID = "1525387248332443781"

// Limits enforced on polling intervals.
const (
	MinPollInterval = 500 * time.Millisecond
	MaxPollInterval = 60 * time.Second
)

// Known value sets.
var (
	LogLevels       = []string{"debug", "info", "warn", "error"}
	StatusDisplays  = []string{"name", "artist", "song"}
	FallbackModes   = []string{"estimate", "skip"}
	KnownArtSources = []string{"mpris", "spotify", "itunes", "musicbrainz"}
)

// Duration is a time.Duration that (un)marshals as a Go duration string ("2.5s").
type Duration struct{ time.Duration }

// D is a convenience constructor.
func D(d time.Duration) Duration { return Duration{d} }

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(formatDuration(d.Duration)), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use e.g. \"500ms\", \"2.5s\", \"1m\")", s)
	}
	d.Duration = v
	return nil
}

func formatDuration(d time.Duration) string {
	if d%time.Second == 0 && d < time.Minute {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}

// Config is the full tunecord configuration.
type Config struct {
	General  General  `toml:"general"`
	Discord  Discord  `toml:"discord"`
	Players  Players  `toml:"players"`
	Polling  Polling  `toml:"polling"`
	Presence Presence `toml:"presence"`
	Progress Progress `toml:"progress"`
	Art      Art      `toml:"art"`
}

// General settings.
type General struct {
	LogLevel string `toml:"log_level" comment:"debug | info | warn | error"`
}

// Discord settings.
type Discord struct {
	ClientID string `toml:"client_id" comment:"Discord application ID. The default works out of the box."`
}

// Players settings.
type Players struct {
	Allowlist []string `toml:"allowlist" comment:"Ordered by priority, top = highest. Use names from 'tunecord list'."`
}

// Polling settings.
type Polling struct {
	IdleInterval    Duration `toml:"idle_interval" comment:"Poll interval when nothing is playing (500ms - 60s)."`
	PlayingInterval Duration `toml:"playing_interval" comment:"Poll interval while playing (500ms - 60s)."`
}

// Presence settings.
type Presence struct {
	StatusDisplay   string `toml:"status_display" comment:"What appears under your name: name | artist | song"`
	ShowAlbum       bool   `toml:"show_album" comment:"Show the album name as hover text on the album art."`
	ShowWhenPaused  bool   `toml:"show_when_paused" comment:"Keep the presence (without progress bar) while paused; false clears it."`
	DefaultArtAsset string `toml:"default_art_asset" comment:"Discord asset key (or https URL) used when no album art is found."`
	PausedAsset     string `toml:"paused_asset" comment:"Optional small image asset key shown while paused. Empty = append ' · Paused' to the state line."`
}

// Progress settings.
type Progress struct {
	Fallback      string   `toml:"fallback" comment:"When a player's position is unreliable: estimate | skip"`
	SeekThreshold Duration `toml:"seek_threshold" comment:"Deviation from the predicted position that counts as a seek."`
}

// Art settings.
type Art struct {
	Sources []string `toml:"sources" comment:"Album art lookup order: mpris, spotify, itunes, musicbrainz"`
	Spotify Spotify  `toml:"spotify"`
}

// Spotify API credentials (optional).
type Spotify struct {
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
}

// Enabled reports whether Spotify credentials are configured.
func (s Spotify) Enabled() bool { return s.ClientID != "" && s.ClientSecret != "" }

// Default returns the default configuration.
func Default() Config {
	return Config{
		General: General{LogLevel: "info"},
		Discord: Discord{ClientID: DefaultClientID},
		Players: Players{Allowlist: []string{}},
		Polling: Polling{IdleInterval: D(5 * time.Second), PlayingInterval: D(time.Second)},
		Presence: Presence{
			StatusDisplay:   "artist",
			ShowAlbum:       false,
			ShowWhenPaused:  true,
			DefaultArtAsset: "music",
		},
		Progress: Progress{Fallback: "estimate", SeekThreshold: D(2500 * time.Millisecond)},
		Art: Art{
			Sources: []string{"mpris", "spotify", "itunes", "musicbrainz"},
		},
	}
}

// Load reads the config at path. A missing file yields defaults with
// exists=false. Unknown keys are returned as warnings; invalid values are
// returned as an error.
func Load(path string) (cfg Config, warnings []string, exists bool, err error) {
	cfg = Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil, false, nil
	}
	if err != nil {
		return cfg, nil, false, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, warnings, err = Parse(data)
	if err != nil {
		return cfg, warnings, true, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, warnings, true, nil
}

// Parse decodes TOML data on top of the defaults and validates it.
func Parse(data []byte) (Config, []string, error) {
	var warnings []string
	strict := Default()
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&strict); err != nil {
		var sme *toml.StrictMissingError
		if !errors.As(err, &sme) {
			return Default(), nil, describeDecodeError(err)
		}
		for _, e := range sme.Errors {
			warnings = append(warnings, fmt.Sprintf("unknown config key %q (ignored)", strings.Join(e.Key(), ".")))
		}
	}
	cfg := Default()
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(&cfg); err != nil {
		return Default(), warnings, describeDecodeError(err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, warnings, err
	}
	return cfg, warnings, nil
}

func describeDecodeError(err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		key := strings.Join(de.Key(), ".")
		if key != "" {
			return fmt.Errorf("line %d, column %d (key %s): %s", row, col, key, de.Error())
		}
		return fmt.Errorf("line %d, column %d: %s", row, col, de.Error())
	}
	return err
}

// Normalize trims and lower-cases values and de-duplicates lists, as done on load and save.
func (c *Config) Normalize() { c.normalize() }

func (c *Config) normalize() {
	c.General.LogLevel = strings.ToLower(strings.TrimSpace(c.General.LogLevel))
	c.Discord.ClientID = strings.TrimSpace(c.Discord.ClientID)
	if c.Discord.ClientID == "" {
		c.Discord.ClientID = DefaultClientID
	}
	c.Presence.StatusDisplay = strings.ToLower(strings.TrimSpace(c.Presence.StatusDisplay))
	c.Presence.DefaultArtAsset = strings.TrimSpace(c.Presence.DefaultArtAsset)
	c.Presence.PausedAsset = strings.TrimSpace(c.Presence.PausedAsset)
	c.Progress.Fallback = strings.ToLower(strings.TrimSpace(c.Progress.Fallback))
	c.Art.Spotify.ClientID = strings.TrimSpace(c.Art.Spotify.ClientID)
	c.Art.Spotify.ClientSecret = strings.TrimSpace(c.Art.Spotify.ClientSecret)

	seen := map[string]bool{}
	var allow []string
	for _, p := range c.Players.Allowlist {
		p = strings.TrimSpace(p)
		k := strings.ToLower(p)
		if p == "" || seen[k] {
			continue
		}
		seen[k] = true
		allow = append(allow, p)
	}
	if allow == nil {
		allow = []string{}
	}
	c.Players.Allowlist = allow

	seen = map[string]bool{}
	var srcs []string
	for _, s := range c.Art.Sources {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		srcs = append(srcs, s)
	}
	if srcs == nil {
		srcs = []string{}
	}
	c.Art.Sources = srcs
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Validate checks all values and returns a combined, human-readable error.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if !contains(LogLevels, c.General.LogLevel) {
		add("general.log_level: %q is invalid (allowed: %s)", c.General.LogLevel, strings.Join(LogLevels, ", "))
	}
	if !isDigits(c.Discord.ClientID) {
		add("discord.client_id: %q must be a numeric Discord application ID", c.Discord.ClientID)
	}
	for name, d := range map[string]time.Duration{
		"polling.idle_interval":    c.Polling.IdleInterval.Duration,
		"polling.playing_interval": c.Polling.PlayingInterval.Duration,
	} {
		if d < MinPollInterval || d > MaxPollInterval {
			add("%s: %s is out of range (allowed: %s to %s)", name, d, MinPollInterval, MaxPollInterval)
		}
	}
	if !contains(StatusDisplays, c.Presence.StatusDisplay) {
		add("presence.status_display: %q is invalid (allowed: %s)", c.Presence.StatusDisplay, strings.Join(StatusDisplays, ", "))
	}
	if c.Presence.DefaultArtAsset == "" {
		add("presence.default_art_asset: must not be empty")
	} else if len(c.Presence.DefaultArtAsset) > 256 {
		add("presence.default_art_asset: longer than 256 characters")
	}
	if len(c.Presence.PausedAsset) > 256 {
		add("presence.paused_asset: longer than 256 characters")
	}
	if !contains(FallbackModes, c.Progress.Fallback) {
		add("progress.fallback: %q is invalid (allowed: %s)", c.Progress.Fallback, strings.Join(FallbackModes, ", "))
	}
	if st := c.Progress.SeekThreshold.Duration; st < 500*time.Millisecond || st > time.Minute {
		add("progress.seek_threshold: %s is out of range (allowed: 500ms to 1m)", st)
	}
	for _, s := range c.Art.Sources {
		if !contains(KnownArtSources, s) {
			add("art.sources: unknown source %q (allowed: %s)", s, strings.Join(KnownArtSources, ", "))
		}
	}
	if (c.Art.Spotify.ClientID == "") != (c.Art.Spotify.ClientSecret == "") {
		add("art.spotify: both client_id and client_secret must be set (or both empty)")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const header = `# tunecord configuration
# Generated by 'tunecord config'. The running service reads this file only at
# startup; 'tunecord config' reloads it automatically after saving. After a
# manual edit run 'systemctl --user restart tunecord.service' (or send SIGHUP).

`

// Marshal encodes the config as commented TOML.
func Marshal(c Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(false)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save validates and atomically writes the config with 0600 permissions.
func Save(path string, c Config) error {
	c.normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

// WriteFileAtomic writes data to a temp file in the same directory and
// renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
