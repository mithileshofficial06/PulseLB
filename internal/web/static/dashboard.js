"use strict";

const FEATURES = {
  outlier: ["Instant ejection", "Pull a server out after 5 failures in a row, without waiting for the next health check."],
  slowstart: ["Slow start", "A recovered or new server gets a small share first and earns the rest by staying fast."],
  retries: ["Smart retries", "Retry failed reads on a different server, capped at ~20% extra load so retries can't snowball."],
  shedding: ["Priority shedding", "When latency passes the target, turn away browsing first so bookings and payments stay fast."],
  autoscale: ["Autoscale", "Replace dead servers and add new ones when the fleet is overloaded."],
};
const CLASSES = [["critical", "Bookings"], ["normal", "Search"], ["low", "Browse"]];

const $ = (id) => document.getElementById(id);
let history = [];
let summary = null;
let chaos = {};
const cards = new Map();

const thrSeries = [
  { key: "ok", label: "Served", color: "--series-1" },
  { key: "shed", label: "Shed", color: "--warning" },
  { key: "errors", label: "Errors", color: "--critical" },
];
const latSeries = [
  { key: "p50", label: "p50", color: "--series-1" },
  { key: "p99", label: "p99", color: "--series-2" },
];
$("lgThroughput").append(legend(thrSeries));
$("lgLatency").append(legend([...latSeries, { label: "Target (SLO)", color: "--ink-2", dash: true }]));
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
    ? `Offering ${fmt.int(t.rate)} req/s from the built-in generator${t.dropped ? ` · ${fmt.int(t.dropped)} could not be sent` : ""}`
    : "Built-in generator is off. For benchmark numbers use blast.";
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
function chaosBtn(label, name, q, cls = "") {
  return el("button", {
    class: cls,
    onclick: async () => {
      try { await api("POST", `/api/chaos?backend=${encodeURIComponent(name)}&${q}`); pollChaos(); }
      catch (err) { alert(err.message); }
    },
  }, label);
}
function card(name) {
  if (cards.has(name)) return cards.get(name);
  const c = {
    root: el("div", { class: "card be" }),
    status: el("span", { class: "status" }),
    badge: el("span", { class: "badge", style: "display:none" }, "autoscaled"),
    share: el("div", { style: "background:var(--series-1);width:0" }),
    shareTxt: el("span", { class: "tag" }),
    rps: el("b", { class: "num" }), ewma: el("b", { class: "num" }), infl: el("b", { class: "num" }), err: el("b", { class: "num" }),
    chips: el("div", { class: "chips" }),
  };
  c.root.append(
    el("div", { class: "head" }, el("span", { class: "name" }, name), el("span", { class: "row" }, c.badge, c.status)),
    el("div", {}, el("div", { class: "bar" }, c.share), el("div", { class: "row", style: "justify-content:space-between;margin-top:4px" }, c.shareTxt)),
    el("div", { class: "stats" },
      el("div", {}, c.rps, el("span", {}, "req/s")),
      el("div", {}, c.ewma, el("span", {}, "latency")),
      el("div", {}, c.infl, el("span", {}, "in flight")),
      el("div", {}, c.err, el("span", {}, "errors"))),
    c.chips,
    el("div", { class: "chaos" },
      chaosBtn("Slow +300ms", name, "delay=300"),
      chaosBtn("30% errors", name, "errors=0.3"),
      chaosBtn("Crash 20s", name, "crash=20", "danger"),
      chaosBtn("Fail health", name, "down=true", "danger"),
      chaosBtn("Reset", name, "reset=true")),
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
    c.shareTxt.textContent = `${fmt.pct(b.share, 0)} of traffic`;
    c.rps.textContent = fmt.rate(b.rps);
    c.ewma.textContent = fmt.ms(b.ewma);
    c.infl.textContent = fmt.int(b.inflight);
    c.err.textContent = fmt.pct(b.err_rate, 0);
    const ch = chaos[b.name];
    const chips = [];
    if (ch === null) chips.push("crashed");
    else if (ch) {
      if (ch.delay_ms) chips.push(`+${ch.delay_ms}ms injected`);
      if (ch.error_rate) chips.push(`${Math.round(ch.error_rate * 100)}% errors injected`);
      if (ch.down) chips.push("health check failing");
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
  host.replaceChildren(...CLASSES.map(([k, label], i) => {
    const ok = p.class_ok[i], shed = p.class_shed[i], err = p.class_err[i];
    const tot = ok + shed + err || 1;
    return el("div", { class: "lane" },
      el("span", {}, el("b", {}, label), el("div", { class: "tag" }, k)),
      el("div", { class: "bar", style: "height:10px" },
        el("div", { style: `width:${(ok / tot) * 100}%;background:var(--series-1)` }),
        el("div", { style: `width:${(shed / tot) * 100}%;background:var(--warning)` }),
        el("div", { style: `width:${(err / tot) * 100}%;background:var(--critical)` })),
      el("span", { class: "nums num" }, `${fmt.rate(ok)} ok · ${fmt.rate(shed)} shed`));
  }));
}

// ---- journal ----
let lastSeq = 0;
function addEvents(events) {
  const host = $("journal");
  for (const e of events) {
    if (e.seq <= lastSeq) continue; // the stream replays history after a reconnect
    lastSeq = e.seq;
    const li = el("li", { class: e.level + " fresh" },
      el("span", { class: "ic", "aria-label": e.level }, LEVEL_ICON[e.level] || "•"),
      el("div", {}, el("time", {}, fmt.time(e.t)), e.msg));
    host.prepend(li);
    setTimeout(() => li.classList.remove("fresh"), 1300);
  }
  while (host.childElementCount > 200) host.lastChild.remove();
}

// ---- main render ----
function render() {
  const p = summary.point;
  setSeg("algo", "a", summary.algorithm);
  $("subtitle").textContent = `${summary.name} · up ${Math.round(summary.uptime)}s · ${fmt.int(summary.totals.received)} requests handled`;
  const done = p.ok + p.errors + p.shed;
  $("kServed").textContent = fmt.rate(p.ok);
  $("kServedSub").textContent = `of ${fmt.rate(p.rps)} received / second`;
  $("kSuccess").textContent = done ? fmt.pct(p.ok / done, done > 100 ? 2 : 1) : "–";
  $("kSuccessSub").textContent = p.errors ? `${fmt.rate(p.errors)} errors/s` : "no errors";
  $("kP99").textContent = fmt.ms(p.p99);
  $("kP99Sub").textContent = `p50 ${fmt.ms(p.p50)} · target ${fmt.ms(summary.slo)}`;
  $("kShed").textContent = fmt.rate(p.shed);
  $("kInflight").textContent = fmt.int(p.inflight);
  $("kLimit").textContent = p.limit ? `limit ${fmt.int(p.limit)}` : "no limit (shedding off)";
  const bs = p.backends || [];
  const up = bs.filter((b) => b.state === "healthy" || b.state === "warming").length;
  $("kServers").textContent = `${up}/${bs.length}`;
  $("kServersSub").textContent = bs.some((b) => b.managed) ? `healthy · ${bs.filter((b) => b.managed).length} autoscaled` : "healthy";
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
  };
  es.onerror = () => { $("offline").style.display = "block"; };
}
start();
