// Package arena races a classic round-robin load balancer against PulseLB.
//
// Both sides get their own identical set of simulated backends, the same
// traffic from the same generator settings, and the same failures at the
// same moment. Mount Handler under /api/arena/ and open arena.html.
package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/backendsim"
	"github.com/mithileshofficial06/pulselb/internal/lb"
	"github.com/mithileshofficial06/pulselb/internal/loadgen"
)

// Config sizes the arena. Zero values get the defaults used in the README.
type Config struct {
	Latency  time.Duration // backend service time (10ms)
	Capacity int           // worker slots per backend (10, i.e. ~1,000 req/s each)
	Peak     float64       // per-side rate during the rush (2,250 = 75% of capacity)
	BasePort int           // classic side uses BasePort+100.., PulseLB BasePort+200.. (8000)
}

// New starts both sides: 3 backends and a balancer each.
func New(cfg Config) (*Arena, error) {
	if cfg.Latency == 0 {
		cfg.Latency = 10 * time.Millisecond
	}
	if cfg.Capacity == 0 {
		cfg.Capacity = 10
	}
	if cfg.Peak == 0 {
		cfg.Peak = 2250
	}
	if cfg.BasePort == 0 {
		cfg.BasePort = 8000
	}
	tmpl := backendsim.Config{Latency: cfg.Latency, Jitter: cfg.Latency / 4, Capacity: cfg.Capacity, QueueTimeout: time.Second}
	a := &Arena{peak: cfg.Peak}
	classic, err := newSide("classic", "Classic round robin", cfg.BasePort+100, tmpl, lb.Options{Name: "Classic", Algorithm: "rr"})
	if err != nil {
		return nil, err
	}
	pulse, err := newSide("pulse", "PulseLB", cfg.BasePort+200, tmpl, lb.Options{Name: "PulseLB", Smart: true, Spawner: &backendsim.Spawner{Template: tmpl}})
	if err != nil {
		classic.close()
		return nil, err
	}
	a.sides = []*side{classic, pulse}
	a.traffic = loadgen.NewController(classic.url, pulse.url)
	return a, nil
}

// Handler serves the arena API under /api/arena/.
func (a *Arena) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/arena/traffic", a.traffic.Handler())
	mux.HandleFunc("GET /api/arena/history", a.handleHistory)
	mux.HandleFunc("GET /api/arena/stream", a.handleStream)
	mux.HandleFunc("POST /api/arena/chaos", admin(a.handleChaos))
	mux.HandleFunc("POST /api/arena/scenario", admin(a.handleScenario))
	return mux
}

// Describe returns where each side listens.
func (a *Arena) Describe() string {
	return fmt.Sprintf("classic LB on %s, PulseLB on %s, 3 backends each", a.sides[0].url, a.sides[1].url)
}

// Close stops traffic, both balancers and all backends.
func (a *Arena) Close() {
	a.scen.stop()
	a.traffic.Stop()
	for _, s := range a.sides {
		s.close()
	}
}

func (s *side) close() {
	s.srv.Close()
	s.lb.Close()
	for _, b := range s.servers {
		b.Stop()
	}
}

type side struct {
	Key     string
	Label   string
	lb      *lb.LB
	srv     *http.Server
	servers []*backendsim.Server
	url     string
}

type Arena struct {
	sides   []*side
	traffic *loadgen.Controller
	scen    scenario
	peak    float64 // per-side rate at the scenario's peak, for scaling copy
}

func newSide(key, label string, basePort int, tmpl backendsim.Config, opts lb.Options) (*side, error) {
	s := &side{Key: key, Label: label, lb: lb.New(opts)}
	for i := 1; i <= 3; i++ {
		cfg := tmpl
		cfg.Name = fmt.Sprintf("backend-%d", i)
		cfg.Addr = fmt.Sprintf("127.0.0.1:%d", basePort+i)
		b := backendsim.New(cfg)
		if err := b.Start(); err != nil {
			return nil, fmt.Errorf("%s %s: %w", key, cfg.Name, err)
		}
		if _, err := s.lb.AddBackend(cfg.Name, b.URL()); err != nil {
			return nil, err
		}
		s.servers = append(s.servers, b)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", basePort)
	s.url = "http://" + addr
	s.srv = &http.Server{Addr: addr, Handler: s.lb, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		if err := s.srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("arena %s balancer: %v", key, err)
		}
	}()
	s.lb.Start()
	return s, nil
}

// ---- chaos: the same failure, applied to both sides at once ----

func (a *Arena) chaos(name, action string, value float64) error {
	found := false
	for _, s := range a.sides {
		for _, b := range s.servers {
			if b.Name() != name {
				continue
			}
			found = true
			var msg string
			switch action {
			case "delay":
				b.SetDelay(time.Duration(value * float64(time.Millisecond)))
				msg = fmt.Sprintf("%s slowed down by +%.0fms.", name, value)
			case "errors":
				b.SetErrorRate(value)
				msg = fmt.Sprintf("%s now fails %.0f%% of requests.", name, value*100)
			case "down":
				b.SetDown(value != 0)
				msg = fmt.Sprintf("%s's health check is failing.", name)
			case "crash":
				b.Crash(time.Duration(value * float64(time.Second)))
				msg = fmt.Sprintf("%s crashed (back in %.0fs).", name, value)
			case "reset":
				b.Reset()
				b.Start()
				msg = fmt.Sprintf("%s repaired.", name)
			default:
				return fmt.Errorf("unknown chaos action %q", action)
			}
			s.lb.Journal().Add(lb.LevelInfo, "Chaos: %s", msg)
		}
	}
	if !found {
		return fmt.Errorf("unknown backend %q", name)
	}
	return nil
}

func (a *Arena) handleChaos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, _ := strconv.ParseFloat(q.Get("value"), 64)
	if err := a.chaos(q.Get("backend"), q.Get("action"), v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- the scripted Tatkal rush ----

type stage struct {
	At     float64 `json:"at"` // seconds from start
	Title  string  `json:"title"`
	Detail string  `json:"detail"`
	do     func()
}

type scenario struct {
	mu       sync.Mutex
	running  bool
	finished bool
	started  time.Time
	stages   []stage
	cur      int
	cancel   context.CancelFunc
	base     []loadgen.Counters
	baseGen  int
	final    []Score
	peakSrv  []int
}

func (a *Arena) tatkalStages() []stage {
	calm := a.peak / 3
	return []stage{
		{0, "9:59 AM — normal morning", fmt.Sprintf("%.0f req/s per side. Both balancers look identical.", calm),
			func() { a.traffic.Set(calm, "tatkal") }},
		{8, "10:00 AM — Tatkal booking opens", fmt.Sprintf("Traffic jumps to %.0f req/s per side: 75%% of fleet capacity.", a.peak),
			func() { a.traffic.Set(a.peak, "tatkal") }},
		{20, "backend-2 slows down", "It still passes health checks but every request takes +400ms: a gray failure.",
			func() { a.chaos("backend-2", "delay", 400) }},
		{36, "backend-3 crashes", "The process dies. It comes back by itself after 15 seconds.",
			func() { a.chaos("backend-3", "crash", 15) }},
		{56, "backend-2 is fixed", "Ops repair the slow server.",
			func() { a.chaos("backend-2", "reset", 0) }},
		{66, "10:05 AM — rush is over", "Traffic returns to normal.",
			func() { a.traffic.Set(calm, "tatkal") }},
		{76, "Final whistle", "Traffic stops; the scoreboard is frozen.",
			func() { a.traffic.Set(0, "tatkal") }},
	}
}

func (a *Arena) startScenario() {
	sc := &a.scen
	sc.stop()
	// Clean slate: repair everything before the race.
	for _, n := range []string{"backend-1", "backend-2", "backend-3"} {
		a.chaos(n, "reset", 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stages := a.tatkalStages()

	sc.mu.Lock()
	sc.running, sc.finished = true, false
	sc.started = time.Now()
	sc.stages = stages
	sc.cur = -1
	sc.cancel = cancel
	sc.final = nil
	sc.base = nil
	sc.peakSrv = make([]int, len(a.sides))
	sc.mu.Unlock()

	for _, s := range a.sides {
		s.lb.Journal().Add(lb.LevelInfo, "Race started: Tatkal Rush scenario.")
	}
	// Track the most healthy servers each side used, whether or not anyone
	// is watching the race.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.trackPeaks()
			}
		}
	}()
	go func() {
		start := time.Now()
		for i, st := range stages {
			wait := time.Until(start.Add(time.Duration(st.At * float64(time.Second))))
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			st.do()
			sc.mu.Lock()
			sc.cur = i
			if i == 0 {
				sc.base = a.traffic.Counters()
				sc.baseGen = a.traffic.Generation()
			}
			sc.mu.Unlock()
		}
		// Let in-flight requests finish (they time out after 5s at most).
		select {
		case <-ctx.Done():
			return
		case <-time.After(6 * time.Second):
		}
		score := a.score()
		sc.mu.Lock()
		sc.final = score
		sc.running, sc.finished = false, true
		sc.mu.Unlock()
		cancel() // race over: stop the peak tracker
		a.traffic.Stop()
		for i, s := range a.sides {
			if i < len(score) {
				s.lb.Journal().Add(lb.LevelGood, "Race over: %.2f%% of requests succeeded, %.2f%% of bookings went through, p99 %s.",
					score[i].SuccessPct, score[i].BookingPct, fmtMs(score[i].P99))
			}
		}
	}()
}

// runHeadless runs the scenario once and prints the scoreboard.
func (a *Arena) RunHeadless(out string) {
	a.startScenario()
	last := -2
	for {
		time.Sleep(500 * time.Millisecond)
		v := a.view()
		if v.Stage != last && v.Stage >= 0 && v.Stage < len(v.Stages) {
			last = v.Stage
			fmt.Printf("[%4.0fs] %s: %s\n", v.Stages[v.Stage].At, v.Stages[v.Stage].Title, v.Stages[v.Stage].Detail)
		}
		if v.Finished {
			printScore(v.Score)
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					log.Printf("write %s: %v", out, err)
					return
				}
				enc := json.NewEncoder(f)
				enc.SetIndent("", "  ")
				enc.Encode(map[string]any{"classic": v.Score[0], "pulselb": v.Score[1], "peak_rate_per_side": a.peak, "stages": v.Stages})
				f.Close()
				fmt.Printf("wrote %s\n", out)
			}
			return
		}
	}
}

func printScore(sc []Score) {
	if len(sc) < 2 {
		fmt.Println("no score recorded")
		return
	}
	c, p := sc[0], sc[1]
	fmt.Printf("\n%-40s %16s %16s\n", "measured at the client", "classic", "pulselb")
	row := func(label, a, b string) { fmt.Printf("%-40s %16s %16s\n", label, a, b) }
	row("requests that succeeded", fmt.Sprintf("%.2f%%", c.SuccessPct), fmt.Sprintf("%.2f%%", p.SuccessPct))
	row("tatkal bookings completed", fmt.Sprintf("%.2f%%", c.BookingPct), fmt.Sprintf("%.2f%%", p.BookingPct))
	row("failed requests", fmt.Sprint(c.Errors), fmt.Sprint(p.Errors))
	row("politely turned away (503)", fmt.Sprint(c.Shed), fmt.Sprint(p.Shed))
	row("p50 latency (successful)", fmtMs(c.P50), fmtMs(p.P50))
	row("p99 latency (successful)", fmtMs(c.P99), fmtMs(p.P99))
	row("most healthy servers", fmt.Sprint(c.PeakServers), fmt.Sprint(p.PeakServers))
}

func (sc *scenario) stop() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.cancel != nil {
		sc.cancel()
		sc.cancel = nil
	}
	sc.running = false
}

func (a *Arena) handleScenario(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "stop":
		a.scen.stop()
		a.traffic.Set(0, "")
	default:
		a.startScenario()
	}
	w.WriteHeader(http.StatusNoContent)
}

// Score is one side's result, measured from the client's point of view.
type Score struct {
	Sent        uint64  `json:"sent"`
	OK          uint64  `json:"ok"`
	Shed        uint64  `json:"shed"`
	Errors      uint64  `json:"errors"`
	SuccessPct  float64 `json:"success_pct"`
	Bookings    uint64  `json:"bookings"`
	BookingsOK  uint64  `json:"bookings_ok"`
	BookingPct  float64 `json:"booking_pct"`
	P50         float64 `json:"p50"`
	P99         float64 `json:"p99"`
	PeakServers int     `json:"peak_servers"`
}

// score compares the generators' counters against the race's baseline.
func (a *Arena) score() []Score {
	sc := &a.scen
	sc.mu.Lock()
	base, baseGen, peaks := sc.base, sc.baseGen, append([]int(nil), sc.peakSrv...)
	sc.mu.Unlock()
	cur := a.traffic.Counters()
	if cur == nil || base == nil {
		return nil
	}
	if a.traffic.Generation() != baseGen {
		base = make([]loadgen.Counters, len(cur))
	}
	out := make([]Score, len(cur))
	for i := range cur {
		d := cur[i].Sub(base[i])
		s := Score{Sent: d.Sent, OK: d.OK, Shed: d.Shed, Errors: d.Errors,
			P50: ms(d.Lat.Quantile(0.5)), P99: ms(d.Lat.Quantile(0.99))}
		if i < len(peaks) {
			s.PeakServers = peaks[i]
		}
		if d.Sent > 0 {
			s.SuccessPct = 100 * float64(d.OK) / float64(d.Sent)
		}
		for j, r := range a.mix() {
			if strings.HasPrefix(r.Path, "/book") && j < len(d.RouteSent) {
				s.Bookings += d.RouteSent[j]
				s.BookingsOK += d.RouteOK[j]
			}
		}
		if s.Bookings > 0 {
			s.BookingPct = 100 * float64(s.BookingsOK) / float64(s.Bookings)
		}
		out[i] = s
	}
	return out
}

func (a *Arena) mix() []loadgen.Route {
	_, mix := a.traffic.State()
	r, _ := loadgen.ParseMix(mix)
	return r
}

type scenarioView struct {
	Running  bool    `json:"running"`
	Finished bool    `json:"finished"`
	Elapsed  float64 `json:"elapsed"`
	Total    float64 `json:"total"`
	Stage    int     `json:"stage"`
	Stages   []stage `json:"stages"`
	Score    []Score `json:"score"`
}

func (a *Arena) view() scenarioView {
	sc := &a.scen
	sc.mu.Lock()
	v := scenarioView{Running: sc.running, Finished: sc.finished, Stage: sc.cur, Stages: sc.stages}
	if v.Stages == nil {
		v.Stages = a.tatkalStages()
	}
	if len(v.Stages) > 0 {
		v.Total = v.Stages[len(v.Stages)-1].At
	}
	if sc.running {
		v.Elapsed = time.Since(sc.started).Seconds()
	}
	final := sc.final
	sc.mu.Unlock()
	if v.Finished {
		v.Score = final
		v.Elapsed = v.Total
	} else if v.Running {
		v.Score = a.score()
	}
	return v
}

// trackPeaks records the most healthy servers each side has in use.
func (a *Arena) trackPeaks() {
	sc := &a.scen
	for i, s := range a.sides {
		n := 0
		for _, b := range s.lb.Latest().Backends {
			if b.State == lb.StateHealthy || b.State == lb.StateWarming {
				n++
			}
		}
		sc.mu.Lock()
		if sc.running && i < len(sc.peakSrv) && n > sc.peakSrv[i] {
			sc.peakSrv[i] = n
		}
		sc.mu.Unlock()
	}
}

func (a *Arena) handleHistory(w http.ResponseWriter, r *http.Request) {
	out := map[string][]lb.Point{}
	for _, s := range a.sides {
		out[s.Key] = s.lb.History()
	}
	writeJSON(w, out)
}

func (a *Arena) handleStream(w http.ResponseWriter, r *http.Request) {
	since := make([]uint64, len(a.sides))
	lb.StreamSSE(w, r, time.Second, func() any {
		sides := make([]map[string]any, len(a.sides))
		for i, s := range a.sides {
			ev := s.lb.Journal().Since(since[i])
			if len(ev) > 0 {
				since[i] = ev[len(ev)-1].Seq
			}
			sides[i] = map[string]any{"key": s.Key, "label": s.Label, "summary": s.lb.Summary(), "events": ev}
		}
		rate, mix := a.traffic.State()
		return map[string]any{
			"sides":    sides,
			"scenario": a.view(),
			"traffic":  map[string]any{"rate": rate, "mix": mix},
		}
	})
}

func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Pulse-Admin") != "1" {
			http.Error(w, "missing X-Pulse-Admin header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

func fmtMs(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", v/1000)
	}
	return fmt.Sprintf("%.0fms", v)
}
