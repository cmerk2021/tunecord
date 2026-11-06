package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tunecord/internal/config"
	"tunecord/internal/mpris"
	"tunecord/internal/progress"
	"tunecord/internal/service"
)

type stubBus struct {
	snaps  []mpris.Snapshot
	events chan mpris.Event
}

func (b *stubBus) ListPlayers(context.Context) ([]mpris.PlayerID, error) {
	var ids []mpris.PlayerID
	for _, s := range b.snaps {
		ids = append(ids, s.ID)
	}
	return ids, nil
}
func (b *stubBus) Identity(_ context.Context, id mpris.PlayerID) string { return "" }
func (b *stubBus) Snapshot(_ context.Context, id mpris.PlayerID) (mpris.Snapshot, error) {
	for _, s := range b.snaps {
		if s.ID == id {
			s.ReadAt = time.Now()
			return s, nil
		}
	}
	return mpris.Snapshot{}, errors.New("gone")
}
func (b *stubBus) Events() <-chan mpris.Event { return b.events }
func (b *stubBus) Close() error               { return nil }

func snap(suffix, identity string, st mpris.Status) mpris.Snapshot {
	id, _ := mpris.ParseBusName(mpris.BusPrefix + suffix)
	return mpris.Snapshot{ID: id, Identity: identity, Status: st, StatusRaw: string(st)}
}

func TestListEntriesPriorityAndActive(t *testing.T) {
	bus := &stubBus{snaps: []mpris.Snapshot{
		snap("chromium.instance42", "Chromium", mpris.Playing),
		snap("vlc", "VLC media player", mpris.Playing),
		snap("org.kde.elisa", "Elisa", mpris.Paused),
	}}
	cfg := config.Default()
	cfg.Players.Allowlist = []string{"elisa", "vlc"}
	es := listEntries(context.Background(), bus, cfg)
	if es[0].Name != "chromium" || es[0].Instance != "chromium.instance42" || es[0].Allowlisted {
		t.Errorf("%+v", es[0])
	}
	if !es[1].Active || es[1].Priority != 2 {
		t.Errorf("vlc should be active (playing beats paused): %+v", es[1])
	}
	if es[2].Priority != 1 || es[2].Active {
		t.Errorf("%+v", es[2])
	}
	if e, ok := activeEntry(es); !ok || e.Name != "vlc" {
		t.Error("active")
	}
	var buf bytes.Buffer
	printList(&buf, es, cfg)
	for _, want := range []string{"NAME", "chromium.instance42", "yes (#1)", "◀ active"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("list output missing %q:\n%s", want, buf.String())
		}
	}

	// Empty allowlist: falls back to marking the first playing player.
	es = listEntries(context.Background(), bus, config.Default())
	if !es[0].Active {
		t.Error("fallback active")
	}
	buf.Reset()
	printList(&buf, es, config.Default())
	if !strings.Contains(buf.String(), "allowlist is empty") {
		t.Error(buf.String())
	}
}

func TestRenderDebugVerdicts(t *testing.T) {
	s := snap("vlc", "VLC", mpris.Playing)
	s.PositionRaw, s.Position, s.PositionOK = "0", 0, true
	s.Metadata = mpris.ParseMetadata(map[string]any{"mpris:length": uint64(200_000_000), "xesam:title": "T", "mpris:artUrl": "file:///tmp/a.png"})
	cfg := config.Default()
	cfg.Players.Allowlist = []string{"vlc"}
	pc := progress.DefaultConfig()
	dp := &debugPlayer{tracker: progress.New(pc), first: time.Now()}
	start := time.Now()
	for i := 0; i < 5; i++ {
		s.ReadAt = start.Add(time.Duration(i) * time.Second)
		dp.tracker.Update(service.SampleFrom(s))
		dp.samples++
	}
	var buf bytes.Buffer
	renderDebug(&buf, s, dp, cfg, start.Add(4*time.Second))
	out := buf.String()
	for _, want := range []string{"(string)", "uint64", "ESTIMATED", "fallback = estimate", "local file", "priority #1", "mpris:length"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug output missing %q:\n%s", want, out)
		}
	}

	pc.Fallback = progress.FallbackSkip
	dp = &debugPlayer{tracker: progress.New(pc), first: start}
	for i := 0; i < 5; i++ {
		s.ReadAt = start.Add(time.Duration(i) * time.Second)
		dp.tracker.Update(service.SampleFrom(s))
		dp.samples++
	}
	buf.Reset()
	renderDebug(&buf, s, dp, cfg, start.Add(4*time.Second))
	if !strings.Contains(buf.String(), "NO BAR") || !strings.Contains(buf.String(), "fallback = skip") {
		t.Error(buf.String())
	}
}

func TestRunDebugOnce(t *testing.T) {
	s := snap("vlc", "VLC", mpris.Playing)
	s.Metadata = mpris.ParseMetadata(map[string]any{"mpris:length": int64(100_000_000)})
	bus := &stubBus{snaps: []mpris.Snapshot{s}, events: make(chan mpris.Event)}
	var buf bytes.Buffer
	if err := runDebug(context.Background(), &buf, bus, config.Default(), nil, true, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "tunecord debug") != 1 || !strings.Contains(buf.String(), "4 samples") {
		t.Fatal(buf.String())
	}
}

func TestInstallRequiresUser(t *testing.T) {
	for _, sub := range []string{"install", "uninstall"} {
		root := NewRoot()
		root.SetArgs([]string{sub})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code == 0 || !strings.Contains(ee.Msg, "--user") {
			t.Errorf("%s without --user: %v", sub, err)
		}
	}
}

func TestPrintStatus(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, statusReport{ConfigPath: "/x/config.toml", Allowlist: []string{}})
	if !strings.Contains(buf.String(), "not running and not installed") || !strings.Contains(buf.String(), "EMPTY") {
		t.Fatal(buf.String())
	}
	buf.Reset()
	printStatus(&buf, statusReport{Running: true, PID: 42, UnitState: "active", ConfigExists: true, ConfigPath: "/c",
		Service: &service.Status{BusConnected: true, DiscordConnected: true, DiscordSocket: "/run/user/1000/discord-ipc-0",
			Allowlist: []string{"spotify", "vlc"}, Player: "vlc", PlaybackStatus: "Playing", Title: "Song", Artist: "Band",
			ProgressMode: "estimated", ProgressReason: "position stuck at 0:00", PresenceShown: true, StartedAt: time.Now()}})
	for _, want := range []string{"systemd user unit, pid 42", "spotify > vlc", "discord-ipc-0", "Song · Band", "estimated (position stuck", "shown"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q:\n%s", want, buf.String())
		}
	}
}
