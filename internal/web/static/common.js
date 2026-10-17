// Shared helpers and a small dependency-free canvas time-series chart.
"use strict";

const fmt = {
  int: (n) => Math.round(n || 0).toLocaleString("en-IN"),
  rate: (n) => {
    n = n || 0;
    return n >= 10000 ? (n / 1000).toFixed(1) + "k" : Math.round(n).toLocaleString("en-IN");
  },
  ms: (v) => {
    if (!v) return "0ms";
    if (v >= 1000) return (v / 1000).toFixed(1) + "s";
    if (v >= 10) return Math.round(v) + "ms";
    return v.toFixed(1) + "ms";
  },
  pct: (v, d = 1) => (v * 100).toFixed(d) + "%",
  time: (ms) => new Date(ms).toLocaleTimeString([], { hour12: false }),
};

async function api(method, path) {
  const res = await fetch(path, { method, headers: { "X-Pulse-Admin": "1" } });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  const ct = res.headers.get("content-type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "style") e.style.cssText = v;
    else e.setAttribute(k, v);
  }
  for (const k of kids) if (k != null) e.append(k);
  return e;
}

function cssVar(node, name) {
  return getComputedStyle(node).getPropertyValue(name).trim();
}

function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

// TimeChart draws the last `window` points of one or more series.
// opts: { series: [{key, label, color: "--series-1", dash}], stack, unit: "rate"|"ms",
//         ref: () => ({value, label}) | null, window }
class TimeChart {
  constructor(host, opts) {
    this.host = host;
    this.opts = Object.assign({ window: 120, unit: "rate" }, opts);
    this.points = [];
    this.hover = -1;
    host.classList.add("chart");
    this.canvas = el("canvas", { role: "img", "aria-label": opts.label || "chart" });
    this.tip = el("div", { class: "tip" });
    host.append(this.canvas, this.tip);
    this.canvas.addEventListener("mousemove", (e) => this.onMove(e));
    this.canvas.addEventListener("mouseleave", () => { this.hover = -1; this.tip.style.display = "none"; this.draw(); });
    new ResizeObserver(() => this.draw()).observe(host);
    matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => this.draw());
  }

  setData(points) {
    this.points = points.slice(-this.opts.window);
    this.draw();
    if (this.hover >= 0) this.showTip();
  }

  geom() {
    const w = this.host.clientWidth, h = this.host.clientHeight;
    return { w, h, l: 48, r: 12, t: 8, b: 22 };
  }

  fmtY(v) { return this.opts.unit === "ms" ? fmt.ms(v) : fmt.rate(v); }

  draw() {
    const c = this.canvas, g = this.geom();
    const dpr = window.devicePixelRatio || 1;
    if (c.width !== Math.round(g.w * dpr) || c.height !== Math.round(g.h * dpr)) {
      c.width = Math.round(g.w * dpr);
      c.height = Math.round(g.h * dpr);
    }
    const x = c.getContext("2d");
    x.setTransform(dpr, 0, 0, dpr, 0, 0);
    x.clearRect(0, 0, g.w, g.h);
    const host = this.host;
    const ink = cssVar(host, "--muted"), grid = cssVar(host, "--grid"), axis = cssVar(host, "--axis"), surf = cssVar(host, "--surface");
    const S = this.opts.series, P = this.points, N = this.opts.window;
    const pw = g.w - g.l - g.r, ph = g.h - g.t - g.b;

    // y range
    let max = 0;
    for (const p of P) {
      if (this.opts.stack) max = Math.max(max, S.reduce((a, s) => a + (p[s.key] || 0), 0));
      else for (const s of S) max = Math.max(max, p[s.key] || 0);
    }
    const ref = this.opts.ref && this.opts.ref();
    if (ref && ref.value) max = Math.max(max, ref.value * 1.15);
    max = niceMax(max * 1.05);
    this.max = max;
    const X = (i) => g.l + pw * ((N - P.length + i) / (N - 1));
    const Y = (v) => g.t + ph - (v / max) * ph;
    this.X = X;

    // grid + y labels
    x.font = "11px system-ui, sans-serif";
    x.textBaseline = "middle";
    x.textAlign = "right";
    for (let i = 0; i <= 4; i++) {
      const v = (max / 4) * i, y = Math.round(Y(v)) + 0.5;
      x.strokeStyle = i === 0 ? axis : grid;
      x.lineWidth = 1;
      x.beginPath(); x.moveTo(g.l, y); x.lineTo(g.l + pw, y); x.stroke();
      x.fillStyle = ink;
      x.fillText(this.fmtY(v), g.l - 8, y);
    }
    // x labels: seconds ago
    x.textAlign = "center";
    x.textBaseline = "top";
    for (const ago of [120, 90, 60, 30, 0]) {
      if (ago >= N) continue;
      const xx = g.l + pw * ((N - 1 - ago) / (N - 1));
      x.fillStyle = ink;
      x.fillText(ago === 0 ? "now" : `-${ago}s`, xx, g.t + ph + 6);
    }
    if (!P.length) return;

    if (this.opts.stack) {
      const base = new Array(P.length).fill(0);
      const tops = [];
      for (const s of S) {
        const top = P.map((p, i) => base[i] + (p[s.key] || 0));
        x.beginPath();
        P.forEach((_, i) => (i ? x.lineTo(X(i), Y(top[i])) : x.moveTo(X(i), Y(top[i]))));
        for (let i = P.length - 1; i >= 0; i--) x.lineTo(X(i), Y(base[i]));
        x.closePath();
        x.fillStyle = cssVar(host, s.color);
        x.globalAlpha = 0.85;
        x.fill();
        x.globalAlpha = 1;
        tops.push(top);
        top.forEach((v, i) => (base[i] = v));
      }
      // 2px surface gap between stacked bands
      x.strokeStyle = surf;
      x.lineWidth = 2;
      for (const top of tops.slice(0, -1)) {
        x.beginPath();
        P.forEach((_, i) => (i ? x.lineTo(X(i), Y(top[i])) : x.moveTo(X(i), Y(top[i]))));
        x.stroke();
      }
    } else {
      for (const s of S) {
        x.beginPath();
        P.forEach((p, i) => (i ? x.lineTo(X(i), Y(p[s.key] || 0)) : x.moveTo(X(i), Y(p[s.key] || 0))));
        x.strokeStyle = cssVar(host, s.color);
        x.lineWidth = 2;
        x.lineJoin = "round";
        x.setLineDash(s.dash ? [5, 4] : []);
        x.stroke();
        x.setLineDash([]);
      }
    }

    if (ref && ref.value && ref.value <= max) {
      const y = Math.round(Y(ref.value)) + 0.5;
      x.strokeStyle = cssVar(host, "--ink-2");
      x.globalAlpha = 0.6;
      x.setLineDash([4, 4]);
      x.lineWidth = 1;
      x.beginPath(); x.moveTo(g.l, y); x.lineTo(g.l + pw, y); x.stroke();
      x.setLineDash([]);
      x.globalAlpha = 1;
      x.fillStyle = cssVar(host, "--ink-2");
      x.textAlign = "left";
      x.textBaseline = "bottom";
      x.fillText(ref.label, g.l + 6, y - 3);
    }

    if (this.hover >= 0 && this.hover < P.length) {
      const hx = Math.round(X(this.hover)) + 0.5;
      x.strokeStyle = cssVar(host, "--ink-2");
      x.lineWidth = 1;
      x.beginPath(); x.moveTo(hx, g.t); x.lineTo(hx, g.t + ph); x.stroke();
      let acc = 0;
      for (const s of S) {
        let v = P[this.hover][s.key] || 0;
        if (this.opts.stack) { acc += v; v = acc; }
        x.beginPath();
        x.arc(hx, Y(v), 4, 0, Math.PI * 2);
        x.fillStyle = cssVar(host, s.color);
        x.strokeStyle = surf;
        x.lineWidth = 2;
        x.fill(); x.stroke();
      }
    }
  }

  onMove(e) {
    const P = this.points;
    if (!P.length) return;
    const r = this.canvas.getBoundingClientRect(), g = this.geom();
    const mx = e.clientX - r.left;
    const pw = g.w - g.l - g.r, N = this.opts.window;
    const idx = Math.round((mx - g.l) / pw * (N - 1)) - (N - P.length);
    this.hover = Math.max(0, Math.min(P.length - 1, idx));
    this.draw();
    this.showTip();
  }

  showTip() {
    const p = this.points[this.hover];
    if (!p) return;
    const rows = this.opts.series.map((s) =>
      `<div class="r"><span><i style="--c:var(${s.color})"></i>${s.label}</span><span class="num">${this.fmtY(p[s.key] || 0)}${this.opts.unit === "rate" ? "/s" : ""}</span></div>`).join("");
    this.tip.innerHTML = `<div class="t">${fmt.time(p.t)}</div>${rows}`;
    const g = this.geom();
    const hx = this.X(this.hover);
    this.tip.style.display = "block";
    const tw = this.tip.offsetWidth;
    this.tip.style.left = (hx + 12 + tw > g.w ? hx - tw - 12 : hx + 12) + "px";
    this.tip.style.top = "8px";
  }
}

function legend(series) {
  return el("div", { class: "legend" }, ...series.map((s) =>
    el("span", {}, el("i", { class: s.dash ? "dash" : "", style: `--c:var(${s.color})` }), s.label)));
}

const STATE_LABEL = {
  healthy: "Healthy", warming: "Warming up", ejected: "Ejected", down: "Down", draining: "Draining",
};
const LEVEL_ICON = { info: "•", good: "✓", warn: "▲", crit: "✕" };
