#!/usr/bin/env bash
set -euo pipefail

curl -s localhost:8081/v1/channels -H 'content-type: application/json' -d '{"id":"c1","name":"FAST Movies","segment_sec":2,"ladder":["1080p","720p","480p"]}' | jq . || true

now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
curl -s localhost:8081/v1/schedule/c1 -H 'content-type: application/json' -d '{"entries":[{"type":"content","asset_id":"m1","uri":"s3://bucket/content/m1/index.m3u8","start":"'"$now"'","duration":30},{"type":"ad_break","start":"'"$now"'","duration":30,"category":"midroll"}]}' | jq . || true

curl -s localhost:8082/v1/epg/c1/now-next | jq .
curl -s localhost:8082/v1/epg/c1/xmltv | head -n 3

curl -s localhost:8083/v1/ads/pod -H 'content-type: application/json' -d '{"channel_id":"c1","break":{"start":"'"$now"'","duration":20,"category":"midroll"}}' | jq .

echo "Check HLS master:"
curl -s localhost:8080/hls/c1/master.m3u8 | head -n 5

