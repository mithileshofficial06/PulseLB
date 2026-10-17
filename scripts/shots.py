"""Takes the project screenshots in docs/shots/ from the running demo.

Start the demo first (bin/demo.exe, serving http://127.0.0.1:9000), then:
    python scripts/shots.py
Shots are 2x resolution, in the brutal skin, with real live traffic.
"""
import os
import time
import urllib.request

from playwright.sync_api import sync_playwright

BASE = "http://127.0.0.1:9000"
OUT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "docs", "shots")
VP = {"width": 1600, "height": 1000}
ADMIN = {"X-Pulse-Admin": "1"}

INIT_JS = """
try {
  localStorage.setItem('pulselb-skin', 'brutal');
  ['home', 'dashboard', 'arena'].forEach((p) => sessionStorage.setItem('pg-hello-' + p, '1'));
} catch (e) {}
"""


def main():
    os.makedirs(OUT, exist_ok=True)
    with sync_playwright() as p:
        browser = p.chromium.launch(channel="chrome")
        ctx = browser.new_context(viewport=VP, device_scale_factor=2)
        ctx.add_init_script(INIT_JS)
        page = ctx.new_page()
        sleep = lambda s: page.wait_for_timeout(int(s * 1000))

        def shot(name, el=None):
            path = os.path.join(OUT, name + ".png")
            (page.locator(el).first.screenshot(path=path) if el else page.screenshot(path=path))
            print("saved", path)

        def to(sel, where="start"):
            page.evaluate("([s, w]) => document.querySelector(s).scrollIntoView({block: w, behavior: 'instant'})", [sel, where])
            sleep(1.6)

        def booted():
            page.wait_for_selector("#boot.gone, #boot.out", state="attached", timeout=20000)
            sleep(1.4)

        # 1. loading screen, mid boot
        page.goto(BASE + "/", wait_until="domcontentloaded")
        # hold the loading screen in place once its live checks have reported
        page.add_style_tag(content="#boot.out { transform: none !important; } #boot.gone { display: grid !important; } #boot.out .boot-inner { transform: none !important; opacity: 1 !important; }")
        page.wait_for_function("document.querySelector('#boot .boot-log').textContent.includes('healthy')", timeout=15000)
        sleep(1.5)
        shot("01-boot")
        page.reload(wait_until="domcontentloaded")
        booted()
        sleep(2.5)  # headline + hero simulation settle
        shot("02-landing-hero")
        for sec, name in [("#problem", "03-landing-problem"), ("#flow", "04-landing-how-it-works"),
                          ("#results", "05-landing-results")]:
            to(sec)
            shot(name)

        # 2. control room: real traffic, then break servers
        page.goto(BASE + "/dashboard.html", wait_until="domcontentloaded")
        booted()
        page.click('#rates button[data-r="2000"]')
        sleep(8)
        page.evaluate("scrollTo(0, 0)")
        sleep(1)
        shot("06-control-room-healthy")
        page.click('[data-be="backend-2"] .chaos button:nth-child(1)')   # slow
        sleep(4)
        page.click('[data-be="backend-3"] .chaos button:nth-child(3)')   # crash
        sleep(5)
        to("#serversSection")
        shot("07-control-room-servers-breaking")
        to("#journalCard", "center")
        shot("08-decision-journal", "#journalCard")
        # heal everything so the demo is left as it was
        page.click('[data-be="backend-2"] .chaos button:nth-child(5)')
        page.click('#rates button[data-r="0"]') if page.locator('#rates button[data-r="0"]').count() else None

        # 3. race mode, full run
        page.goto(BASE + "/arena.html", wait_until="domcontentloaded")
        booted()
        page.click("#run")
        mid = started = False
        t0 = time.time()
        while time.time() - t0 < 180:
            scen = page.evaluate("window.PULSE_STATE && window.PULSE_STATE.scen") or {}
            # the page may still show the previous race until this one starts
            started = started or bool(scen.get("running"))
            if not started:
                sleep(0.3)
                continue
            if not mid and scen.get("stage", -1) >= 3:
                sleep(6)
                to("#sides")
                shot("09-race-mid")
                mid = True
            if scen.get("finished") and scen.get("score"):
                break
            sleep(0.5)
        sleep(1.5)
        to("#scoreCard")
        shot("10-race-scoreboard", "#scoreCard")
        browser.close()
    cover()


def cover():
    """2x2 collage for social posts: hero, control room, race, scoreboard."""
    from PIL import Image, ImageOps
    W, H, G = 1600, 1000, 40  # tile size and gutter
    bg, line = (9, 8, 11), (224, 164, 58)
    names = ["02-landing-hero", "06-control-room-healthy", "09-race-mid", "10-race-scoreboard"]
    sheet = Image.new("RGB", (2 * W + 3 * G, 2 * H + 3 * G), bg)
    for i, n in enumerate(names):
        im = Image.open(os.path.join(OUT, n + ".png")).convert("RGB")
        im = ImageOps.pad(im, (W - 8, H - 8), color=bg, method=Image.LANCZOS)
        tile = Image.new("RGB", (W, H), line)
        tile.paste(im, (4, 4))
        sheet.paste(tile, (G + (i % 2) * (W + G), G + (i // 2) * (H + G)))
    path = os.path.join(OUT, "00-cover.png")
    sheet.save(path, optimize=True)
    print("saved", path)


if __name__ == "__main__":
    main()
