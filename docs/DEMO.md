# Demo script

How to present PulseLB in about 10 minutes.

## Before you start

1. Close heavy apps (games, torrents, browser tabs with video). Background load shows up in the latency numbers.
2. Plug in the charger and stop the laptop from sleeping during the demo.
3. Build once: `go build -o bin/ ./cmd/...`
4. Start everything: `bin\demo.exe`. The browser opens http://127.0.0.1:9000.
5. Turn the volume up. The voice guide uses the browser's built-in voices. In Microsoft Edge, the "Natural" voices sound best.

## 1. Landing page (2 min)

Press **Take the voice tour**. Pulse explains the problem, how a request flows, the self-healing features and the results, spotlighting each section as it goes.
The hero animation keeps running on its own: a server slows down, another crashes, a replacement is launched, and the traffic dots reroute each time.

If someone asks a question, type it into Pulse's **Ask** box, for example "how does routing work?" or "why shed traffic?".

## 2. Control room (4 min)

At the end of the tour, click **Continue in the control room** and press **Play**. Pulse then drives the demo itself:

1. turns on 5,000 req/s of Tatkal traffic
2. reads out the live numbers
3. slows down backend-2 by 300ms and shows its traffic share dropping to around 1%
4. reads the decision journal aloud
5. crashes backend-3 and shows the ejection and the autoscaled replacement
6. repairs everything

Afterwards, let the audience drive. Press **10k/s**, click **Crash 20s** on a server, or switch the self-healing features off and break a server to show how much worse it gets.

## 3. Race Mode (3 min)

Open **Race mode** and press **Play** on the guide (or **Run the Tatkal Rush**). Pulse starts the race and commentates each stage live. At the end it reads out the scoreboard:
typically about 77% of bookings completed for classic round robin against about 99.8% for PulseLB, with 0 failed requests.

## 4. If someone asks "is it really 10,000 requests per second?"

Show `docs/RESULTS.md`: two independent tools, blast and k6, 60-second runs, separate processes. Or run it live in three terminals:

```sh
bin\backend.exe -ports 8001,8002,8003 -latency 5ms
bin\pulselb.exe -backends http://127.0.0.1:8001,http://127.0.0.1:8002,http://127.0.0.1:8003
bin\blast.exe -rate 10000 -duration 30s
```

(Stop `demo.exe` first, because it also uses ports 8001 to 8003.)

## Honest answers to likely questions

- **"Is Pulse a real AI?"** No. It's a scripted guide: the explanations are written in advance and filled with live numbers as it speaks, and questions are matched against a built-in knowledge base. It runs fully offline in the browser. Pulse says this itself if you ask it "are you an AI?".
- **"Why is Race Mode slower than 10k?"** Two complete setups plus the traffic generator share one laptop, so it runs at 2,250 req/s per side (75% of capacity). Race Mode measures behaviour under failure; the separate benchmark measures throughput.
