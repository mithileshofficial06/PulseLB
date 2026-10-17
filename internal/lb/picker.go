package lb

import (
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// pickCtx carries per-request inputs to a Picker.
type pickCtx struct {
	now       int64
	exclude   []*Backend    // backends already tried for this request
	slowStart time.Duration // 0 when slow start is off
}

func (c *pickCtx) usable(b *Backend) bool {
	if !b.Available(c.now) {
		return false
	}
	for _, e := range c.exclude {
		if e == b {
			return false
		}
	}
	return true
}

// Picker chooses a backend for one request.
type Picker interface {
	Name() string
	Pick(bs []*Backend, c *pickCtx) *Backend
}

// NewPicker returns the named algorithm: "p2c", "rr" or "leastconn".
func NewPicker(name string) Picker {
	switch name {
	case "rr", "roundrobin":
		return &roundRobin{}
	case "leastconn", "lc":
		return &leastConn{}
	default:
		return &p2cEWMA{probeEvery: 500 * time.Millisecond}
	}
}

// roundRobin sends requests to each backend in turn, skipping unavailable
// ones. This is what most load balancers do by default.
type roundRobin struct{ next atomic.Uint64 }

func (*roundRobin) Name() string { return "rr" }

func (p *roundRobin) Pick(bs []*Backend, c *pickCtx) *Backend {
	n := uint64(len(bs))
	if n == 0 {
		return nil
	}
	start := p.next.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		if b := bs[(start+i)%n]; c.usable(b) {
			return b
		}
	}
	return nil
}

// leastConn sends each request to the backend with the fewest requests in
// flight, breaking ties from a random starting point.
type leastConn struct{}

func (*leastConn) Name() string { return "leastconn" }

func (*leastConn) Pick(bs []*Backend, c *pickCtx) *Backend {
	n := len(bs)
	if n == 0 {
		return nil
	}
	var best *Backend
	var bestIn int64
	off := rand.IntN(n)
	for i := 0; i < n; i++ {
		b := bs[(off+i)%n]
		if !c.usable(b) {
			continue
		}
		if in := b.inflight.Load(); best == nil || in < bestIn {
			best, bestIn = b, in
		}
	}
	return best
}

// p2cEWMA is PulseLB's adaptive algorithm, "power of two choices" over
// latency: pick two random available backends and send the request to the
// one with the lower score (latency average × requests in flight). Slow or
// overloaded servers lose traffic within milliseconds, and because only two
// are compared, many concurrent requests don't all stampede onto the single
// "best" server.
type p2cEWMA struct{ probeEvery time.Duration }

func (*p2cEWMA) Name() string { return "p2c" }

func (p *p2cEWMA) Pick(bs []*Backend, c *pickCtx) *Backend {
	var buf [32]*Backend
	avail := buf[:0]
	for _, b := range bs {
		if c.usable(b) {
			avail = append(avail, b)
		}
	}
	n := len(avail)
	switch n {
	case 0:
		return nil
	case 1:
		return avail[0]
	}
	for _, b := range avail {
		if b.claimProbe(c.now, p.probeEvery) {
			return b
		}
	}
	i := rand.IntN(n)
	j := rand.IntN(n - 1)
	if j >= i {
		j++
	}
	a, b := avail[i], avail[j]
	if a.score(c.now, c.slowStart) <= b.score(c.now, c.slowStart) {
		return a
	}
	return b
}
