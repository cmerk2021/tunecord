package presence

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tunecord/internal/discord"
)

var opts = Options{StatusDisplay: "artist", DefaultArtAsset: "music"}

func TestFit(t *testing.T) {
	if got := Fit("", "Unknown Artist", 128); got != "Unknown Artist" {
		t.Errorf("fallback: %q", got)
	}
	if got := Fit("  a  ", "x", 128); got != "a\u2800" {
		t.Errorf("pad: %q", got)
	}
	if got := Fit("", "", 128); utf16Len(got) != 2 {
		t.Errorf("empty pad: %q", got)
	}
	long := strings.Repeat("é", 300)
	got := Fit(long, "", 128)
	if utf16Len(got) > 128 || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("truncate: len=%d %q", utf16Len(got), got)
	}
	// Astral-plane runes count as 2 UTF-16 units and must not be split.
	emoji := strings.Repeat("🎵", 100)
	got = Fit(emoji, "", 128)
	if utf16Len(got) > 128 || !utf8.ValidString(got) {
		t.Errorf("emoji truncate: %d", utf16Len(got))
	}
	exact := strings.Repeat("a", 128)
	if Fit(exact, "", 128) != exact {
		t.Error("exactly 128 should be untouched")
	}
	if got := Fit("multi \n  space", "", 128); got != "multi space" {
		t.Errorf("whitespace: %q", got)
	}
}

func TestStatusDisplayMapping(t *testing.T) {
	cases := map[string]int{"name": 0, "artist": 1, "song": 2, "": 1, "SONG": 2}
	for in, want := range cases {
		if got := StatusDisplayType(in); got != want {
			t.Errorf("%q -> %d want %d", in, got, want)
		}
		a := Build(Input{Title: "t"}, Options{StatusDisplay: in, DefaultArtAsset: "music"})
		if a.StatusDisplayType != want {
			t.Errorf("Build %q -> %d", in, a.StatusDisplayType)
		}
	}
}

func TestBuildPlaying(t *testing.T) {
	start := time.Unix(1000, 0)
	in := Input{
		Title: "Song", Artists: []string{"A", " ", "B"}, Album: "Album",
		ArtURL: "https://i.scdn.co/image/abc", HasTimestamps: true,
		Start: start, End: start.Add(3 * time.Minute),
	}
	a := Build(in, opts)
	if a.Type != discord.ActivityListening || a.Details != "Song" || a.State != "A, B" {
		t.Fatalf("%+v", a)
	}
	if a.Timestamps == nil || a.Timestamps.Start != 1_000_000 || a.Timestamps.End != 1_180_000 {
		t.Fatalf("timestamps %+v", a.Timestamps)
	}
	if a.Assets.LargeImage != in.ArtURL {
		t.Errorf("art %q", a.Assets.LargeImage)
	}
	if a.Assets.LargeText != "" {
		t.Error("album must not be sent when show_album=false")
	}
	o := opts
	o.ShowAlbum = true
	if a := Build(in, o); a.Assets.LargeText != "Album" {
		t.Errorf("show_album: %q", a.Assets.LargeText)
	}
}

func TestBuildFallbacks(t *testing.T) {
	a := Build(Input{}, opts)
	if a.Details != UnknownTitle || a.State != UnknownArtist {
		t.Errorf("%+v", a)
	}
	if a.Assets.LargeImage != "music" || a.Timestamps != nil {
		t.Errorf("%+v %+v", a.Assets, a.Timestamps)
	}
	for _, bad := range []string{"file:///home/me/cover.jpg", "data:image/png;base64,xx", "https://x.example/" + strings.Repeat("a", 300), "not a url"} {
		if a := Build(Input{Title: "t", ArtURL: bad}, opts); a.Assets.LargeImage != "music" {
			t.Errorf("%q accepted", bad)
		}
	}
	// show_album with empty album: no large text.
	o := opts
	o.ShowAlbum = true
	if a := Build(Input{Title: "t"}, o); a.Assets.LargeText != "" {
		t.Error("empty album text sent")
	}
}

func TestBuildPaused(t *testing.T) {
	start := time.Unix(1000, 0)
	in := Input{Title: "Song", Artists: []string{"Artist"}, Paused: true, HasTimestamps: true, Start: start, End: start.Add(time.Minute)}
	a := Build(in, opts)
	if a.Timestamps != nil {
		t.Error("paused must not have timestamps")
	}
	if a.State != "Artist · Paused" {
		t.Errorf("state %q", a.State)
	}
	in.Artists = []string{strings.Repeat("x", 200)}
	a = Build(in, opts)
	if utf16Len(a.State) > MaxTextLen || !strings.HasSuffix(a.State, PausedSuffix) {
		t.Errorf("long paused state %d %q", utf16Len(a.State), a.State)
	}
	o := opts
	o.PausedAsset = "pause"
	in.Artists = []string{"Artist"}
	a = Build(in, o)
	if a.State != "Artist" || a.Assets.SmallImage != "pause" || a.Assets.SmallText != "Paused" {
		t.Errorf("paused asset: %+v %+v", a.State, a.Assets)
	}
}
