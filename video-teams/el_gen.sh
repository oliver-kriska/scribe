#!/bin/zsh
# Generate the final ElevenLabs voiceover, one clip per line, into audio_el/lineN.wav
# then rebuild the timeline + master track from the real ElevenLabs durations.
# Usage: ./el_gen.sh <voice_id> [model_id]
set -e
cd "$(dirname "$0")"
VOICE_ID="${1:?need voice_id}"
MODEL="${2:-eleven_multilingual_v2}"
: "${ELEVENLABS_API_KEY:?ELEVENLABS_API_KEY not set}"
OUT=audio_el; mkdir -p "$OUT"

# "RAG" is left as a word — engineers pronounce it "rag". "getscribe dot dev" spelled for clean TTS.
lines=(
"Now imagine that memory isn't just yours."
"A new teammate asks the question you answered six months ago. The decision is in your head, or in a session nobody can find."
"scribe scales to your team. Every engineer's agent sessions write to one shared, git-backed knowledge base."
"Every fix, every rejected library, every decision your team makes becomes context the whole team's agents read before they act."
"A new hire's first Claude Code session already knows why you chose what you chose."
"Shared memory is a trust problem, not a sync problem. A secret gate scans every commit, so no key ever leaks into the wiki."
"Allowed remotes lock which repos it reads. Every promoted note carries where it came from. Still plain markdown in a repo your team owns."
"scribe. One knowledge base, the whole team writes to it. Single-user by default, team-ready when you are. Install at getscribe dot dev."
)

i=1
for text in "${lines[@]}"; do
  echo "gen line $i ..."
  body=$(python3 -c 'import json,sys; print(json.dumps({"text":sys.argv[1],"model_id":sys.argv[2],"voice_settings":{"stability":0.42,"similarity_boost":0.75,"style":0.0,"use_speaker_boost":True}}))' "$text" "$MODEL")
  code=$(curl -s -w "%{http_code}" -o "$OUT/line$i.mp3" -X POST \
    "https://api.elevenlabs.io/v1/text-to-speech/$VOICE_ID?output_format=mp3_44100_128" \
    -H "xi-api-key: $ELEVENLABS_API_KEY" -H "Content-Type: application/json" \
    -H "Accept: audio/mpeg" -d "$body")
  if [ "$code" != "200" ]; then echo "  FAILED http=$code:"; cat "$OUT/line$i.mp3"; echo; exit 1; fi
  ffmpeg -y -loglevel error -i "$OUT/line$i.mp3" -ar 44100 -ac 1 "$OUT/line$i.wav"
  d=$(ffprobe -v quiet -show_entries format=duration -of csv=p=0 "$OUT/line$i.wav")
  printf "  ok %.2fs\n" "$d"
  i=$((i+1))
done

echo "--- rebuild timeline + master (ElevenLabs durations) ---"
python3 build_timeline.py "$OUT"
ffmpeg -y -loglevel error -f concat -safe 0 -i concat_list.txt -c:a aac -b:a 160k "$OUT/voice_master.m4a"
echo "DONE audio. Next: ./run_all.sh $OUT/voice_master.m4a scribe-explainer.mp4"
