// Package presence builds the Discord activity payload from player state.
package presence

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"tunecord/internal/discord"
)

// Discord field limits.
const (
	MinTextLen = 2
	MaxTextLen = 128
	MaxURLLen  = 256
)

// Fallback texts for missing metadata.
const (
	UnknownTitle  = "Unknown Title"
	UnknownArtist = "Unknown Artist"
	PausedSuffix  = " · Paused"
	PausedText    = "Paused"
	padRune       = '\u2800' // Braille blank: not trimmed as whitespace by Discord
	ellipsis      = "…"
)

// Input is the state to display.
type Input struct {
	Title         string
	Artists       []string
	Album         string
	ArtURL        string // resolved album art URL ("" = none)
	Paused        bool
	HasTimestamps bool
	Start, End    time.Time
}

// Options are the presence-related config values.
type Options struct {
	StatusDisplay   string // "name" | "artist" | "song"
	ShowAlbum       bool
	DefaultArtAsset string
	PausedAsset     string
}

// StatusDisplayType maps the config value to Discord's enum.
func StatusDisplayType(s string) int {
	switch strings.ToLower(s) {
	case "name":
		return discord.StatusDisplayName
	case "song":
		return discord.StatusDisplayDetails
	default:
		return discord.StatusDisplayState
	}
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Fit trims s, substitutes fallback when empty, truncates rune-safely to
// at most max UTF-16 code units (Discord measures in JS string length)
// adding "…", and pads to the minimum length.
func Fit(s, fallback string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		s = fallback
	}
	if utf16Len(s) > max {
		budget := max - utf16Len(ellipsis)
		var b strings.Builder
		n := 0
		for _, r := range s {
			l := utf16.RuneLen(r)
			if n+l > budget {
				break
			}
			b.WriteRune(r)
			n += l
		}
		s = strings.TrimRight(b.String(), " ") + ellipsis
	}
	for utf16Len(s) < MinTextLen {
		s += string(padRune)
	}
	return s
}

// ValidImageURL reports whether u is an http(s) URL Discord can display.
func ValidImageURL(u string) bool {
	if u == "" || len(u) > MaxURLLen {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return false
	}
	return p.Scheme == "https" || p.Scheme == "http"
}

// Build creates the Listening activity.
func Build(in Input, o Options) *discord.Activity {
	a := &discord.Activity{
		Type:              discord.ActivityListening,
		StatusDisplayType: StatusDisplayType(o.StatusDisplay),
		Details:           Fit(in.Title, UnknownTitle, MaxTextLen),
	}

	artists := strings.Join(nonEmpty(in.Artists), ", ")
	usePausedImage := in.Paused && o.PausedAsset != ""
	if in.Paused && !usePausedImage {
		a.State = Fit(artists, UnknownArtist, MaxTextLen-utf16Len(PausedSuffix)) + PausedSuffix
	} else {
		a.State = Fit(artists, UnknownArtist, MaxTextLen)
	}

	assets := &discord.Assets{}
	if ValidImageURL(in.ArtURL) {
		assets.LargeImage = in.ArtURL
	} else {
		assets.LargeImage = o.DefaultArtAsset
	}
	if o.ShowAlbum && strings.TrimSpace(in.Album) != "" {
		assets.LargeText = Fit(in.Album, "", MaxTextLen)
	}
	if usePausedImage {
		assets.SmallImage = o.PausedAsset
		assets.SmallText = PausedText
	}
	if *assets != (discord.Assets{}) {
		a.Assets = assets
	}

	if in.HasTimestamps && !in.Paused && !in.Start.IsZero() && in.End.After(in.Start) {
		a.Timestamps = &discord.Timestamps{Start: in.Start.UnixMilli(), End: in.End.UnixMilli()}
	}
	return a
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
