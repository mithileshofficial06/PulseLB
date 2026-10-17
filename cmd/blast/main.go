// Command blast fires HTTP traffic at a fixed rate and reports what the
// system actually completed.
//
//	blast -url http://127.0.0.1:8080 -rate 10000 -duration 60s -ramp 10s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/loadgen"
	"github.com/mithileshofficial06/pulselb/internal/perf"
)

type second struct {
	T      int     `json:"t"`
	Target float64 `json:"target"`
	Sent   uint64  `json:"sent"`
	OK     uint64  `json:"ok"`
	Shed   uint64  `json:"shed"`
	Errors uint64  `json:"errors"`
	P50    float64 `json:"p50_ms"`
	P99    float64 `json:"p99_ms"`
}

type result struct {
	Target      string            `json:"target"`
	Rate        float64           `json:"rate"`
	Duration    float64           `json:"duration_s"`
	Ramp        float64           `json:"ramp_s"`
	Mix         string            `json:"mix"`
	CPUs        int               `json:"cpus"`
	OS          string            `json:"os"`
	Sent        uint64            `json:"sent"`
	OK          uint64            `json:"ok"`
	Shed        uint64            `json:"shed"`
	Errors      uint64            `json:"errors"`
	Timeouts    uint64            `json:"timeouts"`
	Dropped     uint64            `json:"dropped"`
	SteadyOKRPS float64           `json:"steady_ok_rps"`
	SuccessPct  float64           `json:"success_pct"`
	P50         float64           `json:"p50_ms"`
	P90         float64           `json:"p90_ms"`
	P99         float64           `json:"p99_ms"`
	P999        float64           `json:"p999_ms"`
	Max         float64           `json:"max_ms"`
	ErrorKinds  map[string]uint64 `json:"error_kinds,omitempty"`
	Timeline    []second          `json:"timeline"`
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

func main() {
	target := flag.String("url", "http://127.0.0.1:8080", "load balancer URL")
	rate := flag.Float64("rate", 10000, "requests per second")
	duration := flag.Duration("duration", 30*time.Second, "test length")
	ramp := flag.Duration("ramp", 5*time.Second, "linear ramp-up time")
	mixFlag := flag.String("mix", "uniform", `traffic mix: "uniform", "tatkal" or "path:weight,..."`)
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	maxInflight := flag.Int("max-inflight", 20000, "max concurrent requests")
	out := flag.String("json", "", "write the full result to this JSON file")
	warm := flag.Int("warm", 500, "keep-alive connections to open before the test (0 = none)")
	flag.Parse()
	if err := perf.DisablePowerThrottling(); err != nil {
		log.Printf("could not opt out of OS power throttling: %v", err)
	}

	mix, err := loadgen.ParseMix(*mixFlag)
	if err != nil {
		log.Fatal(err)
	}
	g := loadgen.New(loadgen.Config{
		Target: *target, Rate: *rate, Duration: *duration, Ramp: *ramp,
		Mix: mix, Timeout: *timeout, MaxInflight: *maxInflight,
	})

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	fmt.Printf("blast: %s at %.0f req/s for %s (ramp %s, mix %s)\n\n", *target, *rate, *duration, *ramp, *mixFlag)
	fmt.Printf("%5s %8s %8s %8s %7s %7s %9s %9s\n", "t", "target", "sent", "ok", "shed", "errors", "p50", "p99")

	if *warm > 0 {
		t0 := time.Now()
		g.Warm(ctx, *warm)
		fmt.Printf("warmed %d connections in %s\n\n", *warm, time.Since(t0).Round(time.Millisecond))
	}

	done := make(chan struct{})
	start := time.Now()
	go func() { g.Run(ctx); close(done) }()

	var timeline []second
	prev := g.Counters()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	sec := 0
	steadyOK, steadySecs := uint64(0), 0
loop:
	for {
		select {
		case <-done:
			break loop
		case <-t.C:
			sec++
			c := g.Counters()
			w := c.Lat.Sub(prev.Lat)
			tgt := *rate
			if el := time.Since(start); *ramp > 0 && el < *ramp {
				tgt *= float64(el) / float64(*ramp)
			}
			s := second{
				T: sec, Target: tgt, Sent: c.Sent - prev.Sent, OK: c.OK - prev.OK,
				Shed: c.Shed - prev.Shed, Errors: c.Errors - prev.Errors,
				P50: ms(w.Quantile(0.5)), P99: ms(w.Quantile(0.99)),
			}
			timeline = append(timeline, s)
			// Steady state: after the ramp, excluding the last partial second.
			if time.Duration(sec)*time.Second > *ramp+time.Second && time.Duration(sec)*time.Second < *duration {
				steadyOK += s.OK
				steadySecs++
			}
			fmt.Printf("%4ds %8.0f %8d %8d %7d %7d %7.1fms %7.1fms\n", s.T, s.Target, s.Sent, s.OK, s.Shed, s.Errors, s.P50, s.P99)
			prev = c
		}
	}

	c := g.Counters()
	r := result{
		Target: *target, Rate: *rate, Duration: duration.Seconds(), Ramp: ramp.Seconds(), Mix: *mixFlag,
		CPUs: runtime.NumCPU(), OS: runtime.GOOS,
		Sent: c.Sent, OK: c.OK, Shed: c.Shed, Errors: c.Errors, Timeouts: c.Timeouts, Dropped: c.Dropped,
		P50: ms(c.Lat.Quantile(0.5)), P90: ms(c.Lat.Quantile(0.9)), P99: ms(c.Lat.Quantile(0.99)),
		P999: ms(c.Lat.Quantile(0.999)), Max: float64(c.Lat.Max) / 1000, Timeline: timeline,
		ErrorKinds: g.ErrorKinds(),
	}
	if steadySecs > 0 {
		r.SteadyOKRPS = float64(steadyOK) / float64(steadySecs)
	}
	if c.Sent > 0 {
		r.SuccessPct = 100 * float64(c.OK) / float64(c.Sent)
	}

	fmt.Printf("\n=== result ===\n")
	fmt.Printf("sent %d   ok %d (%.2f%%)   shed %d   errors %d (timeouts %d)   dropped by generator %d\n",
		r.Sent, r.OK, r.SuccessPct, r.Shed, r.Errors, r.Timeouts, r.Dropped)
	fmt.Printf("steady-state successful throughput: %.0f req/s (target %.0f)\n", r.SteadyOKRPS, *rate)
	fmt.Printf("latency of successful requests: p50 %.1fms  p90 %.1fms  p99 %.1fms  p99.9 %.1fms  max %.1fms\n",
		r.P50, r.P90, r.P99, r.P999, r.Max)

	for k, v := range r.ErrorKinds {
		fmt.Printf("  client error x%d: %s\n", v, k)
	}

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		f.Close()
		fmt.Printf("wrote %s\n", *out)
	}
}
