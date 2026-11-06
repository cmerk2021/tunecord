package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"tunecord/internal/config"
)

func TestReorderKeys(t *testing.T) {
	m := NewReorder("t", "", []string{"a", "b", "c"})
	m.Key("down")     // cursor b
	m.Key("shift+up") // move b up
	if !reflect.DeepEqual(m.Items, []string{"b", "a", "c"}) || m.Cursor() != 0 {
		t.Fatal(m.Items, m.Cursor())
	}
	m.Key(" ") // grab b
	m.Key("down")
	m.Key("down")
	m.Key("down") // clamped at bottom
	if !reflect.DeepEqual(m.Items, []string{"a", "c", "b"}) || m.Cursor() != 2 {
		t.Fatal(m.Items, m.Cursor())
	}
	m.Key(" ") // drop
	m.Key("up")
	if !reflect.DeepEqual(m.Items, []string{"a", "c", "b"}) || m.Cursor() != 1 {
		t.Fatal("plain up after drop must only move the cursor", m.Items)
	}
	m.Key("K")
	if !reflect.DeepEqual(m.Items, []string{"c", "a", "b"}) {
		t.Fatal(m.Items)
	}
	m.Key("d") // removal disabled
	if len(m.Items) != 3 {
		t.Fatal("removed without AllowRemove")
	}
	m.AllowRemove = true
	m.Key("G")
	m.Key("d")
	if !reflect.DeepEqual(m.Items, []string{"c", "a"}) || m.Cursor() != 1 {
		t.Fatal(m.Items, m.Cursor())
	}
	if !strings.Contains(m.View(), "2. a") {
		t.Fatal(m.View())
	}
	if !m.Key("enter") || !m.Done() || m.Aborted() {
		t.Fatal("enter should finish")
	}
	m2 := NewReorder("t", "", []string{"x"})
	if !m2.Key("esc") || !m2.Aborted() {
		t.Fatal("esc should abort")
	}
}

func TestAllowlistOptions(t *testing.T) {
	det := []Detected{
		{Name: "vlc", Instance: "vlc.instance42", Identity: "VLC media player", Status: "Playing"},
		{Name: "vlc", Instance: "vlc.instance43"},
		{Name: "elisa", Identity: "Elisa"},
	}
	opts := allowlistOptions([]string{"Spotify", "Elisa", "VLC"}, det)
	if len(opts) != 3 {
		t.Fatalf("%+v", opts)
	}
	if opts[0].Value != "vlc" || !opts[0].Selected || !strings.Contains(opts[0].Label, "vlc.instance42") || !strings.Contains(opts[0].Label, "playing") {
		t.Errorf("%+v", opts[0])
	}
	if opts[1].Value != "elisa" || !opts[1].Selected {
		t.Errorf("identity match should preselect: %+v", opts[1])
	}
	if opts[2].Value != "Spotify" || !opts[2].Selected || !strings.Contains(opts[2].Label, "not running") {
		t.Errorf("%+v", opts[2])
	}
}

func TestMergeAllowlist(t *testing.T) {
	det := []Detected{{Name: "vlc"}, {Name: "elisa", Identity: "Elisa"}, {Name: "chromium"}}
	got := mergeAllowlist(
		[]string{"spotify", "Elisa", "vlc", "strawberry"},
		[]string{"vlc", "elisa", "spotify", "chromium"},
		"mpv, org.mpris.MediaPlayer2.cmus VLC",
		det)
	want := []string{"spotify", "Elisa", "vlc", "chromium", "mpv", "cmus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := mergeAllowlist([]string{"vlc"}, nil, "", nil); len(got) != 0 || got == nil {
		t.Fatalf("deselect all: %#v", got)
	}
}

func TestValidateDuration(t *testing.T) {
	v := validateDuration(500*time.Millisecond, time.Minute)
	for s, ok := range map[string]bool{"1s": true, "500ms": true, "2.5s": true, "1m": true, "100ms": false, "2m": false, "abc": false, "": false} {
		if (v(s) == nil) != ok {
			t.Errorf("%q: %v", s, v(s))
		}
	}
}

func TestSummaryMasksSecret(t *testing.T) {
	c := config.Default()
	c.Players.Allowlist = []string{"spotify", "vlc"}
	c.Art.Spotify.ClientID = "abc"
	c.Art.Spotify.ClientSecret = "supersecret"
	s := Summary(c)
	if strings.Contains(s, "supersecret") {
		t.Fatal("secret leaked")
	}
	for _, want := range []string{"1. spotify  2. vlc", "artist", "estimate", "2.5s", "mpris → spotify → itunes → musicbrainz", "(default)"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(Summary(config.Default()), "empty") {
		t.Error("empty allowlist warning")
	}
}
