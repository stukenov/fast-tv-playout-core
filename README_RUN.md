# FTVX MVP (YAGNI) – Local run

## Prereqs
- Docker Desktop

## Run
```bash
# from repo root
docker compose up -d --build
```

Services:
- control-api: http://localhost:8081
- epg: http://localhost:8082
- ads: http://localhost:8083
- scheduler: http://localhost:8084
- playout: http://localhost:8085
- packager: http://localhost:8086
- origin (HLS): http://localhost:8080/hls/
- prometheus: http://localhost:9090
- grafana: http://localhost:3000

## Smoke
```bash
# register a channel
curl -s localhost:8081/v1/channels -H 'X-API-Key: dev-key' -H 'content-type: application/json' -d '{"id":"c1","name":"FAST Movies","segment_sec":2,"ladder":["1080p","720p","480p"]}'

# push a simple 60s schedule
now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
curl -s localhost:8081/v1/schedule/c1 -H 'X-API-Key: dev-key' -H 'content-type: application/json' -d '{"entries":[{"type":"content","asset_id":"m1","uri":"s3://bucket/content/m1/index.m3u8","start":"'"$now"'","duration":30},{"type":"ad_break","start":"'"$now"'","duration":30,"category":"midroll"}]}'

# get Now/Next
curl -s localhost:8082/v1/epg/c1/now-next | jq .
```

## Cleanup
```bash
docker compose down -v
```
