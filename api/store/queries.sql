-- name: UpsertTrackSource :exec
INSERT INTO track_sources (source, label, description)
VALUES (?, ?, ?)
ON CONFLICT (source) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: UpsertQuotaMethod :exec
INSERT INTO quota_methods (method, units)
VALUES (?, ?)
ON CONFLICT (method) DO UPDATE SET units = excluded.units;

-- name: SpendQuota :execrows
-- Charges one call of a method against quota_date, only while the day's total
-- plus the method's cost stays within the daily limit. It writes no row, and
-- reports 0, when the charge would exceed the limit or the method is unknown.
INSERT INTO quota_spends (quota_date, method, units, spent_ts)
SELECT
    sqlc.arg(quota_date),
    quota_methods.method,
    quota_methods.units,
    sqlc.arg(spent_ts)
FROM quota_methods
WHERE
    quota_methods.method = sqlc.arg(method)
    AND (
        SELECT coalesce(sum(quota_spends.units), 0)
        FROM quota_spends
        WHERE quota_spends.quota_date = sqlc.arg(quota_date)
    ) + quota_methods.units <= sqlc.arg(daily_limit);

-- name: QuotaMethodUnits :one
SELECT units
FROM quota_methods
WHERE method = ?;

-- name: QuotaSpentSince :one
SELECT cast(coalesce(sum(units), 0) AS INTEGER) AS units
FROM quota_spends
WHERE spent_ts >= ?;

-- name: QuotaSpent :one
SELECT cast(coalesce(sum(units), 0) AS INTEGER) AS units
FROM quota_spends
WHERE quota_date = ?;

-- name: ImportVideo :exec
-- Writes every column, so it is only for a copy of a whole row. A caller holding
-- some of a video's columns would overwrite the rest.
INSERT INTO videos (
    video_id, title, channel_title, duration_seconds, description, upload_date, is_unavailable, enriched_ts
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (video_id) DO UPDATE SET
    title = excluded.title,
    channel_title = excluded.channel_title,
    duration_seconds = excluded.duration_seconds,
    description = excluded.description,
    upload_date = excluded.upload_date,
    is_unavailable = excluded.is_unavailable,
    enriched_ts = excluded.enriched_ts;

-- name: GetVideo :one
SELECT
    video_id,
    title,
    channel_title,
    duration_seconds,
    description,
    upload_date,
    is_unavailable,
    enriched_ts
FROM videos
WHERE video_id = ?;

-- name: DeleteTracks :exec
DELETE FROM tracks
WHERE video_id = ?;

-- name: InsertTrack :exec
INSERT INTO tracks (video_id, position, start_seconds, end_seconds, artist, title, raw_text, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTracks :many
SELECT
    track_id,
    video_id,
    position,
    start_seconds,
    end_seconds,
    artist,
    title,
    raw_text,
    source
FROM tracks
WHERE video_id = ?
ORDER BY position;

-- name: UpsertEnrichFailure :exec
INSERT INTO enrich_failures (video_id, attempted_ts, reason)
VALUES (?, ?, ?)
ON CONFLICT (video_id) DO UPDATE SET
    attempted_ts = excluded.attempted_ts,
    reason = excluded.reason;

-- name: GetEnrichFailure :one
SELECT
    video_id,
    attempted_ts,
    reason
FROM enrich_failures
WHERE video_id = ?;

-- name: CountVideos :one
SELECT count(*) FROM videos;

-- name: CountTracks :one
SELECT count(*) FROM tracks;

-- name: CountEnrichFailures :one
SELECT count(*) FROM enrich_failures;
