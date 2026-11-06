// Package progress turns noisy MPRIS position data into stable Discord
// progress-bar timestamps.
//
// The model keeps an anchor (anchorWall, anchorPos, rate) and predicts the
// current position as anchorPos + (now-anchorWall)*rate. The anchor only
// moves when something meaningful happens (track change, play/pause, seek,
// rate change, recovery), so the derived timestamps are stable between
// events and Discord is never spammed with jittery recomputations.
package progress

import (
	"fmt"
	"math"
	"time"
)

// Status is the playback status as seen by the tracker.
type Status string

// Playback statuses (same spelling as MPRIS).
const (
	Playing Status = "Playing"
	Paused  Status = "Paused"
	Stopped Status = "Stopped"
)

// Mode describes where the progress bar comes from.
type Mode string

// Progress bar modes.
const (
	ModeNone      Mode = "none"      // no bar
	ModeReal      Mode = "real"      // anchored on the player's reported position
	ModeEstimated Mode = "estimated" // anchored on tunecord's own timer
)

// Fallback selects what to do when the player's position is unreliable.
type Fallback string

// Fallback modes.
const (
	FallbackEstimate Fallback = "estimate"
	FallbackSkip     Fallback = "skip"
)

// Change is a bitmask of what an update changed.
type Change uint

// Change flags.
const (
	ChangeTrack Change = 1 << iota
	ChangeStatus
	ChangeSeek
	ChangeRate
	ChangeLength
	ChangeMode
)

func (c Change) String() string {
	if c == 0 {
		return "none"
	}
	names := []string{"track", "status", "seek", "rate", "length", "mode"}
	s := ""
	for i, n := range names {
		if c&(1<<i) != 0 {
			if s != "" {
				s += "+"
			}
			s += n
		}
	}
	return s
}

// Config tunes the tracker.
type Config struct {
	SeekThreshold      time.Duration // deviation from prediction that counts as a seek
	StuckGrace         time.Duration // how long a non-advancing position is tolerated while Playing
	StuckSamples       int           // minimum samples before declaring a position stuck/bad
	RecoverSamples     int           // consecutive sane, advancing samples to trust a player again
	OvershootTolerance time.Duration // how far outside [0,length] a position may be before it is rejected
	MaxLength          time.Duration // longer lengths are treated as invalid (streams)
	Fallback           Fallback
}

// DefaultConfig returns the default tuning.
func DefaultConfig() Config {
	return Config{
		SeekThreshold:      2500 * time.Millisecond,
		StuckGrace:         3 * time.Second,
		StuckSamples:       3,
		RecoverSamples:     2,
		OvershootTolerance: 2 * time.Second,
		MaxLength:          24 * time.Hour,
		Fallback:           FallbackEstimate,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.SeekThreshold <= 0 {
		c.SeekThreshold = d.SeekThreshold
	}
	if c.StuckGrace <= 0 {
		c.StuckGrace = d.StuckGrace
	}
	if c.StuckSamples <= 0 {
		c.StuckSamples = d.StuckSamples
	}
	if c.RecoverSamples <= 0 {
		c.RecoverSamples = d.RecoverSamples
	}
	if c.OvershootTolerance <= 0 {
		c.OvershootTolerance = d.OvershootTolerance
	}
	if c.MaxLength <= 0 {
		c.MaxLength = d.MaxLength
	}
	if c.Fallback != FallbackSkip {
		c.Fallback = FallbackEstimate
	}
	return c
}

// Sample is one fresh observation of a player.
type Sample struct {
	At         time.Time
	Status     Status
	TrackKey   string
	Length     time.Duration
	LengthOK   bool
	Position   time.Duration
	PositionOK bool
	Rate       float64
	RateOK     bool
}

// Micros converts MPRIS microseconds to a Duration.
func Micros(us int64) time.Duration { return time.Duration(us) * time.Microsecond }

// Tracker models the playback position of one player.
type Tracker struct {
	cfg Config

	hasTrack    bool
	trackKey    string
	status      Status
	rate        float64
	length      time.Duration
	lengthValid bool

	anchorWall time.Time
	anchorPos  time.Duration

	reliable bool
	reason   string // why the position is unreliable

	haveLast     bool
	lastRaw      time.Duration
	lastRawAt    time.Time
	stillSince   time.Time
	stillSamples int
	badCount     int
	firstBad     time.Time
	goodStreak   int
}

// New creates a tracker.
func New(cfg Config) *Tracker {
	return &Tracker{cfg: cfg.withDefaults(), rate: 1}
}

// SetConfig changes the tuning without resetting state.
func (t *Tracker) SetConfig(cfg Config) { t.cfg = cfg.withDefaults() }

// TrackKey returns the key of the current track ("" before the first sample).
func (t *Tracker) TrackKey() string { return t.trackKey }

// HasTrack reports whether the tracker has seen a sample.
func (t *Tracker) HasTrack() bool { return t.hasTrack }

func normRate(s Sample) float64 {
	r := s.Rate
	if !s.RateOK || math.IsNaN(r) || math.IsInf(r, 0) || r <= 0 {
		return 1
	}
	return math.Min(math.Max(r, 0.01), 100)
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(math.Round(float64(d) * f))
}

func fmtPos(d time.Duration) string {
	neg := d < 0
	d = absDur(d).Round(100 * time.Millisecond)
	m := int(d / time.Minute)
	s := float64(d%time.Minute) / float64(time.Second)
	out := fmt.Sprintf("%d:%04.1f", m, s)
	if neg {
		out = "-" + out
	}
	return out
}

func (t *Tracker) lengthOf(s Sample) (time.Duration, bool) {
	if !s.LengthOK || s.Length <= 0 || s.Length > t.cfg.MaxLength {
		return 0, false
	}
	return s.Length, true
}

// sanitize validates a reported position against the current length.
func (t *Tracker) sanitize(s Sample) (time.Duration, bool, string) {
	if !s.PositionOK {
		return 0, false, "Position could not be read or has an unsupported type"
	}
	p := s.Position
	tol := t.cfg.OvershootTolerance
	if p < -tol {
		return 0, false, fmt.Sprintf("negative position %s", fmtPos(p))
	}
	if p < 0 {
		p = 0
	}
	if t.lengthValid && p > t.length+tol {
		return 0, false, fmt.Sprintf("position %s is beyond the track length %s", fmtPos(p), fmtPos(t.length))
	}
	if t.lengthValid && p > t.length {
		p = t.length
	}
	return p, true, ""
}

// Predict returns the modelled position at time now.
func (t *Tracker) Predict(now time.Time) time.Duration {
	p := t.anchorPos
	if t.status == Playing {
		p += scale(now.Sub(t.anchorWall), t.rate)
	}
	if p < 0 {
		p = 0
	}
	if t.lengthValid && p > t.length {
		p = t.length
	}
	return p
}

func (t *Tracker) anchor(at time.Time, pos time.Duration) {
	t.anchorWall = at
	t.anchorPos = pos
}

func (t *Tracker) resetMotion(at time.Time, pos time.Duration, ok bool) {
	t.haveLast = ok
	t.lastRaw = pos
	t.lastRawAt = at
	t.stillSince = at
	t.stillSamples = 0
	t.badCount = 0
	t.goodStreak = 0
}

func (t *Tracker) markUnreliable(reason string) {
	t.reliable = false
	t.reason = reason
	t.goodStreak = 0
}

// Update feeds a fresh sample and returns what changed. The caller should
// read the position fresh for every sample (never from a cache).
func (t *Tracker) Update(s Sample) Change {
	rate := normRate(s)
	if !t.hasTrack || s.TrackKey != t.trackKey {
		return t.startTrack(s, rate)
	}

	var ch Change
	length, lengthValid := t.lengthOf(s)
	if lengthValid != t.lengthValid || absDur(length-t.length) > 500*time.Millisecond {
		ch |= ChangeLength
	}
	t.length, t.lengthValid = length, lengthValid

	pos, posOK, badReason := t.sanitize(s)

	if s.Status != t.status {
		// Freeze the modelled position at the transition, then re-anchor
		// from a fresh read when it can be trusted.
		frozen := t.Predict(s.At)
		t.status = s.Status
		t.rate = rate
		start := frozen
		if t.reliable && posOK {
			start = pos
		}
		t.anchor(s.At, start)
		t.resetMotion(s.At, pos, posOK)
		return ch | ChangeStatus
	}

	if math.Abs(rate-t.rate) > 0.005 {
		cur := t.Predict(s.At)
		if t.reliable && posOK {
			cur = pos
		}
		t.rate = rate
		t.anchor(s.At, cur)
		t.resetMotion(s.At, pos, posOK)
		return ch | ChangeRate
	}

	if t.status != Playing {
		// Seeking while paused: keep the frozen position accurate.
		if t.reliable && posOK && absDur(pos-t.anchorPos) > t.cfg.SeekThreshold {
			t.anchor(s.At, pos)
			ch |= ChangeSeek
		}
		t.resetMotion(s.At, pos, posOK)
		return ch
	}
	return ch | t.observePlaying(s.At, pos, posOK, badReason)
}

func (t *Tracker) startTrack(s Sample, rate float64) Change {
	hadTrack := t.hasTrack
	prevPred := t.Predict(s.At)

	t.hasTrack = true
	t.trackKey = s.TrackKey
	t.status = s.Status
	t.rate = rate
	t.length, t.lengthValid = t.lengthOf(s)
	t.reliable = true
	t.reason = ""

	pos, ok, badReason := t.sanitize(s)
	switch {
	case !ok:
		t.markUnreliable(badReason)
		t.anchor(s.At, 0)
	case hadTrack && pos > 2*time.Second && absDur(pos-prevPred) < 1500*time.Millisecond:
		// Some players keep reporting the previous track's position for a
		// moment after a track change.
		t.markUnreliable(fmt.Sprintf("position %s looked stale (previous track's position) at track start", fmtPos(pos)))
		t.anchor(s.At, 0)
	default:
		t.anchor(s.At, pos)
	}
	t.resetMotion(s.At, pos, ok)
	return ChangeTrack
}

func (t *Tracker) observePlaying(at time.Time, pos time.Duration, ok bool, badReason string) Change {
	if !ok {
		if t.badCount == 0 {
			t.firstBad = at
		}
		t.badCount++
		t.goodStreak = 0
		if t.reliable && t.badCount >= t.cfg.StuckSamples && at.Sub(t.firstBad) >= t.cfg.StuckGrace {
			t.markUnreliable(badReason)
			return ChangeMode
		}
		if !t.reliable && t.badCount >= t.cfg.StuckSamples {
			t.reason = badReason
		}
		return 0
	}
	t.badCount = 0

	advanced := !t.haveLast || absDur(pos-t.lastRaw) >= 50*time.Millisecond
	if advanced {
		t.stillSince = at
		t.stillSamples = 0
	} else {
		t.stillSamples++
	}
	stuck := !advanced && at.Sub(t.stillSince) >= t.cfg.StuckGrace && t.stillSamples >= t.cfg.StuckSamples
	stuckReason := func() string {
		return fmt.Sprintf("position stuck at %s for %s while Playing", fmtPos(pos), at.Sub(t.stillSince).Round(100*time.Millisecond))
	}

	var ch Change
	if t.reliable {
		switch {
		case stuck:
			// Keep the current anchor: the estimate continues from the
			// moment the track started (or the last good position).
			t.markUnreliable(stuckReason())
			ch |= ChangeMode
		case advanced && absDur(pos-t.Predict(at)) > t.cfg.SeekThreshold:
			t.anchor(at, pos)
			ch |= ChangeSeek
		}
	} else {
		switch {
		case advanced && t.haveLast && pos > 0:
			dWall := at.Sub(t.lastRawAt)
			exp := scale(dWall, t.rate)
			dPos := pos - t.lastRaw
			tol := exp / 4
			if tol < 700*time.Millisecond {
				tol = 700 * time.Millisecond
			}
			if dWall > 0 && dPos > 0 && absDur(dPos-exp) <= tol {
				t.goodStreak++
			} else {
				t.goodStreak = 0
			}
		case !advanced:
			t.goodStreak = 0
			if stuck {
				t.reason = stuckReason()
			}
		default:
			t.goodStreak = 0
		}
		if t.goodStreak >= t.cfg.RecoverSamples {
			t.reliable = true
			t.reason = ""
			t.goodStreak = 0
			t.anchor(at, pos)
			ch |= ChangeMode
		}
	}
	t.haveLast = true
	t.lastRaw = pos
	t.lastRawAt = at
	return ch
}

// Seek handles an org.mpris.MediaPlayer2.Player.Seeked signal, which carries
// the new position. It re-anchors immediately in both real and estimated
// mode. Returns ChangeSeek if the anchor moved noticeably.
func (t *Tracker) Seek(at time.Time, pos time.Duration, ok bool) Change {
	if !t.hasTrack || !ok {
		return 0
	}
	if pos < 0 {
		pos = 0
	}
	if t.lengthValid && pos > t.length+t.cfg.OvershootTolerance {
		return 0
	}
	if t.lengthValid && pos > t.length {
		pos = t.length
	}
	if absDur(pos-t.Predict(at)) <= time.Second {
		return 0
	}
	t.anchor(at, pos)
	if t.reliable {
		t.resetMotion(at, pos, true)
	} else {
		// Keep observing raw positions so a stuck Position property is
		// still detected, but restart the recovery streak.
		t.goodStreak = 0
	}
	return ChangeSeek
}

// View is the derived progress-bar state.
type View struct {
	Mode          Mode
	HasTimestamps bool
	Start         time.Time
	End           time.Time
	Position      time.Duration // predicted
	Length        time.Duration
	LengthValid   bool
	Rate          float64
	Status        Status
	Reliable      bool
	Reason        string // human-readable explanation of Mode
}

// View derives the progress bar at time now.
func (t *Tracker) View(now time.Time) View {
	v := View{
		Position:    t.Predict(now),
		Length:      t.length,
		LengthValid: t.lengthValid,
		Rate:        t.rate,
		Status:      t.status,
		Reliable:    t.reliable,
		Mode:        ModeNone,
	}
	switch {
	case !t.hasTrack:
		v.Reason = "no track"
		return v
	case t.status != Playing:
		v.Reason = "not playing (" + string(t.status) + "): no progress bar"
		return v
	case !t.lengthValid:
		v.Reason = "track length missing, zero or longer than 24h (stream?): no progress bar"
		return v
	case t.reliable:
		v.Mode = ModeReal
		v.Reason = "player reports a sane, advancing position"
	case t.cfg.Fallback == FallbackEstimate:
		v.Mode = ModeEstimated
		v.Reason = "position unreliable (" + t.reason + "); using tunecord's own timer (fallback = estimate)"
	default:
		v.Reason = "position unreliable (" + t.reason + "); no progress bar (fallback = skip)"
		return v
	}
	v.HasTimestamps = true
	v.Start = t.anchorWall.Add(-scale(t.anchorPos, 1/t.rate))
	v.End = v.Start.Add(scale(t.length, 1/t.rate))
	return v
}

// UnreliableReason returns why the position is considered unreliable ("" if reliable).
func (t *Tracker) UnreliableReason() string { return t.reason }
