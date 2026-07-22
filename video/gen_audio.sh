#!/bin/zsh
# Generate placeholder VO with macOS `say`, measure durations, emit a timeline JSON.
set -e
cd "$(dirname "$0")"
VOICE="${1:-Daniel}"
RATE="${2:-170}"
OUT=audio
mkdir -p "$OUT"

# 8 narration lines (index 1..8). Scene 0 is a silent 3s poster intro.
lines=(
"The expensive half of the job was never deciding. It's rebuilding the context you already had."
"Close the terminal, and the fix you just earned is gone. Every new agent session starts from zero, re-deriving what you already know."
"scribe reads your git history, your Claude Code and Codex sessions, and the links you send yourself, and writes the wiki for you."
"Memory your agents read before they act. Not a second brain you maintain and never reopen."
"It runs on cron. Four streams in, noise filtered before any model runs, compiled into a typed graph of plain markdown."
"Fix a nasty bug in one project on Monday. Friday, in a different repo, your agent already has your fix."
"Not R.A.G. Not Obsidian. Not another model-on-every-session burner. Plain markdown in git you own, running fully local for zero dollars."
"scribe. Set it up once, and your tools write the notes for you. Install at get scribe dot dev."
)

echo "voice=$VOICE rate=$RATE"
i=1
durs=()
for text in "${lines[@]}"; do
  aiff="$OUT/line$i.aiff"
  wav="$OUT/line$i.wav"
  say -v "$VOICE" -r "$RATE" -o "$aiff" "$text"
  # normalize to 44.1k mono wav
  ffmpeg -y -loglevel error -i "$aiff" -ar 44100 -ac 1 "$wav"
  d=$(ffprobe -v quiet -show_entries format=duration -of csv=p=0 "$wav")
  durs+=("$d")
  printf "line %d  %6.2fs  %s\n" "$i" "$d" "${text:0:48}"
  i=$((i+1))
done

# print durations as a shell array for the next step
printf "DURS="
printf "%s " "${durs[@]}"
printf "\n"