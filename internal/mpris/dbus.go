package mpris

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// ErrNoSessionBus is returned when the session bus cannot be reached.
var ErrNoSessionBus = errors.New("cannot connect to the D-Bus session bus (is DBUS_SESSION_BUS_ADDRESS or XDG_RUNTIME_DIR set? tunecord must run inside your desktop session)")

// DBusClient implements Bus on top of a private session bus connection.
type DBusClient struct {
	conn   *dbus.Conn
	events chan Event

	mu       sync.Mutex
	owners   map[string]string // well-known name -> unique name
	identity map[string]string // well-known name -> Identity

	closeOnce sync.Once
	done      chan struct{}
}

// Connect opens a private session bus connection and subscribes to the
// signals tunecord needs. Player appearance and disappearance is tracked via
// NameOwnerChanged, so the bus never has to be polled for that.
func Connect(ctx context.Context) (*DBusClient, error) {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSessionBus, err)
	}
	c := &DBusClient{
		conn:     conn,
		events:   make(chan Event, 256),
		owners:   map[string]string{},
		identity: map[string]string{},
		done:     make(chan struct{}),
	}
	matches := [][]dbus.MatchOption{
		{
			dbus.WithMatchSender("org.freedesktop.DBus"),
			dbus.WithMatchInterface("org.freedesktop.DBus"),
			dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg0Namespace("org.mpris.MediaPlayer2"),
		},
		{
			dbus.WithMatchObjectPath(ObjectPath),
			dbus.WithMatchInterface(PropsIface),
			dbus.WithMatchMember("PropertiesChanged"),
		},
		{
			dbus.WithMatchObjectPath(ObjectPath),
			dbus.WithMatchInterface(PlayerIface),
			dbus.WithMatchMember("Seeked"),
		},
	}
	for _, m := range matches {
		if err := conn.AddMatchSignalContext(ctx, m...); err != nil {
			conn.Close()
			return nil, fmt.Errorf("subscribe to D-Bus signals: %w", err)
		}
	}
	sigs := make(chan *dbus.Signal, 256)
	conn.Signal(sigs)
	go c.dispatch(sigs)
	return c, nil
}

// Events implements Bus.
func (c *DBusClient) Events() <-chan Event { return c.events }

// Close implements Bus.
func (c *DBusClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.conn.Close()
	})
	return err
}

func (c *DBusClient) emit(e Event) {
	select {
	case c.events <- e:
	default:
		// Consumer is behind; the next poll re-reads everything anyway.
	}
}

func (c *DBusClient) dispatch(sigs chan *dbus.Signal) {
	defer close(c.events)
	for {
		var sig *dbus.Signal
		var ok bool
		select {
		case <-c.done:
			return
		case sig, ok = <-sigs:
			if !ok {
				return
			}
		}
		if sig == nil {
			continue
		}
		now := time.Now()
		switch sig.Name {
		case "org.freedesktop.DBus.NameOwnerChanged":
			if len(sig.Body) < 3 {
				continue
			}
			name, _ := sig.Body[0].(string)
			newOwner, _ := sig.Body[2].(string)
			if _, ok := ParseBusName(name); !ok {
				continue
			}
			c.mu.Lock()
			if newOwner == "" {
				delete(c.owners, name)
				delete(c.identity, name)
			} else {
				c.owners[name] = newOwner
				delete(c.identity, name)
			}
			c.mu.Unlock()
			if newOwner == "" {
				c.emit(Event{Kind: PlayerRemoved, BusName: name, At: now})
			} else {
				c.emit(Event{Kind: PlayerAdded, BusName: name, At: now})
			}
		case PropsIface + ".PropertiesChanged":
			if len(sig.Body) < 1 {
				continue
			}
			iface, _ := sig.Body[0].(string)
			if iface != PlayerIface && iface != RootIface {
				continue
			}
			if iface == RootIface {
				c.invalidateIdentity(sig.Sender)
			}
			for _, name := range c.namesFor(sig.Sender) {
				c.emit(Event{Kind: PropertiesChanged, BusName: name, At: now})
			}
		case PlayerIface + ".Seeked":
			var pos int64
			var ok bool
			if len(sig.Body) > 0 {
				pos, ok = ToMicroseconds(sig.Body[0])
			}
			for _, name := range c.namesFor(sig.Sender) {
				c.emit(Event{Kind: Seeked, BusName: name, Position: pos, PositionOK: ok, At: now})
			}
		}
	}
}

func (c *DBusClient) invalidateIdentity(sender string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, owner := range c.owners {
		if owner == sender {
			delete(c.identity, name)
		}
	}
}

// namesFor maps a signal sender (unique name like ":1.42") back to the MPRIS
// well-known names it owns.
func (c *DBusClient) namesFor(sender string) []string {
	if _, ok := ParseBusName(sender); ok {
		return []string{sender}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, owner := range c.owners {
		if owner == sender {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ListPlayers implements Bus.
func (c *DBusClient) ListPlayers(ctx context.Context) ([]PlayerID, error) {
	var names []string
	if err := c.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return nil, fmt.Errorf("list bus names: %w", err)
	}
	var ids []PlayerID
	for _, n := range names {
		id, ok := ParseBusName(n)
		if !ok {
			continue
		}
		ids = append(ids, id)
		c.mu.Lock()
		_, known := c.owners[n]
		c.mu.Unlock()
		if !known {
			var owner string
			if err := c.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, n).Store(&owner); err == nil {
				c.mu.Lock()
				c.owners[n] = owner
				c.mu.Unlock()
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].BusName < ids[j].BusName })
	return ids, nil
}

// Identity returns the player's Identity property (cached).
func (c *DBusClient) Identity(ctx context.Context, id PlayerID) string {
	c.mu.Lock()
	v, ok := c.identity[id.BusName]
	c.mu.Unlock()
	if ok {
		return v
	}
	var variant dbus.Variant
	obj := c.conn.Object(id.BusName, ObjectPath)
	if err := obj.CallWithContext(ctx, PropsIface+".Get", 0, RootIface, "Identity").Store(&variant); err != nil {
		return ""
	}
	s, _ := ToString(variant)
	s = strings.TrimSpace(s)
	c.mu.Lock()
	c.identity[id.BusName] = s
	c.mu.Unlock()
	return s
}

// ReadPosition reads Position with a direct Properties.Get call. MPRIS does
// not emit PropertiesChanged for Position, so cached values are useless.
func (c *DBusClient) ReadPosition(ctx context.Context, id PlayerID) (int64, any, bool, error) {
	var variant dbus.Variant
	obj := c.conn.Object(id.BusName, ObjectPath)
	if err := obj.CallWithContext(ctx, PropsIface+".Get", 0, PlayerIface, "Position").Store(&variant); err != nil {
		return 0, nil, false, err
	}
	raw := variant.Value()
	us, ok := ToMicroseconds(raw)
	return us, raw, ok, nil
}

// RawPlayerProps returns the raw Player interface properties (diagnostics).
func (c *DBusClient) RawPlayerProps(ctx context.Context, id PlayerID) (map[string]dbus.Variant, error) {
	props := map[string]dbus.Variant{}
	obj := c.conn.Object(id.BusName, ObjectPath)
	if err := obj.CallWithContext(ctx, PropsIface+".GetAll", 0, PlayerIface).Store(&props); err != nil {
		return nil, err
	}
	return props, nil
}

// Snapshot implements Bus.
func (c *DBusClient) Snapshot(ctx context.Context, id PlayerID) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	snap := Snapshot{ID: id}
	props, err := c.RawPlayerProps(ctx, id)
	if err != nil {
		return snap, fmt.Errorf("read %s properties: %w", id.Suffix, err)
	}
	snap.Identity = c.Identity(ctx, id)
	if v, ok := props["PlaybackStatus"]; ok {
		snap.StatusRaw = v.Value()
		s, _ := ToString(v)
		snap.Status = ParseStatus(s)
	} else {
		snap.Status = Stopped
	}
	if v, ok := props["Rate"]; ok {
		snap.RateRaw = v.Value()
		snap.Rate, snap.RateOK = ToFloat(v)
	}
	if v, ok := props["Metadata"]; ok {
		snap.Metadata = ParseMetadata(metadataMap(v.Value()))
	} else {
		snap.Metadata = ParseMetadata(nil)
	}
	snap.Position, snap.PositionRaw, snap.PositionOK, snap.PositionErr = c.ReadPosition(ctx, id)
	snap.ReadAt = time.Now()
	return snap, nil
}

func metadataMap(v any) map[string]any {
	out := map[string]any{}
	switch m := v.(type) {
	case map[string]dbus.Variant:
		for k, x := range m {
			out[k] = x
		}
	case map[string]any:
		for k, x := range m {
			out[k] = x
		}
	case dbus.Variant:
		return metadataMap(m.Value())
	}
	return out
}
