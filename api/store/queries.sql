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

-- name: ListVideoAvailability :many
-- Each of video_ids the store holds, and whether it is unavailable.
SELECT
    video_id,
    is_unavailable
FROM videos
WHERE video_id IN (sqlc.slice(video_ids));

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

-- name: UpdatePlaylistDetails :execrows
-- Sets a stored playlist's title and description, and changes nothing when no
-- playlist has the id.
UPDATE playlists SET
    title = sqlc.arg(title),
    description = sqlc.arg(description)
WHERE playlist_id = sqlc.arg(playlist_id);

-- name: UpsertPlaylistPrivacy :exec
INSERT INTO playlist_privacies (privacy, label, description)
VALUES (?, ?, ?)
ON CONFLICT (privacy) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: GetPlaylist :one
SELECT
    playlist_id,
    title,
    description,
    privacy,
    revision,
    sort,
    unanswered_write_id,
    refused_write_id
FROM playlists
WHERE playlist_id = ?;

-- name: ListPlaylistIDs :many
SELECT playlist_id FROM playlists
ORDER BY playlist_id;

-- name: DeletePlaylist :exec
DELETE FROM playlists
WHERE playlist_id = ?;

-- name: GetPlaylistState :one
-- The revision of the server's order of a playlist, how YouTube orders it, the
-- push write whose answer was lost, and the push write YouTube refused.
SELECT
    revision,
    sort,
    unanswered_write_id,
    refused_write_id
FROM playlists
WHERE playlist_id = ?;

-- name: BumpRevision :execrows
-- Counts a change to the server's order of a playlist, and changes nothing
-- unless the playlist is at revision.
UPDATE playlists SET revision = revision + 1
WHERE playlist_id = sqlc.arg(playlist_id) AND revision = sqlc.arg(revision);

-- name: SetPlaylistSort :exec
UPDATE playlists SET sort = sqlc.arg(sort)
WHERE playlist_id = sqlc.arg(playlist_id);

-- name: SetUnansweredWrite :exec
UPDATE playlists SET unanswered_write_id = sqlc.narg(unanswered_write_id)
WHERE playlist_id = sqlc.arg(playlist_id);

-- name: SetRefusedWrite :exec
UPDATE playlists SET refused_write_id = sqlc.narg(refused_write_id)
WHERE playlist_id = sqlc.arg(playlist_id);

-- name: UpsertPlaylistSort :exec
INSERT INTO playlist_sorts (sort, label, description)
VALUES (?, ?, ?)
ON CONFLICT (sort) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: ListEntries :many
-- The server's order of a playlist.
SELECT
    entry_id,
    video_id,
    item_id
FROM playlist_entries
WHERE playlist_id = ?
ORDER BY position;

-- name: DeleteEntries :exec
DELETE FROM playlist_entries
WHERE playlist_id = ?;

-- name: InsertEntry :exec
-- Inserts an entry under entry_id, or under a new id when entry_id is NULL.
INSERT INTO playlist_entries (entry_id, playlist_id, position, video_id, item_id)
VALUES (sqlc.narg(entry_id), sqlc.arg(playlist_id), sqlc.arg(position), sqlc.arg(video_id), sqlc.narg(item_id));

-- name: SetEntryItem :execrows
-- Records the YouTube item an entry is held in, and changes nothing when no
-- entry has the id.
UPDATE playlist_entries SET item_id = sqlc.arg(item_id)
WHERE entry_id = sqlc.arg(entry_id);

-- name: ListBaseItems :many
-- What YouTube held of a playlist after the server last read it, with each push
-- write YouTube answered since.
SELECT
    item_id,
    video_id,
    is_placed
FROM base_items
WHERE playlist_id = ?
ORDER BY position;

-- name: DeleteBaseItems :exec
DELETE FROM base_items
WHERE playlist_id = ?;

-- name: InsertBaseItem :exec
INSERT INTO base_items (item_id, playlist_id, position, video_id, is_placed)
VALUES (?, ?, ?, ?, ?);

-- name: UpsertSyncOutcome :exec
INSERT INTO sync_outcomes (outcome, label, description)
VALUES (?, ?, ?)
ON CONFLICT (outcome) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: InsertSyncRun :one
INSERT INTO sync_runs (
    started_ts, finished_ts, quota_date, outcome, playlists, playlists_deleted, playlists_skipped,
    playlists_deferred, items_added, items_removed, requests, units, writes, write_units
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
    items_added,
    items_removed,
    requests,
    units,
    playlists_deferred,
    writes,
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

-- name: ListPlaylistSummaries :many
-- Each playlist with how many items it holds, how many of those hold an
-- unavailable video, and how many hold a video enrichment has read, in no
-- order. Names sort by Unicode collation, and SQLite's NOCASE folds only ASCII.
SELECT
    p.playlist_id,
    p.title,
    p.description,
    p.privacy,
    CAST(count(pe.entry_id) AS INTEGER) AS item_count,
    CAST(coalesce(sum(v.is_unavailable), 0) AS INTEGER) AS unavailable_count,
    CAST(count(v.enriched_ts) AS INTEGER) AS enriched_count
FROM playlists AS p
LEFT JOIN playlist_entries AS pe ON p.playlist_id = pe.playlist_id
LEFT JOIN videos AS v ON pe.video_id = v.video_id
GROUP BY p.playlist_id;

-- name: GetPlaylistSummary :one
-- One playlist with the counts ListPlaylistSummaries gives each.
SELECT
    p.playlist_id,
    p.title,
    p.description,
    p.privacy,
    CAST(count(pe.entry_id) AS INTEGER) AS item_count,
    CAST(coalesce(sum(v.is_unavailable), 0) AS INTEGER) AS unavailable_count,
    CAST(count(v.enriched_ts) AS INTEGER) AS enriched_count
FROM playlists AS p
LEFT JOIN playlist_entries AS pe ON p.playlist_id = pe.playlist_id
LEFT JOIN videos AS v ON pe.video_id = v.video_id
WHERE p.playlist_id = ?
GROUP BY p.playlist_id;

-- name: ListPlaylistEntries :many
-- The server's order of a playlist, each entry with its video and the video's
-- track count.
SELECT
    pe.entry_id,
    pe.item_id,
    pe.position,
    v.video_id,
    v.title,
    v.channel_title,
    v.duration_seconds,
    v.upload_date,
    v.is_unavailable,
    v.enriched_ts,
    CAST((SELECT count(*) FROM tracks AS t WHERE t.video_id = v.video_id) AS INTEGER) AS track_count
FROM playlist_entries AS pe
INNER JOIN videos AS v ON pe.video_id = v.video_id
WHERE pe.playlist_id = ?
ORDER BY pe.position;

-- name: ListLibraryVideos :many
-- Every available video some playlist holds, narrowed by each filter that is
-- not NULL: the playlist holding it, and its shortest and longest duration. A
-- video whose duration is unknown satisfies neither bound.
SELECT
    v.video_id,
    v.title,
    v.channel_title,
    v.duration_seconds,
    v.upload_date,
    v.enriched_ts,
    CAST((SELECT count(*) FROM tracks AS t WHERE t.video_id = v.video_id) AS INTEGER) AS track_count
FROM videos AS v
WHERE
    v.is_unavailable = 0
    AND EXISTS (
        SELECT 1 FROM playlist_entries AS pe
        WHERE
            pe.video_id = v.video_id
            AND (CAST(sqlc.narg(playlist_id) AS TEXT) IS NULL OR pe.playlist_id = sqlc.narg(playlist_id))
    )
    AND (CAST(sqlc.narg(min_seconds) AS INTEGER) IS NULL OR v.duration_seconds >= sqlc.narg(min_seconds))
    AND (CAST(sqlc.narg(max_seconds) AS INTEGER) IS NULL OR v.duration_seconds <= sqlc.narg(max_seconds))
ORDER BY v.video_id;

-- name: ListVideoArtists :many
-- Every video's artists, or only the video video_id's when it is not NULL, with
-- how many of its tracks name each, in no order.
SELECT
    video_id,
    artist,
    CAST(count(*) AS INTEGER) AS appearances
FROM tracks
WHERE
    artist IS NOT NULL AND artist != ''
    AND (CAST(sqlc.narg(video_id) AS TEXT) IS NULL OR video_id = sqlc.narg(video_id))
GROUP BY video_id, artist;

-- name: ListVideoPlaylists :many
-- Every playlist holding each video, or only the video video_id when it is not
-- NULL, in no order.
SELECT DISTINCT
    pe.video_id,
    p.playlist_id,
    p.title
FROM playlist_entries AS pe
INNER JOIN playlists AS p ON pe.playlist_id = p.playlist_id
WHERE CAST(sqlc.narg(video_id) AS TEXT) IS NULL OR pe.video_id = sqlc.narg(video_id);

-- name: InsertPlay :execrows
-- Records a play under the next handle, and records nothing when a play with
-- that id is already stored.
INSERT INTO plays (play_id, handle, video_id, played_ts)
VALUES (
    sqlc.arg(play_id),
    (SELECT coalesce(max(p.handle), 0) + 1 FROM plays AS p),
    sqlc.arg(video_id),
    sqlc.arg(played_ts)
)
ON CONFLICT (play_id) DO NOTHING;

-- name: GetPlay :one
-- A play with its video.
SELECT
    pl.play_id,
    pl.handle,
    pl.played_ts,
    v.video_id,
    v.title,
    v.channel_title
FROM plays AS pl
INNER JOIN videos AS v ON pl.video_id = v.video_id
WHERE pl.play_id = ?;

-- name: GetPlayByHandle :one
-- The play with the handle, with its video.
SELECT
    pl.play_id,
    pl.handle,
    pl.played_ts,
    v.video_id,
    v.title,
    v.channel_title
FROM plays AS pl
INNER JOIN videos AS v ON pl.video_id = v.video_id
WHERE pl.handle = ?;

-- name: ListPlaysByTail :many
-- Every play whose id ends with the eight characters tail, with its video.
SELECT
    pl.play_id,
    pl.handle,
    pl.played_ts,
    v.video_id,
    v.title,
    v.channel_title
FROM plays AS pl
INNER JOIN videos AS v ON pl.video_id = v.video_id
WHERE substr(pl.play_id, -8) = sqlc.arg(tail)
ORDER BY pl.handle;

-- name: ListNewestPlays :many
-- The newest plays, each with its video.
SELECT
    pl.play_id,
    pl.handle,
    pl.played_ts,
    v.video_id,
    v.title,
    v.channel_title
FROM plays AS pl
INNER JOIN videos AS v ON pl.video_id = v.video_id
ORDER BY pl.played_ts DESC, pl.play_id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListPlaysBefore :many
-- The plays that follow the play at played_ts with the id play_id in the order
-- newest first, each with its video. The row-value comparison is what lets
-- SQLite seek plays_by_time to the cursor rather than scan to it.
SELECT
    pl.play_id,
    pl.handle,
    pl.played_ts,
    v.video_id,
    v.title,
    v.channel_title
FROM plays AS pl
INNER JOIN videos AS v ON pl.video_id = v.video_id
WHERE (pl.played_ts, pl.play_id) < (sqlc.arg(played_ts), sqlc.arg(play_id))
ORDER BY pl.played_ts DESC, pl.play_id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListSuggestions :many
-- Available videos some playlist holds, or the named playlist when it is not
-- NULL, least recently played first and never played before any. Videos whose
-- last play is the same come in a random order.
SELECT
    v.video_id,
    v.title,
    v.channel_title,
    v.duration_seconds,
    CAST(count(pl.play_id) AS INTEGER) AS play_count,
    max(pl.played_ts) AS last_played_ts
FROM videos AS v
LEFT JOIN plays AS pl ON v.video_id = pl.video_id
WHERE
    v.is_unavailable = 0
    AND EXISTS (
        SELECT 1 FROM playlist_entries AS pe
        WHERE
            pe.video_id = v.video_id
            AND (CAST(sqlc.narg(playlist_id) AS TEXT) IS NULL OR pe.playlist_id = sqlc.narg(playlist_id))
    )
GROUP BY v.video_id
ORDER BY max(pl.played_ts) IS NOT NULL, max(pl.played_ts), random()
LIMIT sqlc.arg(max_rows);

-- name: ListNewestSyncRuns :many
-- The newest runs.
SELECT
    run_id,
    started_ts,
    finished_ts,
    quota_date,
    outcome,
    playlists,
    playlists_deleted,
    playlists_skipped,
    items_added,
    items_removed,
    requests,
    units,
    playlists_deferred,
    writes,
    write_units
FROM sync_runs
ORDER BY run_id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListSyncRunsBefore :many
-- The runs before the run run_id, newest first.
SELECT
    run_id,
    started_ts,
    finished_ts,
    quota_date,
    outcome,
    playlists,
    playlists_deleted,
    playlists_skipped,
    items_added,
    items_removed,
    requests,
    units,
    playlists_deferred,
    writes,
    write_units
FROM sync_runs
WHERE run_id < sqlc.arg(run_id)
ORDER BY run_id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListSyncFailuresBetween :many
-- The failures of every run from first_run_id to last_run_id, both included.
SELECT
    sync_failure_id,
    run_id,
    playlist_id,
    error
FROM sync_failures
WHERE run_id BETWEEN sqlc.arg(first_run_id) AND sqlc.arg(last_run_id)
ORDER BY run_id, sync_failure_id;

-- name: GetLatestSyncRunWithOutcome :one
SELECT
    run_id,
    started_ts,
    finished_ts,
    quota_date,
    outcome,
    playlists,
    playlists_deleted,
    playlists_skipped,
    items_added,
    items_removed,
    requests,
    units,
    playlists_deferred,
    writes,
    write_units
FROM sync_runs
WHERE outcome = ?
ORDER BY run_id DESC
LIMIT 1;

-- name: CountLibrary :one
-- How many playlists, videos some playlist holds, of those videos how many are
-- unavailable and how many enrichment has read, tracks and plays the store
-- holds.
SELECT
    CAST((SELECT count(*) FROM playlists) AS INTEGER) AS playlists,
    CAST((
        SELECT count(*) FROM videos AS v
        WHERE EXISTS (SELECT 1 FROM playlist_entries AS pe WHERE pe.video_id = v.video_id)
    ) AS INTEGER) AS videos,
    CAST((
        SELECT count(*) FROM videos AS v
        WHERE
            v.is_unavailable = 1
            AND EXISTS (SELECT 1 FROM playlist_entries AS pe WHERE pe.video_id = v.video_id)
    ) AS INTEGER) AS unavailable_videos,
    CAST((
        SELECT count(*) FROM videos AS v
        WHERE
            v.enriched_ts IS NOT NULL
            AND EXISTS (SELECT 1 FROM playlist_entries AS pe WHERE pe.video_id = v.video_id)
    ) AS INTEGER) AS enriched_videos,
    CAST((SELECT count(*) FROM tracks) AS INTEGER) AS tracks,
    CAST((SELECT count(*) FROM plays) AS INTEGER) AS plays;

-- name: UpsertYouTubeWriteMethod :exec
INSERT INTO youtube_write_methods (method, label, description)
VALUES (?, ?, ?)
ON CONFLICT (method) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: UpsertYouTubeWriteOutcome :exec
INSERT INTO youtube_write_outcomes (outcome, label, description)
VALUES (?, ?, ?)
ON CONFLICT (outcome) DO UPDATE SET
    label = excluded.label,
    description = excluded.description;

-- name: InsertYouTubeWrite :one
-- Records a write as pending, before it is sent.
INSERT INTO youtube_writes (method, playlist_id, item_id, video_id, entry_id, position, sent_ts, quota_date, outcome)
VALUES (
    sqlc.arg(method),
    sqlc.narg(playlist_id),
    sqlc.narg(item_id),
    sqlc.narg(video_id),
    sqlc.narg(entry_id),
    sqlc.narg(position),
    sqlc.arg(sent_ts),
    sqlc.arg(quota_date),
    'pending'
)
RETURNING write_id;

-- name: SettleYouTubeWrite :execrows
-- Records how a pending write ended, and changes nothing for a write already
-- settled.
UPDATE youtube_writes SET
    playlist_id = sqlc.narg(playlist_id),
    item_id = coalesce(sqlc.narg(item_id), item_id),
    outcome = sqlc.arg(outcome),
    settled_ts = sqlc.arg(settled_ts),
    requests = sqlc.arg(requests),
    units = sqlc.arg(units),
    error = sqlc.narg(error)
WHERE write_id = sqlc.arg(write_id) AND outcome = 'pending';

-- name: GetYouTubeWrite :one
SELECT
    write_id,
    method,
    playlist_id,
    item_id,
    video_id,
    entry_id,
    position,
    sent_ts,
    quota_date,
    outcome,
    settled_ts,
    requests,
    units,
    error
FROM youtube_writes
WHERE write_id = ?;

-- name: CountItemWritesAfter :one
-- How many writes to the items of a playlist settled after after, or were sent
-- after it and have not settled.
SELECT count(*) FROM youtube_writes
WHERE
    playlist_id = sqlc.arg(playlist_id)
    AND method IN ('playlistItems.insert', 'playlistItems.update', 'playlistItems.delete')
    AND coalesce(settled_ts, sent_ts) > sqlc.arg(after);

-- name: SumWriteUnits :one
-- The units the writes sent on quota_date cost, and how many of them have not
-- settled, whose cost is not yet recorded.
SELECT
    CAST(coalesce(sum(units), 0) AS INTEGER) AS units,
    CAST(coalesce(sum(CASE WHEN outcome = 'pending' THEN 1 ELSE 0 END), 0) AS INTEGER) AS pending
FROM youtube_writes
WHERE quota_date = ?;

-- name: SumRunReadUnits :one
-- The units the sync runs on quota_date spent reading.
SELECT CAST(coalesce(sum(units - write_units), 0) AS INTEGER) FROM sync_runs
WHERE quota_date = ?;

-- name: LatestPlaylistWriteSettledAfter :one
-- The latest write creating, updating or deleting the playlist that settled
-- after settled_after with YouTube's answer that it made the write, or that the
-- playlist does not exist.
SELECT
    method,
    outcome
FROM youtube_writes
WHERE
    playlist_id = sqlc.arg(playlist_id)
    AND method IN ('playlists.insert', 'playlists.update', 'playlists.delete')
    AND outcome IN ('applied', 'absent')
    AND settled_ts > sqlc.arg(settled_after)
ORDER BY settled_ts DESC, write_id DESC
LIMIT 1;
