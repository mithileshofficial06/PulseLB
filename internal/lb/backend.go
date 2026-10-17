package lb

import (
	"math"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/hist"
)

// State is a backend's place in the rotation, as shown on the dashboard.
type State string

const (
	StateHealthy  State = "healthy"
	StateWarming  State = "warming"  // back in rotation, ramping up under slow start
	StateEjected  State = "ejected"  // pulled out by passive outlier detection
	StateDown     State = "down"     // failing active health checks
	StateDraining State = "draining" // finishing in-flight requests before removal
)

// ewmaTau is how fast the latency average forgets old samples. A short
// value makes PulseLB react within a couple of seconds.
const ewmaTau = 1500 * time.Millisecond

// Backend is one upstream server and everything PulseLB knows about it.
type Backend struct {
	Name    string
	URL     *url.URL
	Managed bool   // launched by the autoscaler
	stop    func() // shuts down a managed backend

	inflight atomic.Int64
	requests atomic.Uint64
	failures atomic.Uint64
	latency  hist.Histogram

	healthy      atomic.Bool
	healthStreak atomic.Int32 // >0 consecutive passes, <0 consecutive failures
	draining     atomic.Bool
	ejectedUntil atomic.Int64 // unix ns, 0 when not ejected
	ejections    atomic.Int32
	consecFails  atomic.Int32
	warmSince    atomic.Int64 // unix ns when it (re)joined the rotation
	lastProbe    atomic.Int64 // unix ns of the last forced probe request

	ewmaMu     sync.Mutex
	ewmaBits   atomic.Uint64 // float64 ns, readable without the lock
	lastSample atomic.Int64  // unix ns of the last latency sample
}

func newBackend(name string, u *url.URL, now int64) *Backend {
	b := &Backend{Name: name, URL: u}
	b.healthy.Store(true)
	b.warmSince.Store(now)
	return b
}

// Available reports whether new requests may be routed to b.
func (b *Backend) Available(now int64) bool {
	return b.healthy.Load() && !b.draining.Load() && b.ejectedUntil.Load() <= now
}

// State classifies b for display.
func (b *Backend) State(now int64, slowStart time.Duration) State {
	switch {
	case b.draining.Load():
		return StateDraining
	case !b.healthy.Load():
		return StateDown
	case b.ejectedUntil.Load() > now:
		return StateEjected
	case slowStart > 0 && b.warmFactor(now, slowStart) < 1:
		return StateWarming
	}
	return StateHealthy
}

// warmFactor ramps from 0.1 to 1 over the slow-start window after a backend
// joins, so a fresh or recovered server isn't flooded with traffic at once.
func (b *Backend) warmFactor(now int64, window time.Duration) float64 {
	el := now - b.warmSince.Load()
	if window <= 0 || el >= int64(window) {
		return 1
	}
	f := float64(el) / float64(window)
	if f < 0.1 {
		f = 0.1
	}
	return f
}

// observe folds one response time into a peak-sensitive moving average
// (the "peak EWMA" used by Linkerd and Finagle): a slower sample is adopted
// immediately, faster samples pull the average down gradually over time.
func (b *Backend) observe(rtt time.Duration, now int64) {
	b.ewmaMu.Lock()
	r := float64(rtt)
	cur := math.Float64frombits(b.ewmaBits.Load())
	last := b.lastSample.Load()
	switch {
	case last == 0 || cur == 0 || r > cur:
		cur = r
	default:
		w := math.Exp(-float64(now-last) / float64(ewmaTau))
		cur = cur*w + r*(1-w)
	}
	b.ewmaBits.Store(math.Float64bits(cur))
	b.lastSample.Store(now)
	b.ewmaMu.Unlock()
}

// EWMA returns the current latency average.
func (b *Backend) EWMA() time.Duration {
	return time.Duration(math.Float64frombits(b.ewmaBits.Load()))
}

func (b *Backend) resetEWMA(v time.Duration, now int64) {
	b.ewmaMu.Lock()
	b.ewmaBits.Store(math.Float64bits(float64(v)))
	b.lastSample.Store(now)
	b.ewmaMu.Unlock()
}

// score is the cost of sending one more request to b: expected latency
// times queue depth, inflated while the backend is warming up.
func (b *Backend) score(now int64, slowStart time.Duration) float64 {
	e := math.Float64frombits(b.ewmaBits.Load())
	if e <= 0 {
		e = float64(time.Millisecond)
	}
	s := e * float64(b.inflight.Load()+1)
	if slowStart > 0 {
		if w := b.warmFactor(now, slowStart); w < 1 {
			s /= w
		}
	}
	return s
}

// claimProbe returns true at most once per interval when b has had no
// latency sample for that long. Without probes a backend that was slow would
// never get the traffic needed to prove it has recovered.
func (b *Backend) claimProbe(now int64, every time.Duration) bool {
	if now-b.lastSample.Load() < int64(every) {
		return false
	}
	lp := b.lastProbe.Load()
	if now-lp < int64(every) {
		return false
	}
	return b.lastProbe.CompareAndSwap(lp, now)
}

// Pool is the current set of backends. Reads are lock-free; adds and
// removes copy the slice so the hot path never takes a lock.
type Pool struct {
	mu   sync.Mutex
	list atomic.Pointer[[]*Backend]
}

// All returns the current backends. Do not modify the returned slice.
func (p *Pool) All() []*Backend {
	if l := p.list.Load(); l != nil {
		return *l
	}
	return nil
}

func (p *Pool) add(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.All()
	next := make([]*Backend, len(old), len(old)+1)
	copy(next, old)
	next = append(next, b)
	p.list.Store(&next)
}

func (p *Pool) remove(b *Backend) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.All()
	next := make([]*Backend, 0, len(old))
	for _, x := range old {
		if x != b {
			next = append(next, x)
		}
	}
	p.list.Store(&next)
	return len(next) != len(old)
}

// Get finds a backend by name.
func (p *Pool) Get(name string) *Backend {
	for _, b := range p.All() {
		if b.Name == name {
			return b
		}
	}
	return nil
}
