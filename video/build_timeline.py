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
 "The expensive half of the job was never deciding. It's rebuilding the context you already had.",
 "Close the terminal, and the fix you just earned is gone. Every new agent session starts from zero, re-deriving what you already know.",
 "scribe reads your git history, your Claude Code and Codex sessions, and the links you send yourself, and writes the wiki for you.",
 "Memory your agents read before they act. Not a second brain you maintain and never reopen.",
 "It runs on cron. Four streams in, noise filtered before any model runs, compiled into a typed graph of plain markdown.",
 "Fix a nasty bug in one project on Monday. Friday, in a different repo, your agent already has your fix.",
 "Not RAG. Not Obsidian. Not another model-on-every-session burner. Plain markdown in git you own, running fully local for zero dollars.",
 "scribe. Set it up once, and your tools write the notes for you. Install at getscribe.dev.",
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
