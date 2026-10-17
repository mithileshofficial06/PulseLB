package lb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Hop-by-hop headers apply to a single connection and must not be forwarded
// (RFC 9110 section 7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// failurePenalty is the latency recorded for a failed attempt, so a backend
// that fails fast doesn't look fast and attract even more traffic.
const failurePenalty = time.Second

// ServeHTTP proxies one client request to a backend.
func (l *LB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Control endpoints on the backends must never be reachable through the
	// balancer, or any client could crash a server.
	if strings.HasPrefix(r.URL.Path, "/__pulse") {
		http.NotFound(w, r)
		return
	}
	start := time.Now()
	st := l.stats
	st.received.Add(1)
	class := l.opts.Classifier.Classify(r)

	if !l.adm.Acquire(class, l.feat.Shedding.Load()) {
		st.shed.Add(1)
		st.classShed[class].Add(1)
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-Pulse-Shed", class.String())
		http.Error(w, "server busy, please retry shortly", http.StatusServiceUnavailable)
		return
	}
	defer l.adm.Release()

	retries := l.feat.Retries.Load()
	if retries {
		l.budget.deposit()
	}
	canRetry := retries && retryable(r)
	picker := l.picker.Load().p

	var tried [4]*Backend
	n := 0
	for attempt := 0; ; attempt++ {
		pc := pickCtx{now: time.Now().UnixNano(), exclude: tried[:n], slowStart: l.slowStartWindow()}
		b := picker.Pick(l.pool.All(), &pc)
		if b == nil {
			st.noBackend.Add(1)
			http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
			l.finish(start, class, http.StatusServiceUnavailable)
			return
		}
		tried[n] = b
		n++
		last := !canRetry || attempt >= l.opts.MaxRetries || n == len(tried)

		b.inflight.Add(1)
		resp, cancel, rtt, err := l.roundTrip(r, b)
		failed := err != nil || resp.StatusCode >= 500
		l.recordOutcome(b, rtt, failed)

		if failed && !last && l.budget.withdraw() {
			if resp != nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
			}
			cancel()
			b.inflight.Add(-1)
			st.retries.Add(1)
			continue
		}

		if err != nil {
			cancel()
			b.inflight.Add(-1)
			code := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				code = http.StatusGatewayTimeout
			}
			if r.Context().Err() == nil {
				http.Error(w, "upstream error: "+b.Name, code)
			}
			l.finish(start, class, code)
			return
		}

		l.copyResponse(w, resp, b)
		resp.Body.Close()
		cancel()
		b.inflight.Add(-1)
		l.finish(start, class, resp.StatusCode)
		return
	}
}

// retryable reports whether a request can safely be sent twice: an
// idempotent method with no body to replay.
func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.ContentLength == 0
	}
	return false
}

// roundTrip sends one attempt to b. The returned cancel func must be called
// once the response body has been consumed.
func (l *LB) roundTrip(r *http.Request, b *Backend) (*http.Response, context.CancelFunc, time.Duration, error) {
	ctx, cancel := context.WithTimeout(r.Context(), l.opts.AttemptTimeout)
	out := r.Clone(ctx)
	out.RequestURI = ""
	out.URL.Scheme = b.URL.Scheme
	out.URL.Host = b.URL.Host
	if r.ContentLength == 0 {
		out.Body = nil
	}
	removeHopHeaders(out.Header)
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := out.Header.Get("X-Forwarded-For"); prior != "" {
			ip = prior + ", " + ip
		}
		out.Header.Set("X-Forwarded-For", ip)
	}
	out.Header.Set("X-Forwarded-Host", r.Host)
	out.Header.Set("X-Forwarded-Proto", "http")

	t0 := time.Now()
	resp, err := l.transport.RoundTrip(out)
	return resp, cancel, time.Since(t0), err
}

func (l *LB) copyResponse(w http.ResponseWriter, resp *http.Response, b *Backend) {
	removeHopHeaders(resp.Header)
	h := w.Header()
	for k, vv := range resp.Header {
		h[k] = vv
	}
	h.Set("X-Pulse-Backend", b.Name)
	w.WriteHeader(resp.StatusCode)
	buf := l.bufPool.Get().(*[]byte)
	io.CopyBuffer(w, resp.Body, *buf)
	l.bufPool.Put(buf)
}

// recordOutcome updates a backend's latency average and failure tracking.
func (l *LB) recordOutcome(b *Backend, rtt time.Duration, failed bool) {
	now := time.Now().UnixNano()
	b.requests.Add(1)
	b.latency.Record(rtt)
	if !failed {
		b.observe(rtt, now)
		b.consecFails.Store(0)
		return
	}
	b.failures.Add(1)
	if rtt < failurePenalty {
		rtt = failurePenalty
	}
	b.observe(rtt, now)
	if l.feat.Outlier.Load() && b.consecFails.Add(1) >= int32(l.opts.EjectAfter) {
		l.eject(b, now)
	}
}

// eject pulls a backend out of rotation for an exponentially growing time.
func (l *LB) eject(b *Backend, now int64) {
	// Failures from requests that were already in flight when b was
	// ejected arrive afterwards; they don't count as a new ejection.
	if b.ejectedUntil.Load() > now {
		return
	}
	all := l.pool.All()
	ejected := 0
	for _, x := range all {
		if x.ejectedUntil.Load() > now {
			ejected++
		}
	}
	if (ejected+1)*100 > len(all)*l.opts.MaxEjectPct {
		l.journal.AddLimited("eject-cap", 5*time.Second, LevelWarn,
			"%s keeps failing but is NOT ejected: %d of %d backends are already out and ejecting more would overload the rest.",
			b.Name, ejected, len(all))
		return
	}
	cur := b.ejectedUntil.Load()
	if cur > now {
		return
	}
	// 5s, 10s, then 20s at most: long enough to stop hammering a sick
	// server, short enough that a repaired one is back within seconds.
	k := b.ejections.Load()
	if k > 2 {
		k = 2
	}
	d := l.opts.EjectBase << uint(k)
	if !b.ejectedUntil.CompareAndSwap(cur, now+int64(d)) {
		return
	}
	b.ejections.Add(1)
	b.consecFails.Store(0)
	l.journal.Add(LevelCrit,
		"Ejected %s for %s: %d requests in a row failed. Its traffic moves to the other servers instantly, without waiting for the next health check.",
		b.Name, fmtDur(d), l.opts.EjectAfter)
}

func (l *LB) finish(start time.Time, class Class, code int) {
	st := l.stats
	st.latency.Record(time.Since(start))
	if code >= 500 {
		st.errors.Add(1)
		st.classErr[class].Add(1)
	} else {
		st.ok.Add(1)
		st.classOK[class].Add(1)
	}
}
