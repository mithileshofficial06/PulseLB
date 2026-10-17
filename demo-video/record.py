"""Records the PulseLB demo video with on-screen captions.

Drives a real Chrome against the running demo (bin/demo.exe), clicks the
real controls, shows a caption for each narration line, and logs when each
line starts. build.py then adds the voiceover at those times.
"""
import ctypes
import json
import os
import sys
import time

from playwright.sync_api import sync_playwright

HERE = os.path.dirname(os.path.abspath(__file__))
BASE = "http://127.0.0.1:9000"
VP = {"width": 1440, "height": 810}
RATE = 2000  # control-room users per second
META = json.load(open(os.path.join(HERE, "tts", "lines.json"), encoding="utf-8"))
LINES, WPS = META["lines"], META["words_per_sec"]
ADMIN = {"X-Pulse-Admin": "1"}

# Keep the laptop awake while recording (reverts when this process exits).
try:
    ctypes.windll.kernel32.SetThreadExecutionState(0x80000000 | 0x00000001 | 0x00000002)
except Exception:
    pass

INIT_JS = r"""
(() => {
  try {
    localStorage.setItem('pulselb-skin', 'brutal');
    ['home', 'dashboard', 'arena'].forEach((p) => sessionStorage.setItem('pg-hello-' + p, '1'));
  } catch (e) {}
  const CSS = `
    #__cap { position: fixed; left: 50%; bottom: 46px; z-index: 5000; display: flex; gap: 18px; align-items: center;
      width: max-content; max-width: min(1560px, 92vw); padding: 18px 26px;
      background: #09080b; color: #e6dccb; border: 3px solid #e6dccb; box-shadow: 9px 9px 0 #e0a43a;
      font: 600 31px/1.35 "Space Grotesk", "Segoe UI", sans-serif;
      transform: translate(-50%, 40px); opacity: 0; transition: transform .45s cubic-bezier(.2,.9,.2,1), opacity .35s; }
    #__cap.show { transform: translate(-50%, 0); opacity: 1; }
    #__cap b { font: 400 17px "Archivo Black", "Arial Black", sans-serif; text-transform: uppercase; white-space: nowrap;
      background: #e0a43a; color: #09080b; padding: 7px 11px; border: 2px solid #e6dccb; }
    .__fx { position: fixed; z-index: 4999; width: 22px; height: 22px; margin: -11px 0 0 -11px; pointer-events: none;
      border: 4px solid #e0a43a; animation: __ring .9s ease-out forwards; }
    @keyframes __ring { to { transform: scale(5.5); opacity: 0; } }
    .__hit { outline: 5px solid #e0a43a !important; outline-offset: 4px; }
  `;
  const install = () => {
    if (document.getElementById('__vcss')) return;
    const st = document.createElement('style'); st.id = '__vcss'; st.textContent = CSS; document.head.append(st);
    const c = document.createElement('div'); c.id = '__cap'; document.body.append(c);
  };
  window.__cap = (text, label) => {
    install();
    const c = document.getElementById('__cap');
    c.classList.remove('show');
    void c.offsetWidth;
    c.innerHTML = (label ? '<b>' + label + '</b>' : '') + '<span>' + text + '</span>';
    c.classList.add('show');
  };
  window.__capHide = () => { const c = document.getElementById('__cap'); if (c) c.classList.remove('show'); };
  window.__click = (sel) => {
    install();
    const t = document.querySelector(sel);
    if (!t) return false;
    const r = t.getBoundingClientRect();
    const fx = document.createElement('div');
    fx.className = '__fx';
    fx.style.left = (r.left + r.width / 2) + 'px';
    fx.style.top = (r.top + r.height / 2) + 'px';
    document.body.append(fx);
    t.classList.add('__hit');
    setTimeout(() => { fx.remove(); t.classList.remove('__hit'); }, 1000);
    return true;
  };
  // Eased scroll that brings an element to a given spot on screen.
  window.__scroll = (sel, ms, where) => new Promise((res) => {
    const t = typeof sel === 'number' ? null : document.querySelector(sel);
    const y0 = scrollY;
    let y1 = typeof sel === 'number' ? sel : t ? scrollY + t.getBoundingClientRect().top - (where === 'center'
      ? (innerHeight - t.getBoundingClientRect().height) / 2 : 90) : y0;
    y1 = Math.max(0, Math.min(y1, document.documentElement.scrollHeight - innerHeight));
    const s = performance.now();
    const step = (n) => {
      const k = Math.min(1, (n - s) / ms), e = k < .5 ? 4 * k * k * k : 1 - Math.pow(-2 * k + 2, 3) / 2;
      scrollTo(0, y0 + (y1 - y0) * e);
      if (k < 1) requestAnimationFrame(step); else res();
    };
    requestAnimationFrame(step);
  });
})();
"""

CARD_CSS = """
<link href="https://fonts.googleapis.com/css2?family=Archivo+Black&family=Space+Grotesk:wght@500;600&family=Space+Mono:wght@700&display=swap" rel="stylesheet">
<style>
  html, body { margin: 0; height: 100%; background: #09080b; color: #e6dccb; overflow: hidden;
    font-family: "Space Grotesk", sans-serif;
    background-image: radial-gradient(rgba(230,220,203,.10) 1.2px, transparent 1.2px); background-size: 22px 22px; }
  .wrap { height: 100%; display: grid; place-items: center; }
  .box { text-align: center; }
  .mark { display: inline-block; font: 400 150px/1 "Archivo Black", sans-serif; letter-spacing: -4px; }
  .mark span { background: #e0a43a; color: #09080b; padding: 0 18px; margin-left: 10px; box-shadow: 12px 12px 0 #e6dccb; }
  .sub { font: 700 22px "Space Mono", monospace; text-transform: uppercase; letter-spacing: 3px; margin-top: 46px; color: #bdb2a1; }
  .chips { margin-top: 34px; display: flex; gap: 18px; justify-content: center; }
  .chips div { border: 3px solid #e6dccb; padding: 12px 18px; font: 700 20px "Space Mono", monospace; box-shadow: 6px 6px 0 #2aa58c; }
  .row { display: flex; gap: 26px; justify-content: center; margin-top: 40px; }
  .stat { border: 3px solid #e6dccb; padding: 22px 30px; min-width: 300px; box-shadow: 8px 8px 0 #e0a43a; text-align: left; }
  .stat b { display: block; font: 400 64px/1 "Archivo Black", sans-serif; }
  .stat span { font: 700 18px "Space Mono", monospace; text-transform: uppercase; color: #bdb2a1; }
  .stat.p { background: #2aa58c; color: #09080b; } .stat.p span { color: #09080b; }
  .gh { margin-top: 44px; font: 700 26px "Space Mono", monospace; color: #e0a43a; }
  .in { animation: up .8s cubic-bezier(.2,.9,.2,1) both; } @keyframes up { from { transform: translateY(40px); opacity: 0; } }
</style>"""

TITLE_HTML = CARD_CSS + """
<div class="wrap"><div class="box">
  <div class="mark in">PULSE<span>LB</span></div>
  <div class="sub in" style="animation-delay:.25s">a self-healing load balancer · built from scratch in Go</div>
  <div class="chips in" style="animation-delay:.5s"><div>10,000 req/s verified</div><div>Self-healing</div><div>Race vs round robin</div></div>
</div></div>"""


def outro_html(score):
    c, p = score
    return CARD_CSS + f"""
<div class="wrap"><div class="box">
  <div class="mark in" style="font-size:96px">PULSE<span>LB</span></div>
  <div class="row in" style="animation-delay:.2s">
    <div class="stat"><b>{c['booking_pct']:.1f}%</b><span>bookings · round robin</span></div>
    <div class="stat p"><b>{p['booking_pct']:.1f}%</b><span>bookings · PulseLB</span></div>
    <div class="stat"><b>{p['errors']:,}</b><span>failed requests · PulseLB</span></div>
  </div>
  <div class="gh in" style="animation-delay:.45s">github.com/mithileshofficial06/PulseLB</div>
</div></div>"""


def main():
    events = []
    with sync_playwright() as pw:
        browser = pw.chromium.launch(channel="chrome")
        ctx = browser.new_context(viewport=VP, record_video_dir=os.path.join(HERE, "raw"), record_video_size=VP)
        ctx.add_init_script(INIT_JS)
        page = ctx.new_page()
        T0 = time.monotonic()
        now = lambda: time.monotonic() - T0
        js = page.evaluate
        sleep = lambda s: page.wait_for_timeout(int(s * 1000))

        def cap(key, label=None, text=None):
            """Show a caption, log it, and return the time its voice ends."""
            text = text or LINES[key]["text"]
            dur = LINES[key]["dur"] if key in LINES else len(text.split()) / WPS * 1.12 + 0.4
            js("([t, l]) => window.__cap(t, l)", [text, label])
            t = now() + 0.3
            events.append({"t": round(t, 2), "key": key, "text": text, "file": LINES.get(key, {}).get("file")})
            print(f"[{t:6.1f}s] {key}: {text[:70]}", flush=True)
            return t + dur + 0.45

        def hold(until):
            left = until - now()
            if left > 0:
                sleep(left)

        def scroll(sel, ms=1100, where="top"):
            js("([s, m, w]) => window.__scroll(s, m, w)", [sel, ms, where])

        def click(sel):
            js("(s) => window.__click(s)", sel)
            sleep(0.35)
            page.click(sel)

        def state(expr):
            return js(f"() => {{ try {{ return {expr}; }} catch (e) {{ return null; }} }}")

        # ---------------- 1. title ----------------
        page.set_content(TITLE_HTML)
        sleep(0.8)
        hold(cap("title", "PulseLB"))

        # ---------------- 2. home page ----------------
        page.goto(BASE + "/", wait_until="domcontentloaded")
        sleep(2.4)  # the loading screen runs its startup checks on camera
        hold(cap("hero", "Home"))
        end = cap("flow", "Home")
        scroll("#problem", 1300); sleep(2.6)
        scroll("#flow", 1300); sleep(2.6)
        scroll("#features", 1300)
        hold(end)
        scroll("#results", 1300); sleep(1.4)
        hold(cap("results", "Proof"))

        # ---------------- 3. control room ----------------
        page.goto(BASE + "/dashboard.html", wait_until="domcontentloaded")
        sleep(1.2)
        end = cap("cr_intro", "Control room")
        sleep(4); scroll("#nowBanner", 1300)
        hold(end)

        scroll("#trafficCard", 900, "center"); sleep(1.1)
        click(f'#rates button[data-r="{RATE}"]')
        sleep(1.0)
        scroll("#nowBanner", 1000); sleep(3.5)
        p = state("window.PULSE_STATE.summary.point") or {}
        done = (p.get("ok", 0) + p.get("errors", 0) + p.get("shed", 0)) or 1
        text = (f"Sending two thousand users per second. {round(p.get('ok', 0)):,} requests a second are answered, "
                f"{100 * p.get('ok', 0) / done:.1f} percent OK, and the slowest one percent finish in "
                f"{round(p.get('p99', 0))} milliseconds.")
        hold(cap("dyn_5k", "Step 1 · traffic", text))

        scroll('[data-be="backend-2"]', 1100, "center"); sleep(1.3)
        click('[data-be="backend-2"] .chaos button:nth-child(1)')
        hold(cap("cr_slow", "Step 3 · break it"))
        sleep(2.0)
        share = state("window.PULSE_STATE.summary.point.backends.find(b => b.name === 'backend-2').share") or 0
        hold(cap("dyn_share", "Self-healing",
                 f"PulseLB noticed: backend-2 now gets only {max(1, round(share * 100))} percent of the traffic, and users don't feel it."))

        scroll("#routingCard", 1000, "center"); sleep(1.1)
        click('#algo button[data-a="rr"]')
        end = cap("cr_rr", "Round robin")
        sleep(1.2); scroll('[data-be="backend-2"]', 1000, "center"); sleep(3.2)
        scroll("#tiles", 1000)
        hold(end); sleep(0.6)

        scroll("#routingCard", 1000, "center"); sleep(1.1)
        click('#algo button[data-a="p2c"]')
        end = cap("cr_p2c", "Smart P2C")
        sleep(1.0); scroll("#serversSection", 1000)
        hold(end)

        scroll('[data-be="backend-3"]', 1000, "center"); sleep(1.1)
        click('[data-be="backend-3"] .chaos button:nth-child(3)')
        end = cap("cr_crash", "Crash")
        sleep(4.5); scroll("#serversSection", 1100)
        hold(end); sleep(0.8)

        scroll("#journalCard", 1100, "center"); sleep(1.2)
        hold(cap("cr_journal", "Decision journal"))

        scroll('[data-be="backend-2"]', 1000, "center"); sleep(1.1)
        click('[data-be="backend-2"] .chaos button:nth-child(5)')
        end = cap("cr_repair", "Repair")
        sleep(3.5); scroll("#serversSection", 1000)
        hold(end)
        # Stop the control-room traffic before the race (off-camera, via the API).
        page.request.post(BASE + "/api/traffic?rate=0", headers=ADMIN)

        # ---------------- 4. race mode ----------------
        page.goto(BASE + "/arena.html", wait_until="domcontentloaded")
        sleep(1.2)
        hold(cap("race_intro", "Race mode"))
        click("#run")
        race_t0 = now()
        last, voice_free, mid_done = -1, 0.0, False
        scroll_plan = {0: (0, "top"), 2: ("#sides", "top"), 4: ("#compare", "center"), 6: (0, "top")}
        while now() - race_t0 < 120:
            scen = state("window.PULSE_STATE.scen") or {}
            stage = scen.get("stage", -1) if scen.get("running") or scen.get("finished") else -1
            if scen.get("finished") and last >= 6:
                break
            if stage > last and stage >= 0:
                last = stage
                if now() < voice_free:
                    sleep(voice_free - now())
                if last in scroll_plan:
                    sel, where = scroll_plan[last]
                    scroll(sel, 1200, where)
                voice_free = cap(f"st{last}", "Race mode")
                continue
            if last == 3 and not mid_done and now() > voice_free + 2.5:
                mid_done = True
                def pct(key):
                    side = next((s for s in state("window.PULSE_STATE.sides") or [] if s["key"] == key), {})
                    q = side.get("summary", {}).get("point", {})
                    d = (q.get("ok", 0) + q.get("errors", 0) + q.get("shed", 0)) or 1
                    return 100 * q.get("ok", 0) / d
                voice_free = cap("dyn_mid", "Race mode",
                                 f"Right now round robin answers {pct('classic'):.0f} percent of requests. PulseLB answers {pct('pulse'):.0f} percent.")
                continue
            sleep(0.2)

        # wait for the final, frozen scoreboard
        for _ in range(60):
            scen = state("window.PULSE_STATE.scen") or {}
            if scen.get("finished") and scen.get("score"):
                break
            sleep(0.25)
        if now() < voice_free:
            sleep(voice_free - now())
        scroll("#scoreCard", 1000)
        sleep(1.2)
        score = scen.get("score") or []
        if len(score) == 2:
            c, p = score
            text = (f"Result: PulseLB completed {p['booking_pct']:.1f} percent of Tatkal bookings, with {p['errors']:,} failed requests. "
                    f"Round robin completed {c['booking_pct']:.1f} percent, with {c['errors']:,} failed requests.")
            hold(cap("dyn_final", "Result", text))
            sleep(1.0)
            page.set_content(outro_html(score))
            sleep(4.5)
        json.dump({"events": events, "score": score, "length": round(now(), 1)},
                  open(os.path.join(HERE, "events.json"), "w", encoding="utf-8"), indent=2)
        video_path = page.video.path()
        ctx.close()
        browser.close()
    print("VIDEO", video_path)
    print("LENGTH", round(time.monotonic() - T0, 1))


if __name__ == "__main__":
    main()
