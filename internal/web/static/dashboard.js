"use strict";

const FEATURES = {
  outlier: ["Instant ejection", "Pull a server out after 5 failures in a row, without waiting for the next health check."],
  slowstart: ["Slow start", "A recovered or new server gets a small share first and earns the rest by staying fast."],
  retries: ["Smart retries", "Retry failed reads on a different server, capped at ~20% extra load so retries can't snowball."],
  shedding: ["Priority shedding", "When latency passes the target, turn away browsing first so bookings and payments stay fast."],
  autoscale: ["Autoscale", "Replace dead servers and add new ones when the fleet is overloaded."],
};
const CLASSES = [["critical", "Bookings", "most important"], ["normal", "Search", "normal"], ["low", "Browse", "turned away first"]];

// Plain-English help for each part of the page.
const ALGO_HELP = {
  p2c: "<b>Smart P2C</b> (PulseLB's own): for every request it picks 2 servers at random and sends it to the one that has been <i>faster with a shorter queue</i>. A slow server ends up with almost no traffic.",
  leastconn: "<b>Least conn</b>: sends each request to the server with the <i>fewest requests in progress</i>. It notices queues, but not speed.",
  rr: "<b>Round robin</b>: takes turns, 1 → 2 → 3 → 1… It ignores speed and queues, so a slow server still gets a third of the traffic. Most basic load balancers do this.",
};
const STATE_WHY = {
  healthy: "Serving normally.",
  warming: "Back after a problem: getting traffic slowly.",
  ejected: "Kept failing: taken out for a few seconds.",
  down: "Not answering health checks: gets no traffic.",
  draining: "Being retired: finishing its last requests.",
};
const STATE_SAY = {
  warming: "is warming up after a problem",
  ejected: "was ejected for failing",
  down: "is down (not responding)",
  draining: "is being retired",
};

const $ = (id) => document.getElementById(id);
let history = [];
let summary = null;
let chaos = {};
const cards = new Map();

const thrSeries = [
  { key: "ok", label: "Answered", color: "--series-1" },
  { key: "shed", label: "Turned away", color: "--warning" },
  { key: "errors", label: "Failed", color: "--critical" },
];
const latSeries = [
  { key: "p50", label: "Typical (p50)", color: "--series-1" },
  { key: "p99", label: "Slowest 1% (p99)", color: "--series-2" },
];
$("lgThroughput").append(legend(thrSeries));
$("lgLatency").append(legend([...latSeries, { label: "250ms goal", color: "--ink-2", dash: true }]));
const thrChart = new TimeChart($("cThroughput"), { series: thrSeries, stack: true, unit: "rate", label: "Requests per second" });
const latChart = new TimeChart($("cLatency"), {
  series: latSeries, unit: "ms", label: "Latency",
  ref: () => summary && { value: summary.slo, label: "target " + fmt.ms(summary.slo) },
});

// ---- controls ----
$("algo").addEventListener("click", async (e) => {
  const a = e.target.closest("button")?.dataset.a;
  if (!a) return;
  await api("POST", "/api/algorithm?name=" + a);
  setSeg("algo", "a", a);
  $("algoHelp").innerHTML = ALGO_HELP[a] || "";
});
$("rates").addEventListener("click", (e) => {
  const r = e.target.closest("button")?.dataset.r;
  if (r != null) setTraffic(r);
});
$("mix").addEventListener("change", () => {
  const on = document.querySelector("#rates button.on");
  if (on && on.dataset.r !== "0") setTraffic(on.dataset.r);
});
async function setTraffic(r) {
  try {
    const t = await api("POST", `/api/traffic?rate=${r}&mix=${$("mix").value}`);
    showTraffic(t);
  } catch (err) { $("trafficNote").textContent = "Traffic control unavailable: " + err.message; }
}
function showTraffic(t) {
  setSeg("rates", "r", String(t.rate));
  if (t.mix) $("mix").value = t.mix;
  $("trafficNote").textContent = t.rate > 0
    ? `Sending ${fmt.int(t.rate)} fake users per second${t.dropped ? ` · the generator couldn't keep up and skipped ${fmt.int(t.dropped)} (this laptop is overloaded)` : ""}.`
    : "Traffic is off. Press 5k/s to start.";
}
function setSeg(id, attr, val) {
  document.querySelectorAll(`#${id} button`).forEach((b) => b.classList.toggle("on", b.dataset[attr] === val));
}

// ---- features ----
function renderFeatures() {
  const host = $("features");
  if (!host.childElementCount) {
    for (const [k, [name, desc]] of Object.entries(FEATURES)) {
      const sw = el("button", { class: "switch", role: "switch", "aria-label": name, "data-f": k });
      sw.addEventListener("click", async () => {
        try {
          await api("POST", `/api/features?name=${k}&on=${!sw.classList.contains("on")}`);
          summary.features[k] = !sw.classList.contains("on");
          renderFeatures();
        } catch (err) { alert(err.message); }
      });
      host.append(el("div", { class: "feat" }, el("b", {}, name), sw, el("p", {}, desc)));
    }
  }
  host.querySelectorAll(".switch").forEach((sw) => {
    const on = !!summary.features[sw.dataset.f];
    sw.classList.toggle("on", on);
    sw.setAttribute("aria-checked", on);
    if (sw.dataset.f === "autoscale") sw.disabled = !summary.can_scale;
  });
}

// ---- backends ----
function chaosBtn(label, name, q, cls = "", help = "") {
  return el("button", {
    class: cls,
    title: help,
    onclick: async () => {
      try { await api("POST", `/api/chaos?backend=${encodeURIComponent(name)}&${q}`); pollChaos(); }
      catch (err) { alert(err.message); }
    },
  }, label);
}
function card(name) {
  if (cards.has(name)) return cards.get(name);
  const c = {
    root: el("div", { class: "card be", "data-be": name }),
    status: el("span", { class: "status" }),
    badge: el("span", { class: "badge", style: "display:none" }, "autoscaled"),
    share: el("div", { style: "background:var(--series-1);width:0" }),
    shareTxt: el("span", { class: "tag" }),
    rps: el("b", { class: "num" }), ewma: el("b", { class: "num" }), infl: el("b", { class: "num" }), err: el("b", { class: "num" }),
    chips: el("div", { class: "chips" }),
    why: el("div", { class: "why" }),
  };
  c.root.append(
    el("div", { class: "head" }, el("span", { class: "name" }, name), el("span", { class: "row" }, c.badge, c.status)),
    c.why,
    el("div", {}, el("div", { class: "bar" }, c.share), el("div", { class: "row", style: "justify-content:space-between;margin-top:4px" }, c.shareTxt)),
    el("div", { class: "stats" },
      el("div", {}, c.rps, el("span", {}, "req/s")),
      el("div", {}, c.ewma, el("span", {}, "avg time")),
      el("div", {}, c.infl, el("span", {}, "in progress")),
      el("div", {}, c.err, el("span", {}, "failing"))),
    c.chips,
    el("div", { class: "chaos" },
      chaosBtn("🐢 Slow down", name, "delay=300", "", "Every request takes +300ms. Health checks still pass, so it looks healthy."),
      chaosBtn("⚠ 30% errors", name, "errors=0.3", "", "3 in every 10 requests fail."),
      chaosBtn("💥 Crash 20s", name, "crash=20", "danger", "The server dies and restarts by itself after 20 seconds."),
      chaosBtn("🩺 Fail health", name, "down=true", "danger", "Its health check reports sick."),
      chaosBtn("🔧 Repair", name, "reset=true", "", "Undo everything on this server.")),
  );
  cards.set(name, c);
  $("backends").append(c.root);
  return c;
}
function renderBackends(p) {
  const seen = new Set();
  for (const b of p.backends || []) {
    seen.add(b.name);
    const c = card(b.name);
    c.root.className = "card be " + b.state;
    c.status.className = "status " + b.state;
    c.status.textContent = STATE_LABEL[b.state] || b.state;
    c.badge.style.display = b.managed ? "" : "none";
    c.share.style.width = (b.share * 100).toFixed(1) + "%";
    c.shareTxt.textContent = `gets ${fmt.pct(b.share, 0)} of all traffic`;
    c.why.textContent = STATE_WHY[b.state] || "";
    c.rps.textContent = fmt.rate(b.rps);
    c.ewma.textContent = fmt.ms(b.ewma);
    c.infl.textContent = fmt.int(b.inflight);
    c.err.textContent = fmt.pct(b.err_rate, 0);
    const ch = chaos[b.name];
    const chips = [];
    // A missing chaos reply only means "crashed" if PulseLB also sees the
    // server as down; under heavy load a healthy server can just answer late.
    if (ch === null && b.state === "down") chips.push("you crashed it");
    else if (ch) {
      if (ch.delay_ms) chips.push(`you slowed it +${ch.delay_ms}ms`);
      if (ch.error_rate) chips.push(`you made ${Math.round(ch.error_rate * 100)}% fail`);
      if (ch.down) chips.push("you failed its health check");
    }
    c.chips.replaceChildren(...chips.map((t) => el("span", { class: "chip" }, t)));
  }
  for (const [name, c] of cards) if (!seen.has(name)) { c.root.remove(); cards.delete(name); }
}
async function pollChaos() {
  try { chaos = await api("GET", "/api/chaos"); } catch { /* balancer offline */ }
}

// ---- lanes ----
function renderLanes(p) {
  const host = $("lanes");
  host.replaceChildren(...CLASSES.map(([k, label, note], i) => {
    const ok = p.class_ok[i], shed = p.class_shed[i], err = p.class_err[i];
    const tot = ok + shed + err || 1;
    return el("div", { class: "lane" },
      el("span", {}, el("b", {}, label), el("div", { class: "tag" }, note)),
      el("div", { class: "bar", style: "height:10px" },
        el("div", { style: `width:${(ok / tot) * 100}%;background:var(--series-1)` }),
        el("div", { style: `width:${(shed / tot) * 100}%;background:var(--warning)` }),
        el("div", { style: `width:${(err / tot) * 100}%;background:var(--critical)` })),
      el("span", { class: "nums num" }, `${fmt.rate(ok)} answered · ${fmt.rate(shed)} turned away`));
  }));
}

// ---- journal ----
let lastSeq = 0;
const recentEvents = [];
function addEvents(events) {
  const host = $("journal");
  for (const e of events) {
    if (e.seq <= lastSeq) continue; // the stream replays history after a reconnect
    lastSeq = e.seq;
    recentEvents.push(e);
    if (recentEvents.length > 100) recentEvents.shift();
    const li = el("li", { class: e.level + " fresh" },
      el("span", { class: "ic", "aria-label": e.level }, LEVEL_ICON[e.level] || "•"),
      el("div", {}, el("time", {}, fmt.time(e.t)), e.msg));
    host.prepend(li);
    setTimeout(() => li.classList.remove("fresh"), 1300);
  }
  while (host.childElementCount > 200) host.lastChild.remove();
}

// ---- "right now": the whole page in one sentence ----
function nowSentence(p) {
  const bs = p.backends || [];
  if (!p.rps) return ["idle", "Traffic is off. Press 5k/s in step 1 to send fake users."];
  const done = p.ok + p.errors + p.shed;
  const okPct = done ? (100 * p.ok) / done : 100;
  const parts = [`Answering ${fmt.int(p.ok)} requests a second, ${okPct.toFixed(okPct > 99 ? 1 : 0)}% OK; even the slowest 1% finish in ${fmt.ms(p.p99)}.`];
  const live = bs.filter((b) => b.state === "healthy" || b.state === "warming");
  const typical = live.map((b) => b.ewma).sort((a, b) => a - b)[Math.floor(live.length / 2)] || 0;
  let issues = 0;
  for (const b of bs) {
    if (STATE_SAY[b.state]) { parts.push(`${b.name} ${STATE_SAY[b.state]}${b.managed ? " (auto-launched)" : ""}.`); issues++; }
    else if (typical && b.ewma > 3 * typical && b.share < 0.15) {
      parts.push(`${b.name} is slow (${fmt.ms(b.ewma)}), so PulseLB gives it only ${fmt.pct(b.share, 0)} of traffic.`);
      issues++;
    }
  }
  if (p.shed > 0) parts.push(`Turning away ${fmt.int(p.shed)} browse/search requests a second to keep bookings fast.`);
  if (!issues && !p.shed && !p.errors) parts.push(`All ${bs.length} servers are healthy.`);
  const tone = okPct < 95 || p.errors > 0.01 * p.rps ? "bad" : issues || p.shed ? "warn" : "good";
  return [tone, parts.slice(0, 4).join(" ")];
}

// ---- main render ----
function render() {
  const p = summary.point;
  setSeg("algo", "a", summary.algorithm);
  $("algoHelp").innerHTML = ALGO_HELP[summary.algorithm] || "";
  const [tone, msg] = nowSentence(p);
  $("nowBanner").dataset.tone = tone;
  $("nowMsg").textContent = msg;
  $("subtitle").textContent = `${summary.name} · up ${Math.round(summary.uptime)}s · ${fmt.int(summary.totals.received)} requests handled`;
  const done = p.ok + p.errors + p.shed;
  $("kServed").textContent = fmt.rate(p.ok);
  $("kServedSub").textContent = p.rps ? `of ${fmt.rate(p.rps)} arriving each second` : "no traffic yet";
  $("kSuccess").textContent = done ? fmt.pct(p.ok / done, done > 100 ? 2 : 1) : "–";
  $("kSuccessSub").textContent = !done ? "of all requests" :
    p.shed || p.errors ? `${fmt.rate(p.shed)} turned away · ${fmt.rate(p.errors)} failed /s` : "nothing refused, nothing failed";
  $("kP99").textContent = fmt.ms(p.p99);
  $("kP99Sub").textContent = `typical: ${fmt.ms(p.p50)} · goal under ${fmt.ms(summary.slo)}`;
  $("kShed").textContent = fmt.rate(p.shed);
  $("kInflight").textContent = fmt.int(p.inflight);
  $("kLimit").textContent = p.limit ? `allowed at once: ${fmt.int(p.limit)}` : "no limit (protection off)";
  const bs = p.backends || [];
  const up = bs.filter((b) => b.state === "healthy" || b.state === "warming").length;
  $("kServers").textContent = `${up}/${bs.length}`;
  $("kServersSub").textContent = bs.some((b) => b.managed) ? `${bs.filter((b) => b.managed).length} launched automatically` : "up and taking traffic";
  thrChart.setData(history);
  latChart.setData(history);
  renderBackends(p);
  renderLanes(p);
  renderFeatures();
}

async function start() {
  try {
    history = await api("GET", "/api/history");
    showTraffic(await api("GET", "/api/traffic"));
  } catch { /* stream will retry */ }
  pollChaos();
  setInterval(pollChaos, 2000);
  const es = new EventSource("/api/stream");
  es.onmessage = (m) => {
    $("offline").style.display = "none";
    const d = JSON.parse(m.data);
    summary = d.summary;
    const p = summary.point;
    if (p.t && (!history.length || history[history.length - 1].t < p.t)) {
      history.push(p);
      if (history.length > 300) history.shift();
    }
    addEvents(d.events);
    render();
    window.PULSE_STATE = { summary, chaos, events: recentEvents };
  };
  es.onerror = () => { $("offline").style.display = "block"; };
}
start();

// Hooks for the voice guide, which can drive the demo itself.
window.PULSE_ACTIONS = {
  traffic: (rate, mix = "tatkal") => { $("mix").value = mix; return setTraffic(rate); },
  chaos: (name, q) => api("POST", `/api/chaos?backend=${encodeURIComponent(name)}&${q}`).then(pollChaos),
  algorithm: (a) => api("POST", "/api/algorithm?name=" + a).then(() => setSeg("algo", "a", a)),
};
