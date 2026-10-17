// Command backend runs one or more simulated application servers.
//
//	backend -ports 8001,8002,8003 -latency 5ms -jitter 2ms
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mithileshofficial06/pulselb/internal/backendsim"
	"github.com/mithileshofficial06/pulselb/internal/perf"
)

func main() {
	ports := flag.String("ports", "8001,8002,8003", "comma-separated ports, one server per port")
	host := flag.String("host", "127.0.0.1", "interface to listen on")
	prefix := flag.String("name", "backend", "name prefix; servers are named <prefix>-1, <prefix>-2, ...")
	latency := flag.Duration("latency", 5*time.Millisecond, "base service time per request")
	jitter := flag.Duration("jitter", 2*time.Millisecond, "random +/- jitter on the service time")
	capacity := flag.Int("capacity", 0, "concurrent worker slots per server (0 = unlimited)")
	flag.Parse()
	if err := perf.DisablePowerThrottling(); err != nil {
		log.Printf("could not opt out of OS power throttling: %v", err)
	}

	var servers []*backendsim.Server
	list := strings.Split(*ports, ",")
	for i, p := range list {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name := fmt.Sprintf("%s-%d", *prefix, i+1)
		if len(list) == 1 {
			name = *prefix
		}
		s := backendsim.New(backendsim.Config{
			Name:     name,
			Addr:     *host + ":" + p,
			Latency:  *latency,
			Jitter:   *jitter,
			Capacity: *capacity,
		})
		if err := s.Start(); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		log.Printf("%s listening on %s (latency %v ±%v, capacity %d)", name, s.URL(), *latency, *jitter, *capacity)
		servers = append(servers, s)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	for _, s := range servers {
		s.Stop()
	}
}
