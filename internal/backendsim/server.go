// Package backendsim is a simulated application server that PulseLB balances
// traffic across.
//
// It behaves like a real app under load: it has a fixed number of worker
// slots (Capacity) and a service time (Latency), so when more requests arrive
// than it can serve they queue up and latency climbs. It also exposes chaos
// controls under /__pulse/ so a demo can make it slow, flaky, unhealthy or
// crash it outright.
package backendsim

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one simulated server.
type Config struct {
	Name     string
	Addr     string        // listen address, e.g. "127.0.0.1:8001" or "127.0.0.1:0"
	Latency  time.Duration // base service time per request
	Jitter   time.Duration // uniform +/- jitter added to Latency
	Capacity int           // concurrent worker slots; 0 means unlimited
}

// Server is a running simulated backend.
type Server struct {
	cfg   Config
	slots chan struct{}

	delay   atomic.Int64  // extra latency in ns, injected by chaos
	errRate atomic.Uint64 // float64 bits, probability in [0,1] of answering 500
	down    atomic.Bool   // health check fails and requests get 503

	served       atomic.Uint64
	inflight     atomic.Int64
	crashedUntil atomic.Int64 // unix ns; 0 when running

	mu   sync.Mutex
	srv  *http.Server
	addr string // resolved address after the first Start
}

// New creates a server; call Start to begin listening.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, addr: cfg.Addr}
	if cfg.Capacity > 0 {
		s.slots = make(chan struct{}, cfg.Capacity)
	}
	return s
}

// Name returns the configured server name.
func (s *Server) Name() string { return s.cfg.Name }

// Addr returns the address the server listens on (resolved after Start).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// URL returns the base URL of the server.
func (s *Server) URL() string { return "http://" + s.Addr() }

// Start begins serving. Restarting after Stop reuses the same address.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	s.crashedUntil.Store(0)
	go s.srv.Serve(ln)
	return nil
}

// Stop closes the listener and every open connection, like a process dying.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		s.srv.Close()
		s.srv = nil
	}
}

// Crash stops the server and, if d > 0, brings it back after d.
func (s *Server) Crash(d time.Duration) {
	s.Stop()
	if d > 0 {
		s.crashedUntil.Store(time.Now().Add(d).UnixNano())
		time.AfterFunc(d, func() { s.Start() })
	} else {
		s.crashedUntil.Store(math.MaxInt64)
	}
}

// SetDelay injects extra latency into every request.
func (s *Server) SetDelay(d time.Duration) { s.delay.Store(int64(d)) }

// SetErrorRate makes a fraction p of requests fail with HTTP 500.
func (s *Server) SetErrorRate(p float64) {
	s.errRate.Store(math.Float64bits(math.Max(0, math.Min(1, p))))
}

// SetDown makes health checks and requests answer 503.
func (s *Server) SetDown(down bool) { s.down.Store(down) }

// Reset clears all injected chaos.
func (s *Server) Reset() {
	s.SetDelay(0)
	s.SetErrorRate(0)
	s.SetDown(false)
}

// State is the chaos and load state reported by /__pulse/chaos.
type State struct {
	Name       string  `json:"name"`
	Addr       string  `json:"addr"`
	DelayMs    float64 `json:"delay_ms"`
	ErrorRate  float64 `json:"error_rate"`
	Down       bool    `json:"down"`
	Crashed    bool    `json:"crashed"`
	Served     uint64  `json:"served"`
	Inflight   int64   `json:"inflight"`
	Capacity   int     `json:"capacity"`
	BaseLatMs  float64 `json:"base_latency_ms"`
}

// State returns a snapshot of the server's chaos settings and counters.
func (s *Server) State() State {
	return State{
		Name:      s.cfg.Name,
		Addr:      s.Addr(),
		DelayMs:   float64(s.delay.Load()) / 1e6,
		ErrorRate: math.Float64frombits(s.errRate.Load()),
		Down:      s.down.Load(),
		Crashed:   s.crashedUntil.Load() != 0,
		Served:    s.served.Load(),
		Inflight:  s.inflight.Load(),
		Capacity:  s.cfg.Capacity,
		BaseLatMs: float64(s.cfg.Latency) / 1e6,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__pulse/health":
		if s.down.Load() {
			http.Error(w, `{"status":"down"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	case "/__pulse/chaos":
		s.handleChaos(w, r)
	default:
		s.handleWork(w, r)
	}
}

func (s *Server) handleWork(w http.ResponseWriter, r *http.Request) {
	if s.down.Load() {
		http.Error(w, "backend down", http.StatusServiceUnavailable)
		return
	}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	// Wait for a free worker slot: this queue is what makes an overloaded
	// server slow instead of infinitely fast.
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-r.Context().Done():
			return
		}
	}

	d := s.cfg.Latency + time.Duration(s.delay.Load())
	if s.cfg.Jitter > 0 {
		d += time.Duration(rand.Int64N(int64(2*s.cfg.Jitter))) - s.cfg.Jitter
	}
	if d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}

	if p := math.Float64frombits(s.errRate.Load()); p > 0 && rand.Float64() < p {
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}

	n := s.served.Add(1)
	buf := make([]byte, 0, 96)
	buf = append(buf, `{"backend":"`...)
	buf = append(buf, s.cfg.Name...)
	buf = append(buf, `","path":`...)
	buf = strconv.AppendQuote(buf, r.URL.Path)
	buf = append(buf, `,"served":`...)
	buf = strconv.AppendUint(buf, n, 10)
	buf = append(buf, '}')
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf)
}

// handleChaos reads or changes chaos settings.
//
//	GET  /__pulse/chaos                      current state
//	POST /__pulse/chaos?delay=200            add 200ms latency
//	POST /__pulse/chaos?errors=0.3           fail 30% of requests
//	POST /__pulse/chaos?down=true            fail health checks
//	POST /__pulse/chaos?crash=15             crash, restart after 15s
//	POST /__pulse/chaos?reset=true           clear everything
func (s *Server) handleChaos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		q := r.URL.Query()
		if q.Get("reset") == "true" {
			s.Reset()
		}
		if v := q.Get("delay"); v != "" {
			if ms, err := strconv.ParseFloat(v, 64); err == nil {
				s.SetDelay(time.Duration(ms * float64(time.Millisecond)))
			}
		}
		if v := q.Get("errors"); v != "" {
			if p, err := strconv.ParseFloat(v, 64); err == nil {
				s.SetErrorRate(p)
			}
		}
		if v := q.Get("down"); v != "" {
			s.SetDown(v == "true" || v == "1")
		}
		if v := q.Get("crash"); v != "" {
			if sec, err := strconv.ParseFloat(v, 64); err == nil {
				// Crash after replying, otherwise the reply dies with the server.
				d := time.Duration(sec * float64(time.Second))
				time.AfterFunc(50*time.Millisecond, func() { s.Crash(d) })
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.State())
}
