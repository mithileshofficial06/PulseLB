package lb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// AdminAPI returns the control-plane API, mounted under /api/.
//
// Changes are POSTs that must carry an "X-Pulse-Admin: 1" header. Browsers
// can't send a custom header cross-origin without a CORS preflight, which
// this API never approves, so a malicious web page can't poke a local admin
// port. The admin listener should still only be bound to localhost.
func (l *LB) AdminAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, l.Summary())
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, l.History())
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, l.journal.Since(since))
	})
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		var since uint64
		StreamSSE(w, r, time.Second, func() any {
			ev := l.journal.Since(since)
			if len(ev) > 0 {
				since = ev[len(ev)-1].Seq
			}
			return map[string]any{"summary": l.Summary(), "events": ev}
		})
	})
	mux.HandleFunc("GET /api/chaos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, l.ChaosStates(r.Context()))
	})
	mux.HandleFunc("POST /api/chaos", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		b := l.pool.Get(q.Get("backend"))
		if b == nil {
			http.Error(w, "unknown backend", http.StatusNotFound)
			return
		}
		q.Del("backend")
		st, err := l.Chaos(r.Context(), b, q.Encode())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, st)
	}))
	mux.HandleFunc("POST /api/algorithm", admin(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"algorithm": l.SetAlgorithm(r.URL.Query().Get("name"))})
	}))
	mux.HandleFunc("POST /api/features", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := l.SetFeature(q.Get("name"), q.Get("on") == "true"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, l.FeatureStates())
	}))
	mux.HandleFunc("POST /api/backends", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		b, err := l.AddBackend(q.Get("name"), q.Get("url"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l.journal.Add(LevelInfo, "%s added by operator.", b.Name)
		writeJSON(w, map[string]string{"name": b.Name})
	}))
	mux.HandleFunc("DELETE /api/backends", admin(func(w http.ResponseWriter, r *http.Request) {
		if !l.RemoveBackend(r.URL.Query().Get("name")) {
			http.Error(w, "unknown backend", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	return mux
}

// Chaos sends a chaos command (a query string such as "delay=200") to a
// backend's control endpoint. An empty query just reads its state.
func (l *LB) Chaos(ctx context.Context, b *Backend, query string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	method := http.MethodGet
	if query != "" {
		method = http.MethodPost
	}
	req, _ := http.NewRequestWithContext(ctx, method, b.URL.String()+"/__pulse/chaos?"+query, nil)
	resp, err := l.health.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if query != "" {
		l.journal.Add(LevelInfo, "Chaos injected into %s: %s", b.Name, query)
	}
	return body, nil
}

// ChaosStates reads the chaos settings of every backend. Backends that don't
// answer (crashed) map to null.
func (l *LB) ChaosStates(ctx context.Context) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range l.pool.All() {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			st, err := l.Chaos(ctx, b, "")
			mu.Lock()
			if err != nil || !json.Valid(st) {
				out[b.Name] = json.RawMessage("null")
			} else {
				out[b.Name] = st
			}
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	return out
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

// StreamSSE sends fn() as a server-sent event every interval until the
// client goes away.
func StreamSSE(w http.ResponseWriter, r *http.Request, every time.Duration, fn func() any) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func() bool {
		b, err := json.Marshal(fn())
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		w.Write(b)
		w.Write([]byte("\n\n"))
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}
