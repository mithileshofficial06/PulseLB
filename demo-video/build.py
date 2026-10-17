"""Builds the final demo video: voiceover + captions recording -> MP4.

1. Synthesises the lines whose text came from live numbers.
2. Checks no two voice lines overlap.
3. Places every line at the moment its caption appeared.
4. Trims to the length limit, upscales to 1080p, encodes H.264 + AAC.
"""
import glob
import json
import os
import subprocess
import sys

from voice import FFMPEG, TTS_DIR, duration, synth

HERE = os.path.dirname(os.path.abspath(__file__))
TRIM_START = 1.2      # blank page before the title card
MAX_LEN = 269.0       # 4:29

d = json.load(open(os.path.join(HERE, "events.json"), encoding="utf-8"))
events = d["events"]
video = glob.glob(os.path.join(HERE, "raw", "*.webm"))[0]

for e in events:
    if not e["file"]:
        path = os.path.join(TTS_DIR, e["key"] + ".mp3")
        synth(e["text"], path)
        e["file"] = path
    e["dur"] = duration(e["file"])

print("line         start   end    gap-to-next")
problems = 0
for i, e in enumerate(events):
    end = e["t"] + e["dur"]
    nxt = events[i + 1]["t"] if i + 1 < len(events) else None
    gap = (nxt - end) if nxt is not None else None
    flag = "  <-- OVERLAP" if gap is not None and gap < 0 else ""
    problems += bool(flag)
    print(f"{e['key']:11s} {e['t']:6.1f} {end:6.1f}  {'' if gap is None else f'{gap:5.1f}'}{flag}")

# Timeline edits (in raw-video seconds):
#  - drop the blank page before the title card
#  - cut 4s of dead time while the control-room numbers ramp up (no voice)
#  - end half a second after the last voice line, then a 3.5s end card
CUT = (58.8, 62.8)
# Measured caption times (sync.py) when available.
for e in events:
    e["t"] = e.get("video_t", e["t"])
# The recorder swapped to its own end card after holding the last caption
# for its *estimated* length; cut just before that, and let the last line
# finish over the clean end card instead.
WPS = json.load(open(os.path.join(TTS_DIR, "lines.json"), encoding="utf-8"))["words_per_sec"]
last = events[-1]
est = len(last["text"].split()) / WPS * 1.12 + 0.4
END = last["t"] + est + 0.45 + 0.8
OUTRO = max(4.5, last["t"] + last["dur"] - END + 2.0)
for e in events:
    assert not (CUT[0] < e["t"] < CUT[1]), f"voice line {e['key']} starts inside the cut"
seg1 = CUT[0] - TRIM_START
seg2 = END - CUT[1]
length = seg1 + seg2 + OUTRO
print(f"final length {length:.1f}s ({int(length // 60)}:{length % 60:04.1f})")
assert length <= MAX_LEN + 0.5, "too long"


def place(t):
    """Raw-video time -> final-video time."""
    return t - TRIM_START if t < CUT[0] else t - TRIM_START - (CUT[1] - CUT[0])


cmd = [FFMPEG, "-y", "-loglevel", "error", "-i", video,
       "-loop", "1", "-framerate", "30", "-t", f"{OUTRO}", "-i", os.path.join(HERE, "outro.png")]
for e in events:
    cmd += ["-i", e["file"]]
V = "scale=1920:1080:flags=lanczos,fps=30,format=yuv420p,setsar=1"
parts = [
    f"[0:v]trim=start={TRIM_START}:end={CUT[0]},setpts=PTS-STARTPTS,{V}[v1]",
    f"[0:v]trim=start={CUT[1]}:end={END:.2f},setpts=PTS-STARTPTS,{V}[v2]",
    f"[1:v]{V},fade=t=in:st=0:d=0.4,fade=t=out:st={OUTRO - 0.6}:d=0.6[v3]",
    "[v1][v2][v3]concat=n=3:v=1:a=0[vout]",
]
for i, e in enumerate(events, start=2):
    ms = max(0, int(place(e["t"]) * 1000))
    parts.append(f"[{i}:a]adelay={ms}:all=1,volume=1.6[a{i}]")
mix = "".join(f"[a{i}]" for i in range(2, len(events) + 2))
parts.append(f"{mix}amix=inputs={len(events)}:normalize=0:dropout_transition=0,apad,atrim=0:{length:.2f}[aout]")
out = os.path.join(HERE, "pulselb-demo.mp4")
cmd += [
    "-filter_complex", ";".join(parts),
    "-map", "[vout]", "-map", "[aout]",
    "-c:v", "libx264", "-preset", "slow", "-crf", "18",
    "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart",
    out,
]
subprocess.run(cmd, check=True)
print("wrote", out, f"{os.path.getsize(out) / 1e6:.1f} MB", "overlaps:", problems)
