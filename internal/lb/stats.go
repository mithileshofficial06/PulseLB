package lb

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/hist"
)

// Stats holds the balancer's counters and per-second history.
type Stats struct {
	received  atomic.Uint64
	ok        atomic.Uint64
	errors    atomic.Uint64
	shed      atomic.Uint64
	retries   atomic.Uint64
	noBackend atomic.Uint64
	classOK   [3]atomic.Uint64
	classShed [3]atomic.Uint64
	classErr  [3]atomic.Uint64
	latency   hist.Histogram // end-to-end, admitted requests only

	ctrlPrev hist.Snapshot // owned by the control loop

	mu      sync.RWMutex
	history []Point
	latest  Point

	// owned by the collect loop
	prevT    time.Time
	prev     counters
	prevLat  hist.Snapshot
	prevBack map[*Backend]uint64
	prevFail map[*Backend]uint64
	flagged  map[*Backend]bool
}

type counters struct {
	received, ok, errors, shed, retries uint64
	classOK, classShed, classErr        [3]uint64
}

const historySize = 300

func newStats() *Stats {
	return &Stats{
		prevT:    time.Now(),
		prevBack: make(map[*Backend]uint64),
		prevFail: make(map[*Backend]uint64),
		flagged:  make(map[*Backend]bool),
	}
}

func (s *Stats) counters() counters {
	c := counters{
		received: s.received.Load(),
		ok:       s.ok.Load(),
		errors:   s.errors.Load(),
		shed:     s.shed.Load(),
		retries:  s.retries.Load(),
	}
	for i := 0; i < 3; i++ {
		c.classOK[i] = s.classOK[i].Load()
		c.classShed[i] = s.classShed[i].Load()
		c.classErr[i] = s.classErr[i].Load()
	}
	return c
}

// controlWindow returns latency observations since the previous call.
func (s *Stats) controlWindow(h *hist.Histogram) hist.Snapshot {
	cur := h.Snapshot()
	w := cur.Sub(s.ctrlPrev)
	s.ctrlPrev = cur
	return w
}

// Point is one second of balancer activity. Rates are per second and
// latencies are in milliseconds.
type Point struct {
	T        int64          `json:"t"`
	RPS      float64        `json:"rps"`
	OK       float64        `json:"ok"`
	Errors   float64        `json:"errors"`
	Shed     float64        `json:"shed"`
	Retries  float64        `json:"retries"`
	P50      float64        `json:"p50"`
	P90      float64        `json:"p90"`
	P99      float64        `json:"p99"`
	Inflight int64          `json:"inflight"`
	Limit    int64          `json:"limit"`
	ClassOK  [3]float64     `json:"class_ok"`
	ClassShd [3]float64     `json:"class_shed"`
	ClassErr [3]float64     `json:"class_err"`
	Backends []BackendPoint `json:"backends"`
}

// BackendPoint is one backend's activity during a Point.
type BackendPoint struct {
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	State    State   `json:"state"`
	RPS      float64 `json:"rps"`
	Share    float64 `json:"share"` // fraction of all backend traffic, 0..1
	ErrRate  float64 `json:"err_rate"`
	Inflight int64   `json:"inflight"`
	EWMA     float64 `json:"ewma"` // ms
	Managed  bool    `json:"managed"`
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// collect runs every second: it builds a Point, stores it in the history,
// and explains any notable change in how traffic is being split.
func (l *LB) collect() {
	s := l.stats
	now := time.Now()
	dt := now.Sub(s.prevT).Seconds()
	if dt <= 0 {
		return
	}
	c := s.counters()
	lat := s.latency.Snapshot()
	win := lat.Sub(s.prevLat)
	rate := func(cur, prev uint64) float64 { return float64(cur-prev) / dt }

	p := Point{
		T:        now.UnixMilli(),
		RPS:      rate(c.received, s.prev.received),
		OK:       rate(c.ok, s.prev.ok),
		Errors:   rate(c.errors, s.prev.errors),
		Shed:     rate(c.shed, s.prev.shed),
		Retries:  rate(c.retries, s.prev.retries),
		P50:      ms(win.Quantile(0.50)),
		P90:      ms(win.Quantile(0.90)),
		P99:      ms(win.Quantile(0.99)),
		Inflight: l.adm.Inflight(),
	}
	if l.feat.Shedding.Load() {
		p.Limit = l.adm.Limit()
	}
	for i := 0; i < 3; i++ {
		p.ClassOK[i] = rate(c.classOK[i], s.prev.classOK[i])
		p.ClassShd[i] = rate(c.classShed[i], s.prev.classShed[i])
		p.ClassErr[i] = rate(c.classErr[i], s.prev.classErr[i])
	}

	nowNs := now.UnixNano()
	ss := l.slowStartWindow()
	all := l.pool.All()
	var total float64
	for _, b := range all {
		req, fail := b.requests.Load(), b.failures.Load()
		bp := BackendPoint{
			Name:     b.Name,
			URL:      b.URL.String(),
			State:    b.State(nowNs, ss),
			RPS:      rate(req, s.prevBack[b]),
			Inflight: b.inflight.Load(),
			EWMA:     ms(b.EWMA()),
			Managed:  b.Managed,
		}
		if bp.RPS > 0 {
			bp.ErrRate = rate(fail, s.prevFail[b]) / bp.RPS
		}
		total += bp.RPS
		s.prevBack[b], s.prevFail[b] = req, fail
		p.Backends = append(p.Backends, bp)
	}
	if total > 0 {
		for i := range p.Backends {
			p.Backends[i].Share = p.Backends[i].RPS / total
		}
	}
	for b := range s.prevBack {
		if l.pool.Get(b.Name) != b {
			delete(s.prevBack, b)
			delete(s.prevFail, b)
			delete(s.flagged, b)
		}
	}

	s.prevT, s.prev, s.prevLat = now, c, lat
	s.mu.Lock()
	s.latest = p
	s.history = append(s.history, p)
	if len(s.history) > historySize {
		s.history = append(s.history[:0:0], s.history[len(s.history)-historySize:]...)
	}
	s.mu.Unlock()

	if p.RPS >= 20 {
		l.explain(p)
	}
}

// explain turns the traffic split into plain-English journal entries.
func (l *LB) explain(p Point) {
	var live []BackendPoint
	for _, b := range p.Backends {
		if b.State == StateHealthy || b.State == StateWarming {
			live = append(live, b)
		}
	}
	if len(live) < 2 {
		return
	}
	fair := 1 / float64(len(live))
	algo := l.Algorithm()
	for _, b := range live {
		var others []float64
		for _, o := range live {
			if o.Name != b.Name && o.EWMA > 0 {
				others = append(others, o.EWMA)
			}
		}
		if len(others) == 0 || b.EWMA <= 0 {
			continue
		}
		ref := median(others)
		ratio := b.EWMA / ref
		bk := l.pool.Get(b.Name)
		if bk == nil {
			continue
		}
		switch {
		case algo == "rr" && ratio > 3 && b.Share > 0.8*fair:
			l.journal.AddLimited("rr-blind-"+b.Name, 6*time.Second, LevelWarn,
				"Round robin is still sending %.0f%% of traffic to %s even though it answers in %s vs %s for the others (%.0f× slower). Every request it gets is a slow one.",
				b.Share*100, b.Name, fmtMs(b.EWMA), fmtMs(ref), ratio)
		case algo != "rr" && ratio > 2 && b.Share < 0.5*fair && !l.stats.flagged[bk]:
			l.stats.flagged[bk] = true
			l.journal.Add(LevelWarn,
				"Sending only %.0f%% of traffic to %s instead of ~%.0f%%: it answers in %s vs %s for the others (%.0f× slower). The fast servers pick up the slack.",
				b.Share*100, b.Name, fair*100, fmtMs(b.EWMA), fmtMs(ref), ratio)
		case l.stats.flagged[bk] && b.Share > 0.7*fair && ratio < 1.5:
			delete(l.stats.flagged, bk)
			l.journal.Add(LevelGood, "%s is fast again (%s), so it has earned back %.0f%% of traffic.",
				b.Name, fmtMs(b.EWMA), b.Share*100)
		}
	}
}

func median(v []float64) float64 {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	return v[len(v)/2]
}

func fmtMs(v float64) string { return fmtDur(time.Duration(v * 1e6)) }

// Latest returns the most recent Point.
func (l *LB) Latest() Point {
	l.stats.mu.RLock()
	defer l.stats.mu.RUnlock()
	return l.stats.latest
}

// History returns up to the last five minutes of Points.
func (l *LB) History() []Point {
	l.stats.mu.RLock()
	defer l.stats.mu.RUnlock()
	return append([]Point(nil), l.stats.history...)
}

// Totals are counters since start.
type Totals struct {
	Received  uint64            `json:"received"`
	OK        uint64            `json:"ok"`
	Errors    uint64            `json:"errors"`
	Shed      uint64            `json:"shed"`
	Retries   uint64            `json:"retries"`
	NoBackend uint64            `json:"no_backend"`
	ClassOK   map[string]uint64 `json:"class_ok"`
	ClassShed map[string]uint64 `json:"class_shed"`
	ClassErr  map[string]uint64 `json:"class_err"`
	P50       float64           `json:"p50"`
	P99       float64           `json:"p99"`
	P999      float64           `json:"p999"`
}

// Totals returns all-time counters and latency quantiles.
func (l *LB) Totals() Totals {
	s := l.stats
	c := s.counters()
	lat := s.latency.Snapshot()
	t := Totals{
		Received: c.received, OK: c.ok, Errors: c.errors, Shed: c.shed,
		Retries: c.retries, NoBackend: s.noBackend.Load(),
		ClassOK: map[string]uint64{}, ClassShed: map[string]uint64{}, ClassErr: map[string]uint64{},
		P50: ms(lat.Quantile(0.5)), P99: ms(lat.Quantile(0.99)), P999: ms(lat.Quantile(0.999)),
	}
	for i := 0; i < 3; i++ {
		t.ClassOK[classNames[i]] = c.classOK[i]
		t.ClassShed[classNames[i]] = c.classShed[i]
		t.ClassErr[classNames[i]] = c.classErr[i]
	}
	return t
}

// Summary is everything the dashboard needs in one object.
type Summary struct {
	Name      string          `json:"name"`
	Algorithm string          `json:"algorithm"`
	Features  map[string]bool `json:"features"`
	SLO       float64         `json:"slo"`
	Uptime    float64         `json:"uptime"`
	CanScale  bool            `json:"can_scale"`
	Totals    Totals          `json:"totals"`
	Point     Point           `json:"point"`
}

// Summary returns the current state of the balancer.
func (l *LB) Summary() Summary {
	return Summary{
		Name:      l.opts.Name,
		Algorithm: l.Algorithm(),
		Features:  l.FeatureStates(),
		SLO:       ms(l.opts.SLO),
		Uptime:    time.Since(l.started).Seconds(),
		CanScale:  l.opts.Spawner != nil,
		Totals:    l.Totals(),
		Point:     l.Latest(),
	}
}
