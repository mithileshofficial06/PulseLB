package backendsim

// Spawner launches new simulated backends in the current process, on a free
// port, using Template for latency and capacity. It satisfies lb.Spawner.
type Spawner struct {
	Template Config
}

// Spawn starts a backend called name and returns its URL and a stop func.
func (sp *Spawner) Spawn(name string) (string, func(), error) {
	cfg := sp.Template
	cfg.Name = name
	cfg.Addr = "127.0.0.1:0"
	s := New(cfg)
	if err := s.Start(); err != nil {
		return "", nil, err
	}
	return s.URL(), s.Stop, nil
}
