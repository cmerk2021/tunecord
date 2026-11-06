package progress

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const sec = time.Second

func at(d time.Duration) time.Time { return t0.Add(d) }

// sim is a fake player clock producing samples.
type sim struct {
	key    string
	status Status
	length time.Duration
	rate   float64
	// true position model of the "player"
	posAt   time.Duration
	posWall time.Duration
	// overrides
	stuck      bool          // report a constant position
	stuckValue time.Duration // constant value when stuck
	noPos      bool
}

func newSim(key string, length time.Duration) *sim {
	return &sim{key: key, status: Playing, length: length, rate: 1}
}

func (s *sim) truePos(now time.Duration) time.Duration {
	if s.status != Playing {
		return s.posAt
	}
	return s.posAt + scale(now-s.posWall, s.rate)
}

func (s *sim) set(now, pos time.Duration) { s.posAt, s.posWall = pos, now }

func (s *sim) sample(now time.Duration) Sample {
	p := s.truePos(now)
	if s.stuck {
		p = s.stuckValue
	}
	return Sample{
		At: at(now), Status: s.status, TrackKey: s.key,
		Length: s.length, LengthOK: true,
		Position: p, PositionOK: !s.noPos,
		Rate: s.rate, RateOK: true,
	}
}

func approx(t *testing.T, what string, got, want time.Time, tol time.Duration) {
	t.Helper()
	if absDur(got.Sub(want)) > tol {
		t.Fatalf("%s = %v, want %v (±%v)", what, got.Sub(t0), want.Sub(t0), tol)
	}
}

func TestBasicTimestamps(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 200*sec)
	p.set(0, 30*sec)
	if ch := tr.Update(p.sample(0)); ch&ChangeTrack == 0 {
		t.Fatalf("want track change, got %v", ch)
	}
	v := tr.View(at(0))
	if v.Mode != ModeReal || !v.HasTimestamps {
		t.Fatalf("view %+v", v)
	}
	approx(t, "start", v.Start, at(-30*sec), time.Millisecond)
	approx(t, "end", v.End, at(170*sec), time.Millisecond)
}

func TestSteadyPlaybackNoChangesAndJitterIgnored(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	first := tr.View(at(0))
	jitter := []time.Duration{120, -300, 800, -900, 0, 450}
	for i := 1; i <= 60; i++ {
		s := p.sample(time.Duration(i) * sec)
		s.Position += jitter[i%len(jitter)] * time.Millisecond
		if ch := tr.Update(s); ch != 0 {
			t.Fatalf("tick %d: unexpected change %v", i, ch)
		}
	}
	v := tr.View(at(60 * sec))
	if !v.Start.Equal(first.Start) || !v.End.Equal(first.End) {
		t.Fatalf("timestamps drifted: %v/%v vs %v/%v", v.Start, v.End, first.Start, first.End)
	}
}

func TestSeekDetectedByDeviation(t *testing.T) {
	cfg := DefaultConfig()
	tr := New(cfg)
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	tr.Update(p.sample(sec))
	p.set(2*sec, 100*sec) // user seeks forward
	if ch := tr.Update(p.sample(2 * sec)); ch&ChangeSeek == 0 {
		t.Fatalf("seek not detected: %v", ch)
	}
	v := tr.View(at(2 * sec))
	approx(t, "start", v.Start, at(2*sec-100*sec), time.Millisecond)
	// A deviation below the threshold is not a seek.
	p.set(3*sec, 101*sec+2*sec)
	if ch := tr.Update(p.sample(3 * sec)); ch != 0 {
		t.Fatalf("2s deviation treated as change: %v", ch)
	}
	// Backwards seek.
	p.set(4*sec, 10*sec)
	if ch := tr.Update(p.sample(4 * sec)); ch&ChangeSeek == 0 {
		t.Fatalf("backward seek not detected: %v", ch)
	}
}

func TestSeekedSignal(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	if ch := tr.Seek(at(5*sec), 120*sec, true); ch != ChangeSeek {
		t.Fatalf("seek signal: %v", ch)
	}
	approx(t, "start", tr.View(at(5*sec)).Start, at(5*sec-120*sec), time.Millisecond)
	// A Seeked signal matching the prediction is a no-op.
	if ch := tr.Seek(at(6*sec), 121*sec, true); ch != 0 {
		t.Fatalf("redundant seek: %v", ch)
	}
	// Invalid seek data is ignored.
	if ch := tr.Seek(at(6*sec), 999*sec, true); ch != 0 {
		t.Fatalf("overshooting seek accepted: %v", ch)
	}
	if ch := tr.Seek(at(6*sec), 0, false); ch != 0 {
		t.Fatalf("unparseable seek accepted: %v", ch)
	}
	// After the signal, polled positions consistent with it are no change.
	p.set(5*sec, 120*sec)
	if ch := tr.Update(p.sample(7 * sec)); ch != 0 {
		t.Fatalf("post-seek poll: %v", ch)
	}
}

func TestPauseResume(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	tr.Update(p.sample(10 * sec))
	p.set(20*sec, p.truePos(20*sec))
	p.status = Paused
	if ch := tr.Update(p.sample(20 * sec)); ch&ChangeStatus == 0 {
		t.Fatalf("pause: %v", ch)
	}
	v := tr.View(at(25 * sec))
	if v.HasTimestamps || v.Mode != ModeNone {
		t.Fatalf("paused view should have no bar: %+v", v)
	}
	if v.Position != 20*sec {
		t.Fatalf("frozen position %v", v.Position)
	}
	// Polling while paused produces no changes.
	for i := 21; i < 60; i++ {
		if ch := tr.Update(p.sample(time.Duration(i) * sec)); ch != 0 {
			t.Fatalf("paused poll change %v", ch)
		}
	}
	// Resume 40s later: start must be re-anchored from the fresh position.
	p.status = Playing
	p.set(60*sec, 20*sec)
	if ch := tr.Update(p.sample(60 * sec)); ch&ChangeStatus == 0 {
		t.Fatalf("resume: %v", ch)
	}
	v = tr.View(at(60 * sec))
	approx(t, "start", v.Start, at(40*sec), time.Millisecond)
	approx(t, "end", v.End, at(340*sec), time.Millisecond)
}

func TestSeekWhilePaused(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	p.status = Paused
	p.set(5*sec, 5*sec)
	tr.Update(p.sample(5 * sec))
	p.set(6*sec, 200*sec)
	if ch := tr.Update(p.sample(6 * sec)); ch&ChangeSeek == 0 {
		t.Fatalf("paused seek not tracked: %v", ch)
	}
	p.status = Playing
	p.set(10*sec, 200*sec)
	tr.Update(p.sample(10 * sec))
	approx(t, "start", tr.View(at(10*sec)).Start, at(10*sec-200*sec), time.Millisecond)
}

func TestRateChange(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	tr.Update(p.sample(10 * sec))
	// Switch to 2x at 10s (position 10s).
	p.set(10*sec, 10*sec)
	p.rate = 2
	if ch := tr.Update(p.sample(10 * sec)); ch&ChangeRate == 0 {
		t.Fatalf("rate change: %v", ch)
	}
	v := tr.View(at(10 * sec))
	// end = now + (length - pos)/rate = 10 + 290/2 = 155
	approx(t, "end", v.End, at(155*sec), time.Millisecond)
	// effective elapsed = pos/rate = 5s -> start = 5s
	approx(t, "start", v.Start, at(5*sec), time.Millisecond)
	// Steady 2x playback is not a seek.
	for i := 11; i < 40; i++ {
		if ch := tr.Update(p.sample(time.Duration(i) * sec)); ch != 0 {
			t.Fatalf("tick %d change %v", i, ch)
		}
	}
	// Invalid rates are treated as 1.0.
	s := p.sample(40 * sec)
	s.Rate = 0
	if ch := tr.Update(s); ch&ChangeRate == 0 {
		t.Fatalf("rate 0 should normalise to 1: %v", ch)
	}
	if tr.View(at(40*sec)).Rate != 1 {
		t.Fatal("rate not 1")
	}
}

func TestTrackChange(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 200*sec)
	tr.Update(p.sample(0))
	q := newSim("b", 100*sec)
	q.set(50*sec, 0)
	if ch := tr.Update(q.sample(50 * sec)); ch != ChangeTrack {
		t.Fatalf("track change: %v", ch)
	}
	v := tr.View(at(50 * sec))
	approx(t, "start", v.Start, at(50*sec), time.Millisecond)
	approx(t, "end", v.End, at(150*sec), time.Millisecond)
}

func TestStalePositionAfterTrackChange(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	tr.Update(p.sample(100 * sec))
	// New track, but the player still reports the old track's position.
	q := newSim("b", 250*sec)
	q.set(101*sec, 0)
	s := q.sample(101 * sec)
	s.Position = 101 * sec
	tr.Update(s)
	v := tr.View(at(101 * sec))
	if v.Mode != ModeEstimated {
		t.Fatalf("stale position should not be trusted: %+v", v)
	}
	approx(t, "start", v.Start, at(101*sec), time.Millisecond)
	// Then real data arrives and is trusted again.
	tr.Update(q.sample(102 * sec))
	tr.Update(q.sample(103 * sec))
	ch := tr.Update(q.sample(104 * sec))
	v = tr.View(at(104 * sec))
	if v.Mode != ModeReal {
		t.Fatalf("did not recover (ch=%v): %+v", ch, v)
	}
	approx(t, "start", v.Start, at(101*sec), 50*time.Millisecond)
}

func TestPositionBeyondLengthAtTrackStart(t *testing.T) {
	tr := New(DefaultConfig())
	q := newSim("b", 100*sec)
	s := q.sample(0)
	s.Position = 250 * sec
	tr.Update(s)
	v := tr.View(at(0))
	if v.Mode != ModeEstimated || !strings.Contains(v.Reason, "beyond") {
		t.Fatalf("%+v", v)
	}
	approx(t, "start", v.Start, at(0), 0)
}

func TestClampSmallOvershoot(t *testing.T) {
	tr := New(DefaultConfig())
	q := newSim("b", 100*sec)
	s := q.sample(0)
	s.Position = 101 * sec // within tolerance -> clamp
	tr.Update(s)
	if v := tr.View(at(0)); v.Mode != ModeReal || v.Position != 100*sec {
		t.Fatalf("%+v", v)
	}
	s.Position = -time.Second // small negative -> clamp to 0
	tr2 := New(DefaultConfig())
	tr2.Update(s)
	if v := tr2.View(at(0)); v.Mode != ModeReal || v.Position != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestInvalidLengthNoBar(t *testing.T) {
	for name, mut := range map[string]func(*Sample){
		"missing":  func(s *Sample) { s.LengthOK = false },
		"zero":     func(s *Sample) { s.Length = 0 },
		"negative": func(s *Sample) { s.Length = -5 * sec },
		"absurd":   func(s *Sample) { s.Length = 25 * time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			tr := New(DefaultConfig())
			s := newSim("a", 100*sec).sample(0)
			mut(&s)
			tr.Update(s)
			v := tr.View(at(0))
			if v.HasTimestamps || v.Mode != ModeNone || !strings.Contains(v.Reason, "length") {
				t.Fatalf("%+v", v)
			}
		})
	}
}

func TestLengthArrivesLate(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 100*sec)
	s := p.sample(0)
	s.LengthOK = false
	tr.Update(s)
	if ch := tr.Update(p.sample(sec)); ch&ChangeLength == 0 {
		t.Fatalf("length change: %v", ch)
	}
	v := tr.View(at(sec))
	if v.Mode != ModeReal {
		t.Fatalf("%+v", v)
	}
	approx(t, "end", v.End, at(100*sec), time.Millisecond)
}

func stuckScenario(t *testing.T, fb Fallback) (*Tracker, *sim) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Fallback = fb
	tr := New(cfg)
	p := newSim("a", 200*sec)
	p.stuck = true
	tr.Update(p.sample(0))
	return tr, p
}

func TestStuckAtZeroEstimate(t *testing.T) {
	tr, p := stuckScenario(t, FallbackEstimate)
	var changed Change
	// Before the grace period nothing changes and no false seek is reported.
	for i := 1; i <= 2; i++ {
		if ch := tr.Update(p.sample(time.Duration(i) * sec)); ch != 0 {
			t.Fatalf("tick %d: %v", i, ch)
		}
	}
	if tr.View(at(2*sec)).Mode != ModeReal {
		t.Fatal("marked unreliable too early")
	}
	for i := 3; i <= 5; i++ {
		changed |= tr.Update(p.sample(time.Duration(i) * sec))
	}
	if changed&ChangeMode == 0 {
		t.Fatalf("stuck position not detected")
	}
	v := tr.View(at(5 * sec))
	if v.Mode != ModeEstimated || !v.HasTimestamps || !strings.Contains(v.Reason, "stuck") {
		t.Fatalf("%+v", v)
	}
	// Estimate is anchored at the track start with position 0.
	approx(t, "start", v.Start, at(0), time.Millisecond)
	approx(t, "end", v.End, at(200*sec), time.Millisecond)
	// Stays estimated and stable.
	for i := 6; i < 30; i++ {
		if ch := tr.Update(p.sample(time.Duration(i) * sec)); ch != 0 {
			t.Fatalf("tick %d change %v", i, ch)
		}
	}
	// Pause / resume in estimated mode resumes from the last known position.
	p.status = Paused
	tr.Update(p.sample(30 * sec))
	if v := tr.View(at(30 * sec)); v.Position != 30*sec {
		t.Fatalf("frozen at %v", v.Position)
	}
	p.status = Playing
	tr.Update(p.sample(50 * sec))
	approx(t, "start after resume", tr.View(at(50*sec)).Start, at(20*sec), time.Millisecond)
	// New track in estimated mode starts from zero again.
	p.key = "b"
	tr.Update(p.sample(60 * sec))
	approx(t, "new track start", tr.View(at(60*sec)).Start, at(60*sec), time.Millisecond)
}

func TestStuckAtZeroSkip(t *testing.T) {
	tr, p := stuckScenario(t, FallbackSkip)
	for i := 1; i <= 5; i++ {
		tr.Update(p.sample(time.Duration(i) * sec))
	}
	v := tr.View(at(5 * sec))
	if v.HasTimestamps || v.Mode != ModeNone || !strings.Contains(v.Reason, "skip") {
		t.Fatalf("%+v", v)
	}
}

func TestStuckRequiresSamples(t *testing.T) {
	tr, p := stuckScenario(t, FallbackEstimate)
	// Only one sample after a long gap: not enough evidence.
	tr.Update(p.sample(10 * sec))
	if tr.View(at(10*sec)).Mode != ModeReal {
		t.Fatal("declared stuck from a single sample")
	}
}

func TestStuckMidTrackThenRecovers(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 300*sec)
	tr.Update(p.sample(0))
	for i := 1; i <= 10; i++ {
		tr.Update(p.sample(time.Duration(i) * sec))
	}
	p.stuck, p.stuckValue = true, 10*sec
	for i := 11; i <= 15; i++ {
		tr.Update(p.sample(time.Duration(i) * sec))
	}
	v := tr.View(at(15 * sec))
	if v.Mode != ModeEstimated {
		t.Fatalf("%+v", v)
	}
	approx(t, "estimate continues", v.Start, at(0), time.Millisecond)
	// Player starts reporting sane data again (it had actually seeked to 100s).
	p.stuck = false
	p.set(20*sec, 100*sec)
	tr.Update(p.sample(20 * sec))
	tr.Update(p.sample(21 * sec))
	ch := tr.Update(p.sample(22 * sec))
	if ch&ChangeMode == 0 {
		t.Fatalf("no recovery: %v", ch)
	}
	v = tr.View(at(22 * sec))
	if v.Mode != ModeReal {
		t.Fatalf("%+v", v)
	}
	approx(t, "re-anchored", v.Start, at(20*sec-100*sec), time.Millisecond)
}

func TestUnreadablePosition(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 200*sec)
	p.noPos = true
	tr.Update(p.sample(0))
	v := tr.View(at(0))
	if v.Mode != ModeEstimated || !v.HasTimestamps {
		t.Fatalf("%+v", v)
	}
	approx(t, "start", v.Start, at(0), 0)
	// Unreadable mid-track: becomes unreliable after grace + samples.
	tr2 := New(DefaultConfig())
	q := newSim("a", 200*sec)
	tr2.Update(q.sample(0))
	q.noPos = true
	var ch Change
	for i := 1; i <= 5; i++ {
		ch |= tr2.Update(q.sample(time.Duration(i) * sec))
	}
	if ch&ChangeMode == 0 || tr2.View(at(5*sec)).Mode != ModeEstimated {
		t.Fatalf("ch=%v view=%+v", ch, tr2.View(at(5*sec)))
	}
	approx(t, "estimate continues", tr2.View(at(5*sec)).Start, at(0), time.Millisecond)
}

func TestNegativeGarbagePositionIgnored(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 200*sec)
	tr.Update(p.sample(0))
	s := p.sample(sec)
	s.Position = -50 * sec
	if ch := tr.Update(s); ch != 0 {
		t.Fatalf("single garbage sample caused change %v", ch)
	}
	if tr.View(at(sec)).Mode != ModeReal {
		t.Fatal("single garbage sample made it unreliable")
	}
}

func TestStoppedNoBar(t *testing.T) {
	tr := New(DefaultConfig())
	p := newSim("a", 200*sec)
	p.status = Stopped
	tr.Update(p.sample(0))
	if v := tr.View(at(0)); v.HasTimestamps {
		t.Fatalf("%+v", v)
	}
}

func TestChangeString(t *testing.T) {
	if (ChangeTrack|ChangeSeek).String() != "track+seek" || Change(0).String() != "none" {
		t.Fatal((ChangeTrack | ChangeSeek).String())
	}
}
