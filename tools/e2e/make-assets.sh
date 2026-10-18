#!/usr/bin/env bash
# Generates small but real HLS/CMAF sources for the end-to-end test: solid-colour 720p25 video with a burned-in
# label and a tone. Every source goes through tools/normalize.sh, exactly as a user's files do: that gives fMP4
# segments of exactly one second AND identical encoder settings. The settings matter: ffmpeg's HLS demuxer (and
# some players) keep the codec parameters of the first init section, so sources that were encoded with different
# parameter sets do not decode across a change of source. m3 lasts 5.6 s to test the fade-out and the black fill.
set -euo pipefail

OUT=${1:-/media}
FONT=/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf

# gen <dir> <seconds> <colour> <label> <tone Hz>
gen() {
  local dir="$OUT/$1" raw="/tmp/raw_$(echo "$1" | tr / _).mp4"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "color=c=$3:s=1280x720:r=25,drawtext=fontfile=$FONT:text='$4':fontsize=110:fontcolor=white:x=(w-text_w)/2:y=200,drawtext=fontfile=$FONT:text='%{pts\:hms}':fontsize=70:fontcolor=white:x=(w-text_w)/2:y=420" \
    -f lavfi -i "sine=frequency=$5:sample_rate=48000" -t "$2" \
    -c:v libx264 -preset ultrafast -crf 12 -pix_fmt yuv420p -c:a aac -b:a 192k "$raw"
  normalize.sh "$raw" "$dir" 25 1280x720 | tail -1
  rm -f "$raw"
}

gen m1          30   blue   "MOVIE 1" 440
gen m2          30   green  "MOVIE 2" 660
gen m3          5.6  orange "TAIL"    550
gen ads/ad1     10   red    "AD 1"    880
gen ads/ad2      6   red    "AD 2"    990
gen ads/house1   1   red    "HOUSE"   770
gen slate       10   gray   "SLATE"   330

cat > "$OUT/ads/_registry.json" <<JSON
{"version":1,"updated_at":"2025-01-01T00:00:00Z","creatives":[
 {"id":"ad1","type":"partner","category":["any"],"duration":10,"uri":"file:///media/ads/ad1/index.m3u8","active":true},
 {"id":"ad2","type":"partner","category":["any"],"duration":6,"uri":"file:///media/ads/ad2/index.m3u8","active":true},
 {"id":"house1","type":"house","category":["any"],"duration":1,"uri":"file:///media/ads/house1/index.m3u8","active":true}
]}
JSON
echo "registry written"
