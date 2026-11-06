package mpris

import (
	"math"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestToMicroseconds(t *testing.T) {
	v := dbus.MakeVariant(int64(123))
	nested := dbus.MakeVariant(dbus.MakeVariant(uint32(77)))
	cases := []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{"int64", int64(183_000_000), 183_000_000, true},
		{"uint64", uint64(183_000_000), 183_000_000, true},
		{"int32", int32(5_000_000), 5_000_000, true},
		{"uint32", uint32(4_000_000_000), 4_000_000_000, true},
		{"int16", int16(-5), -5, true},
		{"uint16", uint16(5), 5, true},
		{"int8", int8(3), 3, true},
		{"byte", uint8(9), 9, true},
		{"int", int(42), 42, true},
		{"uint", uint(42), 42, true},
		{"float64", float64(1234.6), 1235, true},
		{"float32", float32(10), 10, true},
		{"negative int64", int64(-1), -1, true},
		{"string int", "183000000", 183_000_000, true},
		{"string spaced", "  42 ", 42, true},
		{"string float", "1.5e6", 1_500_000, true},
		{"string garbage", "abc", 0, false},
		{"empty string", "", 0, false},
		{"bytes", []byte("77"), 77, true},
		{"variant", v, 123, true},
		{"variant ptr", &v, 123, true},
		{"nested variant", nested, 77, true},
		{"nil", nil, 0, false},
		{"nil variant ptr", (*dbus.Variant)(nil), 0, false},
		{"uint64 overflow", uint64(math.MaxUint64), 0, false},
		{"NaN", math.NaN(), 0, false},
		{"Inf", math.Inf(1), 0, false},
		{"huge float", 1e300, 0, false},
		{"bool", true, 0, false},
		{"slice", []int64{1}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ToMicroseconds(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("ToMicroseconds(%#v) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestToFloat(t *testing.T) {
	for in, want := range map[any]float64{1.5: 1.5, int32(2): 2, "0.75": 0.75, float32(1.25): 1.25} {
		got, ok := ToFloat(in)
		if !ok || got != want {
			t.Errorf("ToFloat(%v)=%v,%v", in, got, ok)
		}
	}
	if _, ok := ToFloat(math.NaN()); ok {
		t.Error("NaN accepted")
	}
	if _, ok := ToFloat("x"); ok {
		t.Error("garbage accepted")
	}
}

func TestParseBusName(t *testing.T) {
	cases := []struct {
		bus, suffix, base, inst string
		ok                      bool
	}{
		{"org.mpris.MediaPlayer2.vlc", "vlc", "vlc", "", true},
		{"org.mpris.MediaPlayer2.vlc.instance12345", "vlc.instance12345", "vlc", "instance12345", true},
		{"org.mpris.MediaPlayer2.chromium.instance1234", "chromium.instance1234", "chromium", "instance1234", true},
		{"org.mpris.MediaPlayer2.firefox.instance_1_77", "firefox.instance_1_77", "firefox", "instance_1_77", true},
		{"org.mpris.MediaPlayer2.spotify", "spotify", "spotify", "", true},
		{"org.mpris.MediaPlayer2.", "", "", "", false},
		{"org.freedesktop.Notifications", "", "", "", false},
	}
	for _, c := range cases {
		id, ok := ParseBusName(c.bus)
		if ok != c.ok {
			t.Fatalf("%s ok=%v", c.bus, ok)
		}
		if ok && (id.Suffix != c.suffix || id.Base != c.base || id.Instance != c.inst) {
			t.Errorf("%s => %+v", c.bus, id)
		}
	}
}

func TestMatchesEntry(t *testing.T) {
	id, _ := ParseBusName("org.mpris.MediaPlayer2.vlc.instance42")
	for _, e := range []string{"vlc", "VLC", " vlc ", "vlc.instance42", "VLC media player"} {
		if !MatchesEntry(id, "VLC media player", e) {
			t.Errorf("%q should match", e)
		}
	}
	for _, e := range []string{"", "vl", "spotify", "vlc.instance43"} {
		if MatchesEntry(id, "VLC media player", e) {
			t.Errorf("%q should not match", e)
		}
	}
}

func TestParseMetadata(t *testing.T) {
	raw := map[string]any{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/org/vlc/track/3")),
		"xesam:title":   dbus.MakeVariant(" Song "),
		"xesam:artist":  dbus.MakeVariant([]string{"A", "", "B"}),
		"xesam:album":   dbus.MakeVariant("Album"),
		"mpris:length":  dbus.MakeVariant(uint64(200_000_000)),
		"mpris:artUrl":  dbus.MakeVariant("https://i.scdn.co/image/x"),
	}
	m := ParseMetadata(raw)
	if m.TrackID != "/org/vlc/track/3" || m.Title != "Song" || m.Album != "Album" {
		t.Errorf("%+v", m)
	}
	if len(m.Artists) != 2 || m.Artists[1] != "B" {
		t.Errorf("artists %v", m.Artists)
	}
	if !m.LengthOK || m.Length != 200_000_000 {
		t.Errorf("length %d %v", m.Length, m.LengthOK)
	}
	if m.RawTypes["mpris:length"] != "uint64" {
		t.Errorf("raw type %s", m.RawTypes["mpris:length"])
	}
	// Single-string artist and albumArtist fallback.
	m2 := ParseMetadata(map[string]any{"xesam:albumArtist": dbus.MakeVariant("Solo"), "mpris:length": dbus.MakeVariant("1000")})
	if len(m2.Artists) != 1 || m2.Artists[0] != "Solo" || m2.Length != 1000 {
		t.Errorf("%+v", m2)
	}
	// Album change is not a track change.
	m3 := m
	m3.Album = "Other"
	if m3.TrackKey() != m.TrackKey() {
		t.Error("album change must not change track key")
	}
	m4 := m
	m4.Title = "Next"
	if m4.TrackKey() == m.TrackKey() {
		t.Error("title change must change track key")
	}
}
