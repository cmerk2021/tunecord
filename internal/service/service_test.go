package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tunecord/internal/art"
	"tunecord/internal/config"
	"tunecord/internal/discord"
	"tunecord/internal/mpris"
	"tunecord/internal/progress"
)

// ---------------------------------------------------------------- fakes

type fakePlayer struct {
	id       mpris.PlayerID
	identity string
	status   mpris.Status
	meta     map[string]any
	rate     float64
	// position model: posAt at wall posWall, advancing while Playing
	posAt   time.Duration
	posWall time.Time
	stuck   bool
	posType string // "int64", "uint64", "int32", "string", "float64"
}

func (p *fakePlayer) position(now time.Time) time.Duration {
	if p.stuck {
		return 0
	}
	if p.status != mpris.Playing {
		return p.posAt
	}
	return p.posAt + time.Duration(float64(now.Sub(p.posWall))*p.rate)
}

type fakeBus struct {
	mu      sync.Mutex
	players map[string]*fakePlayer
	events  chan mpris.Event
	closed  bool
}

func newFakeBus() *fakeBus {
	return &fakeBus{players: map[string]*fakePlayer{}, events: make(chan mpris.Event, 64)}
}

func (b *fakeBus) add(suffix, identity, title, artist string, length time.Duration) *fakePlayer {
	id, _ := mpris.ParseBusName(mpris.BusPrefix + suffix)
	p := &fakePlayer{id: id, identity: identity, status: mpris.Playing, rate: 1, posWall: time.Now(), posType: "int64"}
	p.meta = map[string]any{
		"mpris:trackid": "/track/" + title,
		"xesam:title":   title,
		"xesam:artist":  []string{artist},
		"xesam:album":   "Album " + title,
		"mpris:length":  int64(length / time.Microsecond),
	}
	b.mu.Lock()
	b.players[id.BusName] = p
	b.mu.Unlock()
	b.emit(mpris.Event{Kind: mpris.PlayerAdded, BusName: id.BusName})
	return p
}

func (b *fakeBus) update(p *fakePlayer, f func(p *fakePlayer)) {
	b.mu.Lock()
	f(p)
	b.mu.Unlock()
	b.emit(mpris.Event{Kind: mpris.PropertiesChanged, BusName: p.id.BusName})
}

func (b *fakeBus) remove(p *fakePlayer) {
	b.mu.Lock()
	delete(b.players, p.id.BusName)
	b.mu.Unlock()
	b.emit(mpris.Event{Kind: mpris.PlayerRemoved, BusName: p.id.BusName})
}

func (b *fakeBus) emit(e mpris.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.events <- e
	}
}

func (b *fakeBus) ListPlayers(context.Context) ([]mpris.PlayerID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []mpris.PlayerID
	for _, p := range b.players {
		ids = append(ids, p.id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].BusName < ids[j].BusName })
	return ids, nil
}

func (b *fakeBus) Identity(_ context.Context, id mpris.PlayerID) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.players[id.BusName]; ok {
		return p.identity
	}
	return ""
}

func (b *fakeBus) Snapshot(_ context.Context, id mpris.PlayerID) (mpris.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.players[id.BusName]
	if !ok {
		return mpris.Snapshot{}, errors.New("org.freedesktop.DBus.Error.ServiceUnknown")
	}
	now := time.Now()
	us := p.position(now).Microseconds()
	var raw any
	switch p.posType {
	case "uint64":
		raw = uint64(us)
	case "int32":
		raw = int32(us)
	case "string":
		raw = fmt.Sprint(us)
	case "float64":
		raw = float64(us)
	default:
		raw = us
	}
	pos, posOK := mpris.ToMicroseconds(raw)
	return mpris.Snapshot{
		ID: id, Identity: p.identity, Status: p.status,
		Metadata: mpris.ParseMetadata(p.meta),
		Rate:     p.rate, RateOK: true,
		Position: pos, PositionOK: posOK, PositionRaw: raw,
		ReadAt: now,
	}, nil
}

func (b *fakeBus) Events() <-chan mpris.Event { return b.events }

func (b *fakeBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.events)
	}
	return nil
}

type recConn struct {
	mu   sync.Mutex
	acts []*discord.Activity
	done chan struct{}
}

func (c *recConn) SetActivity(_ context.Context, a *discord.Activity) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acts = append(c.acts, a)
	return nil
}
func (c *recConn) Done() <-chan struct{} { return c.done }
func (c *recConn) Path() string          { return "/fake/discord-ipc-0" }
func (c *recConn) Close() error          { return nil }
func (c *recConn) last() (*discord.Activity, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.acts) == 0 {
		return nil, 0
	}
	return c.acts[len(c.acts)-1], len(c.acts)
}

// ---------------------------------------------------------------- harness

type harness struct {
	t      *testing.T
	bus    *fakeBus
	conn   *recConn
	cancel context.CancelFunc
	done   chan error
}

type slowSource struct {
	delay time.Duration
	url   string
}

func (s slowSource) Name() string { return "itunes" }
func (s slowSource) Lookup(ctx context.Context, _ art.Query) (string, error) {
	select {
	case <-time.After(s.delay):
		return s.url, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func start(t *testing.T, cfg config.Config, bus *fakeBus, tweak func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/config.toml"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	// Bypass validation limits for fast tests.
	h := &harness{t: t, bus: bus, conn: &recConn{done: make(chan struct{})}, done: make(chan error, 1)}
	o := Options{
		ConfigPath: path,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewBus:     func(context.Context) (mpris.Bus, error) { return bus, nil },
		Dialer:     func(context.Context, string) (discord.Conn, error) { return h.conn, nil },
		NewLimiter: func() *discord.Limiter {
			return &discord.Limiter{MinSpacing: 20 * time.Millisecond, Window: time.Second, Max: 50}
		},
		SettleDelay: 10 * time.Millisecond,
		ArtCache:    art.OpenCache(""),
		NewResolver: func(c config.Config, cache *art.Cache, log *slog.Logger) *art.Resolver {
			return art.NewResolverWith([]string{"mpris"}, nil, cache, log)
		},
		ProgressTweak: func(c *progress.Config) {
			c.StuckGrace = 300 * time.Millisecond
		},
	}
	if tweak != nil {
		tweak(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- Run(ctx, o) }()
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not stop")
	}
}

func (h *harness) waitAct(what string, cond func(a *discord.Activity) bool) *discord.Activity {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, n := h.conn.last()
		if n > 0 && cond(a) {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, _ := h.conn.last()
	h.t.Fatalf("timed out waiting for %s; last activity %+v", what, a)
	return nil
}

func fastCfg(allow ...string) config.Config {
	c := config.Default()
	c.Players.Allowlist = allow
	return c
}

// ---------------------------------------------------------------- tests

func TestSelectPriority(t *testing.T) {
	mk := func(s string, st mpris.Status) Candidate {
		id, _ := mpris.ParseBusName(mpris.BusPrefix + s)
		return Candidate{ID: id, Status: st}
	}
	allow := []string{"spotify", "vlc", "elisa"}
	cases := []struct {
		name   string
		cands  []Candidate
		paused bool
		want   int
	}{
		{"higher playing wins", []Candidate{mk("vlc", mpris.Playing), mk("spotify", mpris.Playing)}, true, 1},
		{"playing beats higher paused", []Candidate{mk("spotify", mpris.Paused), mk("vlc", mpris.Playing)}, true, 1},
		{"highest paused when none playing", []Candidate{mk("elisa", mpris.Paused), mk("vlc", mpris.Paused)}, true, 1},
		{"paused ignored when show_when_paused=false", []Candidate{mk("vlc", mpris.Paused)}, false, -1},
		{"not allowlisted ignored", []Candidate{mk("firefox", mpris.Playing)}, true, -1},
		{"stopped ignored", []Candidate{mk("vlc", mpris.Stopped)}, true, -1},
		{"instance suffix matches base", []Candidate{mk("chromium.instance1", mpris.Playing), mk("vlc.instance99", mpris.Playing)}, true, 1},
	}
	for _, c := range cases {
		if got := Select(c.cands, allow, c.paused); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	if Select([]Candidate{mk("vlc", mpris.Playing)}, nil, true) != -1 {
		t.Error("empty allowlist must select nothing")
	}
	// Identity matching, case-insensitive.
	c := mk("org.kde.elisa", mpris.Playing)
	c.Identity = "Elisa"
	if Select([]Candidate{c}, []string{"ELISA"}, true) != 0 {
		t.Error("identity match")
	}
}

func TestServicePriorityPauseAndExit(t *testing.T) {
	bus := newFakeBus()
	sp := bus.add("spotify", "Spotify", "SpotSong", "SpotArtist", 200*time.Second)
	vlc := bus.add("vlc.instance123", "VLC media player", "VlcSong", "VlcArtist", 300*time.Second)
	bus.add("firefox.instance1", "Firefox", "Web", "Web", 100*time.Second)
	h := start(t, fastCfg("spotify", "vlc"), bus, nil)
	defer h.stop()

	a := h.waitAct("spotify shown", func(a *discord.Activity) bool { return a != nil && a.Details == "SpotSong" })
	if a.Type != 2 || a.State != "SpotArtist" || a.Timestamps == nil || a.StatusDisplayType != 1 {
		t.Fatalf("%+v", a)
	}
	if a.Assets == nil || a.Assets.LargeImage != "music" || a.Assets.LargeText != "" {
		t.Fatalf("assets %+v", a.Assets)
	}
	// Spotify pauses: VLC (playing, lower priority) wins.
	bus.update(sp, func(p *fakePlayer) { p.posAt = p.position(time.Now()); p.status = mpris.Paused })
	h.waitAct("vlc shown", func(a *discord.Activity) bool { return a != nil && a.Details == "VlcSong" })
	// VLC stops: highest paused (spotify) shown, without timestamps.
	bus.update(vlc, func(p *fakePlayer) { p.status = mpris.Stopped })
	a = h.waitAct("paused spotify", func(a *discord.Activity) bool { return a != nil && a.Details == "SpotSong" })
	if a.Timestamps != nil || !strings.HasSuffix(a.State, " · Paused") {
		t.Fatalf("paused presence %+v", a)
	}
	// Spotify resumes from where it was.
	bus.update(sp, func(p *fakePlayer) { p.posWall = time.Now(); p.status = mpris.Playing })
	h.waitAct("resumed", func(a *discord.Activity) bool { return a != nil && a.Timestamps != nil && a.Details == "SpotSong" })
	// Player exit clears.
	bus.remove(sp)
	h.waitAct("cleared on exit", func(a *discord.Activity) bool { return a == nil })
}

func TestServiceClearsWhenPausedIfConfigured(t *testing.T) {
	bus := newFakeBus()
	p := bus.add("vlc", "VLC", "Song", "Artist", 100*time.Second)
	cfg := fastCfg("vlc")
	cfg.Presence.ShowWhenPaused = false
	h := start(t, cfg, bus, nil)
	h.waitAct("shown", func(a *discord.Activity) bool { return a != nil })
	bus.update(p, func(p *fakePlayer) { p.status = mpris.Paused })
	h.waitAct("cleared on pause", func(a *discord.Activity) bool { return a == nil })
	bus.update(p, func(p *fakePlayer) { p.posWall = time.Now(); p.status = mpris.Playing })
	h.waitAct("shown again", func(a *discord.Activity) bool { return a != nil })
	h.stop()
	if a, _ := h.conn.last(); a != nil {
		t.Fatal("presence not cleared on shutdown")
	}
}

func TestServicePositionTypes(t *testing.T) {
	for _, typ := range []string{"int64", "uint64", "int32", "string", "float64"} {
		t.Run(typ, func(t *testing.T) {
			bus := newFakeBus()
			p := bus.add("vlc", "VLC", "Song", "Artist", 600*time.Second)
			p.posType = typ
			p.posAt = 60 * time.Second
			p.posWall = time.Now()
			h := start(t, fastCfg("vlc"), bus, nil)
			defer h.stop()
			a := h.waitAct("timestamps", func(a *discord.Activity) bool { return a != nil && a.Timestamps != nil })
			now := time.Now().UnixMilli()
			wantStart := now - 60_000
			if d := a.Timestamps.Start - wantStart; d < -1500 || d > 1500 {
				t.Fatalf("start off by %dms", d)
			}
			if a.Timestamps.End-a.Timestamps.Start != 600_000 {
				t.Fatalf("length %d", a.Timestamps.End-a.Timestamps.Start)
			}
		})
	}
}

func TestServiceStuckPositionFallbacks(t *testing.T) {
	for _, fb := range []string{"estimate", "skip"} {
		t.Run(fb, func(t *testing.T) {
			bus := newFakeBus()
			p := bus.add("brokenplayer", "Broken", "Song", "Artist", 200*time.Second)
			p.stuck = true
			cfg := fastCfg("brokenplayer")
			cfg.Progress.Fallback = fb
			cfg.Polling.PlayingInterval = config.D(500 * time.Millisecond)
			h := start(t, cfg, bus, nil)
			defer h.stop()
			h.waitAct("shown", func(a *discord.Activity) bool { return a != nil })
			if fb == "estimate" {
				// The estimated bar keeps the original anchor (track start).
				time.Sleep(2500 * time.Millisecond)
				a, _ := h.conn.last()
				if a == nil || a.Timestamps == nil {
					t.Fatalf("estimate should keep a bar: %+v", a)
				}
			} else {
				h.waitAct("bar removed", func(a *discord.Activity) bool { return a != nil && a.Timestamps == nil })
			}
		})
	}
}

func TestServiceEmptyAllowlistShowsNothing(t *testing.T) {
	bus := newFakeBus()
	bus.add("vlc", "VLC", "Song", "Artist", 100*time.Second)
	h := start(t, fastCfg(), bus, nil)
	time.Sleep(300 * time.Millisecond)
	if a, n := h.conn.last(); n != 0 || a != nil {
		t.Fatalf("pushed with empty allowlist: %+v", a)
	}
	h.stop()
}

func TestServiceAsyncArtDoesNotBlock(t *testing.T) {
	bus := newFakeBus()
	bus.add("vlc", "VLC", "Song", "Artist", 100*time.Second)
	h := start(t, fastCfg("vlc"), bus, func(o *Options) {
		o.NewResolver = func(c config.Config, cache *art.Cache, log *slog.Logger) *art.Resolver {
			return art.NewResolverWith([]string{"mpris", "itunes"},
				map[string]art.Source{"itunes": slowSource{delay: 300 * time.Millisecond, url: "https://is1.mzstatic.com/x/600x600bb.jpg"}}, cache, log)
		}
	})
	defer h.stop()
	a := h.waitAct("default art first", func(a *discord.Activity) bool { return a != nil })
	if a.Assets.LargeImage != "music" {
		t.Fatalf("first push should use default art: %+v", a.Assets)
	}
	h.waitAct("resolved art", func(a *discord.Activity) bool {
		return a != nil && a.Assets.LargeImage == "https://is1.mzstatic.com/x/600x600bb.jpg"
	})
}

func TestServiceSeekSignalAndTrackChange(t *testing.T) {
	bus := newFakeBus()
	p := bus.add("vlc", "VLC", "Song", "Artist", 300*time.Second)
	h := start(t, fastCfg("vlc"), bus, nil)
	defer h.stop()
	a := h.waitAct("shown", func(a *discord.Activity) bool { return a != nil && a.Timestamps != nil })
	firstStart := a.Timestamps.Start
	// Seek to 120s, announced via the Seeked signal.
	bus.update(p, func(p *fakePlayer) { p.posAt, p.posWall = 120*time.Second, time.Now() })
	bus.emit(mpris.Event{Kind: mpris.Seeked, BusName: p.id.BusName, Position: 120_000_000, PositionOK: true, At: time.Now()})
	h.waitAct("re-synced after seek", func(a *discord.Activity) bool {
		return a != nil && a.Timestamps != nil && firstStart-a.Timestamps.Start > 100_000
	})
	// Skip to the next track.
	bus.update(p, func(p *fakePlayer) {
		p.meta["xesam:title"] = "Next"
		p.meta["mpris:trackid"] = "/track/next"
		p.meta["mpris:length"] = uint64(100_000_000)
		p.posAt, p.posWall = 0, time.Now()
	})
	a = h.waitAct("next track", func(a *discord.Activity) bool { return a != nil && a.Details == "Next" })
	if a.Timestamps == nil || a.Timestamps.End-a.Timestamps.Start != 100_000 {
		t.Fatalf("%+v", a.Timestamps)
	}
	// Playback rate change: 2x halves the remaining time.
	bus.update(p, func(p *fakePlayer) { p.posAt, p.posWall, p.rate = p.position(time.Now()), time.Now(), 2 })
	h.waitAct("rate applied", func(a *discord.Activity) bool {
		return a != nil && a.Timestamps != nil && a.Timestamps.End-a.Timestamps.Start == 50_000
	})
}

func TestServiceReload(t *testing.T) {
	bus := newFakeBus()
	bus.add("vlc", "VLC", "Song", "Artist", 100*time.Second)
	bus.add("spotify", "Spotify", "Other", "Artist", 100*time.Second)
	reload := make(chan struct{}, 1)
	var cfgPath string
	h := start(t, fastCfg("vlc", "spotify"), bus, func(o *Options) {
		o.Reload = reload
		cfgPath = o.ConfigPath
		// Each engine gets a fresh bus (the old one is closed on reload).
		first := true
		o.NewBus = func(context.Context) (mpris.Bus, error) {
			if first {
				first = false
				return bus, nil
			}
			nb := newFakeBus()
			bus.mu.Lock()
			for k, v := range bus.players {
				nb.players[k] = v
			}
			bus.mu.Unlock()
			return nb, nil
		}
	})
	defer h.stop()
	h.waitAct("vlc first", func(a *discord.Activity) bool { return a != nil && a.Details == "Song" })
	if err := config.Save(cfgPath, fastCfg("spotify", "vlc")); err != nil {
		t.Fatal(err)
	}
	reload <- struct{}{}
	h.waitAct("spotify after reload", func(a *discord.Activity) bool { return a != nil && a.Details == "Other" })
}
