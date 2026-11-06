package mpris

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// startBus launches a private dbus-daemon and points the session bus at it.
func startBus(t *testing.T) {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command(bin, "--session", "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start dbus-daemon: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read bus address: %v", err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", strings.TrimSpace(addr))
}

// fakeMPRIS exports a minimal MPRIS player whose property types are configurable.
type fakeMPRIS struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	position any
	metadata map[string]dbus.Variant
}

func (f *fakeMPRIS) props(iface string) map[string]dbus.Variant {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch iface {
	case RootIface:
		return map[string]dbus.Variant{"Identity": dbus.MakeVariant("Fake Player")}
	case PlayerIface:
		return map[string]dbus.Variant{
			"PlaybackStatus": dbus.MakeVariant("Playing"),
			"Rate":           dbus.MakeVariant(1.0),
			"Metadata":       dbus.MakeVariant(f.metadata),
			"Position":       dbus.MakeVariant(f.position),
		}
	}
	return nil
}

func (f *fakeMPRIS) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if v, ok := f.props(iface)[name]; ok {
		return v, nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(os.ErrNotExist)
}

func (f *fakeMPRIS) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	return f.props(iface), nil
}

func exportFake(t *testing.T, suffix string, f *fakeMPRIS) {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	f.conn = conn
	t.Cleanup(func() { conn.Close() })
	if err := conn.ExportMethodTable(map[string]any{"Get": f.Get, "GetAll": f.GetAll}, ObjectPath, PropsIface); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName(BusPrefix+suffix, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("request name: %v %v", reply, err)
	}
}

func waitEvent(t *testing.T, c *DBusClient, kind EventKind) Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.Kind == kind {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestDBusClientIntegration(t *testing.T) {
	startBus(t)
	ctx := context.Background()
	c, err := Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	f := &fakeMPRIS{
		position: "42000000", // string position: non-standard but seen in the wild
		metadata: map[string]dbus.Variant{
			"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/track/1")),
			"xesam:title":   dbus.MakeVariant("Song"),
			"xesam:artist":  dbus.MakeVariant([]string{"A", "B"}),
			"mpris:length":  dbus.MakeVariant(uint64(180_000_000)),
			"mpris:artUrl":  dbus.MakeVariant("https://i.scdn.co/image/abc"),
		},
	}
	exportFake(t, "fakeplayer.instance77", f)
	ev := waitEvent(t, c, PlayerAdded)
	if ev.BusName != BusPrefix+"fakeplayer.instance77" {
		t.Fatal(ev)
	}

	ids, err := c.ListPlayers(ctx)
	if err != nil || len(ids) != 1 || ids[0].Base != "fakeplayer" || ids[0].Suffix != "fakeplayer.instance77" {
		t.Fatalf("%v %+v", err, ids)
	}
	snap, err := c.Snapshot(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if snap.Identity != "Fake Player" || snap.Status != Playing || !snap.RateOK || snap.Rate != 1 {
		t.Fatalf("%+v", snap)
	}
	if !snap.PositionOK || snap.Position != 42_000_000 {
		t.Fatalf("position %v %v (%T)", snap.Position, snap.PositionOK, snap.PositionRaw)
	}
	m := snap.Metadata
	if m.Title != "Song" || strings.Join(m.Artists, ",") != "A,B" || !m.LengthOK || m.Length != 180_000_000 || m.TrackID != "/track/1" {
		t.Fatalf("%+v", m)
	}

	// Position as int32 microseconds.
	f.mu.Lock()
	f.position = int32(1_500_000)
	f.mu.Unlock()
	if us, raw, ok, err := c.ReadPosition(ctx, ids[0]); err != nil || !ok || us != 1_500_000 {
		t.Fatalf("%v %v %T %v", us, ok, raw, err)
	}

	// Seeked signal from the player.
	if err := f.conn.Emit(ObjectPath, PlayerIface+".Seeked", int64(90_000_000)); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, c, Seeked)
	if !ev.PositionOK || ev.Position != 90_000_000 || ev.BusName != ids[0].BusName {
		t.Fatalf("%+v", ev)
	}

	// PropertiesChanged.
	if err := f.conn.Emit(ObjectPath, PropsIface+".PropertiesChanged", PlayerIface,
		map[string]dbus.Variant{"PlaybackStatus": dbus.MakeVariant("Paused")}, []string{}); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, c, PropertiesChanged)
	if ev.BusName != ids[0].BusName {
		t.Fatalf("%+v", ev)
	}

	// Player exits.
	f.conn.Close()
	ev = waitEvent(t, c, PlayerRemoved)
	if ev.BusName != ids[0].BusName {
		t.Fatalf("%+v", ev)
	}
	if _, err := c.Snapshot(ctx, ids[0]); err == nil {
		t.Fatal("snapshot of a gone player should fail")
	}
}

func TestConnectWithoutBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/tunecord-test-bus")
	if _, err := Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "session bus") {
		t.Fatalf("want ErrNoSessionBus, got %v", err)
	}
}
