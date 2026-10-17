// Hero animation: a miniature PulseLB routing live "requests" (dots) from
// users to three servers. Every 16 seconds a server slows down, another
// crashes, a replacement is launched and everything recovers, and the dots
// visibly reroute. The routing uses the same idea as the real balancer:
// pick two servers at random, send the request to the one with the lower
// latency × queue score.
"use strict";
(() => {
  const cv = document.getElementById("sim");
  if (!cv) return;
  const cap = document.getElementById("simCaption");
  const x = cv.getContext("2d");
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  // Colours and shapes come from CSS variables, so the drawing follows the
  // page's skin (smooth or brutal).
  const C = {};
  let RAD = 10, GLOW = 1;
  function readTheme() {
    const cs = getComputedStyle(document.documentElement);
    const v = (n, d) => cs.getPropertyValue(n).trim() || d;
    C.mint = v("--sim-ok", "#3ddc97"); C.blue = v("--sim-a", "#5b9dff"); C.amber = v("--sim-warn", "#ffc24b");
    C.red = v("--sim-bad", "#ff5470"); C.node = v("--sim-node", "#3ddc97"); C.ink = v("--ink", "#e9eef6");
    C.muted = v("--muted", "#6b778a"); C.surface = v("--surface-2", "#131a26");
    RAD = +v("--sim-radius", "10"); GLOW = +v("--sim-glow", "1");
    C.line = GLOW ? "rgba(255,255,255,.08)" : "rgba(242,239,230,.22)";
  }
  readTheme();
  new MutationObserver(readTheme).observe(document.documentElement, { attributes: true, attributeFilter: ["data-skin"] });

  let W = 0, H = 0;
  const lb = { x: 0, y: 0 }, users = { x: 0, y: 0 };
  const mk = (name) => ({ name, extra: 0, state: "up", alpha: 1, target: 1, warm: 1, inflight: 0, rate: 0, hits: 0, flash: 0, x: 0, y: 0 });
  const servers = [mk("backend-1"), mk("backend-2"), mk("backend-3"), Object.assign(mk("auto-1"), { alpha: 0, target: 0, state: "off" })];
  const dots = [];

  function resize() {
    const dpr = devicePixelRatio || 1;
    const r = cv.getBoundingClientRect();
    W = r.width; H = r.height;
    cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr);
    x.setTransform(dpr, 0, 0, dpr, 0, 0);
  }
  function layout() {
    users.x = Math.max(34, W * 0.08); users.y = H / 2;
    lb.x = W * 0.42; lb.y = H / 2;
    const vis = servers.filter((s) => s.alpha > 0.02 || s.target > 0);
    vis.forEach((s, i) => {
      const ty = H * (i + 1) / (vis.length + 1);
      s.y = s.y ? s.y + (ty - s.y) * 0.08 : ty;
      s.x = W - Math.min(150, W * 0.36) - 8;
    });
  }

  // ---- the scripted story, looping every 16 s ----
  const phases = [
    { at: 0, cap: "Normal traffic · P2C spreads the load evenly", tone: "mint", run: () => {
      servers.forEach((s, i) => { if (i < 3) { s.state = "up"; s.extra = 0; s.target = 1; } });
      const a = servers[3]; a.target = 0; a.state = "off";
    } },
    { at: 4, cap: "backend-2 slowed by +400ms · traffic flows around it", tone: "amber", run: () => { servers[1].state = "slow"; servers[1].extra = 400; } },
    { at: 8, cap: "backend-3 crashed · ejected within milliseconds", tone: "red", run: () => { servers[2].state = "down"; servers[2].flash = 1; } },
    { at: 9.6, cap: "auto-1 launched to replace lost capacity", tone: "blue", run: () => { const a = servers[3]; a.state = "warm"; a.target = 1; a.warm = 0.1; } },
    { at: 12.5, cap: "Servers repaired · warming up with slow start", tone: "mint", run: () => {
      servers[1].state = "up"; servers[1].extra = 0;
      servers[2].state = "warm"; servers[2].warm = 0.1;
    } },
    { at: 15, cap: "Rush handled · surplus server retired", tone: "mint", run: () => { const a = servers[3]; a.target = 0; a.state = "drain"; } },
  ];
  let clock = 0, phaseIdx = -1;
  function story(dt) {
    clock = (clock + dt) % 16;
    let idx = 0;
    for (let i = 0; i < phases.length; i++) if (clock >= phases[i].at) idx = i;
    if (idx !== phaseIdx) {
      if (idx < phaseIdx || phaseIdx === -1) phases[0].run();
      phaseIdx = idx;
      phases[idx].run();
      if (cap) {
        cap.textContent = phases[idx].cap;
        cap.dataset.tone = phases[idx].tone;
        cap.classList.remove("swap"); void cap.offsetWidth; cap.classList.add("swap");
      }
    }
  }

  // ---- routing ----
  const live = () => servers.filter((s) => s.state !== "down" && s.state !== "off" && s.state !== "drain" && s.alpha > 0.3);
  function pick() {
    const c = live();
    if (!c.length) return null;
    if (c.length === 1) return c[0];
    const a = c[Math.floor(Math.random() * c.length)];
    let b = a;
    while (b === a) b = c[Math.floor(Math.random() * c.length)];
    const score = (s) => (12 + s.extra) * (s.inflight + 1) / Math.max(0.1, s.warm);
    return score(a) <= score(b) ? a : b;
  }

  let spawnAcc = 0;
  function update(dt) {
    story(dt);
    layout();
    for (const s of servers) {
      s.alpha += (s.target - s.alpha) * Math.min(1, dt * 3);
      if (s.state === "warm") { s.warm = Math.min(1, s.warm + dt / 2.5); if (s.warm >= 1) s.state = "up"; }
      s.flash = Math.max(0, s.flash - dt * 1.2);
      s.rate += (s.hits / Math.max(dt, 1e-3) - s.rate) * Math.min(1, dt * 2);
      s.hits = 0;
    }
    spawnAcc += dt * 46;
    while (spawnAcc >= 1) {
      spawnAcc--;
      dots.push({ stage: 0, t: 0, ox: users.x + (Math.random() - 0.5) * 14, oy: users.y + (Math.random() - 0.5) * 40, s: null, jitter: (Math.random() - 0.5) * 26, hue: Math.random() });
    }
    for (let i = dots.length - 1; i >= 0; i--) {
      const d = dots[i];
      if (d.stage === 0) {
        d.t += dt * 1.5;
        if (d.t >= 1) {
          d.s = pick();
          if (!d.s) { dots.splice(i, 1); continue; }
          d.s.inflight++; d.stage = 1; d.t = 0;
        }
      } else {
        const s = d.s;
        if (s.state === "down" && d.t > 0.5) { s.flash = Math.max(s.flash, 0.6); s.inflight--; dots.splice(i, 1); continue; }
        d.t += dt * 1.25 / (1 + s.extra / 140);
        if (d.t >= 1) { s.inflight--; s.hits++; dots.splice(i, 1); }
      }
    }
  }

  // ---- drawing ----
  function rr(px, py, w, h, r) {
    x.beginPath();
    x.moveTo(px + r, py); x.arcTo(px + w, py, px + w, py + h, r); x.arcTo(px + w, py + h, px, py + h, r);
    x.arcTo(px, py + h, px, py, r); x.arcTo(px, py, px + w, py, r); x.closePath();
  }
  const stateColor = (s) => s.state === "down" ? C.red : s.state === "slow" ? C.amber : s.state === "warm" ? C.blue : C.mint;
  const curve = (s, t, jitter) => {
    const cx = (lb.x + s.x) / 2, cy = (lb.y + s.y) / 2 + jitter;
    const u = 1 - t;
    return [u * u * lb.x + 2 * u * t * cx + t * t * s.x, u * u * lb.y + 2 * u * t * cy + t * t * (s.y)];
  };

  let pulse = 0;
  function draw(dt) {
    pulse += dt;
    x.clearRect(0, 0, W, H);
    const totalRate = servers.reduce((a, s) => a + s.rate, 0) || 1;

    // links
    for (const s of servers) {
      if (s.alpha < 0.02) continue;
      x.globalAlpha = s.alpha * (s.state === "down" ? 0.35 : 0.9);
      x.strokeStyle = s.state === "down" ? "rgba(255,84,112,.25)" : C.line;
      x.lineWidth = 1.5;
      x.setLineDash(s.state === "down" ? [4, 6] : []);
      x.beginPath(); x.moveTo(lb.x, lb.y); x.quadraticCurveTo((lb.x + s.x) / 2, (lb.y + s.y) / 2, s.x, s.y); x.stroke();
      x.setLineDash([]);
    }
    x.globalAlpha = 1;
    x.strokeStyle = C.line;
    x.beginPath(); x.moveTo(users.x, users.y); x.lineTo(lb.x, lb.y); x.stroke();

    // dots
    for (const d of dots) {
      let px, py;
      if (d.stage === 0) { px = d.ox + (lb.x - d.ox) * d.t; py = d.oy + (lb.y - d.oy) * d.t; }
      else [px, py] = curve(d.s, d.t, d.jitter);
      const col = d.stage === 1 && d.s.state === "down" ? C.red : d.stage === 1 && d.s.state === "slow" ? C.amber : d.hue > 0.5 ? C.mint : C.blue;
      x.fillStyle = col;
      x.shadowColor = col; x.shadowBlur = 8 * GLOW;
      x.beginPath();
      if (GLOW) x.arc(px, py, 2.4, 0, Math.PI * 2); else x.rect(px - 2.5, py - 2.5, 5, 5);
      x.fill();
    }
    x.shadowBlur = 0;

    // users
    for (let i = -1; i <= 1; i++) {
      x.fillStyle = "rgba(233,238,246,.85)";
      x.beginPath(); x.arc(users.x + i * 11, users.y - 6 + Math.abs(i) * 4, 5, 0, Math.PI * 2); x.fill();
      x.beginPath(); x.ellipse(users.x + i * 11, users.y + 9 + Math.abs(i) * 4, 8, 6, 0, Math.PI, 0); x.fill();
    }
    x.fillStyle = C.muted; x.font = "500 11px 'Space Grotesk', system-ui"; x.textAlign = "center";
    x.fillText("USERS", users.x, users.y + 34);

    // load balancer
    const R = 26;
    const ring = (pulse % 1.6) / 1.6;
    if (GLOW) {
      x.strokeStyle = `rgba(61,220,151,${0.6 * (1 - ring)})`; x.lineWidth = 2;
      x.beginPath(); x.arc(lb.x, lb.y, R + ring * 22, 0, Math.PI * 2); x.stroke();
      const g = x.createRadialGradient(lb.x - 8, lb.y - 8, 4, lb.x, lb.y, R);
      g.addColorStop(0, "#9fffd6"); g.addColorStop(0.5, C.mint); g.addColorStop(1, "#1b5d73");
      x.fillStyle = g; x.shadowColor = C.mint; x.shadowBlur = 26;
      x.beginPath(); x.arc(lb.x, lb.y, R, 0, Math.PI * 2); x.fill();
      x.shadowBlur = 0;
    } else {
      // Brutal: a flat square with a hard shadow and an expanding square ring.
      const k = R + ring * 18;
      x.strokeStyle = C.node; x.globalAlpha = 1 - ring; x.lineWidth = 3;
      x.strokeRect(lb.x - k, lb.y - k, k * 2, k * 2); x.globalAlpha = 1;
      x.fillStyle = C.ink; x.fillRect(lb.x - R + 6, lb.y - R + 6, R * 2, R * 2);
      x.fillStyle = C.node; x.fillRect(lb.x - R, lb.y - R, R * 2, R * 2);
      x.strokeStyle = "#0c0c0c"; x.lineWidth = 3; x.strokeRect(lb.x - R, lb.y - R, R * 2, R * 2);
    }
    x.strokeStyle = "#04140c"; x.lineWidth = 2.5; x.beginPath();
    const ex = lb.x - 14;
    x.moveTo(ex, lb.y); x.lineTo(ex + 7, lb.y); x.lineTo(ex + 11, lb.y - 9); x.lineTo(ex + 16, lb.y + 9); x.lineTo(ex + 20, lb.y); x.lineTo(ex + 28, lb.y); x.stroke();
    x.fillStyle = C.ink; x.font = GLOW ? "600 12px 'Space Grotesk', system-ui" : "12px 'Archivo Black', 'Arial Black', sans-serif";
    x.fillText(GLOW ? "PulseLB" : "PULSELB", lb.x, lb.y + R + 18);

    // servers
    const cw = Math.min(150, W * 0.36), ch = 44;
    x.textAlign = "left";
    for (const s of servers) {
      if (s.alpha < 0.02) continue;
      const px = s.x, py = s.y - ch / 2, col = stateColor(s);
      x.globalAlpha = s.alpha;
      if (!GLOW) { x.fillStyle = s.state === "down" ? C.red : s.state === "slow" ? C.amber : C.ink; x.fillRect(px + 5, py + 5, cw, ch); }
      rr(px, py, cw, ch, RAD);
      x.fillStyle = s.state === "down" ? (GLOW ? "rgba(255,84,112,.10)" : "#2a0d0d") : (GLOW ? "rgba(19,26,38,.95)" : C.surface);
      x.fill();
      x.strokeStyle = !GLOW ? C.ink : s.flash > 0 ? `rgba(255,84,112,${0.4 + s.flash * 0.6})` : s.state === "slow" ? "rgba(255,194,75,.55)" : "rgba(255,255,255,.12)";
      x.lineWidth = GLOW ? (s.flash > 0 ? 2 : 1) : 3;
      if (s.flash > 0) { x.shadowColor = C.red; x.shadowBlur = 20 * s.flash; }
      x.stroke(); x.shadowBlur = 0;
      x.fillStyle = col; x.shadowColor = col; x.shadowBlur = 10 * GLOW;
      x.beginPath();
      if (GLOW) x.arc(px + 13, py + 15, 3.5, 0, Math.PI * 2); else x.rect(px + 9, py + 11, 8, 8);
      x.fill(); x.shadowBlur = 0;
      x.fillStyle = C.ink; x.font = `600 ${cw < 120 ? 10 : 12}px 'Space Grotesk', system-ui`;
      x.fillText(s.name, px + 23, py + 19);
      const share = s.rate / totalRate;
      const bh = GLOW ? 5 : 7, br = GLOW ? 2.5 : 0;
      x.fillStyle = "rgba(255,255,255,.1)"; rr(px + 12, py + 28, cw - 58, bh, br); x.fill();
      if (share > 0.003) { x.fillStyle = col; rr(px + 12, py + 28, Math.max(4, (cw - 58) * Math.min(1, share * 2)), bh, br); x.fill(); }
      x.fillStyle = C.muted; x.font = "500 10px 'JetBrains Mono', monospace"; x.textAlign = "right";
      x.fillText(s.state === "down" ? "DOWN" : `${Math.round(share * 100)}%`, px + cw - 10, py + 34);
      x.textAlign = "left";
    }
    x.globalAlpha = 1;
  }

  let last = performance.now(), visible = true, raf = 0;
  function frame(now) {
    const dt = Math.min(0.05, (now - last) / 1000);
    last = now;
    update(dt); draw(dt);
    raf = visible ? requestAnimationFrame(frame) : 0;
  }
  new ResizeObserver(() => { resize(); layout(); if (reduce) { update(0.016); draw(0); } }).observe(cv);
  new IntersectionObserver(([e]) => {
    visible = e.isIntersecting;
    if (visible && !raf && !reduce) { last = performance.now(); raf = requestAnimationFrame(frame); }
  }).observe(cv);
  resize(); layout();
  if (reduce) {
    // A single calm frame instead of the animation.
    for (let i = 0; i < 120; i++) update(1 / 60);
    draw(0);
  }
})();
