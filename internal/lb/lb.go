// Package lb is PulseLB: an HTTP load balancer written from scratch.
//
// Requests are routed by a pluggable Picker (round robin, least connections,
// or PulseLB's latency-aware power-of-two-choices). On top of that sit the
// self-healing features, each of which can be switched on or off at runtime:
// passive outlier ejection, slow start, budgeted retries, priority-based load
// shedding and autoscaling. Every decision is explained in a journal.
package lb

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a load balancer. Zero values get sensible defaults.
type Options struct {
	Name      string // label shown on the dashboard, e.g. "PulseLB" or "Classic"
	Algorithm string // "p2c" (default), "rr" or "leastconn"

	// Smart turns on every self-healing feature. A "classic" balancer is
	// round robin with active health checks only.
	Smart bool

	HealthInterval time.Duration // active health check period (1s)
	HealthTimeout  time.Duration // per check (500ms)
	HealthFall     int           // failures before marking down (2)
	HealthRise     int           // passes before marking up (2)

	AttemptTimeout time.Duration // per upstream attempt (3s)
	SLO            time.Duration // latency target driving load shedding (250ms)
	MaxInflight    int64         // concurrency ceiling (4096)
	MaxRetries     int           // extra attempts for idempotent requests (2)
	SlowStart      time.Duration // ramp-up window after a backend joins (8s)

	EjectAfter  int           // consecutive failures before ejection (5)
	EjectBase   time.Duration // first ejection length, doubling each time (5s)
	MaxEjectPct int           // never eject more than this % of backends (50)

	Classifier Classifier

	Spawner     Spawner // starts new backends; nil disables autoscaling
	MaxBackends int     // autoscaler ceiling (6)
}

func (o *Options) defaults() {
	if o.Name == "" {
		o.Name = "PulseLB"
	}
	if o.Algorithm == "" {
		o.Algorithm = "p2c"
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.HealthInterval, time.Second)
	def(&o.HealthTimeout, 500*time.Millisecond)
	def(&o.AttemptTimeout, 3*time.Second)
	def(&o.SLO, 250*time.Millisecond)
	def(&o.SlowStart, 8*time.Second)
	def(&o.EjectBase, 5*time.Second)
	if o.HealthFall == 0 {
		o.HealthFall = 2
	}
	if o.HealthRise == 0 {
		o.HealthRise = 2
	}
	if o.MaxInflight == 0 {
		o.MaxInflight = 4096
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 2
	}
	if o.EjectAfter == 0 {
		o.EjectAfter = 5
	}
	if o.MaxEjectPct == 0 {
		o.MaxEjectPct = 50
	}
	if o.MaxBackends == 0 {
		o.MaxBackends = 6
	}
	if o.Classifier.CriticalPrefixes == nil && o.Classifier.LowPrefixes == nil {
		o.Classifier = DefaultClassifier()
	}
}

// Features are the self-healing behaviours, switchable at runtime.
type Features struct {
	Outlier   atomic.Bool // eject backends after consecutive failures
	SlowStart atomic.Bool // ramp traffic up gradually to new/recovered backends
	Retries   atomic.Bool // retry idempotent requests on another backend
	Shedding  atomic.Bool // priority-based load shedding
	Autoscale atomic.Bool // launch/retire backends to match load
}

// FeatureNames lists the feature switches in display order.
var FeatureNames = []string{"outlier", "slowstart", "retries", "shedding", "autoscale"}

func (f *Features) get(name string) *atomic.Bool {
	switch name {
	case "outlier":
		return &f.Outlier
	case "slowstart":
		return &f.SlowStart
	case "retries":
		return &f.Retries
	case "shedding":
		return &f.Shedding
	case "autoscale":
		return &f.Autoscale
	}
	return nil
}

type pickerBox struct{ p Picker }

// LB is a running load balancer. It implements http.Handler.
type LB struct {
	opts      Options
	pool      Pool
	picker    atomic.Pointer[pickerBox]
	feat      Features
	adm       *Admission
	budget    retryBudget
	stats     *Stats
	journal   *Journal
	transport *http.Transport
	health    *http.Client
	bufPool   sync.Pool
	started   time.Time

	scaler *autoscaler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a load balancer. Add backends, then call Start.
func New(opts Options) *LB {
	opts.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	l := &LB{
		opts:    opts,
		adm:     newAdmission(16, opts.MaxInflight),
		stats:   newStats(),
		journal: newJournal(),
		ctx:     ctx,
		cancel:  cancel,
		started: time.Now(),
		transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// Keep plenty of idle connections per backend. Go's default of 2
			// would open a new TCP connection for most requests, and at 10k
			// req/s Windows runs out of ephemeral ports within seconds.
			MaxIdleConns:        0,
			MaxIdleConnsPerHost: 4096,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		health: &http.Client{
			Timeout: opts.HealthTimeout,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConnsPerHost: 2,
				DisableCompression:  true,
			},
		},
	}
	l.bufPool.New = func() any { b := make([]byte, 32*1024); return &b }
	l.picker.Store(&pickerBox{NewPicker(opts.Algorithm)})
	if opts.Smart {
		l.feat.Outlier.Store(true)
		l.feat.SlowStart.Store(true)
		l.feat.Retries.Store(true)
		l.feat.Shedding.Store(true)
		l.feat.Autoscale.Store(opts.Spawner != nil)
	}
	l.scaler = newAutoscaler(l)
	return l
}

// Name returns the balancer's display name.
func (l *LB) Name() string { return l.opts.Name }

// Journal returns the decision journal.
func (l *LB) Journal() *Journal { return l.journal }

// Pool returns the backend pool.
func (l *LB) Pool() *Pool { return &l.pool }

// AddBackend registers an upstream server by base URL.
func (l *LB) AddBackend(name, rawURL string) (*Backend, error) {
	return l.addBackend(name, rawURL, nil)
}

// addBackend registers a backend; a non-nil stop marks it as managed by the
// autoscaler. Both are set before it is published to the pool.
func (l *LB) addBackend(name, rawURL string, stop func()) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("backend URL must look like http://host:port, got %q", rawURL)
	}
	if name == "" {
		name = u.Host
	}
	if l.pool.Get(name) != nil {
		return nil, fmt.Errorf("backend %q already exists", name)
	}
	b := newBackend(name, u, time.Now().UnixNano())
	b.Managed, b.stop = stop != nil, stop
	b.resetEWMA(l.medianEWMA(), time.Now().UnixNano())
	l.pool.add(b)
	return b, nil
}

// RemoveBackend drains a backend and removes it from the pool.
func (l *LB) RemoveBackend(name string) bool {
	b := l.pool.Get(name)
	if b == nil {
		return false
	}
	go l.drain(b, "removed by operator")
	return true
}

// SetAlgorithm switches the routing algorithm at runtime.
func (l *LB) SetAlgorithm(name string) string {
	p := NewPicker(name)
	l.picker.Store(&pickerBox{p})
	l.journal.Add(LevelInfo, "Routing algorithm switched to %s.", algoLabel(p.Name()))
	return p.Name()
}

// Algorithm returns the active algorithm name.
func (l *LB) Algorithm() string { return l.picker.Load().p.Name() }

// SetFeature turns a self-healing feature on or off.
func (l *LB) SetFeature(name string, on bool) error {
	f := l.feat.get(name)
	if f == nil {
		return fmt.Errorf("unknown feature %q", name)
	}
	if name == "autoscale" && on && l.opts.Spawner == nil {
		return fmt.Errorf("autoscale needs a spawner (run with -demo or -spawn)")
	}
	f.Store(on)
	state := "off"
	if on {
		state = "on"
	}
	l.journal.Add(LevelInfo, "Feature %s switched %s.", name, state)
	return nil
}

// FeatureStates returns every feature switch.
func (l *LB) FeatureStates() map[string]bool {
	m := make(map[string]bool, len(FeatureNames))
	for _, n := range FeatureNames {
		m[n] = l.feat.get(n).Load()
	}
	return m
}

// Start launches the background loops: health checks, control and stats.
func (l *LB) Start() {
	l.journal.Add(LevelInfo, "%s started with %d backends, routing by %s.",
		l.opts.Name, len(l.pool.All()), algoLabel(l.Algorithm()))
	l.loop(l.opts.HealthInterval, l.checkAll)
	l.loop(250*time.Millisecond, l.control)
	l.loop(time.Second, l.collect)
	l.loop(time.Second, l.scaler.step)
}

// Close stops background loops and any backends the autoscaler launched.
func (l *LB) Close() {
	l.cancel()
	l.wg.Wait()
	for _, b := range l.pool.All() {
		if b.Managed && b.stop != nil {
			b.stop()
		}
	}
	l.transport.CloseIdleConnections()
}

func (l *LB) loop(every time.Duration, fn func()) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-t.C:
				fn()
			}
		}
	}()
}

func (l *LB) slowStartWindow() time.Duration {
	if l.feat.SlowStart.Load() {
		return l.opts.SlowStart
	}
	return 0
}

// markAvailable is called when a backend (re)joins the rotation.
func (l *LB) markAvailable(b *Backend) {
	now := time.Now().UnixNano()
	b.consecFails.Store(0)
	b.warmSince.Store(now)
	b.resetEWMA(l.medianEWMA(), now)
}

// medianEWMA is the median latency average of the available backends, used
// as the starting estimate for a backend that just joined.
func (l *LB) medianEWMA() time.Duration {
	now := time.Now().UnixNano()
	var vals []time.Duration
	for _, b := range l.pool.All() {
		if b.Available(now) {
			if e := b.EWMA(); e > 0 {
				vals = append(vals, e)
			}
		}
	}
	if len(vals) == 0 {
		return 0
	}
	for i := 1; i < len(vals); i++ {
		for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
			vals[j], vals[j-1] = vals[j-1], vals[j]
		}
	}
	return vals[len(vals)/2]
}

func algoLabel(name string) string {
	switch name {
	case "rr":
		return "round robin"
	case "leastconn":
		return "least connections"
	default:
		return "power-of-two-choices over latency (P2C-EWMA)"
	}
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d >= 10*time.Millisecond:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fms", float64(d)/1e6)
	}
}
