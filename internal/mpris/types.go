// Package mpris talks to MPRIS media players over the D-Bus session bus.
package mpris

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// D-Bus names used by MPRIS.
const (
	BusPrefix     = "org.mpris.MediaPlayer2."
	ObjectPath    = "/org/mpris/MediaPlayer2"
	RootIface     = "org.mpris.MediaPlayer2"
	PlayerIface   = "org.mpris.MediaPlayer2.Player"
	PropsIface    = "org.freedesktop.DBus.Properties"
	NoTrackID     = "/org/mpris/MediaPlayer2/TrackList/NoTrack"
	instanceToken = ".instance"
)

// PlayerID identifies an MPRIS player.
type PlayerID struct {
	BusName  string // org.mpris.MediaPlayer2.vlc.instance1234
	Suffix   string // vlc.instance1234
	Base     string // vlc
	Instance string // instance1234 (may be empty)
}

// ParseBusName parses an MPRIS bus name. ok is false for non-MPRIS names.
func ParseBusName(bus string) (PlayerID, bool) {
	if !strings.HasPrefix(bus, BusPrefix) || len(bus) == len(BusPrefix) {
		return PlayerID{}, false
	}
	suffix := strings.TrimPrefix(bus, BusPrefix)
	id := PlayerID{BusName: bus, Suffix: suffix, Base: suffix}
	// Instance suffixes look like "vlc.instance12345" or
	// "chromium.instance1234"; some players use other dotted suffixes
	// (e.g. "firefox.instance_1_23"), so split on the first ".instance".
	if i := strings.Index(suffix, instanceToken); i > 0 {
		id.Base = suffix[:i]
		id.Instance = suffix[i+1:]
	}
	return id, true
}

// MatchesEntry reports whether an allowlist entry refers to this player.
// Matching is case-insensitive against the base name, the full suffix and
// the player's Identity.
func MatchesEntry(id PlayerID, identity, entry string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	if e == "" {
		return false
	}
	if e == strings.ToLower(id.Base) || e == strings.ToLower(id.Suffix) {
		return true
	}
	return identity != "" && e == strings.ToLower(strings.TrimSpace(identity))
}

// Status is the MPRIS PlaybackStatus.
type Status string

// Playback statuses.
const (
	Playing Status = "Playing"
	Paused  Status = "Paused"
	Stopped Status = "Stopped"
)

// ParseStatus normalizes a PlaybackStatus value.
func ParseStatus(s string) Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "playing":
		return Playing
	case "paused":
		return Paused
	default:
		return Stopped
	}
}

// Metadata is the subset of mpris metadata tunecord uses.
type Metadata struct {
	TrackID  string
	Title    string
	Artists  []string
	Album    string
	Length   int64 // microseconds
	LengthOK bool
	ArtURL   string
	URL      string
	Raw      map[string]any // unwrapped raw values, for diagnostics
	RawTypes map[string]string
}

// TrackKey identifies a track. The trackid is combined with title and artist
// because several players reuse one trackid for every track (or omit it).
// Length and album are deliberately excluded: many players publish them a
// moment after the track starts, which must not look like a track change
// (they are handled as metadata updates instead).
func (m Metadata) TrackKey() string {
	tid := m.TrackID
	if tid == NoTrackID {
		tid = ""
	}
	url := ""
	if m.Title == "" {
		url = m.URL
	}
	return strings.Join([]string{tid, m.Title, strings.Join(m.Artists, ", "), url}, "\x1f")
}

// Empty reports whether there is nothing to display.
func (m Metadata) Empty() bool {
	return m.Title == "" && len(m.Artists) == 0 && m.Album == "" && m.URL == ""
}

// ParseMetadata converts an MPRIS metadata map.
func ParseMetadata(raw map[string]any) Metadata {
	m := Metadata{Raw: map[string]any{}, RawTypes: map[string]string{}}
	for k, v := range raw {
		u := unwrap(v)
		m.Raw[k] = u
		m.RawTypes[k] = fmt.Sprintf("%T", u)
	}
	if s, ok := ToString(raw["mpris:trackid"]); ok {
		m.TrackID = s
	}
	if s, ok := ToString(raw["xesam:title"]); ok {
		m.Title = strings.TrimSpace(s)
	}
	m.Artists = ToStringSlice(raw["xesam:artist"])
	if len(m.Artists) == 0 {
		m.Artists = ToStringSlice(raw["xesam:albumArtist"])
	}
	if s, ok := ToString(raw["xesam:album"]); ok {
		m.Album = strings.TrimSpace(s)
	}
	if s, ok := ToString(raw["mpris:artUrl"]); ok {
		m.ArtURL = strings.TrimSpace(s)
	}
	if s, ok := ToString(raw["xesam:url"]); ok {
		m.URL = strings.TrimSpace(s)
	}
	m.Length, m.LengthOK = ToMicroseconds(raw["mpris:length"])
	return m
}

// Snapshot is a fresh read of a player's state.
type Snapshot struct {
	ID          PlayerID
	Identity    string
	Status      Status
	StatusRaw   any
	Metadata    Metadata
	Rate        float64
	RateOK      bool
	RateRaw     any
	Position    int64 // microseconds
	PositionOK  bool
	PositionRaw any
	PositionErr error
	ReadAt      time.Time
}

// EventKind classifies bus events.
type EventKind int

// Event kinds.
const (
	PlayerAdded EventKind = iota
	PlayerRemoved
	PropertiesChanged
	Seeked
)

func (k EventKind) String() string {
	switch k {
	case PlayerAdded:
		return "added"
	case PlayerRemoved:
		return "removed"
	case PropertiesChanged:
		return "properties-changed"
	case Seeked:
		return "seeked"
	}
	return "unknown"
}

// Event is a signal-driven notification from the bus.
type Event struct {
	Kind       EventKind
	BusName    string
	Position   int64 // Seeked only, microseconds
	PositionOK bool
	At         time.Time
}

// Bus is the D-Bus access used by the service. It is an interface so that
// the service and progress logic can be tested with fake players.
type Bus interface {
	// ListPlayers returns all MPRIS players currently on the bus.
	ListPlayers(ctx context.Context) ([]PlayerID, error)
	// Identity returns the player's Identity property ("" if unknown).
	Identity(ctx context.Context, id PlayerID) string
	// Snapshot reads status, metadata, rate and (directly, uncached) the
	// position of a player.
	Snapshot(ctx context.Context, id PlayerID) (Snapshot, error)
	// Events delivers player add/remove, PropertiesChanged and Seeked
	// notifications. It is closed when the bus connection is lost.
	Events() <-chan Event
	// Close releases the connection.
	Close() error
}
