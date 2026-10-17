package lb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Spawner launches a new backend and returns its base URL and a function
// that stops it.
type Spawner interface {
	Spawn(name string) (url string, stop func(), err error)
}

// autoscaler keeps the number of healthy backends at a target ("desired")
// size. The target rises when the fleet is overloaded and falls back when
// traffic drops; dead servers are replaced and surplus ones drained.
type autoscaler struct {
	l *LB

	base       int // fleet size when autoscaling was switched on
	desired    int
	hot, cool  int // consecutive overloaded / calm seconds
	short      int // consecutive seconds below target
	rpsAtUp    float64
	lastChange time.Time
	lastSpawn  time.Time
	seq        int
	busy       atomic.Bool
}

func newAutoscaler(l *LB) *autoscaler { return &autoscaler{l: l} }

func (a *autoscaler) step() {
	l := a.l
	if !l.feat.Autoscale.Load() || l.opts.Spawner == nil {
		a.desired = 0
		return
	}
	now := time.Now()
	nowNs := now.UnixNano()
	var healthy []*Backend
	for _, b := range l.pool.All() {
		if b.healthy.Load() && !b.draining.Load() {
			healthy = append(healthy, b)
		}
	}
	if a.desired == 0 {
		a.base = len(l.pool.All())
		a.desired = a.base
		a.lastChange = now
	}

	p := l.Latest()
	slo := ms(l.opts.SLO)
	shedFrac := 0.0
	if p.RPS > 0 {
		shedFrac = p.Shed / p.RPS
	}
	overloaded := p.RPS > 0 && (p.P90 > 0.8*slo || shedFrac > 0.01)
	if overloaded {
		a.hot++
		a.cool = 0
	} else {
		a.hot = 0
		a.cool++
	}

	switch {
	case a.hot >= 3 && a.desired < l.opts.MaxBackends && now.Sub(a.lastChange) > 5*time.Second:
		a.desired++
		a.rpsAtUp = p.RPS
		a.lastChange = now
		a.hot = 0
		l.journal.Add(LevelWarn, "Overloaded for 3s (p90 %s, %.0f%% of requests shed). Raising the target fleet size to %d servers.",
			fmtMs(p.P90), shedFrac*100, a.desired)
	case a.cool >= 30 && a.desired > a.base && p.RPS < 0.6*a.rpsAtUp && now.Sub(a.lastChange) > 20*time.Second:
		a.desired--
		a.lastChange = now
		a.cool = 0
		l.journal.Add(LevelInfo, "Traffic has dropped to %.0f req/s (from %.0f). Lowering the target fleet size to %d servers.",
			p.RPS, a.rpsAtUp, a.desired)
	}

	if len(healthy) < a.desired {
		a.short++
	} else {
		a.short = 0
	}

	// Servers this autoscaler launched that have been dead for 10 health
	// checks are removed rather than left in the pool forever.
	all := l.pool.All()
	for _, b := range all {
		if b.Managed && !b.healthy.Load() && !b.draining.Load() && b.healthStreak.Load() <= -10 {
			go l.drain(b, "it has not answered health checks for 10 seconds")
		}
	}

	switch {
	// Never grow past MaxBackends in total, dead ones included. When the
	// machine itself is overloaded, health checks time out and servers look
	// dead; launching more servers on the same machine would only add load.
	case len(all) >= l.opts.MaxBackends && a.short >= 3:
		l.journal.AddLimited("scale-cap", 15*time.Second, LevelWarn,
			"Only %d of %d target servers are healthy, but the fleet is already at its limit of %d servers, so no more are launched. If every server is struggling at once, the machine itself is probably overloaded.",
			len(healthy), a.desired, l.opts.MaxBackends)
	case a.short >= 3 && !a.busy.Load() && now.Sub(a.lastSpawn) > 3*time.Second:
		a.busy.Store(true)
		a.lastSpawn = now
		a.seq++
		name := fmt.Sprintf("auto-%d", a.seq)
		l.journal.Add(LevelWarn, "Only %d of %d target servers are healthy. Launching %s.", len(healthy), a.desired, name)
		go a.spawn(name)
	case len(healthy) > a.desired && !a.busy.Load() && now.Sub(a.lastChange) > 10*time.Second && now.Sub(a.lastSpawn) > 10*time.Second:
		// Retire the newest managed backend that is fully warmed up.
		var victim *Backend
		for _, b := range healthy {
			if b.Managed && b.Available(nowNs) && (victim == nil || b.warmSince.Load() > victim.warmSince.Load()) {
				victim = b
			}
		}
		if victim != nil {
			a.lastSpawn = now
			go l.drain(victim, fmt.Sprintf("fleet is %d servers, target is %d", len(healthy), a.desired))
		}
	}
}

func (a *autoscaler) spawn(name string) {
	defer a.busy.Store(false)
	l := a.l
	t0 := time.Now()
	url, stop, err := l.opts.Spawner.Spawn(name)
	if err != nil {
		l.journal.Add(LevelCrit, "Could not launch %s: %v", name, err)
		return
	}
	// Wait until it answers health checks before sending it traffic.
	ok := false
	for i := 0; i < 50 && l.ctx.Err() == nil; i++ {
		ctx, cancel := context.WithTimeout(l.ctx, 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/__pulse/health", nil)
		resp, err := l.health.Do(req)
		cancel()
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ok = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		stop()
		l.journal.Add(LevelCrit, "%s never became healthy and was shut down.", name)
		return
	}
	if _, err := l.addBackend(name, url, stop); err != nil {
		stop()
		l.journal.Add(LevelCrit, "Could not add %s: %v", name, err)
		return
	}
	l.journal.Add(LevelGood, "%s is up after %s and joins the rotation with slow start.", name, fmtDur(time.Since(t0)))
}
