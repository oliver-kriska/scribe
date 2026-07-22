#!/bin/zsh
set -e
cd "$(dirname "$0")"
AUDIO="${1:-audio/voice_master.m4a}"
OUT="${2:-scribe-explainer-macvoice.mp4}"
echo "[1/3] clean frames"
rm -rf frames && mkdir -p frames
echo "[2/3] render all frames (headless screencast)"
node render.mjs full
echo "[3/3] mux → $OUT"
ffmpeg -y -loglevel error -framerate 30 -i frames/%05d.png -i "$AUDIO" \
  -c:v libx264 -pix_fmt yuv420p -crf 18 -preset medium \
  -c:a aac -b:a 160k -shortest -movflags +faststart "$OUT"
echo "DONE $OUT"
ffprobe -v quiet -show_entries format=duration,size -of default=noprint_wrappers=1 "$OUT"
ls -lh "$OUT"
