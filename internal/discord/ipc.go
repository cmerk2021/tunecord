package discord

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// IPC opcodes.
const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
	opPing      = 3
	opPong      = 4
)

const maxFrameSize = 1 << 20

// ErrNotRunning is returned when no Discord IPC socket accepts a connection.
var ErrNotRunning = errors.New("no Discord IPC socket found (is Discord running?)")

// socketSubdirs are relative to $XDG_RUNTIME_DIR (and the temp dirs) and
// cover native, Flatpak, Snap and popular third-party clients.
var socketSubdirs = []string{
	"",
	"app/com.discordapp.Discord",
	"app/com.discordapp.DiscordCanary",
	"app/com.discordapp.DiscordPTB",
	".flatpak/com.discordapp.Discord/xdg-run",
	"snap.discord",
	"snap.discord-canary",
	"app/dev.vencord.Vesktop",
	".flatpak/dev.vencord.Vesktop/xdg-run",
	"app/xyz.armcord.ArmCord",
	"app/app.legcord.Legcord",
	"app/io.github.spacingbat3.webcord",
}

// SocketCandidates returns all paths searched for a Discord IPC socket, in
// order of preference.
func SocketCandidates() []string {
	var bases []string
	seen := map[string]bool{}
	addBase := func(b string) {
		if b == "" || !filepath.IsAbs(b) || seen[b] {
			return
		}
		seen[b] = true
		bases = append(bases, b)
	}
	addBase(os.Getenv("XDG_RUNTIME_DIR"))
	addBase(fmt.Sprintf("/run/user/%d", os.Getuid()))
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		addBase(os.Getenv(k))
	}
	addBase("/tmp")

	var out []string
	for bi, base := range bases {
		for _, sub := range socketSubdirs {
			// Only the plain directory is checked in temp-dir fallbacks.
			if bi > 1 && sub != "" {
				continue
			}
			dir := filepath.Join(base, sub)
			for i := 0; i < 10; i++ {
				out = append(out, filepath.Join(dir, fmt.Sprintf("discord-ipc-%d", i)))
			}
		}
	}
	return out
}

// IPCConn is a connected, handshaken Discord IPC session.
type IPCConn struct {
	conn net.Conn
	path string

	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan frameMsg

	done    chan struct{}
	errOnce sync.Once
	err     error
}

type frameMsg struct {
	Cmd   string          `json:"cmd"`
	Evt   string          `json:"evt"`
	Nonce string          `json:"nonce"`
	Data  json.RawMessage `json:"data"`
}

type errorData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Dial finds a Discord IPC socket and performs the handshake.
func Dial(ctx context.Context, clientID string) (*IPCConn, error) {
	var lastErr error
	for _, p := range SocketCandidates() {
		st, err := os.Stat(p)
		if err != nil || st.Mode()&os.ModeSocket == 0 {
			continue
		}
		c, err := dialPath(ctx, p, clientID)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRunning, lastErr)
	}
	return nil, ErrNotRunning
}

func dialPath(ctx context.Context, path, clientID string) (*IPCConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	c := &IPCConn{conn: nc, path: path, pending: map[string]chan frameMsg{}, done: make(chan struct{})}
	deadline, _ := ctx.Deadline()
	_ = nc.SetDeadline(deadline)
	if err := c.handshake(clientID); err != nil {
		nc.Close()
		return nil, fmt.Errorf("handshake on %s: %w", path, err)
	}
	_ = nc.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

func (c *IPCConn) handshake(clientID string) error {
	payload, _ := json.Marshal(map[string]any{"v": 1, "client_id": clientID})
	if err := writeFrame(c.conn, opHandshake, payload); err != nil {
		return err
	}
	for {
		op, data, err := readFrame(c.conn)
		if err != nil {
			return err
		}
		switch op {
		case opClose:
			var e errorData
			_ = json.Unmarshal(data, &e)
			return fmt.Errorf("discord closed the connection: %s (code %d)", e.Message, e.Code)
		case opPing:
			if err := writeFrame(c.conn, opPong, data); err != nil {
				return err
			}
		case opFrame:
			var m frameMsg
			if err := json.Unmarshal(data, &m); err != nil {
				return fmt.Errorf("bad handshake response: %w", err)
			}
			if m.Evt == "READY" {
				return nil
			}
			if m.Evt == "ERROR" {
				var e errorData
				_ = json.Unmarshal(m.Data, &e)
				return fmt.Errorf("discord error: %s (code %d)", e.Message, e.Code)
			}
		}
	}
}

// Path returns the socket path in use.
func (c *IPCConn) Path() string { return c.path }

// Done is closed when the connection is lost.
func (c *IPCConn) Done() <-chan struct{} { return c.done }

// Err returns why the connection was lost.
func (c *IPCConn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *IPCConn) fail(err error) {
	c.errOnce.Do(func() {
		c.err = err
		close(c.done)
		_ = c.conn.Close()
	})
}

// Close closes the connection (Discord clears the activity of a closed
// connection on its own).
func (c *IPCConn) Close() error {
	c.fail(errors.New("closed"))
	return nil
}

func (c *IPCConn) readLoop() {
	for {
		op, data, err := readFrame(c.conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("discord closed the IPC socket")
			}
			c.fail(err)
			return
		}
		switch op {
		case opClose:
			var e errorData
			_ = json.Unmarshal(data, &e)
			c.fail(fmt.Errorf("discord closed the connection: %s (code %d)", e.Message, e.Code))
			return
		case opPing:
			c.wmu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := writeFrame(c.conn, opPong, data)
			c.wmu.Unlock()
			if err != nil {
				c.fail(err)
				return
			}
		case opFrame:
			var m frameMsg
			if json.Unmarshal(data, &m) != nil || m.Nonce == "" {
				continue
			}
			c.mu.Lock()
			ch := c.pending[m.Nonce]
			delete(c.pending, m.Nonce)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

func newNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// SetActivity sets (or clears, when a is nil) the activity and waits for
// Discord's acknowledgement.
func (c *IPCConn) SetActivity(ctx context.Context, a *Activity) error {
	nonce := newNonce()
	payload, err := json.Marshal(map[string]any{
		"cmd":   "SET_ACTIVITY",
		"args":  map[string]any{"pid": os.Getpid(), "activity": a},
		"nonce": nonce,
	})
	if err != nil {
		return err
	}
	ch := make(chan frameMsg, 1)
	c.mu.Lock()
	c.pending[nonce] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, nonce)
		c.mu.Unlock()
	}()

	c.wmu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = writeFrame(c.conn, opFrame, payload)
	c.wmu.Unlock()
	if err != nil {
		c.fail(err)
		return err
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case m := <-ch:
		if m.Evt == "ERROR" {
			var e errorData
			_ = json.Unmarshal(m.Data, &e)
			return fmt.Errorf("SET_ACTIVITY rejected: %s (code %d)", e.Message, e.Code)
		}
		return nil
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		err := errors.New("timed out waiting for Discord to acknowledge SET_ACTIVITY")
		c.fail(err)
		return err
	}
}

func writeFrame(w io.Writer, op uint32, payload []byte) error {
	buf := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], op)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) (uint32, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := binary.LittleEndian.Uint32(hdr[0:4])
	n := binary.LittleEndian.Uint32(hdr[4:8])
	if n > maxFrameSize {
		return 0, nil, fmt.Errorf("frame too large (%d bytes)", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return op, data, nil
}
