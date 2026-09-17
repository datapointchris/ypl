-- name: UpsertTrackSource :exec
INSERT INTO track_sources (source, label, description)
VALUES (?, ?, ?)
ON CONFLICT (source) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

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

-- name: UpsertAvailableVideo :exec
-- Sets what a playlist read reports for a video that plays, and leaves every
-- column enrichment writes as it is.
INSERT INTO videos (video_id, title, channel_title, is_unavailable)
VALUES (?, ?, ?, 0)
ON CONFLICT (video_id) DO UPDATE SET
    title = excluded.title,
    channel_title = excluded.channel_title,
    is_unavailable = 0;

-- name: UpsertUnavailableVideo :exec
-- Records a video a playlist read reports private or deleted. A stored video
-- keeps its title and channel, which YouTube no longer reports.
INSERT INTO videos (video_id, title, channel_title, is_unavailable)
VALUES (?, ?, '', 1)
ON CONFLICT (video_id) DO UPDATE SET
    is_unavailable = 1;

-- name: UpsertPlaylist :exec
INSERT INTO playlists (playlist_id, title, description, privacy)
VALUES (?, ?, ?, ?)
ON CONFLICT (playlist_id) DO UPDATE SET
    title = excluded.title,
    description = excluded.description,
    privacy = excluded.privacy;

-- name: GetPlaylist :one
SELECT
    playlist_id,
    title,
    description,
    privacy,
    is_sorted_manually
FROM playlists
WHERE playlist_id = ?;

-- name: MarkPlaylistNotSortedManually :exec
UPDATE playlists
SET is_sorted_manually = 0
WHERE playlist_id = ?;

-- name: ListPlaylistIDs :many
SELECT playlist_id FROM playlists
ORDER BY playlist_id;

-- name: DeletePlaylist :exec
DELETE FROM playlists
WHERE playlist_id = ?;

-- name: DeletePlaylistItems :exec
DELETE FROM playlist_items
WHERE playlist_id = ?;

-- name: InsertPlaylistItem :exec
INSERT INTO playlist_items (playlist_id, position, video_id)
VALUES (?, ?, ?);

-- name: ListPlaylistVideoIDs :many
SELECT video_id FROM playlist_items
WHERE playlist_id = ?
ORDER BY position;

-- name: DeleteBaseItems :exec
DELETE FROM base_items
WHERE playlist_id = ?;

-- name: InsertBaseItem :exec
INSERT INTO base_items (item_id, playlist_id, position, video_id)
VALUES (?, ?, ?, ?);

-- name: ListBaseItems :many
SELECT
    item_id,
    video_id
FROM base_items
WHERE playlist_id = ?
ORDER BY position;

-- name: UpsertPushRefusal :exec
INSERT INTO push_refusals (playlist_id, video_id, refused_ts, reason)
VALUES (?, ?, ?, ?)
ON CONFLICT (playlist_id, video_id) DO UPDATE SET
    refused_ts = excluded.refused_ts,
    reason = excluded.reason;

-- name: ListPushRefusedVideoIDs :many
SELECT video_id FROM push_refusals
WHERE playlist_id = ?
ORDER BY video_id;

-- name: UpsertSyncOutcome :exec
INSERT INTO sync_outcomes (outcome, label, description)
VALUES (?, ?, ?)
ON CONFLICT (outcome) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: InsertSyncRun :one
INSERT INTO sync_runs (
    started_ts, finished_ts, quota_date, outcome, playlists, playlists_deleted, playlists_skipped,
    pulled_in, pulled_out, writes, requests, read_units, write_units
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING run_id;

-- name: InsertSyncFailure :exec
INSERT INTO sync_failures (run_id, playlist_id, error)
VALUES (?, ?, ?);

-- name: GetSyncRun :one
SELECT
    run_id,
    started_ts,
    finished_ts,
    quota_date,
    outcome,
    playlists,
    playlists_deleted,
    playlists_skipped,
    pulled_in,
    pulled_out,
    writes,
    requests,
    read_units,
    write_units
FROM sync_runs
WHERE run_id = ?;

-- name: ListSyncFailures :many
SELECT
    sync_failure_id,
    run_id,
    playlist_id,
    error
FROM sync_failures
WHERE run_id = ?
ORDER BY sync_failure_id;

-- name: CountQuotaSpentRuns :one
-- How many runs on quota_date ended on YouTube's quota refusal.
SELECT count(*) FROM sync_runs
WHERE quota_date = ? AND outcome = 'quota_spent';

-- name: SumWriteUnits :one
-- The write units every run on quota_date spent, which is the day's write
-- spend the worker's allowance is checked against.
SELECT CAST(coalesce(sum(write_units), 0) AS INTEGER) FROM sync_runs
WHERE quota_date = ?;
