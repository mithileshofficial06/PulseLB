"use strict";

const $ = (id) => document.getElementById(id);
const SIDE_COLOR = { classic: "--series-2", pulse: "--series-1" };
const hist = { classic: [], pulse: [] };
const ui = {};
let lastSeq = { classic: 0, pulse: 0 };
let scen = null;

const thrSeries = [
  { key: "ok", label: "Served", color: "--series-1" },
  { key: "shed", label: "Shed", color: "--warning" },
  { key: "errors", label: "Errors", color: "--critical" },
];
const vsSeries = [
  { key: "classic", label: "Classic", color: "--series-2" },
  { key: "pulse", label: "PulseLB", color: "--series-1" },
];
$("lgP99").append(legend(vsSeries));
$("lgOK").append(legend(vsSeries));
const p99Chart = new TimeChart($("cP99"), { series: vsSeries, unit: "ms", label: "p99 latency per side" });
const okChart = new TimeChart($("cOK"), { series: vsSeries, unit: "pct", max: 100, label: "Success rate per side" });

function sideUI(key, label) {
  if (ui[key]) return ui[key];
  const u = {
    succ: el("b", { class: "num" }, "–"), p99: el("b", { class: "num" }, "–"),
    err: el("b", { class: "num" }, "–"), shed: el("b", { class: "num" }, "–"),
    pills: el("div", { class: "pills" }), journal: el("ul", { class: "journal" }),
    chartHost: el("div"), sub: el("span", { class: "tag" }),
  };
  const card = el("section", { class: "card side" },
    el("div", { class: "side-head" }, el("h2", {}, el("i", { style: `--c:var(${SIDE_COLOR[key]})` }), label), u.sub),
    el("div", { class: "mini" },
      el("div", {}, el("span", {}, "Success"), u.succ),
      el("div", {}, el("span", {}, "p99 latency"), u.p99),
      el("div", {}, el("span", {}, "Errors / s"), u.err),
      el("div", {}, el("span", {}, "Shed / s"), u.shed)),
    legend(thrSeries), u.chartHost, u.pills, u.journal);
  $("sides").append(card);
  u.chart = new TimeChart(u.chartHost, { series: thrSeries, stack: true, unit: "rate", window: 90, label: label + " throughput" });
  ui[key] = u;
  return u;
}

function successPct(p) {
  const done = p.ok + p.errors + p.shed;
  return done ? (100 * p.ok) / done : null;
}

function renderSide(s) {
  const u = sideUI(s.key, s.label);
  const p = s.summary.point;
  const sp = successPct(p);
  u.succ.textContent = sp == null ? "–" : sp.toFixed(1) + "%";
  u.p99.textContent = fmt.ms(p.p99);
  u.err.textContent = fmt.rate(p.errors);
  u.shed.textContent = fmt.rate(p.shed);
  u.sub.textContent = s.key === "pulse" ? "P2C-EWMA + self-healing" : "round robin + health checks";
  u.pills.replaceChildren(...(p.backends || []).map((b) =>
    el("span", { class: "pill" }, el("span", { class: "status " + b.state }, b.name),
      el("span", { class: "num ink2" }, fmt.pct(b.share, 0)))));
  for (const e of s.events) {
    if (e.seq <= lastSeq[s.key]) continue;
    lastSeq[s.key] = e.seq;
    const li = el("li", { class: e.level + " fresh" },
      el("span", { class: "ic" }, LEVEL_ICON[e.level] || "•"),
      el("div", {}, el("time", {}, fmt.time(e.t)), e.msg));
    u.journal.prepend(li);
    setTimeout(() => li.classList.remove("fresh"), 1300);
  }
  while (u.journal.childElementCount > 60) u.journal.lastChild.remove();
  u.chart.setData(hist[s.key]);
}

function renderCompare() {
  const a = hist.classic, b = hist.pulse;
  const n = Math.min(a.length, b.length);
  const pts = [], oks = [];
  for (let i = 0; i < n; i++) {
    const pa = a[a.length - n + i], pb = b[b.length - n + i];
    pts.push({ t: pb.t, classic: pa.p99, pulse: pb.p99 });
    oks.push({ t: pb.t, classic: successPct(pa) ?? 100, pulse: successPct(pb) ?? 100 });
  }
  p99Chart.setData(pts);
  okChart.setData(oks);
}

function renderScenario(v, traffic) {
  scen = v;
  const st = v.stages || [];
  const cur = v.stage >= 0 ? st[v.stage] : null;
  $("run").textContent = v.running ? "■ Stop" : v.finished ? "↻ Run again" : "▶ Run the Tatkal Rush";
  if (v.running && cur) {
    $("stageTitle").textContent = cur.title;
    $("stageDetail").textContent = cur.detail;
  } else if (v.finished) {
    $("stageTitle").textContent = "Race finished";
    $("stageDetail").textContent = "Scoreboard below is final. Measured by the traffic generator, i.e. what users experienced.";
  }
  const pct = v.total ? Math.min(100, (100 * v.elapsed) / v.total) : 0;
  $("fill").style.width = pct + "%";
  $("clock").textContent = `${Math.floor(v.elapsed)}s / ${Math.round(v.total)}s`;
  $("trafficNow").textContent = traffic.rate ? `${fmt.int(traffic.rate)} req/s per side (${traffic.mix} mix)` : "traffic off";
  const prog = $("progress");
  prog.querySelectorAll(".tick").forEach((t) => t.remove());
  st.forEach((s) => prog.append(el("div", { class: "tick", style: `left:${(100 * s.at) / v.total}%` })));
  $("stages").replaceChildren(...st.map((s, i) =>
    el("span", { class: !v.running && !v.finished ? "" : i < v.stage || v.finished ? "done" : i === v.stage ? "now" : "" },
      `${Math.round(s.at)}s · ${s.title}`)));
  setSeg("rates", "r", String(traffic.rate));
  renderScore(v);
}

function renderScore(v) {
  const sc = v.score;
  if (!sc || sc.length < 2) { $("scoreCard").style.display = "none"; return; }
  $("scoreCard").style.display = "";
  const [c, p] = sc;
  const rows = [
    ["Requests that succeeded", (s) => s.success_pct.toFixed(2) + "%", (s) => s.success_pct, "high"],
    ["Tatkal bookings completed", (s) => `${fmt.int(s.bookings_ok)} / ${fmt.int(s.bookings)} (${s.booking_pct.toFixed(1)}%)`, (s) => s.booking_pct, "high"],
    ["Failed requests (errors, timeouts)", (s) => fmt.int(s.errors), (s) => s.errors, "low"],
    ["Politely turned away (503 + Retry-After)", (s) => fmt.int(s.shed), null, null],
    ["p50 latency of successful requests", (s) => fmt.ms(s.p50), (s) => s.p50, "low"],
    ["p99 latency of successful requests", (s) => fmt.ms(s.p99), (s) => s.p99, "low"],
    ["Most healthy servers in use", (s) => String(s.peak_servers), null, null],
  ];
  $("scoreBody").replaceChildren(...rows.map(([label, show, val, better]) => {
    let wc = "", wp = "";
    if (val && val(c) !== val(p)) {
      const pWins = better === "high" ? val(p) > val(c) : val(p) < val(c);
      if (pWins) wp = "win"; else wc = "win";
    }
    return el("tr", {}, el("td", {}, label), el("td", { class: "num " + wc }, show(c)), el("td", { class: "num " + wp }, show(p)));
  }));
  const lost = (s) => s.bookings - s.bookings_ok;
  const saved = lost(c) - lost(p);
  $("verdict").textContent = v.finished
    ? (saved > 0
      ? `PulseLB completed ${fmt.int(saved)} more Tatkal bookings than classic round robin (${c.booking_pct.toFixed(1)}% → ${p.booking_pct.toFixed(1)}%).`
      : `Both balancers completed a similar share of bookings (${c.booking_pct.toFixed(1)}% vs ${p.booking_pct.toFixed(1)}%).`)
    : "Live score (race in progress)";
}

function setSeg(id, attr, val) {
  document.querySelectorAll(`#${id} button`).forEach((b) => b.classList.toggle("on", b.dataset[attr] === val));
}

// ---- controls ----
$("run").addEventListener("click", () =>
  api("POST", "/api/arena/scenario?action=" + (scen && scen.running ? "stop" : "start")));
$("rates").addEventListener("click", (e) => {
  const r = e.target.closest("button")?.dataset.r;
  if (r != null) api("POST", `/api/traffic?rate=${r}&mix=tatkal`);
});
for (const n of ["backend-1", "backend-2", "backend-3"]) {
  const btn = (label, action, value) => el("button", { onclick: () => api("POST", `/api/arena/chaos?backend=${n}&action=${action}&value=${value}`) }, label);
  $("manualChaos").append(el("div", { class: "row" },
    el("span", { class: "tag", style: "width:70px" }, n),
    btn("Slow +400ms", "delay", 400), btn("30% errors", "errors", 0.3), btn("Crash 15s", "crash", 15), btn("Reset", "reset", 0)));
}

async function start() {
  try {
    const h = await api("GET", "/api/arena/history");
    hist.classic = h.classic || [];
    hist.pulse = h.pulse || [];
  } catch { /* stream will fill it */ }
  const es = new EventSource("/api/arena/stream");
  es.onmessage = (m) => {
    const d = JSON.parse(m.data);
    for (const s of d.sides) {
      const p = s.summary.point, h = hist[s.key];
      if (p.t && (!h.length || h[h.length - 1].t < p.t)) {
        h.push(p);
        if (h.length > 300) h.shift();
      }
      renderSide(s);
    }
    renderCompare();
    renderScenario(d.scenario, d.traffic);
  };
}
start();
