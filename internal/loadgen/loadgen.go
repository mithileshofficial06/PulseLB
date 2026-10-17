// Package loadgen generates HTTP traffic at a fixed arrival rate.
//
// It is an open-model generator: requests are launched on schedule whether
// or not earlier ones have finished, the way real users arrive. A closed-model
// tool (N virtual users that each wait for a reply) silently sends less
// traffic when the system slows down, which flatters the system under test.
// If the generator itself can't keep up, it counts the shortfall as Dropped
// instead of hiding it.
package loadgen

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/hist"
)

// Route is a request path and its relative weight in the traffic mix.
type Route struct {
	Path   string
	Weight int
}

// Mix presets.
var presets = map[string]string{
	"uniform": "/api/trains:1",
	// Tatkal rush: 20% bookings (critical), 50% train lookups (normal),
	// 30% browsing (low priority).
	"tatkal": "/book/tatkal:20,/api/trains:50,/browse/offers:30",
}

// ParseMix parses a preset name or "path:weight,path:weight".
func ParseMix(s string) ([]Route, error) {
	if p, ok := presets[s]; ok {
		s = p
	}
	var out []Route
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		path, w, ok := strings.Cut(part, ":")
		weight := 1
		if ok {
			n, err := strconv.Atoi(w)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad weight in %q", part)
			}
			weight = n
		}
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("path must start with /: %q", path)
		}
		out = append(out, Route{Path: path, Weight: weight})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty mix")
	}
	return out, nil
}

// Config controls a run.
type Config struct {
	Target      string        // base URL, e.g. http://127.0.0.1:8080
	Rate        float64       // requests per second
	Duration    time.Duration // 0 = until the context is cancelled
	Ramp        time.Duration // linear ramp from 0 to Rate
	Mix         []Route
	Timeout     time.Duration // per request
	MaxInflight int           // cap on concurrent requests
	MaxConns    int           // cap on open connections (2048)
}

// Counters are cumulative totals.
type Counters struct {
	Sent, OK, Shed, Errors, Timeouts, Dropped uint64
	Lat                                       hist.Snapshot // successful requests only
}

// Generator produces traffic. Rate can be changed while it runs.
type Generator struct {
	cfg    Config
	client *http.Client
	rate   atomic.Uint64 // float64 bits
	cum    []int
	total  int

	inflight atomic.Int64
	sent     atomic.Uint64
	ok       atomic.Uint64
	shed     atomic.Uint64
	errors   atomic.Uint64
	timeouts atomic.Uint64
	dropped  atomic.Uint64
	lat      hist.Histogram
	wg       sync.WaitGroup

	errMu    sync.Mutex
	errKinds map[string]uint64
}

// New creates a generator.
func New(cfg Config) *Generator {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 2048
	}
	if cfg.MaxInflight == 0 {
		cfg.MaxInflight = 20000
	}
	if len(cfg.Mix) == 0 {
		cfg.Mix, _ = ParseMix("uniform")
	}
	g := &Generator{cfg: cfg, errKinds: make(map[string]uint64)}
	g.client = &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        0,
			MaxIdleConnsPerHost: cfg.MaxConns,
			// Requests beyond MaxConns wait for a free connection (and that
			// wait counts in their latency) rather than opening new sockets.
			MaxConnsPerHost:    cfg.MaxConns,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: true,
		},
	}
	for _, r := range cfg.Mix {
		g.total += r.Weight
		g.cum = append(g.cum, g.total)
	}
	g.SetRate(cfg.Rate)
	return g
}

// SetRate changes the target rate.
func (g *Generator) SetRate(r float64) { g.rate.Store(math.Float64bits(math.Max(0, r))) }

// Rate returns the target rate.
func (g *Generator) Rate() float64 { return math.Float64frombits(g.rate.Load()) }

// Counters returns the totals so far.
func (g *Generator) Counters() Counters {
	return Counters{
		Sent: g.sent.Load(), OK: g.ok.Load(), Shed: g.shed.Load(), Errors: g.errors.Load(),
		Timeouts: g.timeouts.Load(), Dropped: g.dropped.Load(), Lat: g.lat.Snapshot(),
	}
}

// Run sends traffic until Duration elapses or ctx is cancelled, then waits
// for outstanding requests.
func (g *Generator) Run(ctx context.Context) {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	last := start
	carry := 0.0
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-tick.C:
			el := now.Sub(start)
			if g.cfg.Duration > 0 && el >= g.cfg.Duration {
				break loop
			}
			r := g.Rate()
			if g.cfg.Ramp > 0 && el < g.cfg.Ramp {
				r *= float64(el) / float64(g.cfg.Ramp)
			}
			carry += r * now.Sub(last).Seconds()
			last = now
			n := int(carry)
			carry -= float64(n)
			for i := 0; i < n; i++ {
				if g.inflight.Load() >= int64(g.cfg.MaxInflight) {
					g.dropped.Add(1)
					continue
				}
				g.inflight.Add(1)
				g.wg.Add(1)
				go g.fire()
			}
		}
	}
	g.wg.Wait()
	g.client.CloseIdleConnections()
}

// noteError counts errors by their innermost message, e.g.
// "connectex: No connection could be made...".
func (g *Generator) noteError(err error) {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	g.errMu.Lock()
	g.errKinds[msg]++
	g.errMu.Unlock()
}

// ErrorKinds returns client-side error counts by cause.
func (g *Generator) ErrorKinds() map[string]uint64 {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	out := make(map[string]uint64, len(g.errKinds))
	for k, v := range g.errKinds {
		out[k] = v
	}
	return out
}

func (g *Generator) pick() string {
	if len(g.cum) == 1 {
		return g.cfg.Mix[0].Path
	}
	x := rand.IntN(g.total)
	for i, c := range g.cum {
		if x < c {
			return g.cfg.Mix[i].Path
		}
	}
	return g.cfg.Mix[0].Path
}

func (g *Generator) fire() {
	defer g.wg.Done()
	defer g.inflight.Add(-1)
	g.sent.Add(1)
	t0 := time.Now()
	resp, err := g.client.Get(g.cfg.Target + g.pick())
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			g.timeouts.Add(1)
		}
		g.errors.Add(1)
		g.noteError(err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("X-Pulse-Shed") != "":
		g.shed.Add(1)
	case resp.StatusCode >= 500:
		g.errors.Add(1)
	default:
		g.ok.Add(1)
		g.lat.Record(time.Since(t0))
	}
}
