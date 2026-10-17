package lb

import (
	"context"
	"io"
	"net/http"
	"time"
)

// checkAll runs one active health check against every backend in parallel.
func (l *LB) checkAll() {
	for _, b := range l.pool.All() {
		if b.draining.Load() {
			continue
		}
		go l.check(b)
	}
}

func (l *LB) check(b *Backend) {
	ctx, cancel := context.WithTimeout(l.ctx, l.opts.HealthTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.String()+"/__pulse/health", nil)
	resp, err := l.health.Do(req)
	ok := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if l.ctx.Err() != nil {
		return
	}

	if ok {
		s := b.healthStreak.Load()
		if s < 0 {
			s = 0
		}
		s++
		b.healthStreak.Store(s)
		if !b.healthy.Load() && int(s) >= l.opts.HealthRise {
			l.markAvailable(b)
			b.healthy.Store(true)
			if l.feat.SlowStart.Load() {
				l.journal.Add(LevelGood, "%s passed %d health checks in a row and is back in rotation, ramping up over %s (slow start).",
					b.Name, s, fmtDur(l.opts.SlowStart))
			} else {
				l.journal.Add(LevelGood, "%s passed %d health checks in a row and is back in rotation at full share.", b.Name, s)
			}
		}
		return
	}

	s := b.healthStreak.Load()
	if s > 0 {
		s = 0
	}
	s--
	b.healthStreak.Store(s)
	if b.healthy.Load() && int(-s) >= l.opts.HealthFall {
		b.healthy.Store(false)
		reason := "health endpoint did not answer"
		if err == nil {
			reason = "health endpoint returned " + resp.Status
		}
		l.journal.Add(LevelCrit, "%s failed %d health checks in a row (%s) and is out of rotation.", b.Name, -s, reason)
	}
}

// control runs every 250ms: returns ejected backends whose time is up and
// adapts the load-shedding limit.
func (l *LB) control() {
	l.budget.refillFloor()
	now := time.Now().UnixNano()
	for _, b := range l.pool.All() {
		if u := b.ejectedUntil.Load(); u != 0 && u <= now && b.ejectedUntil.CompareAndSwap(u, 0) {
			l.markAvailable(b)
			l.journal.Add(LevelInfo, "%s's ejection is over. It gets a small share of traffic first and earns the rest back only if it stays fast.", b.Name)
		}
	}

	win := l.stats.controlWindow(&l.stats.latency)
	if l.feat.Shedding.Load() {
		if win.Total > 0 {
			l.adm.adjust(win.Quantile(0.90), l.opts.SLO, l.journal)
		}
	} else if l.adm.Limit() != l.opts.MaxInflight {
		l.adm.limit.Store(l.opts.MaxInflight)
	}
}

// drain stops routing to b, waits for its in-flight requests to finish,
// then removes it and stops it if the autoscaler started it.
func (l *LB) drain(b *Backend, why string) {
	if !b.draining.CompareAndSwap(false, true) {
		return
	}
	l.journal.Add(LevelInfo, "Draining %s (%s): no new requests, waiting for in-flight ones to finish.", b.Name, why)
	deadline := time.Now().Add(10 * time.Second)
	for b.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	l.pool.remove(b)
	if b.Managed && b.stop != nil {
		b.stop()
	}
	l.journal.Add(LevelInfo, "%s removed from the pool.", b.Name)
}
