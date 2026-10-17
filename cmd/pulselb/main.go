// Command pulselb runs the PulseLB load balancer and its dashboard.
//
// Quick demo (3 simulated backends in-process, autoscaling enabled):
//
//	pulselb -demo
//
// Against real backends:
//
//	pulselb -backends http://127.0.0.1:8001,http://127.0.0.1:8002
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/backendsim"
	"github.com/mithileshofficial06/pulselb/internal/lb"
	"github.com/mithileshofficial06/pulselb/internal/web"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address for client traffic")
	adminAddr := flag.String("admin", "127.0.0.1:9090", "address for the dashboard and admin API (keep it on localhost)")
	backends := flag.String("backends", "", "comma-separated backend URLs, optionally name=url")
	algo := flag.String("algo", "p2c", "routing algorithm: p2c, rr or leastconn")
	classic := flag.Bool("classic", false, "behave like a classic balancer: round robin, health checks only")
	slo := flag.Duration("slo", 250*time.Millisecond, "latency target that drives load shedding")
	demo := flag.Bool("demo", false, "start 3 simulated backends in-process (autoscaling enabled)")
	demoLat := flag.Duration("demo-latency", 10*time.Millisecond, "service time of demo backends")
	demoCap := flag.Int("demo-capacity", 64, "worker slots per demo backend (64 slots / 10ms = 6,400 req/s each)")
	spawn := flag.String("spawn", "", "path to the backend binary, enables autoscaling with real processes")
	flag.Parse()

	opts := lb.Options{Algorithm: *algo, Smart: !*classic, SLO: *slo, Name: "PulseLB"}
	if *classic {
		opts.Algorithm = "rr"
		opts.Name = "Classic"
	}

	tmpl := backendsim.Config{Latency: *demoLat, Jitter: *demoLat / 4, Capacity: *demoCap}
	switch {
	case *demo:
		opts.Spawner = &backendsim.Spawner{Template: tmpl}
	case *spawn != "":
		opts.Spawner = &execSpawner{bin: *spawn}
	}
	balancer := lb.New(opts)

	if *demo {
		for i := 1; i <= 3; i++ {
			cfg := tmpl
			cfg.Name = fmt.Sprintf("backend-%d", i)
			cfg.Addr = fmt.Sprintf("127.0.0.1:%d", 8000+i)
			s := backendsim.New(cfg)
			if err := s.Start(); err != nil {
				log.Fatalf("demo backend: %v", err)
			}
			defer s.Stop()
			if _, err := balancer.AddBackend(cfg.Name, s.URL()); err != nil {
				log.Fatal(err)
			}
		}
	}
	for i, raw := range strings.Split(*backends, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name := fmt.Sprintf("backend-%d", i+1)
		if k, v, ok := strings.Cut(raw, "="); ok {
			name, raw = k, v
		}
		if _, err := balancer.AddBackend(name, raw); err != nil {
			log.Fatal(err)
		}
	}
	if len(balancer.Pool().All()) == 0 {
		log.Fatal("no backends: pass -backends or -demo")
	}
	balancer.Start()
	defer balancer.Close()

	adminMux := http.NewServeMux()
	adminMux.Handle("/api/", balancer.AdminAPI())
	adminMux.Handle("/", web.Handler())

	proxy := &http.Server{
		Addr:              *listen,
		Handler:           balancer,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	adminSrv := &http.Server{Addr: *adminAddr, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := adminSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("admin: %v", err)
		}
	}()
	go func() {
		if err := proxy.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("proxy: %v", err)
		}
	}()
	mode := "PulseLB (smart)"
	if *classic {
		mode = "classic round robin"
	}
	log.Printf("%s balancing %d backends on http://%s", mode, len(balancer.Pool().All()), *listen)
	log.Printf("dashboard: http://%s", *adminAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy.Shutdown(ctx)
	adminSrv.Shutdown(ctx)
}

// execSpawner launches real backend processes for the autoscaler.
type execSpawner struct{ bin string }

func (e *execSpawner) Spawn(name string) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := exec.Command(e.bin, "-ports", strconv.Itoa(port), "-name", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), stop, nil
}
