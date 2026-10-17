package lb

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstream is a test backend that counts hits and can be told to fail or
// slow down.
type upstream struct {
	*httptest.Server
	name  string
	hits  atomic.Int64
	fail  atomic.Bool
	delay atomic.Int64
	last  atomic.Value // *http.Request headers of the last request
}

func newUpstream(t *testing.T, name string) *upstream {
	u := &upstream{name: name}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__pulse/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		u.hits.Add(1)
		u.last.Store(r.Header.Clone())
		if d := u.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		if u.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Connection", "keep-alive, X-Secret-Hop")
		w.Header().Set("X-Secret-Hop", "should not leak")
		fmt.Fprint(w, u.name)
	}))
	t.Cleanup(u.Close)
	return u
}

func newTestLB(t *testing.T, opts Options, ups ...*upstream) *LB {
	l := New(opts)
	for _, u := range ups {
		if _, err := l.AddBackend(u.name, u.URL); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(l.Close)
	return l
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoundRobinSplitsEvenly(t *testing.T) {
	a, b, c := newUpstream(t, "a"), newUpstream(t, "b"), newUpstream(t, "c")
	l := newTestLB(t, Options{Algorithm: "rr"}, a, b, c)
	for i := 0; i < 300; i++ {
		if rec := get(t, l, "/x"); rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	for _, u := range []*upstream{a, b, c} {
		if n := u.hits.Load(); n != 100 {
			t.Errorf("%s got %d requests, want 100", u.name, n)
		}
	}
}

func TestP2CStarvesSlowBackend(t *testing.T) {
	fast1, fast2, slow := newUpstream(t, "fast1"), newUpstream(t, "fast2"), newUpstream(t, "slow")
	slow.delay.Store(int64(40 * time.Millisecond))
	l := newTestLB(t, Options{Algorithm: "p2c"}, fast1, fast2, slow)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := 0; i < 800; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			get(t, l, "/x")
		}()
	}
	wg.Wait()
	total := fast1.hits.Load() + fast2.hits.Load() + slow.hits.Load()
	share := float64(slow.hits.Load()) / float64(total)
	if share > 0.10 {
		t.Errorf("slow backend got %.0f%% of traffic, want < 10%%", share*100)
	}
	t.Logf("slow backend share: %.1f%%", share*100)
}

func TestRetryMovesToAnotherBackend(t *testing.T) {
	bad, good := newUpstream(t, "bad"), newUpstream(t, "good")
	bad.fail.Store(true)
	l := newTestLB(t, Options{Algorithm: "rr", Smart: true}, bad, good)
	for i := 0; i < 20; i++ {
		rec := get(t, l, "/x")
		if rec.Code != 200 || rec.Body.String() != "good" {
			t.Fatalf("request %d: %d %q, want 200 from good", i, rec.Code, rec.Body.String())
		}
	}
}

func TestNoRetryForPost(t *testing.T) {
	bad := newUpstream(t, "bad")
	bad.fail.Store(true)
	good := newUpstream(t, "good")
	l := newTestLB(t, Options{Algorithm: "rr", Smart: true}, bad, good)
	l.feat.Outlier.Store(false)
	failures := 0
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/book", strings.NewReader(`{"seat":1}`))
		rec := httptest.NewRecorder()
		l.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			failures++
		}
	}
	if bad.hits.Load() == 0 || failures == 0 {
		t.Fatalf("expected POSTs to bad backend to fail without retry; hits=%d failures=%d", bad.hits.Load(), failures)
	}
	if int64(failures) != bad.hits.Load() {
		t.Errorf("every POST that reached the bad backend should fail once: hits=%d failures=%d", bad.hits.Load(), failures)
	}
}

func TestOutlierEjection(t *testing.T) {
	bad, good := newUpstream(t, "bad"), newUpstream(t, "good")
	bad.fail.Store(true)
	l := newTestLB(t, Options{Algorithm: "rr", Smart: true}, bad, good)
	l.feat.Retries.Store(false) // see the failures directly
	for i := 0; i < 20; i++ {
		get(t, l, "/x")
	}
	b := l.pool.Get("bad")
	if b.ejectedUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("bad backend was not ejected")
	}
	before := bad.hits.Load()
	for i := 0; i < 20; i++ {
		if rec := get(t, l, "/x"); rec.Code != 200 {
			t.Fatalf("status %d after ejection", rec.Code)
		}
	}
	if bad.hits.Load() != before {
		t.Errorf("ejected backend still received %d requests", bad.hits.Load()-before)
	}
}

func TestOutlierEjectionNeverEjectsEverything(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	a.fail.Store(true)
	b.fail.Store(true)
	l := newTestLB(t, Options{Algorithm: "rr", Smart: true}, a, b)
	l.feat.Retries.Store(false)
	for i := 0; i < 50; i++ {
		get(t, l, "/x")
	}
	now := time.Now().UnixNano()
	ejected := 0
	for _, x := range l.pool.All() {
		if x.ejectedUntil.Load() > now {
			ejected++
		}
	}
	if ejected > 1 {
		t.Errorf("%d of 2 backends ejected; the 50%% cap should keep one in rotation", ejected)
	}
}

func TestHeadersAndControlPaths(t *testing.T) {
	u := newUpstream(t, "a")
	l := newTestLB(t, Options{}, u)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Drop-Me", "1")
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, req)

	got := u.last.Load().(http.Header)
	if got.Get("X-Forwarded-For") != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q", got.Get("X-Forwarded-For"))
	}
	if got.Get("X-Drop-Me") != "" {
		t.Error("header named in Connection was forwarded upstream")
	}
	if rec.Header().Get("X-Secret-Hop") != "" {
		t.Error("hop-by-hop response header leaked to the client")
	}
	if rec.Header().Get("X-Pulse-Backend") != "a" {
		t.Errorf("X-Pulse-Backend = %q", rec.Header().Get("X-Pulse-Backend"))
	}

	before := u.hits.Load()
	for _, p := range []string{"/__pulse/chaos", "/__pulse/health"} {
		if rec := get(t, l, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s through the balancer: status %d, want 404", p, rec.Code)
		}
	}
	if u.hits.Load() != before {
		t.Error("control path reached the backend")
	}
}

func TestNoBackendsAvailable(t *testing.T) {
	u := newUpstream(t, "a")
	l := newTestLB(t, Options{}, u)
	l.pool.Get("a").healthy.Store(false)
	if rec := get(t, l, "/x"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestAdmissionShedsLowPriorityFirst(t *testing.T) {
	a := newAdmission(1, 10)
	admitted := map[Class]int{}
	// Fill up with low-priority requests until refused.
	for i := 0; i < 20; i++ {
		if a.Acquire(Low, true) {
			admitted[Low]++
		}
	}
	if admitted[Low] != 5 {
		t.Errorf("low admitted %d, want 5 (50%% of limit 10)", admitted[Low])
	}
	for i := 0; i < 20; i++ {
		if a.Acquire(Critical, true) {
			admitted[Critical]++
		}
	}
	if admitted[Critical] != 5 {
		t.Errorf("critical admitted %d on top, want 5 (up to the full limit)", admitted[Critical])
	}
	if a.Acquire(Normal, true) {
		t.Error("normal admitted above 80% of the limit")
	}
	if !a.Acquire(Low, false) {
		t.Error("enforce=false must always admit")
	}
}

func TestAdmissionLimitAdapts(t *testing.T) {
	a := newAdmission(16, 1000)
	j := newJournal()
	for i := 0; i < 200; i++ {
		a.Acquire(Normal, false)
	}
	a.adjust(400*time.Millisecond, 250*time.Millisecond, j)
	if lim := a.Limit(); lim > 200 {
		t.Fatalf("limit %d after latency over SLO, want below the in-flight peak of 200", lim)
	}
	for i := 0; i < 200; i++ {
		a.Release()
	}
	cut := a.Limit()
	for i := 0; i < 100; i++ {
		a.adjust(10*time.Millisecond, 250*time.Millisecond, j)
	}
	if a.Limit() != 1000 || cut >= 1000 {
		t.Errorf("limit %d after recovery, want back to 1000", a.Limit())
	}
}

func TestClassifier(t *testing.T) {
	c := DefaultClassifier()
	cases := map[string]Class{"/book/tatkal": Critical, "/pay": Critical, "/api/trains": Normal, "/browse/offers": Low, "/": Normal}
	for path, want := range cases {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Priority", "critical")
		if got := c.Classify(r); got != want {
			t.Errorf("%s: %s, want %s (the header must be ignored by default)", path, got, want)
		}
	}
	c.TrustHeader = true
	r := httptest.NewRequest(http.MethodGet, "/browse", nil)
	r.Header.Set("X-Priority", "critical")
	if c.Classify(r) != Critical {
		t.Error("trusted header was ignored")
	}
}

func TestRetryBudget(t *testing.T) {
	var b retryBudget
	if b.withdraw() {
		t.Fatal("empty budget allowed a retry")
	}
	for i := 0; i < 10; i++ {
		b.deposit()
	}
	n := 0
	for b.withdraw() {
		n++
	}
	if n != 2 {
		t.Errorf("10 requests funded %d retries, want 2 (20%%)", n)
	}
	for i := 0; i < 4; i++ {
		b.refillFloor()
	}
	n = 0
	for b.withdraw() {
		n++
	}
	if n != 10 {
		t.Errorf("one second of floor refills funded %d retries, want 10", n)
	}
}

func TestRetryWorksRightAfterStartup(t *testing.T) {
	bad, good := newUpstream(t, "bad"), newUpstream(t, "good")
	bad.fail.Store(true)
	l := newTestLB(t, Options{Algorithm: "rr", Smart: true}, bad, good)
	l.feat.Outlier.Store(false)
	// The very first request must already be able to retry.
	if rec := get(t, l, "/x"); rec.Code != 200 {
		t.Fatalf("first request after startup: %d", rec.Code)
	}
}

func TestAdminAPIRequiresHeader(t *testing.T) {
	u := newUpstream(t, "a")
	l := newTestLB(t, Options{}, u)
	api := l.AdminAPI()

	req := httptest.NewRequest(http.MethodPost, "/api/algorithm?name=rr", nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without header: %d, want 403", rec.Code)
	}
	if l.Algorithm() != "p2c" {
		t.Fatal("algorithm changed without the admin header")
	}
	req.Header.Set("X-Pulse-Admin", "1")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 200 || l.Algorithm() != "rr" {
		t.Fatalf("POST with header: %d, algorithm %s", rec.Code, l.Algorithm())
	}
}

func TestStreamsLargeBodies(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, big) }))
	defer srv.Close()
	l := New(Options{})
	defer l.Close()
	l.AddBackend("big", srv.URL)
	if rec := get(t, l, "/"); rec.Body.Len() != len(big) {
		t.Fatalf("body %d bytes, want %d", rec.Body.Len(), len(big))
	}
}
