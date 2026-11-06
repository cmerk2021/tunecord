// Package service wires MPRIS, the progress model, album art and Discord
// together into the long-running tunecord service.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"tunecord/internal/art"
	"tunecord/internal/config"
	"tunecord/internal/control"
	"tunecord/internal/discord"
	"tunecord/internal/mpris"
	"tunecord/internal/presence"
	"tunecord/internal/progress"
	"tunecord/internal/version"
)

// Defaults for timing that is not user-configurable.
const (
	DefaultSettleDelay = 400 * time.Millisecond
	resyncInterval     = 60 * time.Second
	statusHeartbeat    = 15 * time.Second
)

// Options configures Run. Zero values select production defaults.
type Options struct {
	ConfigPath string
	Logger     *slog.Logger
	LevelVar   *slog.LevelVar // set from general.log_level unless Verbose
	Verbose    bool
	Reload     <-chan struct{} // SIGHUP
	NewBus     func(ctx context.Context) (mpris.Bus, error)
	Dialer     discord.Dialer
	NewLimiter func() *discord.Limiter
	// SettleDelay is how long to wait after a track change before trusting
	// the position (some players report the previous track's position for
	// a few hundred ms).
	SettleDelay  time.Duration
	ArtCache     *art.Cache
	NewResolver  func(cfg config.Config, cache *art.Cache, log *slog.Logger) *art.Resolver
	WriteStatus  bool
	WritePidfile bool
	// ProgressTweak adjusts the progress model tuning (tests).
	ProgressTweak func(*progress.Config)
}

func (o *Options) defaults() {
	if o.ConfigPath == "" {
		o.ConfigPath = config.ConfigPath()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.NewBus == nil {
		o.NewBus = func(ctx context.Context) (mpris.Bus, error) { return mpris.Connect(ctx) }
	}
	if o.Dialer == nil {
		o.Dialer = discord.DefaultDialer
	}
	if o.NewLimiter == nil {
		o.NewLimiter = discord.DefaultLimiter
	}
	if o.SettleDelay < 0 {
		o.SettleDelay = 0
	} else if o.SettleDelay == 0 {
		o.SettleDelay = DefaultSettleDelay
	}
	if o.ArtCache == nil {
		o.ArtCache = art.OpenCache(art.DefaultCachePath())
	}
	if o.NewResolver == nil {
		o.NewResolver = func(cfg config.Config, cache *art.Cache, log *slog.Logger) *art.Resolver {
			return art.NewResolver(cfg.Art, version.UserAgent(), cache, log)
		}
	}
}

// ParseLevel converts a config log level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func loadConfig(o *Options) (config.Config, bool, error) {
	cfg, warnings, exists, err := config.Load(o.ConfigPath)
	if err != nil {
		return cfg, exists, err
	}
	if o.LevelVar != nil && !o.Verbose {
		o.LevelVar.Set(ParseLevel(cfg.General.LogLevel))
	}
	for _, w := range warnings {
		o.Logger.Warn("config: " + w)
	}
	if !exists {
		o.Logger.Info("no config file found, using defaults; run 'tunecord config' to set up your player allowlist", "path", o.ConfigPath)
	} else {
		o.Logger.Info("loaded config", "path", o.ConfigPath)
	}
	return cfg, exists, nil
}

// Run runs the service until ctx is cancelled. A value on o.Reload re-reads
// the config and re-initializes all connection state without exiting. The
// config is only read at startup and on reload (there is no file watcher).
func Run(ctx context.Context, o Options) error {
	o.defaults()
	log := o.Logger

	if o.WritePidfile {
		pidPath := config.PidfilePath()
		if err := control.WritePidfile(pidPath); err != nil {
			return err
		}
		defer control.RemovePidfile(pidPath)
	}
	if o.WriteStatus {
		defer os.Remove(config.StatusPath())
	}

	cfg, exists, err := loadConfig(&o)
	if err != nil {
		return err
	}

	// Fail fast with a clear message if there is no session bus at all;
	// later losses of the bus are retried.
	bus, err := o.NewBus(ctx)
	if err != nil {
		return err
	}
	log.Info("tunecord started", "version", version.Get(), "pid", os.Getpid())

	started := time.Now()
	for {
		ectx, cancel := context.WithCancel(ctx)
		e := newEngine(cfg, exists, &o, started)
		done := make(chan struct{})
		go func(b mpris.Bus) {
			defer close(done)
			e.run(ectx, b)
		}(bus)
		bus = nil

	wait:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				log.Info("tunecord stopped")
				return nil
			case <-o.Reload:
				log.Info("reload requested (SIGHUP), re-reading config")
				newCfg, newExists, err := loadConfig(&o)
				if err != nil {
					log.Error("config reload failed, keeping the current configuration", "err", err)
					continue
				}
				cancel()
				<-done
				cfg, exists = newCfg, newExists
				break wait
			}
		}
	}
}

type player struct {
	id       mpris.PlayerID
	identity string
	snap     mpris.Snapshot
	hasSnap  bool
	tracker  *progress.Tracker
}

type current struct {
	bus      string
	trackKey string
	status   mpris.Status
	artKey   string
	artURL   string
	cancel   context.CancelFunc
	seq      int
}

type artResult struct {
	seq int
	url string
}

type engine struct {
	cfg       config.Config
	cfgExists bool
	o         *Options
	log       *slog.Logger
	mgr       *discord.Manager
	resolver  *art.Resolver
	pcfg      progress.Config
	popts     presence.Options
	started   time.Time

	players map[string]*player
	artCh   chan artResult
	artWG   sync.WaitGroup
	cur     current

	busErr      string
	busUp       bool
	warnedEmpty bool
	lastStatus  string
	lastWrite   time.Time
	anyPlaying  bool
}

func newEngine(cfg config.Config, exists bool, o *Options, started time.Time) *engine {
	log := o.Logger
	e := &engine{
		cfg: cfg, cfgExists: exists, o: o, log: log, started: started,
		players: map[string]*player{},
		artCh:   make(chan artResult, 4),
	}
	e.mgr = discord.NewManager(discord.ManagerOptions{
		ClientID: cfg.Discord.ClientID,
		Dialer:   o.Dialer,
		Limiter:  o.NewLimiter(),
		Logger:   log,
	})
	e.resolver = o.NewResolver(cfg, o.ArtCache, log)
	e.pcfg = progress.DefaultConfig()
	e.pcfg.SeekThreshold = cfg.Progress.SeekThreshold.Duration
	e.pcfg.Fallback = progress.Fallback(cfg.Progress.Fallback)
	if o.ProgressTweak != nil {
		o.ProgressTweak(&e.pcfg)
	}
	e.popts = presence.Options{
		StatusDisplay:   cfg.Presence.StatusDisplay,
		ShowAlbum:       cfg.Presence.ShowAlbum,
		DefaultArtAsset: cfg.Presence.DefaultArtAsset,
		PausedAsset:     cfg.Presence.PausedAsset,
	}
	return e
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *engine) run(ctx context.Context, bus mpris.Bus) {
	var mgrWG sync.WaitGroup
	mgrWG.Add(1)
	go func() {
		defer mgrWG.Done()
		e.mgr.Run(ctx) // clears the presence when ctx is cancelled
	}()
	defer func() {
		e.cancelArt()
		e.artWG.Wait()
		mgrWG.Wait()
	}()

	if len(e.cfg.Players.Allowlist) == 0 {
		e.log.Warn("the player allowlist is empty, so nothing will be shown; run 'tunecord list' to see player names and 'tunecord config' to add them")
		e.warnedEmpty = true
	}

	backoff := time.Second
	for ctx.Err() == nil {
		if bus == nil {
			b, err := e.o.NewBus(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if e.busErr != err.Error() {
					e.log.Warn("session bus unavailable, retrying", "err", err, "retry_in", backoff)
				}
				e.busErr = err.Error()
				e.busUp = false
				e.publishStatus(nil, progress.View{}, true)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			bus = b
			if e.busErr != "" {
				e.log.Info("reconnected to the session bus")
			}
		}
		backoff = time.Second
		e.busErr = ""
		e.busUp = true
		e.loop(ctx, bus)
		bus.Close()
		bus = nil
		e.players = map[string]*player{}
		e.clear()
		if ctx.Err() == nil {
			e.log.Warn("lost the session bus connection, reconnecting")
			e.busErr = "connection lost"
			e.busUp = false
		}
	}
}

func (e *engine) addPlayer(ctx context.Context, bus mpris.Bus, id mpris.PlayerID) {
	if _, ok := e.players[id.BusName]; ok {
		return
	}
	p := &player{id: id, identity: bus.Identity(ctx, id), tracker: progress.New(e.pcfg)}
	e.players[id.BusName] = p
	allowed := AllowlistIndex(id, p.identity, e.cfg.Players.Allowlist) >= 0
	e.log.Debug("player appeared", "player", id.Suffix, "identity", p.identity, "allowlisted", allowed)
}

func (e *engine) syncPlayers(ctx context.Context, bus mpris.Bus) error {
	ids, err := bus.ListPlayers(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id.BusName] = true
		e.addPlayer(ctx, bus, id)
	}
	for name := range e.players {
		if !seen[name] {
			delete(e.players, name)
		}
	}
	return nil
}

func (e *engine) handleEvent(ctx context.Context, bus mpris.Bus, ev mpris.Event) {
	switch ev.Kind {
	case mpris.PlayerAdded:
		if id, ok := mpris.ParseBusName(ev.BusName); ok {
			e.addPlayer(ctx, bus, id)
		}
	case mpris.PlayerRemoved:
		if p, ok := e.players[ev.BusName]; ok {
			e.log.Debug("player disappeared", "player", p.id.Suffix)
			if e.cur.bus == ev.BusName {
				e.log.Info("player closed", "player", p.id.Suffix)
			}
			delete(e.players, ev.BusName)
		}
	case mpris.PropertiesChanged:
		if _, ok := e.players[ev.BusName]; !ok {
			if id, ok := mpris.ParseBusName(ev.BusName); ok {
				e.addPlayer(ctx, bus, id)
			}
		}
	case mpris.Seeked:
		if p, ok := e.players[ev.BusName]; ok && p.tracker.HasTrack() {
			at := ev.At
			if at.IsZero() {
				at = time.Now()
			}
			if ch := p.tracker.Seek(at, progress.Micros(ev.Position), ev.PositionOK); ch != 0 {
				e.log.Debug("seek signal re-anchored progress", "player", p.id.Suffix, "position", progress.Micros(ev.Position))
			}
		}
	}
}

func (e *engine) interval() time.Duration {
	if e.anyPlaying {
		return e.cfg.Polling.PlayingInterval.Duration
	}
	return e.cfg.Polling.IdleInterval.Duration
}

func (e *engine) loop(ctx context.Context, bus mpris.Bus) {
	if err := e.syncPlayers(ctx, bus); err != nil {
		e.log.Warn("listing MPRIS players failed", "err", err)
		return
	}
	e.evaluate(ctx, bus)
	timer := time.NewTimer(e.interval())
	defer timer.Stop()
	resync := time.NewTicker(resyncInterval)
	defer resync.Stop()
	events := bus.Events()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			e.handleEvent(ctx, bus, ev)
			// Signals arrive in bursts (metadata + status + seek); coalesce.
			if !sleepCtx(ctx, 30*time.Millisecond) {
				return
			}
		drain:
			for i := 0; i < 256; i++ {
				select {
				case ev, ok := <-events:
					if !ok {
						return
					}
					e.handleEvent(ctx, bus, ev)
				default:
					break drain
				}
			}
		case r := <-e.artCh:
			if r.seq != e.cur.seq {
				continue
			}
			e.cur.cancel = nil
			if r.url != e.cur.artURL {
				e.log.Debug("album art resolved", "url", r.url)
				e.cur.artURL = r.url
			} else {
				continue
			}
		case <-timer.C:
		case <-resync.C:
			// Safety net only; players are normally tracked via NameOwnerChanged.
			if err := e.syncPlayers(ctx, bus); err != nil {
				e.log.Debug("player resync failed", "err", err)
			}
		}
		e.evaluate(ctx, bus)
		timer.Reset(e.interval())
	}
}

// SampleFrom converts an MPRIS snapshot into a progress sample.
func SampleFrom(s mpris.Snapshot) progress.Sample {
	at := s.ReadAt
	if at.IsZero() {
		at = time.Now()
	}
	m := s.Metadata
	return progress.Sample{
		At:         at,
		Status:     progress.Status(s.Status),
		TrackKey:   m.TrackKey(),
		Length:     progress.Micros(m.Length),
		LengthOK:   m.LengthOK,
		Position:   progress.Micros(s.Position),
		PositionOK: s.PositionOK,
		Rate:       s.Rate,
		RateOK:     s.RateOK,
	}
}

// settle re-reads a player shortly after a track change, because some
// players report the previous track's position for a few hundred ms.
func (e *engine) settle(ctx context.Context, bus mpris.Bus, p *player, snap mpris.Snapshot) mpris.Snapshot {
	if !p.tracker.HasTrack() || snap.Metadata.TrackKey() == p.tracker.TrackKey() || e.o.SettleDelay <= 0 {
		return snap
	}
	if !sleepCtx(ctx, e.o.SettleDelay) {
		return snap
	}
	s2, err := bus.Snapshot(ctx, p.id)
	if err != nil {
		return snap
	}
	return s2
}

func (e *engine) sortedPlayers() []*player {
	ps := make([]*player, 0, len(e.players))
	for _, p := range e.players {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].id.BusName < ps[j].id.BusName })
	return ps
}

func (e *engine) evaluate(ctx context.Context, bus mpris.Bus) {
	if ctx.Err() != nil {
		return
	}
	allow := e.cfg.Players.Allowlist
	e.anyPlaying = false
	if len(allow) == 0 {
		e.clear()
		e.publishStatus(nil, progress.View{}, false)
		return
	}

	var cands []Candidate
	var cps []*player
	for _, p := range e.sortedPlayers() {
		if p.identity == "" {
			p.identity = bus.Identity(ctx, p.id)
		}
		if AllowlistIndex(p.id, p.identity, allow) < 0 {
			continue
		}
		snap, err := bus.Snapshot(ctx, p.id)
		if err != nil {
			e.log.Debug("reading player failed", "player", p.id.Suffix, "err", err)
			p.hasSnap = false
			continue
		}
		snap = e.settle(ctx, bus, p, snap)
		if ctx.Err() != nil {
			return
		}
		if snap.PositionErr != nil {
			e.log.Debug("reading Position failed", "player", p.id.Suffix, "err", snap.PositionErr)
		}
		p.snap, p.hasSnap = snap, true
		if ch := p.tracker.Update(SampleFrom(snap)); ch != 0 {
			v := p.tracker.View(time.Now())
			e.log.Debug("progress model updated", "player", p.id.Suffix, "change", ch.String(),
				"mode", v.Mode, "position", v.Position.Round(time.Millisecond), "length", v.Length, "rate", v.Rate, "reason", v.Reason)
		}
		if snap.Status == mpris.Playing {
			e.anyPlaying = true
		}
		cands = append(cands, Candidate{ID: p.id, Identity: p.identity, Status: snap.Status})
		cps = append(cps, p)
	}

	idx := Select(cands, allow, e.cfg.Presence.ShowWhenPaused)
	if idx < 0 || cps[idx].snap.Metadata.Empty() {
		if e.cur.bus != "" {
			e.log.Info("nothing to show, clearing presence")
		}
		e.clear()
		e.publishStatus(nil, progress.View{}, false)
		return
	}
	p := cps[idx]
	m := p.snap.Metadata
	key := m.TrackKey()

	if e.cur.bus != p.id.BusName || e.cur.trackKey != key {
		e.log.Info("now playing", "player", p.id.Suffix, "title", m.Title, "artist", strings.Join(m.Artists, ", "), "status", p.snap.Status)
	} else if e.cur.status != p.snap.Status {
		e.log.Info("playback status changed", "player", p.id.Suffix, "status", p.snap.Status)
	}
	e.updateArt(ctx, p, key)
	e.cur.bus, e.cur.trackKey, e.cur.status = p.id.BusName, key, p.snap.Status

	now := time.Now()
	v := p.tracker.View(now)
	act := presence.Build(presence.Input{
		Title:         m.Title,
		Artists:       m.Artists,
		Album:         m.Album,
		ArtURL:        e.cur.artURL,
		Paused:        p.snap.Status == mpris.Paused,
		HasTimestamps: v.HasTimestamps,
		Start:         v.Start,
		End:           v.End,
	}, e.popts)
	e.mgr.Set(act)
	e.publishStatus(p, v, false)
}

func (e *engine) updateArt(ctx context.Context, p *player, key string) {
	m := p.snap.Metadata
	artKey := strings.Join([]string{p.id.BusName, key, m.Album, m.ArtURL}, "\x1f")
	if artKey == e.cur.artKey {
		return
	}
	sameTrack := e.cur.bus == p.id.BusName && e.cur.trackKey == key
	e.cancelArt()
	e.cur.seq++
	e.cur.artKey = artKey
	q := art.Query{Title: m.Title, Artists: m.Artists, Album: m.Album, ArtURL: m.ArtURL}
	u, done := e.resolver.Immediate(q)
	if done || !sameTrack {
		// Never block: show the default art until the lookup completes.
		e.cur.artURL = u
	}
	if done {
		return
	}
	actx, cancel := context.WithCancel(ctx)
	e.cur.cancel = cancel
	seq := e.cur.seq
	e.artWG.Add(1)
	go func() {
		defer e.artWG.Done()
		defer cancel()
		u := e.resolver.Lookup(actx, q)
		select {
		case e.artCh <- artResult{seq: seq, url: u}:
		case <-actx.Done():
		}
	}()
}

func (e *engine) cancelArt() {
	if e.cur.cancel != nil {
		e.cur.cancel()
		e.cur.cancel = nil
	}
}

func (e *engine) clear() {
	e.cancelArt()
	e.cur = current{seq: e.cur.seq + 1}
	e.mgr.Set(nil)
}

func (e *engine) publishStatus(p *player, v progress.View, force bool) {
	if !e.o.WriteStatus {
		return
	}
	ds := e.mgr.Status()
	s := Status{
		PID:              os.Getpid(),
		Version:          version.Get(),
		StartedAt:        e.started,
		ConfigPath:       e.o.ConfigPath,
		ConfigLoaded:     e.cfgExists,
		BusConnected:     e.busUp,
		BusError:         e.busErr,
		DiscordConnected: ds.Connected,
		DiscordSocket:    ds.Socket,
		DiscordError:     ds.LastError,
		Allowlist:        e.cfg.Players.Allowlist,
		AllowlistEmpty:   len(e.cfg.Players.Allowlist) == 0,
	}
	if s.AllowlistEmpty {
		s.Warnings = append(s.Warnings, "the player allowlist is empty: nothing will be shown (run 'tunecord config')")
	}
	if !e.cfgExists {
		s.Warnings = append(s.Warnings, "no config file: running with defaults (run 'tunecord config')")
	}
	if p != nil {
		m := p.snap.Metadata
		s.Player = p.id.Suffix
		s.PlayerIdentity = p.identity
		s.PlaybackStatus = string(p.snap.Status)
		s.Title = m.Title
		s.Artist = strings.Join(m.Artists, ", ")
		if e.cfg.Presence.ShowAlbum {
			s.Album = m.Album
		}
		s.ArtURL = e.cur.artURL
		s.ProgressMode = string(v.Mode)
		s.ProgressReason = v.Reason
		s.PresenceShown = true
	}
	data, _ := json.Marshal(s)
	now := time.Now()
	if !force && string(data) == e.lastStatus && now.Sub(e.lastWrite) < statusHeartbeat {
		return
	}
	e.lastStatus = string(data)
	e.lastWrite = now
	s.UpdatedAt = now
	if err := writeStatus(s); err != nil {
		e.log.Debug("writing status file failed", "err", err)
	}
}

// ErrNoBus re-exports the session bus error for callers.
var ErrNoBus = mpris.ErrNoSessionBus

// IsNoBus reports whether err means the session bus is unreachable.
func IsNoBus(err error) bool { return errors.Is(err, mpris.ErrNoSessionBus) }

// Describe formats a duration for humans.
func Describe(d time.Duration) string {
	if d <= 0 {
		return "0:00"
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d/time.Minute), int(d%time.Minute/time.Second))
}
