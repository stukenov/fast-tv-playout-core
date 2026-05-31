# FAST TV Playout Core

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![CI](https://github.com/stukenov/fast-tv-playout-core/actions/workflows/ci.yml/badge.svg)](https://github.com/stukenov/fast-tv-playout-core/actions/workflows/ci.yml)

> Turn a library of VOD files into a 24/7 ad-supported live TV channel — HLS/CMAF out, SSAI ad breaks, and EPG included.

A Go-based core system for FAST (Free Ad-Supported Streaming TV) channels with VOD-to-Live conversion, server-side ad insertion (SSAI), and Electronic Program Guide (EPG) support. Built following YAGNI (You Aren't Gonna Need It) principles for a minimal viable broadcast automation system.

### What is FAST / VOD-to-Live?

**FAST** (Free Ad-Supported Streaming TV) channels are linear, "lean-back" TV channels delivered over the internet — think the always-on channels you scroll through on Samsung TV Plus, Pluto, or Rakuten. Unlike on-demand catalogs, viewers don't pick titles; they tune into a programmed stream that's running 24/7 and monetized with ad breaks.

**VOD-to-Live** is the technique that makes this affordable: instead of running a live encoder around the clock, you take a catalog of on-demand assets (already encoded as HLS/CMAF), schedule them on a rolling timeline, and stitch their segments into a single continuous live playlist. This core does exactly that — scheduling, seamless stitching, server-side ad insertion at break boundaries, and EPG generation — so you can launch a channel from existing content instead of a broadcast chain.

### Key features

- **VOD-to-Live** — rolling timeline scheduler turns a catalog of VOD assets into a continuous 24/7 channel.
- **SSAI** — server-side ad insertion with VAST 4.x, duration-based pod assembly, and house-ad fallback.
- **HLS / CMAF** — fMP4 segments, rolling playlist window, `EXT-X-PROGRAM-DATE-TIME`, and ABR ladder support.
- **EPG** — real-time Now/Next API and XMLTV export for FAST platforms (Samsung TV Plus, Pluto, Rakuten).
- **Observability** — Prometheus metrics for playlist age, segment buffer, and ad fill rate, with Grafana dashboards.
- **Microservice core** — six small, independently buildable Go services (control-api, scheduler, playout, ads, packager, epg).

## Overview

This is a production-ready playout engine that transforms VOD (Video On Demand) assets into 24/7 linear streaming channels with:
- Automated content scheduling
- Server-side ad insertion (SSAI) with VAST 4.x
- HLS/CMAF streaming output
- EPG generation (Now/Next and XMLTV)
- Real-time monitoring and alerting

## Quickstart

The fastest way to see a channel go live is the bundled Docker Compose stack (Postgres, Redis, an Nginx origin, all six services, plus Prometheus and Grafana).

```bash
# 1. Clone
git clone https://github.com/stukenov/fast-tv-playout-core.git
cd fast-tv-playout-core

# 2. Bring up the whole stack
docker compose up -d --build

# 3. Register a channel (control-api, port 8081)
curl -s localhost:8081/v1/channels \
  -H 'X-API-Key: dev-key' -H 'content-type: application/json' \
  -d '{"id":"c1","name":"FAST Movies","segment_sec":2,"ladder":["1080p","720p","480p"]}'

# 4. Push a short schedule
now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
curl -s localhost:8081/v1/schedule/c1 \
  -H 'X-API-Key: dev-key' -H 'content-type: application/json' \
  -d '{"entries":[{"type":"content","asset_id":"m1","uri":"s3://bucket/content/m1/index.m3u8","start":"'"$now"'","duration":30},{"type":"ad_break","start":"'"$now"'","duration":30,"category":"midroll"}]}'

# 5. Read the EPG (epg, port 8082) and the HLS master (origin, port 8080)
curl -s localhost:8082/v1/epg/c1/now-next | jq .
curl -s localhost:8080/hls/c1/master.m3u8 | head -n 5
```

Service ports: control-api `8081`, epg `8082`, ads `8083`, scheduler `8084`, playout `8085`, packager `8086`, HLS origin `8080`, Prometheus `9090`, Grafana `3000`. See [README_RUN.md](README_RUN.md) for the full local-run guide and `tools/smoke.sh` for an end-to-end smoke test.

### Build from source

The services are plain Go binaries (module `ftvx`, Go 1.25) with no codegen step — `go build ./...` is exactly what CI runs:

```bash
go build ./...        # build every service
go test ./...         # run the unit/integration tests
go build -o bin/control-api ./services/control-api   # build a single service
```

## Architecture

```
[Asset Catalog] → [Scheduler] → [Playout Engine] → [HLS Packager]
                                        ↓
                                 [Ad Orchestrator]
                                        ↓
                               [Origin Server] → [CDN] → Viewers

Parallel: [EPG Service] → FAST Platforms
Parallel: [Monitoring] → Prometheus/Grafana
```

## Core Components

### Services

1. **control-api** - REST API for asset management and scheduling
2. **scheduler** - Timeline generation with rolling window planning
3. **playout** - Core playout engine with seamless content stitching
4. **ads** - VAST-based ad orchestration and insertion
5. **packager** - HLS/CMAF playlist generation
6. **epg** - Electronic Program Guide (Now/Next, XMLTV export)

### Supporting Infrastructure

- **PostgreSQL** - Metadata storage (channels, assets, timeline)
- **Redis** - Caching and timeline distribution
- **Nginx** - Origin server for HLS delivery
- **Prometheus** - Metrics and monitoring
- **Grafana** - Visualization dashboards

## Features

### Content Management
- VOD asset cataloging with metadata
- Automated content normalization (H.264/AAC)
- GOP alignment validation (2-second segments)
- Asset integrity checks

### Scheduling
- 6-12 hour rolling timeline generation
- Event types: content, ad_break, bumper, slate
- Automatic gap filling with slates
- Frame-accurate scheduling

### Playout
- Seamless segment stitching
- Continuous timecode management
- DISCONTINUITY tags only at ad boundaries
- Automatic slate insertion on errors

### Ad Insertion (SSAI)
- VAST 4.x protocol support
- Pre-normalized ad creative matching
- Duration-based pod assembly
- House ad fallback
- Fill rate optimization

### Streaming Output
- HLS with CMAF/fMP4 segments
- 2-second segment duration
- Rolling playlist window (1-3 minutes)
- `EXT-X-PROGRAM-DATE-TIME` support
- Multiple bitrate support (ABR ladder)

### EPG
- Real-time Now/Next API
- XMLTV export (15-minute updates)
- Platform mapping (Samsung TV+, Pluto, Rakuten)
- Schedule metadata enrichment

### Monitoring
- Playlist age tracking
- Segment availability metrics
- Ad fill rate monitoring
- HTTP error tracking
- Prometheus integration

## Prerequisites

- Go 1.22 or later
- PostgreSQL 15+
- Redis 7+
- FFmpeg (for asset normalization)
- Docker and Docker Compose

## Installation

### Using Docker Compose (Recommended)

1. Clone the repository:
   ```bash
   git clone https://github.com/stukenov/fast-tv-playout-core.git
   cd fast-tv-playout-core
   ```

2. Configure environment:
   ```bash
   cp .env.example .env
   # Edit .env with your settings
   ```

3. Start services:
   ```bash
   docker-compose up -d
   ```

### Manual Installation

1. Install dependencies:
   ```bash
   go mod download
   ```

2. Set up PostgreSQL:
   ```bash
   psql -U postgres -f deploy/schema.sql
   ```

3. Build services:
   ```bash
   go build -o bin/control-api ./services/control-api
   go build -o bin/scheduler ./services/scheduler
   go build -o bin/playout ./services/playout
   go build -o bin/ads ./services/ads
   go build -o bin/packager ./services/packager
   go build -o bin/epg ./services/epg
   ```

4. Run services:
   ```bash
   ./bin/control-api &
   ./bin/scheduler &
   ./bin/playout &
   ./bin/ads &
   ./bin/packager &
   ./bin/epg &
   ```

## Configuration

### Environment Variables

```env
# Database
DATABASE_URL=postgres://user:pass@localhost:5432/fast

# Redis
REDIS_URL=redis://localhost:6379

# S3/Object Storage
S3_BUCKET=my-fast-content
S3_ENDPOINT=https://s3.amazonaws.com

# Playout
SEGMENT_DURATION=2
PLAYLIST_WINDOW=90

# Monitoring
PROMETHEUS_PORT=9090
```

### Channel Configuration

```yaml
channel_id: c1
name: "FAST Movies"
profiles: [1080p, 720p, 480p]
segment: 2s
slate: s3://bucket/slates/default_2s_cmaf.m3u8
ad_breaks:
  default_duration: 120
  vast_tags:
    - https://ads.example.com/vast?channel=c1
```

## API Reference

### Asset Management

```bash
# Register asset
POST /v1/assets
{
  "uri": "s3://bucket/content/video.mp4",
  "duration_s": 1800,
  "tags": ["movies", "action"]
}

# List assets
GET /v1/assets?tags=movies
```

### Channel Operations

```bash
# Create channel
POST /v1/channels
{
  "id": "c1",
  "name": "My Channel",
  "segment_sec": 2
}

# Get channel status
GET /v1/channels/c1
```

### Scheduling

```bash
# Generate schedule
POST /v1/schedule/c1
{
  "entries": [
    {
      "type": "content",
      "asset_id": "m_001",
      "start": "2025-02-17T12:00:00Z",
      "duration_s": 1800
    },
    {
      "type": "ad_break",
      "start": "2025-02-17T12:30:00Z",
      "duration_s": 120
    }
  ]
}
```

### EPG

```bash
# Get Now/Next
GET /v1/epg/c1/now-next

# Get XMLTV
GET /v1/epg/c1/xmltv
```

### Streaming

```bash
# HLS master playlist
GET /hls/c1/master.m3u8

# Media playlist
GET /hls/c1/720p/index.m3u8

# Segment
GET /hls/c1/720p/segment_12345.m4s
```

## Asset Normalization

### Video Requirements

- Codec: H.264 High@4.1
- GOP: 2 seconds (keyint=48 for 24fps)
- Resolution: Multiple profiles (1080p, 720p, 480p)
- Frame rate: 24/25/30 fps (consistent)
- Container: Fragmented MP4 (fMP4)

### Audio Requirements

- Codec: AAC-LC
- Channels: Stereo (2ch)
- Sample rate: 48kHz
- Bitrate: 128kbps

### FFmpeg Normalization Command

```bash
ffmpeg -i input.mov \
  -c:v libx264 -profile:v high -level:v 4.1 \
  -x264-params keyint=48:min-keyint=48:scenecut=0 \
  -r 24 -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 128k -ac 2 -ar 48000 \
  -movflags +faststart+frag_keyframe+empty_moov \
  output_1080p.mp4
```

## Monitoring

### Key Metrics

- `playout_playlist_age_seconds{channel}` - Playlist freshness
- `packager_segments_ahead{channel}` - Buffer health
- `ad_fill_ratio{channel}` - Ad inventory utilization
- `vast_errors_total{code}` - VAST response errors
- `origin_http_5xx_total` - Origin server errors

### Alerts

- **Critical**: `playlist_age > 10s`
- **Warning**: `ad_fill < 0.6`
- **Warning**: `segments_ahead < 5`

### Grafana Dashboard

Import the provided dashboard:
```bash
curl http://localhost:3000/api/dashboards/import \
  -H "Content-Type: application/json" \
  -d @deploy/grafana-dashboard.json
```

## Database Schema

```sql
CREATE TABLE channel (
  id           text PRIMARY KEY,
  name         text NOT NULL,
  segment_sec  int DEFAULT 2,
  created_at   timestamptz DEFAULT now()
);

CREATE TABLE asset (
  id           text PRIMARY KEY,
  uri          text NOT NULL,
  duration_s   int NOT NULL,
  tags         text[],
  created_at   timestamptz DEFAULT now()
);

CREATE TABLE timeline_entry (
  channel_id   text REFERENCES channel(id),
  start_at     timestamptz NOT NULL,
  type         text NOT NULL,
  asset_id     text REFERENCES asset(id),
  duration_s   int NOT NULL,
  PRIMARY KEY (channel_id, start_at)
);
```

## Development

### Running Tests

```bash
go test ./...
```

### Building

```bash
make build
```

### Linting

```bash
golangci-lint run
```

## Deployment

### Production Checklist

- [ ] Configure PostgreSQL with replication
- [ ] Set up Redis cluster
- [ ] Configure S3/object storage with CDN
- [ ] Enable TLS for all endpoints
- [ ] Set up log aggregation (Loki)
- [ ] Configure alerting (Alertmanager)
- [ ] Implement backup strategy
- [ ] Load test with expected viewer count

### Scaling

- **Horizontal**: Run multiple playout instances per channel pool
- **Vertical**: Increase resources for packager services
- **Caching**: Use Redis for hot timeline data
- **CDN**: Distribute load geographically

## Platform Integration

### Samsung TV Plus

- Provide HLS endpoint
- Submit XMLTV EPG
- Channel logo (720x720 PNG)

### Pluto TV

- Implement Pluto EPG format
- Add SCTE-35 markers
- Provide channel metadata

### Rakuten TV

- Submit channel information
- Provide M3U playlist
- Include age ratings

## Troubleshooting

### Playlist Not Updating

Check playout service logs:
```bash
docker-compose logs -f playout
```

### Segments Missing

Verify asset normalization:
```bash
ffprobe -show_format -show_streams asset.mp4
```

### Ad Fill Low

Check VAST endpoint connectivity:
```bash
curl -v "https://ads.example.com/vast?channel=c1"
```

## Performance

- **Latency**: 4-8 seconds (2-4 segments)
- **Throughput**: 1000+ concurrent viewers per instance
- **Resource**: ~100MB RAM per channel
- **CPU**: <1 core per channel (copy codec)

## Roadmap

### Future Enhancements (NOT in MVP)

- Low-latency HLS (LL-HLS)
- DASH output support
- DRM (Widevine, FairPlay)
- Dynamic graphics/CG overlays
- Live input switching
- Multi-region deployment
- ML-based content recommendations

## Contributing

Contributions are welcome! Please:
1. Fork the repository
2. Create a feature branch
3. Write tests
4. Submit a pull request

## Related projects

This playout core is part of a broader broadcast and streaming suite:

- **[live-streaming-server](https://github.com/stukenov/live-streaming-server)** — live ingest and streaming server.
- **[vod-streaming-server](https://github.com/stukenov/vod-streaming-server)** — VOD packaging and delivery, the natural source of assets for VOD-to-Live.
- **[tv-playout-backend](https://github.com/stukenov/tv-playout-backend)** — playout management and control backend.

## License

MIT License - see LICENSE file for details.

Copyright (c) 2025 Saken Tukenov

## References

- [HLS Specification](https://datatracker.ietf.org/doc/html/rfc8216)
- [VAST 4.x Protocol](https://www.iab.com/guidelines/vast/)
- [XMLTV Format](http://wiki.xmltv.org/index.php/XMLTVFormat)
- [CMAF Specification](https://www.iso.org/standard/71975.html)

## Support

For issues and questions:
- GitHub Issues: https://github.com/stukenov/fast-tv-playout-core/issues
