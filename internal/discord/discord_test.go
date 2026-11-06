package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := DefaultLimiter()
	now := time.Unix(1000, 0)
	if !l.Next(now).Equal(now) {
		t.Fatal("first push should be immediate")
	}
	l.Record(now)
	if got := l.Next(now.Add(time.Second)); !got.Equal(now.Add(4 * time.Second)) {
		t.Fatalf("min spacing: %v", got.Sub(now))
	}
	// Record 5 pushes as fast as allowed; the window must never exceed 5/20s.
	l = &Limiter{MinSpacing: time.Second, Window: 20 * time.Second, Max: 5}
	var times []time.Time
	cur := now
	for i := 0; i < 12; i++ {
		cur = l.Next(cur)
		l.Record(cur)
		times = append(times, cur)
	}
	for i := range times {
		n := 0
		for j := range times {
			if !times[j].Before(times[i]) && times[j].Sub(times[i]) < 20*time.Second {
				n++
			}
		}
		if n > 5 {
			t.Fatalf("window from %v has %d pushes", times[i].Sub(now), n)
		}
	}
	if times[5].Sub(times[0]) < 20*time.Second {
		t.Fatalf("6th push too early: %v", times[5].Sub(times[0]))
	}
}

func TestSame(t *testing.T) {
	a := &Activity{Type: 2, Details: "x", State: "y", Timestamps: &Timestamps{Start: 1000, End: 5000}}
	b := *a
	b.Timestamps = &Timestamps{Start: 1500, End: 5500}
	if !Same(a, &b) {
		t.Error("sub-second timestamp jitter should be same")
	}
	b.Timestamps = &Timestamps{Start: 4000, End: 8000}
	if Same(a, &b) {
		t.Error("3s shift must differ")
	}
	c := *a
	c.Timestamps = nil
	if Same(a, &c) {
		t.Error("timestamps removed must differ")
	}
	d := *a
	d.Assets = &Assets{LargeImage: "music"}
	if Same(a, &d) {
		t.Error("asset change must differ")
	}
	if !Same(nil, nil) || Same(nil, a) {
		t.Error("nil handling")
	}
}

type fakeConn struct {
	mu   sync.Mutex
	sets []*Activity
	at   []time.Time
	done chan struct{}
	fail bool
}

func newFakeConn() *fakeConn { return &fakeConn{done: make(chan struct{})} }

func (f *fakeConn) SetActivity(_ context.Context, a *Activity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("broken pipe")
	}
	f.sets = append(f.sets, a)
	f.at = append(f.at, time.Now())
	return nil
}
func (f *fakeConn) Done() <-chan struct{} { return f.done }
func (f *fakeConn) Path() string          { return "/fake" }
func (f *fakeConn) Close() error          { return nil }
func (f *fakeConn) snapshot() ([]*Activity, []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Activity(nil), f.sets...), append([]time.Time(nil), f.at...)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestManagerCoalescesAndRateLimits(t *testing.T) {
	fc := newFakeConn()
	spacing := 150 * time.Millisecond
	m := NewManager(ManagerOptions{
		ClientID: "1",
		Dialer:   func(context.Context, string) (Conn, error) { return fc, nil },
		Limiter:  &Limiter{MinSpacing: spacing, Window: time.Second, Max: 5},
		Logger:   quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	m.Set(&Activity{Type: 2, Details: "one"})
	waitFor(t, "first push", func() bool { s, _ := fc.snapshot(); return len(s) == 1 })
	// Burst of updates within one spacing window: only the latest is sent.
	for i := 0; i < 20; i++ {
		m.Set(&Activity{Type: 2, Details: "burst", State: strings.Repeat("x", i+2)})
	}
	waitFor(t, "coalesced push", func() bool { s, _ := fc.snapshot(); return len(s) == 2 })
	time.Sleep(3 * spacing)
	sets, at := fc.snapshot()
	if len(sets) != 2 {
		t.Fatalf("want 2 pushes, got %d", len(sets))
	}
	if sets[1].State != strings.Repeat("x", 21) {
		t.Fatalf("latest state not sent: %q", sets[1].State)
	}
	if at[1].Sub(at[0]) < spacing-5*time.Millisecond {
		t.Fatalf("spacing violated: %v", at[1].Sub(at[0]))
	}
	// Identical state is not re-sent.
	m.Set(&Activity{Type: 2, Details: "burst", State: strings.Repeat("x", 21)})
	time.Sleep(3 * spacing)
	if s, _ := fc.snapshot(); len(s) != 2 {
		t.Fatalf("duplicate pushed: %d", len(s))
	}
	// Shutdown clears the presence.
	cancel()
	<-done
	sets, _ = fc.snapshot()
	if sets[len(sets)-1] != nil {
		t.Fatal("presence not cleared on shutdown")
	}
}

func TestManagerReconnects(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	conns := []*fakeConn{}
	m := NewManager(ManagerOptions{
		ClientID: "1",
		Dialer: func(context.Context, string) (Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts <= 2 {
				return nil, ErrNotRunning
			}
			c := newFakeConn()
			conns = append(conns, c)
			return c, nil
		},
		Limiter:    &Limiter{MinSpacing: time.Millisecond, Window: time.Second, Max: 100},
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 40 * time.Millisecond,
		Logger:     quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	m.Set(&Activity{Type: 2, Details: "song"})
	waitFor(t, "first connection push", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(conns) == 0 {
			return false
		}
		s, _ := conns[0].snapshot()
		return len(s) == 1
	})
	if !m.Status().Connected {
		t.Fatal("status not connected")
	}
	// Discord restarts: connection drops; the desired state is re-sent on
	// the new connection without any new Set call.
	mu.Lock()
	close(conns[0].done)
	mu.Unlock()
	waitFor(t, "re-push after reconnect", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(conns) < 2 {
			return false
		}
		s, _ := conns[1].snapshot()
		return len(s) == 1 && s[0].Details == "song"
	})
	// Write errors also trigger a reconnect.
	mu.Lock()
	conns[1].mu.Lock()
	conns[1].fail = true
	conns[1].mu.Unlock()
	mu.Unlock()
	m.Set(&Activity{Type: 2, Details: "next"})
	waitFor(t, "push after write error", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(conns) < 3 {
			return false
		}
		s, _ := conns[2].snapshot()
		return len(s) == 1 && s[0].Details == "next"
	})
}

// fakeDiscord is a minimal Discord IPC server on a unix socket.
func fakeDiscord(t *testing.T, path string, got chan<- map[string]any) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		op, data, err := readFrame(c)
		if err != nil || op != opHandshake {
			return
		}
		var hs map[string]any
		_ = json.Unmarshal(data, &hs)
		got <- hs
		ready, _ := json.Marshal(map[string]any{"cmd": "DISPATCH", "evt": "READY", "data": map[string]any{"v": 1}})
		_ = writeFrame(c, opFrame, ready)
		for {
			op, data, err := readFrame(c)
			if err != nil {
				return
			}
			if op != opFrame {
				continue
			}
			var req map[string]any
			_ = json.Unmarshal(data, &req)
			got <- req
			resp, _ := json.Marshal(map[string]any{"cmd": "SET_ACTIVITY", "nonce": req["nonce"], "data": nil})
			_ = writeFrame(c, opFrame, resp)
		}
	}()
}

func TestIPCRoundTrip(t *testing.T) {
	dir, err := os.MkdirTemp("", "tcipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "app", "com.discordapp.Discord"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := make(chan map[string]any, 4)
	fakeDiscord(t, filepath.Join(dir, "app", "com.discordapp.Discord", "discord-ipc-0"), got)

	c, err := Dial(context.Background(), "1525387248332443781")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	hs := <-got
	if hs["client_id"] != "1525387248332443781" || hs["v"].(float64) != 1 {
		t.Fatalf("handshake %v", hs)
	}
	act := &Activity{Type: ActivityListening, StatusDisplayType: StatusDisplayState, Details: "Song", State: "Artist",
		Timestamps: &Timestamps{Start: 1, End: 2}}
	if err := c.SetActivity(context.Background(), act); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	req := <-got
	if req["cmd"] != "SET_ACTIVITY" || req["nonce"] == "" {
		t.Fatalf("req %v", req)
	}
	args := req["args"].(map[string]any)
	if int(args["pid"].(float64)) != os.Getpid() {
		t.Fatalf("pid %v", args["pid"])
	}
	a := args["activity"].(map[string]any)
	if a["type"].(float64) != 2 || a["status_display_type"].(float64) != 1 || a["details"] != "Song" {
		t.Fatalf("activity %v", a)
	}
	// Clearing sends a null activity.
	if err := c.SetActivity(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	req = <-got
	if req["args"].(map[string]any)["activity"] != nil {
		t.Fatal("clear should send null activity")
	}
}

func TestDialNoDiscord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("TMPDIR", dir)
	if _, err := Dial(context.Background(), "1"); !errors.Is(err, ErrNotRunning) {
		// /tmp or /run/user may contain a real socket on a dev machine.
		if err != nil {
			t.Fatalf("unexpected error type: %v", err)
		}
	}
}

func TestSocketCandidates(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	c := SocketCandidates()
	want := []string{
		"/run/user/1000/discord-ipc-0",
		"/run/user/1000/discord-ipc-9",
		"/run/user/1000/app/com.discordapp.Discord/discord-ipc-0",
		"/run/user/1000/snap.discord/discord-ipc-0",
		"/run/user/1000/app/dev.vencord.Vesktop/discord-ipc-0",
		"/run/user/1000/app/app.legcord.Legcord/discord-ipc-3",
		"/tmp/discord-ipc-0",
	}
	set := map[string]bool{}
	for _, p := range c {
		set[p] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing candidate %s", w)
		}
	}
	if c[0] != "/run/user/1000/discord-ipc-0" {
		t.Errorf("first candidate %s", c[0])
	}
}
