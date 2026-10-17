# Results and methodology

Everything here was measured on 17 October 2026 on a single laptop. Raw output for each run quoted below is in [`results/`](../results/).

## Test machine

| | |
|---|---|
| CPU | Intel Core 5 210H, 8 cores (hybrid performance and efficiency cores), 12 threads |
| OS | Windows 11 Home |
| Go | 1.27.0 |
| Power | Plugged in, Balanced power plan |
| Network | Loopback only (`127.0.0.1`); generator, balancer and backends on the same machine |

## 1. The 10,000 req/s target

**Setup:** three separate processes.

```sh
bin/backend -ports 8001,8002,8003 -latency 5ms -jitter 2ms     # 3 backends, 5 ms ± 2 ms service time
bin/pulselb -backends http://127.0.0.1:8001,http://127.0.0.1:8002,http://127.0.0.1:8003
```

PulseLB ran in its default smart mode (P2C-EWMA plus every self-healing feature), so the overhead of those features is included.

### blast, 60 s at 10,000 req/s (5 s ramp) — `results/blast-10k.json`

| Metric | Value |
|---|---|
| Requests sent | 575,009 |
| Succeeded | 574,828 (99.97%) |
| Steady-state successful throughput | **9,997 req/s** |
| Shed by PulseLB | 0 |
| Failed | 181, all "connection refused" in two bursts (seconds 8 and 30) |
| Dropped by the generator | 0 |
| Latency p50 / p90 / p99 / p99.9 / max | 5.8 / 7.3 / **18.9** / 96 / 216 ms |

### k6, 60 s at 10,000 req/s — `results/k6-10k-summary.json`, script `scripts/k6-10k.js`

| Metric | Value |
|---|---|
| Requests | 599,637 |
| Throughput | **9,993 req/s** |
| Failed | **0 (0.00%)** |
| Latency p50 / p90 / p95 / p99 / max | 5.5 / 7.2 / 7.6 / **8.7** / 28 ms |
| Dropped iterations | 364 (k6 itself didn't have a free virtual user in time; these requests were never sent) |

An earlier k6 run that pre-opened 400 connections in the same instant got 145 "connection refused" errors, all in the first second (99.98% success).
That's the Windows listen backlog (see below), which is why the script now starts with 100.

### Run-to-run variation

These runs were made while the laptop was otherwise idle. Runs at 10k made while it was in active use (browser and Task Manager in the foreground)
ranged from **99.47% to 99.97%** success. The failures were almost all refused connections during latency hiccups.
One run made just after the laptop woke from sleep reached only 6,498 req/s and is not counted. One run was **discarded**
because the laptop went to sleep in the middle of it: the generator's clock jumped 74 minutes, and it reported a 4,417-second "request".

### Ceiling on this machine

Quick 15 s runs before tuning: at a 20,000 req/s target the system completed **~14,200 req/s**. Beyond that, Windows ran out of socket buffers
("system lacked sufficient buffer space") and ephemeral ports, with the generator, balancer and backends competing for the same 12 threads.
To find PulseLB's own ceiling you would need the generator on a different machine.

## 2. Race Mode

**Setup:** `bin/arena -race -json results/race-N.json`. Two identical worlds, each with 3 simulated backends
(10 worker slots × 10 ms ≈ 1,000 req/s each, 1 s queue timeout). Both get the same traffic from the same generator settings
(Tatkal mix: 20% `/book`, 50% `/api/trains`, 30% `/browse`) and the same chaos at the same instant.

- **Classic:** round robin, active health checks (1 s interval, 2 failures to mark down), no retries, no shedding, no autoscaling.
- **PulseLB:** P2C-EWMA, ejection, slow start, budgeted retries, priority shedding, autoscaling.

All numbers are measured by the generator, i.e. what clients experienced.

| Run | Side | Success | Bookings completed | Failed | Shed | p50 | p99 | Most healthy servers |
|---|---|---|---|---|---|---|---|---|
| 1 | Classic | 77.61% | 22,519 / 29,006 (77.64%) | 32,238 | 0 | 606 ms | 4.8 s | 3 |
| 1 | PulseLB | **97.60%** | 28,831 / 28,906 (**99.74%**) | **0** | 3,451 | 12 ms | 258 ms | 6 |
| 2 | Classic | 77.64% | 22,384 / 28,884 (77.50%) | 32,191 | 0 | 606 ms | 4.8 s | 3 |
| 2 | PulseLB | **97.58%** | 28,638 / 28,689 (**99.82%**) | **0** | 3,484 | 12 ms | 270 ms | 5 |
| 3 | Classic | 77.68% | 22,430 / 28,841 (77.77%) | 32,135 | 0 | 606 ms | 4.8 s | 3 |
| 3 | PulseLB | **97.64%** | 28,996 / 29,051 (**99.81%**) | **0** | 3,401 | 12 ms | 258 ms | 6 |

The latencies are histogram bucket midpoints (±3%), which is why some values repeat exactly across runs.

### Why the classic side loses

- **Gray failure.** backend-2 gets +400 ms but its health endpoint still answers instantly, so active health checks never notice.
  Round robin keeps sending it a third of the traffic, far beyond what it can serve, so its queue fills and requests time out.
- **Crash.** It takes 2 failed health checks (~2 s) to stop sending traffic to backend-3. Meanwhile one server is slow, one is dead and one is overloaded.

### What PulseLB does instead (from its journal)

1. backend-2 slows → within about a second its share drops to ~1% (probes only). The two remaining servers can't carry 2,250 req/s,
   so latency passes the target and **browse traffic is shed** while bookings continue.
2. After 3 s of overload the autoscaler raises the target fleet size and launches `auto-1`, which ramps up under slow start. Shedding stops.
3. backend-3 crashes → **ejected after 5 failed requests**, its in-flight reads retried elsewhere, and a replacement server launched.
4. Repairs → servers return with slow start; surplus autoscaled servers are drained once the rush ends.

The price is ~3,450 browse requests per run answered `503 Retry-After: 1`. That's the design choice: under overload, refuse
the least important requests quickly rather than letting every request get slow.

### Fairness notes

- Each side has its own backends, so one side's overload can't slow the other side's servers.
- They do share a machine. Early versions of the race let the classic side's timeouts churn sockets until the machine ran out of ports,
  which also hurt PulseLB's numbers. The backend queue timeout and the race's scale (75% of fleet capacity, ~4.5k req/s total) were chosen
  so that doesn't happen. With these settings PulseLB recorded 0 failed requests in three runs.
- PulseLB is allowed to add servers and the classic side isn't. That's one of the features under test, so the "most healthy servers" row is reported alongside the results.

## Reproducing

```sh
go build -o bin/ ./cmd/...
go test ./...
bin/arena -race -json results/race.json
# 10k: start backend and pulselb as above, then
bin/blast -rate 10000 -duration 60s -json results/blast.json
k6 run scripts/k6-10k.js
```

Close other applications first. The background load of a laptop in use is clearly visible in p99 and p99.9.
