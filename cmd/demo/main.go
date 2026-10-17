// Command demo runs the whole PulseLB show from one process:
//
//	http://127.0.0.1:9000/                 landing page + voice guide
//	http://127.0.0.1:9000/dashboard.html   control room (PulseLB + 3 servers)
//	http://127.0.0.1:9000/arena.html       Race Mode (classic vs PulseLB)
//
// Client traffic for the control-room balancer goes to http://127.0.0.1:8080.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/arena"
	"github.com/mithileshofficial06/pulselb/internal/backendsim"
	"github.com/mithileshofficial06/pulselb/internal/lb"
	"github.com/mithileshofficial06/pulselb/internal/loadgen"
	"github.com/mithileshofficial06/pulselb/internal/perf"
	"github.com/mithileshofficial06/pulselb/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "address for the web pages")
	listen := flag.String("listen", "127.0.0.1:8080", "client traffic address of the control-room balancer")
	open := flag.Bool("open", true, "open the landing page in the default browser")
	raceCap := flag.Int("race-capacity", 10, "race mode: worker slots per backend (10 slots x 10ms = 1,000 req/s each)")
	racePeak := flag.Float64("race-peak", 2250, "race mode: per-side request rate at the peak of the rush")
	flag.Parse()
	if err := perf.DisablePowerThrottling(); err != nil {
		log.Printf("could not opt out of OS power throttling: %v", err)
	}

	// Control room: PulseLB in front of 3 simulated servers (6,400 req/s each).
	tmpl := backendsim.Config{Latency: 10 * time.Millisecond, Jitter: 2500 * time.Microsecond, Capacity: 64, QueueTimeout: time.Second}
	balancer := lb.New(lb.Options{Name: "PulseLB", Smart: true, Spawner: &backendsim.Spawner{Template: tmpl}})
	for i := 1; i <= 3; i++ {
		cfg := tmpl
		cfg.Name = fmt.Sprintf("backend-%d", i)
		cfg.Addr = fmt.Sprintf("127.0.0.1:%d", 8000+i)
		s := backendsim.New(cfg)
		if err := s.Start(); err != nil {
			log.Fatalf("%s: %v (is another PulseLB demo already running?)", cfg.Name, err)
		}
		defer s.Stop()
		if _, err := balancer.AddBackend(cfg.Name, s.URL()); err != nil {
			log.Fatal(err)
		}
	}
	balancer.Start()
	defer balancer.Close()
	proxy := &http.Server{Addr: *listen, Handler: balancer, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		if err := proxy.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("proxy: %v", err)
		}
	}()
	traffic := loadgen.NewController("http://" + *listen)
	defer traffic.Stop()

	// Race Mode: two more worlds on ports 8101-8103 and 8201-8203.
	race, err := arena.New(arena.Config{Capacity: *raceCap, Peak: *racePeak})
	if err != nil {
		log.Fatalf("race mode: %v", err)
	}
	defer race.Close()

	mux := http.NewServeMux()
	mux.Handle("/api/arena/", race.Handler())
	mux.Handle("/api/traffic", traffic.Handler())
	mux.Handle("/api/", balancer.AdminAPI())
	mux.Handle("/", web.Handler())
	site := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := site.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("web: %v", err)
		}
	}()

	url := "http://" + *addr + "/"
	fmt.Println()
	fmt.Println("  PULSELB DEMO IS RUNNING")
	fmt.Println("  ----------------------------------------------")
	fmt.Println("  Landing page + voice guide : " + url)
	fmt.Println("  Control room               : " + url + "dashboard.html")
	fmt.Println("  Race mode                  : " + url + "arena.html")
	fmt.Println("  Balancer (client traffic)  : http://" + *listen)
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()
	if *open {
		openBrowser(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	proxy.Shutdown(ctx)
	site.Shutdown(ctx)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}
