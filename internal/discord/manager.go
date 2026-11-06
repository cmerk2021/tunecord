package discord

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Limiter enforces a minimum spacing between pushes and a sliding window
// cap (Discord allows about 5 activity updates per 20 seconds).
type Limiter struct {
	MinSpacing time.Duration
	Window     time.Duration
	Max        int
	sent       []time.Time
}

// DefaultLimiter returns the limiter used by the service: at most one push
// per 4s and never more than 5 per 20s.
func DefaultLimiter() *Limiter {
	return &Limiter{MinSpacing: 4 * time.Second, Window: 20 * time.Second, Max: 5}
}

// Next returns the earliest time a push is allowed.
func (l *Limiter) Next(now time.Time) time.Time {
	l.prune(now)
	earliest := now
	if n := len(l.sent); n > 0 {
		if t := l.sent[n-1].Add(l.MinSpacing); t.After(earliest) {
			earliest = t
		}
		if l.Max > 0 && n >= l.Max {
			if t := l.sent[n-l.Max].Add(l.Window); t.After(earliest) {
				earliest = t
			}
		}
	}
	return earliest
}

// Record notes a push at now.
func (l *Limiter) Record(now time.Time) {
	l.prune(now)
	l.sent = append(l.sent, now)
}

func (l *Limiter) prune(now time.Time) {
	keep := l.Window
	if l.MinSpacing > keep {
		keep = l.MinSpacing
	}
	i := 0
	for i < len(l.sent) && now.Sub(l.sent[i]) >= keep {
		i++
	}
	l.sent = l.sent[i:]
}

// Conn is a live Discord connection.
type Conn interface {
	SetActivity(ctx context.Context, a *Activity) error
	Done() <-chan struct{}
	Path() string
	Close() error
}

// Dialer opens a Discord connection.
type Dialer func(ctx context.Context, clientID string) (Conn, error)

// DefaultDialer dials the real Discord IPC socket.
func DefaultDialer(ctx context.Context, clientID string) (Conn, error) {
	c, err := Dial(ctx, clientID)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Status describes the manager's connection state.
type Status struct {
	Connected bool      `json:"connected"`
	Socket    string    `json:"socket,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	LastPush  time.Time `json:"last_push,omitempty"`
	Pushes    int       `json:"pushes"`
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	ClientID   string
	Dialer     Dialer
	Limiter    *Limiter
	MinBackoff time.Duration
	MaxBackoff time.Duration
	Logger     *slog.Logger
}

// Manager owns the Discord connection. Callers publish the desired activity
// with Set; the manager coalesces rapid changes (only the latest state is
// sent), enforces rate limits, deduplicates identical presences, and
// reconnects with exponential backoff whenever Discord is absent or
// restarts. It never returns an error to its caller.
type Manager struct {
	opts ManagerOptions
	log  *slog.Logger
	wake chan struct{}

	mu      sync.Mutex
	desired *Activity
	status  Status
}

// NewManager creates a manager; call Run to start it.
func NewManager(o ManagerOptions) *Manager {
	if o.Dialer == nil {
		o.Dialer = DefaultDialer
	}
	if o.Limiter == nil {
		o.Limiter = DefaultLimiter()
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Manager{opts: o, log: o.Logger, wake: make(chan struct{}, 1)}
}

// Set publishes the desired activity (nil clears the presence). It never blocks.
func (m *Manager) Set(a *Activity) {
	m.mu.Lock()
	m.desired = a
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Status returns the current connection status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) getDesired() *Activity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.desired
}

func (m *Manager) setStatus(f func(*Status)) {
	m.mu.Lock()
	f(&m.status)
	m.mu.Unlock()
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

// Run drives the connection until ctx is cancelled. On exit it clears the
// presence (best effort) and closes the connection.
func (m *Manager) Run(ctx context.Context) {
	var conn Conn
	var sent *Activity
	backoff := m.opts.MinBackoff
	lastDialErr := ""

	defer func() {
		if conn == nil {
			return
		}
		if sent != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := conn.SetActivity(cctx, nil); err != nil {
				m.log.Debug("clearing presence on shutdown failed", "err", err)
			} else {
				m.log.Info("presence cleared")
			}
			cancel()
		}
		conn.Close()
		m.setStatus(func(s *Status) { s.Connected = false })
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		if conn == nil {
			c, err := m.opts.Dialer(ctx, m.opts.ClientID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if err.Error() != lastDialErr {
					m.log.Info("Discord not available, will retry", "err", err, "retry_in", backoff)
					lastDialErr = err.Error()
				} else {
					m.log.Debug("Discord not available, will retry", "err", err, "retry_in", backoff)
				}
				m.setStatus(func(s *Status) { s.Connected = false; s.Socket = ""; s.LastError = err.Error() })
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff *= 2
				if backoff > m.opts.MaxBackoff {
					backoff = m.opts.MaxBackoff
				}
				continue
			}
			conn = c
			sent = nil // a fresh connection shows nothing
			backoff = m.opts.MinBackoff
			lastDialErr = ""
			m.log.Info("connected to Discord", "socket", c.Path())
			m.setStatus(func(s *Status) { s.Connected = true; s.Socket = c.Path(); s.LastError = "" })
		}

		desired := m.getDesired()
		if !Same(desired, sent) {
			now := time.Now()
			if next := m.opts.Limiter.Next(now); next.After(now) {
				// Wait for the rate limit; new Set calls are coalesced and
				// the latest desired state is re-read after the wait.
				t := time.NewTimer(next.Sub(now))
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-conn.Done():
					t.Stop()
					m.lost(&conn)
				case <-t.C:
				}
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			err := conn.SetActivity(cctx, desired)
			cancel()
			m.opts.Limiter.Record(time.Now())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.log.Warn("updating Discord presence failed, reconnecting", "err", err)
				m.setStatus(func(s *Status) { s.LastError = err.Error() })
				m.lost(&conn)
				continue
			}
			sent = desired
			if desired == nil {
				m.log.Debug("presence cleared")
			} else {
				m.log.Debug("presence updated", "details", desired.Details, "state", desired.State, "timestamps", desired.Timestamps != nil)
			}
			m.setStatus(func(s *Status) { s.LastPush = time.Now(); s.Pushes++ })
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-conn.Done():
			m.lost(&conn)
		}
	}
}

func (m *Manager) lost(conn *Conn) {
	if *conn == nil {
		return
	}
	m.log.Info("disconnected from Discord")
	(*conn).Close()
	*conn = nil
	m.setStatus(func(s *Status) { s.Connected = false; s.Socket = "" })
}
