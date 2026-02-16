package storage

import (
    "context"
    "errors"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"

    "ftvx/pkg/models"
)

type Postgres struct {
    pool *pgxpool.Pool
}

func ConnectPostgres(ctx context.Context, dsn string) (*Postgres, error) {
    pool, err := pgxpool.New(ctx, dsn)
    if err != nil { return nil, err }
    p := &Postgres{pool: pool}
    if err := p.migrate(ctx); err != nil { pool.Close(); return nil, err }
    return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) migrate(ctx context.Context) error {
    _, err := p.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS channel (
  id           text PRIMARY KEY,
  name         text NOT NULL,
  segment_sec  int  NOT NULL DEFAULT 2,
  ladder       jsonb NOT NULL DEFAULT '[]'::jsonb,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS asset (
  id           text PRIMARY KEY,
  uri          text NOT NULL,
  duration_s   int  NOT NULL,
  tags         text[] DEFAULT '{}',
  meta         jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS timeline_entry (
  channel_id   text REFERENCES channel(id),
  start_at     timestamptz NOT NULL,
  type         text CHECK (type IN ('content','ad_break','bumper','slate')),
  asset_id     text REFERENCES asset(id),
  uri          text,
  duration_s   int NOT NULL,
  category     text,
  scte35       text,
  PRIMARY KEY (channel_id, start_at)
);
CREATE INDEX IF NOT EXISTS idx_timeline_range ON timeline_entry(channel_id, start_at);
CREATE UNIQUE INDEX IF NOT EXISTS ux_asset_uri ON asset(uri);
`)
    return err
}

func (p *Postgres) InsertAsset(ctx context.Context, a models.Asset) error {
    _, err := p.pool.Exec(ctx, `INSERT INTO asset(id, uri, duration_s, tags, meta, created_at) VALUES($1,$2,$3,$4,$5,$6)`,
        a.ID, a.URI, a.DurationS, a.Tags, a.Meta, a.CreatedAt)
    return err
}

func (p *Postgres) GetAsset(ctx context.Context, id string) (models.Asset, error) {
    var a models.Asset
    err := p.pool.QueryRow(ctx, `SELECT id, uri, duration_s, COALESCE(tags,'{}'), COALESCE(meta,'{}'), created_at FROM asset WHERE id=$1`, id).
        Scan(&a.ID, &a.URI, &a.DurationS, &a.Tags, &a.Meta, &a.CreatedAt)
    return a, err
}

func (p *Postgres) GetAssetByURI(ctx context.Context, uri string) (models.Asset, error) {
    var a models.Asset
    err := p.pool.QueryRow(ctx, `SELECT id, uri, duration_s, COALESCE(tags,'{}'), COALESCE(meta,'{}'), created_at FROM asset WHERE uri=$1`, uri).
        Scan(&a.ID, &a.URI, &a.DurationS, &a.Tags, &a.Meta, &a.CreatedAt)
    return a, err
}

func (p *Postgres) InsertChannel(ctx context.Context, c models.Channel) error {
    _, err := p.pool.Exec(ctx, `INSERT INTO channel(id, name, segment_sec, ladder, created_at) VALUES($1,$2,$3,$4,$5)`,
        c.ID, c.Name, c.SegmentSec, c.Ladder, c.CreatedAt)
    return err
}

func (p *Postgres) GetChannel(ctx context.Context, id string) (models.Channel, error) {
    var c models.Channel
    err := p.pool.QueryRow(ctx, `SELECT id, name, segment_sec, COALESCE(ladder,'[]'), created_at FROM channel WHERE id=$1`, id).
        Scan(&c.ID, &c.Name, &c.SegmentSec, &c.Ladder, &c.CreatedAt)
    return c, err
}

func (p *Postgres) ListChannels(ctx context.Context, limit int) ([]models.Channel, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, name, segment_sec, COALESCE(ladder,'[]'), created_at FROM channel ORDER BY id LIMIT $1`, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.Channel{}
    for rows.Next() {
        var c models.Channel
        if err := rows.Scan(&c.ID, &c.Name, &c.SegmentSec, &c.Ladder, &c.CreatedAt); err != nil { return nil, err }
        out = append(out, c)
    }
    return out, rows.Err()
}

func (p *Postgres) UpsertTimeline(ctx context.Context, channelID string, entries []models.TimelineEntry) error {
    if channelID == "" { return errors.New("channel_id required") }
    batch := &pgx.Batch{}
    for _, e := range entries {
        batch.Queue(`INSERT INTO timeline_entry(channel_id,start_at,type,asset_id,uri,duration_s,category,scte35) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
          ON CONFLICT(channel_id,start_at) DO UPDATE SET type=EXCLUDED.type, asset_id=EXCLUDED.asset_id, uri=EXCLUDED.uri, duration_s=EXCLUDED.duration_s, category=EXCLUDED.category, scte35=EXCLUDED.scte35`,
            channelID, e.Start, e.Type, e.AssetID, e.URI, e.DurationS, e.Category, e.SCTE35)
    }
    br := p.pool.SendBatch(ctx, batch)
    defer br.Close()
    for range entries {
        if _, err := br.Exec(); err != nil { return err }
    }
    return nil
}

func (p *Postgres) GetTimelineRange(ctx context.Context, channelID string, from time.Time, to time.Time) ([]models.TimelineEntry, error) {
    rows, err := p.pool.Query(ctx, `SELECT type, COALESCE(asset_id,''), COALESCE(uri,''), start_at, duration_s, COALESCE(category,''), COALESCE(scte35,'') FROM timeline_entry WHERE channel_id=$1 AND start_at >= $2 AND start_at < $3 ORDER BY start_at`, channelID, from, to)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.TimelineEntry{}
    for rows.Next() {
        var e models.TimelineEntry
        if err := rows.Scan(&e.Type, &e.AssetID, &e.URI, &e.Start, &e.DurationS, &e.Category, &e.SCTE35); err != nil { return nil, err }
        out = append(out, e)
    }
    return out, rows.Err()
}

// ReplaceTimelineRange deletes all entries in [from,to) for the channel and inserts provided entries in a transaction.
func (p *Postgres) ReplaceTimelineRange(ctx context.Context, channelID string, from time.Time, to time.Time, entries []models.TimelineEntry) error {
    tx, err := p.pool.Begin(ctx)
    if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    if _, err := tx.Exec(ctx, `DELETE FROM timeline_entry WHERE channel_id=$1 AND start_at >= $2 AND start_at < $3`, channelID, from, to); err != nil { return err }
    batch := &pgx.Batch{}
    for _, e := range entries {
        batch.Queue(`INSERT INTO timeline_entry(channel_id,start_at,type,asset_id,uri,duration_s,category,scte35) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
            channelID, e.Start, e.Type, e.AssetID, e.URI, e.DurationS, e.Category, e.SCTE35)
    }
    br := tx.SendBatch(ctx, batch)
    for range entries {
        if _, err := br.Exec(); err != nil { br.Close(); return err }
    }
    if err := br.Close(); err != nil { return err }
    return tx.Commit(ctx)
}

// HorizonCoveredUntil returns the maximal end time of entries for a channel (MAX(start_at + duration)). If none, returns zero.
func (p *Postgres) HorizonCoveredUntil(ctx context.Context, channelID string) (time.Time, error) {
    var t time.Time
    // start_at + duration_s seconds
    err := p.pool.QueryRow(ctx, `SELECT COALESCE(MAX(start_at + make_interval(secs => duration_s)), to_timestamp(0)) FROM timeline_entry WHERE channel_id=$1`, channelID).Scan(&t)
    return t, err
}

// TrimTimelinePast deletes entries older than the provided cutoff for a channel. If channelID is empty, all channels.
func (p *Postgres) TrimTimelinePast(ctx context.Context, channelID string, cutoff time.Time) (int64, error) {
    var cmd string
    var tag pgconn.CommandTag
    var err error
    if channelID == "" {
        cmd = `DELETE FROM timeline_entry WHERE start_at < $1`
        tag, err = p.pool.Exec(ctx, cmd, cutoff)
    } else {
        cmd = `DELETE FROM timeline_entry WHERE channel_id=$1 AND start_at < $2`
        tag, err = p.pool.Exec(ctx, cmd, channelID, cutoff)
    }
    if err != nil { return 0, err }
    return tag.RowsAffected(), nil
}

// SelectAssetsByTag returns assets matching a tag/library. Minimal fields only.
func (p *Postgres) SelectAssetsByTag(ctx context.Context, tag string, minDuration int) ([]models.Asset, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, uri, duration_s FROM asset WHERE $1 = ANY(tags) AND duration_s >= $2 ORDER BY id`, tag, minDuration)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.Asset{}
    for rows.Next() {
        var a models.Asset
        if err := rows.Scan(&a.ID, &a.URI, &a.DurationS); err != nil { return nil, err }
        out = append(out, a)
    }
    return out, rows.Err()
}


