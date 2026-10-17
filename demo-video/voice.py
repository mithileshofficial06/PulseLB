"""Voiceover helpers for the PulseLB demo video.

Lines are synthesised with Microsoft's neural TTS (edge-tts) and measured
with ffmpeg, so the recorder can hold each caption exactly as long as its
voice line lasts.
"""
import asyncio
import json
import os
import re
import subprocess
import sys

import edge_tts
import imageio_ffmpeg

VOICE = "en-IN-NeerjaNeural"
RATE = "+4%"
FFMPEG = imageio_ffmpeg.get_ffmpeg_exe()
HERE = os.path.dirname(os.path.abspath(__file__))
TTS_DIR = os.path.join(HERE, "tts")

# Fixed lines, in the order they appear. Lines whose text depends on live
# numbers are generated after recording (see build.py).
LINES = {
    "title": "This is PulseLB, a load balancer built from scratch in Go. It routes around slow servers, heals itself, and keeps bookings working under heavy traffic.",
    "hero": "The idea: when a server slows down or crashes, traffic moves to the healthy ones.",
    "flow": "Every request is classified by priority, sent to the faster of two servers, and retried elsewhere if it fails.",
    "results": "Measured on this laptop: ten thousand requests per second, confirmed by two separate tools.",
    "cr_intro": "The Control room: one PulseLB in front of three servers. We send fake users, break servers, and watch it heal.",
    "cr_slow": "Now backend-2 slows down by 300 milliseconds, but it still passes health checks.",
    "cr_rr": "With round robin, a normal load balancer, the slow server gets a third of the traffic again, and wait times jump.",
    "cr_p2c": "Back to Smart P2C: the slow server is avoided again within seconds.",
    "cr_crash": "Now backend-3 crashes. PulseLB ejects it in milliseconds, retries the failed requests, and launches a replacement server.",
    "cr_journal": "Every decision is explained in plain English, in the decision journal.",
    "cr_repair": "Repairing backend-2. Backend-3 restarts on its own, and both come back gradually, so they aren't flooded.",
    "race_intro": "Race mode: a normal round-robin load balancer against PulseLB. Same traffic, same failures, same moment.",
    "st0": "Nine fifty-nine: normal morning traffic.",
    "st1": "Ten A.M.: Tatkal booking opens, and traffic triples.",
    "st2": "Backend-2 slows down. Round robin keeps feeding it a third of the traffic.",
    "st3": "Backend-3 crashes. PulseLB ejects it and launches a replacement.",
    "st4": "Backend-2 is repaired.",
    "st5": "The rush is over.",
    "st6": "Final whistle. Here is the scoreboard.",
}


def synth(text, path):
    asyncio.run(edge_tts.Communicate(text, VOICE, rate=RATE).save(path))


def duration(path):
    out = subprocess.run([FFMPEG, "-i", path], capture_output=True, text=True).stderr
    m = re.search(r"Duration: (\d+):(\d+):([\d.]+)", out)
    h, mi, s = m.groups()
    return int(h) * 3600 + int(mi) * 60 + float(s)


def generate_fixed():
    os.makedirs(TTS_DIR, exist_ok=True)
    info = {}
    for key, text in LINES.items():
        path = os.path.join(TTS_DIR, key + ".mp3")
        if not os.path.exists(path):
            synth(text, path)
        info[key] = {"text": text, "file": path, "dur": round(duration(path), 2)}
    words = sum(len(v["text"].split()) for v in info.values())
    secs = sum(v["dur"] for v in info.values())
    meta = {"lines": info, "words_per_sec": round(words / secs, 3)}
    with open(os.path.join(TTS_DIR, "lines.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=2)
    return meta


if __name__ == "__main__":
    meta = generate_fixed()
    for k, v in meta["lines"].items():
        print(f"{k:11s} {v['dur']:5.1f}s")
    print("words/sec:", meta["words_per_sec"], " total:", round(sum(v["dur"] for v in meta["lines"].values()), 1), "s")
