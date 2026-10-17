// Command arena races a classic round-robin load balancer against PulseLB.
//
// Both sides get their own identical set of simulated backends, the same
// traffic from the same generator settings, and the same failures at the
// same moment. Open http://127.0.0.1:9000/arena.html and press "Run".
//
//	arena -race -json results/race.json   # headless: print the scoreboard
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/arena"
	"github.com/mithileshofficial06/pulselb/internal/perf"
	"github.com/mithileshofficial06/pulselb/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "arena dashboard address")
	latency := flag.Duration("latency", 10*time.Millisecond, "backend service time")
	capacity := flag.Int("capacity", 10, "worker slots per backend (10 slots / 10ms = 1,000 req/s each)")
	peak := flag.Float64("peak", 2250, "per-side request rate during the Tatkal rush (75% of a 3-server fleet)")
	race := flag.Bool("race", false, "run the Tatkal Rush once without a browser, print the scoreboard and exit")
	out := flag.String("json", "", "with -race: also write the scoreboard to this JSON file")
	flag.Parse()
	if err := perf.DisablePowerThrottling(); err != nil {
		log.Printf("could not opt out of OS power throttling: %v", err)
	}

	a, err := arena.New(arena.Config{Latency: *latency, Capacity: *capacity, Peak: *peak})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	log.Print(a.Describe())

	if *race {
		a.RunHeadless(*out)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/api/arena/", a.Handler())
	mux.Handle("/", web.Handler())
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	log.Printf("race mode: http://%s/arena.html", *addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
