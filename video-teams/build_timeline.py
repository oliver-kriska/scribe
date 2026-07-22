#!/usr/bin/env python3
"""Compute the video timeline from per-line audio durations and emit:
   - timeline.js  (window.TIMELINE consumed by the HTML)
   - concat_list.txt (ffmpeg concat demuxer input for the master voice track)
Reuse for ElevenLabs by pointing --audio at a dir with line1.wav..line8.wav.
"""
import subprocess, sys, os, json, pathlib

AUDIO = sys.argv[1] if len(sys.argv) > 1 else "audio"
HERE = pathlib.Path(__file__).parent.resolve()
adir = (HERE / AUDIO).resolve()

INTRO = 3.0      # silent poster frame before line 1
GAP   = 0.45     # breathing room between lines
OUTRO = 2.2      # tail hold after line 8

LINES = [
 "Now imagine that memory isn't just yours.",
 "A new teammate asks the question you answered six months ago. The decision is in your head, or in a session nobody can find.",
 "scribe scales to your team. Every engineer's agent sessions write to one shared, git-backed knowledge base.",
 "Every fix, every rejected library, every decision your team makes becomes context the whole team's agents read before they act.",
 "A new hire's first Claude Code session already knows why you chose what you chose.",
 "Shared memory is a trust problem, not a sync problem. A secret gate scans every commit, so no key ever leaks into the wiki.",
 "Allowed remotes lock where it can push. Every promoted note is signed with where it came from. Still plain markdown in a repo your team owns.",
 "scribe. One knowledge base, the whole team writes to it. Single-user by default, team-ready when you are. Install at getscribe.dev.",
]

def dur(p):
    out = subprocess.check_output(
        ["ffprobe","-v","quiet","-show_entries","format=duration","-of","csv=p=0",str(p)])
    return float(out.decode().strip())

durs = [dur(adir / f"line{i+1}.wav") for i in range(8)]

# lay out audio start/end per line
lines = []
t = INTRO
for i, d in enumerate(durs):
    lines.append({"i": i+1, "start": round(t,3), "end": round(t+d,3), "text": LINES[i]})
    t += d + (GAP if i < 7 else 0)
total = round(t + OUTRO, 3)

# foreground scene windows (0 = poster)
scenes = [{"id":0, "t0":0.0, "t1": round(lines[0]["start"]+0.4,3)}]
for k in range(8):
    ln = lines[k]
    lead = 0.6
    t0 = max(ln["start"] - lead, scenes[-1]["t1"] - 0.3)
    t1 = round(lines[k+1]["start"] - 0.05, 3) if k < 7 else total
    scenes.append({"id": k+1, "t0": round(t0,3), "t1": round(t1,3)})

# dark->light + chaos->order transition rides scene 3 ("writes the wiki for you")
tr0 = round(lines[2]["start"] + 0.4, 3)
tr1 = round(lines[2]["end"]   - 0.3, 3)

TL = {
  "total": total,
  "audioSrc": f"{AUDIO}/voice_master.m4a",
  "intro": INTRO,
  "transition": {"t0": tr0, "t1": tr1},
  "lines": lines,
  "scenes": scenes,
}

(HERE / "timeline.js").write_text("window.TIMELINE = " + json.dumps(TL, indent=2) + ";\n")

# concat list: intro silence, then line + gap-silence between, outro silence
cl = [f"file '{HERE/'audio'/'sil_intro.wav'}'"]
for i in range(8):
    cl.append(f"file '{adir/('line%d.wav'%(i+1))}'")
    if i < 7:
        cl.append(f"file '{HERE/'audio'/'sil_gap.wav'}'")
cl.append(f"file '{HERE/'audio'/'sil_outro.wav'}'")
(HERE / "concat_list.txt").write_text("\n".join(cl) + "\n")

print(f"total={total}s  narration={sum(durs):.2f}s")
for s in scenes: print(f"  scene {s['id']}: {s['t0']:6.2f} -> {s['t1']:6.2f}")
print(f"  transition: {tr0} -> {tr1}")
print(f"INTRO={INTRO} GAP={GAP} OUTRO={OUTRO}")
