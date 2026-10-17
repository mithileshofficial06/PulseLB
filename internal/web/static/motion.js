// PulseLB — loading screen and page motion.
//
// The loading screen runs real checks (is the balancer up, how many servers
// are healthy, is race mode running, is a voice available) and prints what
// it actually found. The first visit in a session gets the full show; later
// page loads get a quick version.
"use strict";
(() => {
  const root = document.documentElement;
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const race = (p, ms, fallback) => Promise.race([p, wait(ms).then(() => fallback)]);
  const ss = {
    get: (k) => { try { return sessionStorage.getItem(k); } catch { return null; } },
    set: (k, v) => { try { sessionStorage.setItem(k, v); } catch { /* private mode */ } },
  };

  // ---------------- loading screen ----------------
  window.PULSE_BOOT = (async () => {
    const boot = document.getElementById("boot");
    if (!boot || reduce) {
      root.classList.remove("booting");
      if (boot) boot.classList.add("gone");
      return;
    }
    const first = !ss.get("pulselb-booted");
    const log = boot.querySelector(".boot-log");
    const bar = boot.querySelector(".boot-bar");
    let skipped = false;
    const finish = () => {
      if (boot.classList.contains("out")) return;
      ss.set("pulselb-booted", "1");
      boot.classList.add("out");
      root.classList.remove("booting");
      setTimeout(() => boot.classList.add("gone"), 900);
    };
    boot.querySelector(".boot-skip")?.addEventListener("click", () => { skipped = true; finish(); });
    // Never block the page for long, whatever happens.
    const hardStop = setTimeout(finish, 4500);

    const line = (label) => {
      const value = document.createElement("b");
      const li = document.createElement("li");
      li.className = "wait";
      li.append(Object.assign(document.createElement("span"), { textContent: "› " + label }), value);
      log.append(li);
      return (text, bad) => { li.className = bad ? "bad" : ""; value.textContent = text; };
    };
    const getJSON = async (path) => {
      const r = await race(fetch(path, { cache: "no-store" }), 2000, null);
      if (!r || !r.ok) return null;
      return r.json().catch(() => null);
    };
    const checks = [
      ["interface", async () => {
        const ok = document.fonts ? await race(document.fonts.ready.then(() => true), 1500, false) : true;
        return ok ? ["ready"] : ["system fonts", true];
      }],
      ["load balancer", async () => {
        const d = await getJSON("/api/summary");
        if (!d) return ["not running here", true];
        const bs = (d.point && d.point.backends) || [];
        const up = bs.filter((b) => b.state === "healthy" || b.state === "warming").length;
        return [`online · ${up}/${bs.length} servers healthy`];
      }],
      ["race mode", async () => (await getJSON("/api/arena/history")) ? ["online"] : ["not running here", true]],
      ["voice guide", async () => {
        if (!("speechSynthesis" in window)) return ["captions only", true];
        let v = speechSynthesis.getVoices();
        if (!v.length) {
          await race(new Promise((r) => speechSynthesis.addEventListener("voiceschanged", r, { once: true })), 1200);
          v = speechSynthesis.getVoices();
        }
        return v.length ? [`${v.length} voices ready`] : ["captions only", true];
      }],
    ];
    const t0 = performance.now();
    let done = 0;
    for (const [label, fn] of checks) {
      if (skipped) break;
      const set = line(label);
      const [text, bad] = await fn().catch(() => ["error", true]);
      if (first) await wait(120);
      set(text, bad);
      bar.style.setProperty("--p", (++done / checks.length).toFixed(2));
    }
    const minShow = first ? 1900 : 650;
    const left = minShow - (performance.now() - t0);
    if (left > 0 && !skipped) await wait(left);
    clearTimeout(hardStop);
    finish();
  })();

  if (reduce) return;

  // ---------------- scroll progress ----------------
  const sp = document.createElement("div");
  sp.className = "scroll-progress";
  document.body.append(sp);
  let spQueued = false;
  addEventListener("scroll", () => {
    if (spQueued) return;
    spQueued = true;
    requestAnimationFrame(() => {
      const max = document.documentElement.scrollHeight - innerHeight;
      sp.style.setProperty("--sp", max > 0 ? (scrollY / max).toFixed(4) : 0);
      spQueued = false;
    });
  }, { passive: true });

  // ---------------- headline: words rise one by one ----------------
  document.querySelectorAll("[data-split]").forEach((host) => {
    let i = 0;
    const walk = (node) => {
      for (const child of [...node.childNodes]) {
        if (child.nodeType === Node.TEXT_NODE) {
          const frag = document.createDocumentFragment();
          for (const part of child.textContent.split(/(\s+)/)) {
            if (!part) continue;
            if (/^\s+$/.test(part)) { frag.append(" "); continue; }
            const w = document.createElement("span");
            w.className = "w";
            const inner = document.createElement("span");
            inner.textContent = part;
            inner.style.setProperty("--i", i++);
            w.append(inner);
            frag.append(w);
          }
          child.replaceWith(frag);
        } else if (child.nodeType === Node.ELEMENT_NODE) {
          walk(child);
        }
      }
    };
    walk(host);
    host.classList.add("split");
  });

  // ---------------- rotating words ----------------
  document.querySelectorAll("[data-rotate]").forEach((slot) => {
    const words = slot.dataset.rotate.split("|");
    const spans = words.map((w) => Object.assign(document.createElement("span"), { textContent: w }));
    slot.replaceChildren(...spans);
    const fit = () => { slot.style.width = Math.ceil(Math.max(...spans.map((s) => s.offsetWidth))) + 4 + "px"; };
    fit();
    if (document.fonts) document.fonts.ready.then(fit);
    addEventListener("resize", fit);
    let k = 0;
    spans[0].classList.add("on");
    setInterval(() => {
      const cur = spans[k];
      k = (k + 1) % spans.length;
      cur.classList.remove("on"); cur.classList.add("off");
      setTimeout(() => cur.classList.remove("off"), 600);
      spans[k].classList.add("on");
    }, 2300);
  });

  // ---------------- 3D tilt ----------------
  document.querySelectorAll("[data-tilt]").forEach((card) => {
    const max = +(card.dataset.tilt || 6);
    card.addEventListener("pointermove", (e) => {
      const r = card.getBoundingClientRect();
      const x = (e.clientX - r.left) / r.width - 0.5, y = (e.clientY - r.top) / r.height - 0.5;
      card.style.setProperty("--ry", (x * max).toFixed(2) + "deg");
      card.style.setProperty("--rx", (-y * max).toFixed(2) + "deg");
    });
    card.addEventListener("pointerleave", () => { card.style.setProperty("--rx", "0deg"); card.style.setProperty("--ry", "0deg"); });
  });

  // ---------------- magnetic buttons ----------------
  document.querySelectorAll("[data-magnetic]").forEach((b) => {
    b.addEventListener("pointermove", (e) => {
      const r = b.getBoundingClientRect();
      b.style.setProperty("--mx2", ((e.clientX - r.left - r.width / 2) * 0.22).toFixed(1) + "px");
      b.style.setProperty("--my2", ((e.clientY - r.top - r.height / 2) * 0.3).toFixed(1) + "px");
    });
    b.addEventListener("pointerleave", () => { b.style.setProperty("--mx2", "0px"); b.style.setProperty("--my2", "0px"); });
  });

  // ---------------- floating shapes follow the cursor (parallax) ----------------
  const fl = document.querySelector(".floaters");
  const hero = document.getElementById("hero");
  if (fl && hero) {
    hero.addEventListener("pointermove", (e) => {
      const r = hero.getBoundingClientRect();
      fl.style.setProperty("--px", (((e.clientX - r.left) / r.width - 0.5) * 2).toFixed(3));
      fl.style.setProperty("--py", (((e.clientY - r.top) / r.height - 0.5) * 2).toFixed(3));
    });
  }
})();
