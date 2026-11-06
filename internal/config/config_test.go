package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultsValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if c.Presence.ShowAlbum {
		t.Error("show_album must default to false")
	}
	if !c.Presence.ShowWhenPaused {
		t.Error("show_when_paused must default to true")
	}
	if c.Progress.SeekThreshold.Duration != 2500*time.Millisecond {
		t.Errorf("seek_threshold default = %v", c.Progress.SeekThreshold)
	}
	if c.Discord.ClientID != "1525387248332443781" {
		t.Errorf("client id default = %q", c.Discord.ClientID)
	}
}

func TestParseFullAndUnknownKeys(t *testing.T) {
	data := `
[general]
log_level = "DEBUG"
[players]
allowlist = ["vlc", "Spotify", "vlc", " elisa "]
[polling]
idle_interval = "10s"
playing_interval = "750ms"
[presence]
status_display = "song"
show_album = true
show_when_paused = false
bogus = 1
[progress]
fallback = "skip"
seek_threshold = "3s"
[art]
sources = ["itunes", "mpris"]
[mystery]
x = 1
`
	c, warns, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(warns) != 2 {
		t.Fatalf("want 2 unknown-key warnings, got %v", warns)
	}
	joined := strings.Join(warns, "|")
	if !strings.Contains(joined, "presence.bogus") || !strings.Contains(joined, "mystery") {
		t.Errorf("warnings missing keys: %v", warns)
	}
	if c.General.LogLevel != "debug" {
		t.Errorf("log level %q", c.General.LogLevel)
	}
	if !reflect.DeepEqual(c.Players.Allowlist, []string{"vlc", "Spotify", "elisa"}) {
		t.Errorf("allowlist %v", c.Players.Allowlist)
	}
	if c.Polling.PlayingInterval.Duration != 750*time.Millisecond || c.Polling.IdleInterval.Duration != 10*time.Second {
		t.Errorf("polling %+v", c.Polling)
	}
	if c.Presence.StatusDisplay != "song" || !c.Presence.ShowAlbum || c.Presence.ShowWhenPaused {
		t.Errorf("presence %+v", c.Presence)
	}
	if c.Progress.Fallback != "skip" || c.Progress.SeekThreshold.Duration != 3*time.Second {
		t.Errorf("progress %+v", c.Progress)
	}
	// Unspecified values keep defaults.
	if c.Discord.ClientID != DefaultClientID || c.Presence.DefaultArtAsset != "music" {
		t.Errorf("defaults lost: %+v %+v", c.Discord, c.Presence)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"log level":      "[general]\nlog_level = \"loud\"",
		"poll too fast":  "[polling]\nplaying_interval = \"100ms\"",
		"poll too slow":  "[polling]\nidle_interval = \"5m\"",
		"bad duration":   "[polling]\nidle_interval = \"soon\"",
		"status display": "[presence]\nstatus_display = \"album\"",
		"fallback":       "[progress]\nfallback = \"guess\"",
		"seek threshold": "[progress]\nseek_threshold = \"10ms\"",
		"art source":     "[art]\nsources = [\"lastfm\"]",
		"client id":      "[discord]\nclient_id = \"abc\"",
		"half spotify":   "[art.spotify]\nclient_id = \"x\"",
		"wrong type":     "[presence]\nshow_album = \"yes\"",
		"syntax":         "[presence\nshow_album = true",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Parse([]byte(data)); err == nil {
				t.Fatalf("expected error for %q", data)
			}
		})
	}
}

func TestRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")
	c := Default()
	c.Players.Allowlist = []string{"spotify", "vlc"}
	c.Presence.StatusDisplay = "name"
	c.Presence.PausedAsset = "pause"
	c.Progress.SeekThreshold = D(1500 * time.Millisecond)
	c.Art.Spotify = Spotify{ClientID: "id", ClientSecret: "secret"}
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", st.Mode().Perm())
	}
	got, warns, exists, err := Load(path)
	if err != nil || !exists || len(warns) != 0 {
		t.Fatalf("Load: %v exists=%v warns=%v", err, exists, warns)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, c)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestLoadMissing(t *testing.T) {
	c, _, exists, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || exists {
		t.Fatalf("err=%v exists=%v", err, exists)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Error("missing config should yield defaults")
	}
}

func TestPathsRespectXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	t.Setenv("XDG_CACHE_HOME", "/x/cache")
	t.Setenv("XDG_RUNTIME_DIR", "/x/run")
	if ConfigPath() != "/x/cfg/tunecord/config.toml" {
		t.Error(ConfigPath())
	}
	if CacheDir() != "/x/cache/tunecord" {
		t.Error(CacheDir())
	}
	if PidfilePath() != "/x/run/tunecord.pid" {
		t.Error(PidfilePath())
	}
	if SystemdUserDir() != "/x/cfg/systemd/user" {
		t.Error(SystemdUserDir())
	}
}
