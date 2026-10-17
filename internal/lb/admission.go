package lb

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Class is a request's priority under overload.
type Class int

const (
	Critical Class = iota // bookings and payments: shed last
	Normal                // regular API calls
	Low                   // browsing and search: shed first
)

var classNames = [...]string{"critical", "normal", "low"}

func (c Class) String() string { return classNames[c] }

// classShare is the fraction of the concurrency limit each class may use.
// When in-flight requests pass 50% of the limit, low-priority requests are
// turned away; past 80%, normal ones too; critical ones can use all of it.
var classShare = [...]float64{Critical: 1.0, Normal: 0.8, Low: 0.5}

// Classifier assigns a priority class from the request path.
type Classifier struct {
	CriticalPrefixes []string
	LowPrefixes      []string
	// TrustHeader honours an X-Priority request header. Off by default:
	// any client could claim to be critical, so only enable it behind a
	// trusted edge that sets the header itself.
	TrustHeader bool
}

// DefaultClassifier fits the Tatkal demo: bookings and payments are critical,
// browsing and search are low priority, everything else is normal.
func DefaultClassifier() Classifier {
	return Classifier{
		CriticalPrefixes: []string{"/book", "/pay", "/checkout", "/api/book", "/api/pay"},
		LowPrefixes:      []string{"/browse", "/search", "/static", "/api/browse", "/api/search"},
	}
}

// Classify returns the request's priority class.
func (c *Classifier) Classify(r *http.Request) Class {
	if c.TrustHeader {
		switch strings.ToLower(r.Header.Get("X-Priority")) {
		case "critical", "high":
			return Critical
		case "normal":
			return Normal
		case "low":
			return Low
		}
	}
	p := r.URL.Path
	for _, pre := range c.CriticalPrefixes {
		if strings.HasPrefix(p, pre) {
			return Critical
		}
	}
	for _, pre := range c.LowPrefixes {
		if strings.HasPrefix(p, pre) {
			return Low
		}
	}
	return Normal
}

// Admission tracks requests in flight and, when shedding is enabled,
// enforces an adaptive concurrency limit with per-class thresholds.
//
// The limit adapts to latency: when the p90 goes over the SLO the limit is
// cut below the current in-flight peak, which immediately starts turning
// away low-priority traffic so the backends' queues drain and critical
// requests stay fast. When latency is comfortably low the limit grows back.
type Admission struct {
	inflight atomic.Int64
	limit    atomic.Int64
	peak     atomic.Int64
	minLimit int64
	maxLimit int64
}

func newAdmission(minLimit, maxLimit int64) *Admission {
	a := &Admission{minLimit: minLimit, maxLimit: maxLimit}
	a.limit.Store(maxLimit)
	return a
}

// Acquire admits a request, or refuses it when enforce is set and the class
// threshold is exceeded. Every admitted request must call Release.
func (a *Admission) Acquire(c Class, enforce bool) bool {
	n := a.inflight.Add(1)
	if enforce && float64(n) > float64(a.limit.Load())*classShare[c] {
		a.inflight.Add(-1)
		return false
	}
	for {
		p := a.peak.Load()
		if n <= p || a.peak.CompareAndSwap(p, n) {
			return true
		}
	}
}

// Release marks an admitted request as finished.
func (a *Admission) Release() { a.inflight.Add(-1) }

// Inflight returns the number of admitted requests in progress.
func (a *Admission) Inflight() int64 { return a.inflight.Load() }

// Limit returns the current concurrency limit.
func (a *Admission) Limit() int64 { return a.limit.Load() }

// adjust runs every control tick with the window's p90 latency.
func (a *Admission) adjust(p90, slo time.Duration, j *Journal) {
	peak := a.peak.Swap(a.inflight.Load())
	lim := a.limit.Load()
	switch {
	case p90 > slo:
		base := lim
		if peak < base {
			base = peak
		}
		nl := int64(float64(base) * 0.8)
		if nl < a.minLimit {
			nl = a.minLimit
		}
		if nl < lim {
			a.limit.Store(nl)
			j.AddLimited("admission-cut", 3*time.Second, LevelWarn,
				"Latency p90 %s is over the %s target, so the concurrency limit drops %d → %d. Low-priority traffic (browse/search) is turned away first so bookings stay fast.",
				fmtDur(p90), fmtDur(slo), lim, nl)
		}
	case p90 < slo/2 && lim < a.maxLimit:
		step := lim / 8
		if step < 8 {
			step = 8
		}
		nl := lim + step
		if nl >= a.maxLimit {
			nl = a.maxLimit
			j.AddLimited("admission-restore", 10*time.Second, LevelGood,
				"Latency is healthy again (p90 %s); the concurrency limit is fully restored and nothing is being shed.", fmtDur(p90))
		}
		a.limit.Store(nl)
	}
}

// retryBudget caps retries to a fraction of normal traffic, so a failing
// backend can't trigger a retry storm that knocks over the healthy ones.
// Each request deposits 0.2 tokens; each retry spends 1, so retries add at
// most ~20% extra load. A floor of about 10 retries per second (refilled by
// the control loop) keeps retries working at low traffic and right after
// startup, when 20% of almost nothing would be no retries at all.
type retryBudget struct{ milli atomic.Int64 }

const (
	budgetDeposit = 200
	budgetCost    = 1000
	budgetCap     = 100 * budgetCost
	budgetFloor   = 10 * budgetCost
)

// refillFloor tops the budget up towards the floor; called every 250ms it
// grants at most 2.5 retries per tick, i.e. ~10 per second.
func (b *retryBudget) refillFloor() {
	if v := b.milli.Load(); v < budgetFloor {
		b.milli.Add(budgetFloor / 4)
	}
}

func (b *retryBudget) deposit() {
	if b.milli.Load() < budgetCap {
		b.milli.Add(budgetDeposit)
	}
}

func (b *retryBudget) withdraw() bool {
	for {
		v := b.milli.Load()
		if v < budgetCost {
			return false
		}
		if b.milli.CompareAndSwap(v, v-budgetCost) {
			return true
		}
	}
}
