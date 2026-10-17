package loadgen

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
)

// Controller drives identical traffic at one or more targets and lets the
// dashboard change the rate and mix live. With two targets (the arena) both
// receive the same rate and mix, so the comparison is fair.
type Controller struct {
	targets []string

	mu     sync.Mutex
	gens   []*Generator
	cancel context.CancelFunc
	wg     sync.WaitGroup
	rate   float64
	mix    string
	gen    int
}

// NewController creates an idle controller.
func NewController(targets ...string) *Controller {
	return &Controller{targets: targets, mix: "tatkal"}
}

// Set changes the rate and mix. A rate of 0 pauses traffic.
func (c *Controller) Set(rate float64, mix string) error {
	if mix == "" {
		mix = c.mix
	}
	routes, err := ParseMix(mix)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rate < 0 {
		rate = 0
	}
	// Same mix: just change the rate, keeping the generators (and their
	// cumulative counters) alive. Rate 0 pauses traffic.
	if c.gens != nil && mix == c.mix {
		for _, g := range c.gens {
			g.SetRate(rate)
		}
		c.rate = rate
		return nil
	}
	if rate == 0 {
		c.stopLocked()
		c.rate, c.mix = 0, mix
		return nil
	}
	c.stopLocked()
	c.gen++
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	for _, t := range c.targets {
		g := New(Config{Target: t, Rate: rate, Mix: routes})
		c.gens = append(c.gens, g)
		c.wg.Add(1)
		go func() { defer c.wg.Done(); g.Run(ctx) }()
	}
	c.rate, c.mix = rate, mix
	return nil
}

func (c *Controller) stopLocked() {
	if c.cancel != nil {
		c.cancel()
		c.wg.Wait()
	}
	c.gens, c.cancel = nil, nil
}

// Stop ends all traffic and discards the generators.
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	c.rate = 0
}

// State returns the current rate and mix.
func (c *Controller) State() (float64, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate, c.mix
}

// Counters returns each target's totals, in target order, or nil when idle.
func (c *Controller) Counters() []Counters {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens == nil {
		return nil
	}
	out := make([]Counters, len(c.gens))
	for i, g := range c.gens {
		out[i] = g.Counters()
	}
	return out
}

// Generation increases every time the generators are recreated, so callers
// can tell when cumulative counters have restarted from zero.
func (c *Controller) Generation() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Dropped returns how many requests the generators could not send.
func (c *Controller) Dropped() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n uint64
	for _, g := range c.gens {
		n += g.dropped.Load()
	}
	return n
}

// Handler serves GET/POST /api/traffic?rate=10000&mix=tatkal.
func (c *Controller) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("X-Pulse-Admin") != "1" {
				http.Error(w, "missing X-Pulse-Admin header", http.StatusForbidden)
				return
			}
			rate, err := strconv.ParseFloat(r.URL.Query().Get("rate"), 64)
			if err != nil || rate < 0 || rate > 50000 {
				http.Error(w, "rate must be 0..50000", http.StatusBadRequest)
				return
			}
			if err := c.Set(rate, r.URL.Query().Get("mix")); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		rate, mix := c.State()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"rate": rate, "mix": mix, "dropped": c.Dropped()})
	})
}
