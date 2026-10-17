"""Finds when each caption really appears in the recorded video.

The browser's recorder can drop frames when the laptop is busy, so its
timeline drifts from the wall clock the recorder logged. This scans a strip
of the caption box, finds the moments its content changes, and matches each
logged caption to the nearest change. build.py places the voice there.
"""
import glob
import json
import os
import subprocess

import numpy as np

from voice import FFMPEG

HERE = os.path.dirname(os.path.abspath(__file__))
FPS = 20
W, H = 1440, 810
# A strip inside the caption box (bottom centre), present for every caption.
X0, X1, Y0, Y1 = 560, 880, 700, 770


def caption_changes(video):
    w, h = X1 - X0, Y1 - Y0
    cmd = [FFMPEG, "-loglevel", "error", "-i", video,
           "-vf", f"fps={FPS},crop={w}:{h}:{X0}:{Y0},format=gray",
           "-f", "rawvideo", "-"]
    raw = subprocess.run(cmd, capture_output=True, check=True).stdout
    frames = np.frombuffer(raw, np.uint8).reshape(-1, h, w).astype(np.int16)
    diff = np.abs(np.diff(frames, axis=0)).mean(axis=(1, 2))
    return diff  # diff[i] = change between frame i and i+1


def main():
    d = json.load(open(os.path.join(HERE, "events.json"), encoding="utf-8"))
    video = glob.glob(os.path.join(HERE, "raw", "*.webm"))[0]
    diff = caption_changes(video)
    peaks = [i for i in range(1, len(diff) - 1) if diff[i] > 6 and diff[i] >= diff[i - 1] and diff[i] >= diff[i + 1]]
    times = [(i + 1) / FPS for i in peaks]
    print(f"{len(times)} candidate change moments in {len(diff) / FPS:.1f}s of video")
    prev = 0.0
    for e in d["events"]:
        logged = e["t"] - 0.3  # the log adds 0.3s for the slide-in animation
        cands = [t for t in times if t > prev + 1.0 and logged - 6.0 <= t <= logged + 1.5]
        if cands:
            t = min(cands, key=lambda c: abs(c - logged))
            e["video_t"] = round(t + 0.3, 2)
        else:
            e["video_t"] = round(e["t"], 2)
            print("  no match for", e["key"], "- keeping logged time")
        prev = e["video_t"] - 0.3
        print(f"  {e['key']:11s} logged {e['t']:6.1f}  in video {e['video_t']:6.1f}  drift {e['video_t'] - e['t']:+5.1f}s")
    json.dump(d, open(os.path.join(HERE, "events.json"), "w", encoding="utf-8"), indent=2)


if __name__ == "__main__":
    main()
