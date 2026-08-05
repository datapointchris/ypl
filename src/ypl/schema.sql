-- Executed on every open, so every statement here must be idempotent. That is
-- what lets a new table reach an already-populated mirror without a migration
-- system: adding one here is enough. Changing or dropping a *column* still is
-- not — the mirror rebuilds from `ypl sync` at no API cost, which is what makes
-- "delete it and re-sync" an acceptable answer to the rare case that needs one.

CREATE TABLE IF NOT EXISTS playlists (
    playlist_id TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    channel     TEXT NOT NULL DEFAULT '',
    item_count  INTEGER NOT NULL DEFAULT 0,
    synced_ts   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS videos (
    video_id         TEXT PRIMARY KEY,
    title            TEXT NOT NULL,
    channel          TEXT NOT NULL DEFAULT '',
    duration_seconds INTEGER,
    description      TEXT NOT NULL DEFAULT '',
    upload_date      TEXT,
    is_unavailable   INTEGER NOT NULL DEFAULT 0,
    -- NULL until a full per-video extraction has run. `ypl sync` populates the
    -- flat fields for free; only enrichment fetches the description and
    -- chapters, which costs one request per video.
    enriched_ts      TEXT
);

-- No UNIQUE(playlist_id, video_id): YouTube permits the same video twice in one
-- playlist, and a music playlist built up over years does contain repeats.
-- Position is the identity of a slot; the video is what fills it.
CREATE TABLE IF NOT EXISTS playlist_videos (
    playlist_video_id INTEGER PRIMARY KEY,
    playlist_id       TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    video_id          TEXT NOT NULL REFERENCES videos (video_id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    UNIQUE (playlist_id, position)
);

CREATE TABLE IF NOT EXISTS track_sources (
    source      TEXT PRIMARY KEY,
    label       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT ''
);

-- One row per track inside a video. For a DJ mix that is the tracklist; for a
-- single song it is one row or none.
CREATE TABLE IF NOT EXISTS tracks (
    track_id      INTEGER PRIMARY KEY,
    video_id      TEXT NOT NULL REFERENCES videos (video_id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    start_seconds INTEGER,
    end_seconds   INTEGER,
    -- Nullable because the split genuinely fails sometimes: a chapter reading
    -- "ID - ID" or "Intro" has no artist to record, and storing '' would make
    -- "unknown artist" indistinguishable from "no artist in the source".
    artist        TEXT,
    title         TEXT NOT NULL,
    raw_text      TEXT NOT NULL,
    source        TEXT NOT NULL REFERENCES track_sources (source),
    UNIQUE (video_id, position)
);

-- Videos a full extraction says will never read: deleted, private, members-only,
-- region-locked. A table rather than a flag on `videos`, for two reasons. A new
-- table reaches an existing mirror and a new column does not, and `is_unavailable`
-- is what the flat playlist listing says — `ypl sync` rewrites it on every run and
-- would undo anything recorded here.
--
-- Without this an unattended enrich re-asks about every dead video on every run,
-- forever, and a library with a few hundred of them never gets past them.
CREATE TABLE IF NOT EXISTS enrich_failures (
    video_id     TEXT PRIMARY KEY REFERENCES videos (video_id) ON DELETE CASCADE,
    attempted_ts TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT ''
);

-- Listening history is deliberately NOT here. It cannot be rebuilt by re-reading
-- YouTube, so by the same rule that put local playlists in files it is data
-- rather than state — see `history.py`. A mirror that can be deleted and
-- re-synced must not be the only copy of something unrecoverable.

INSERT OR IGNORE INTO track_sources (source, label, description) VALUES
    ('chapter',     'Chapter',     'YouTube chapter marker, carries real timestamps'),
    ('description', 'Description', 'Parsed from the video description'),
    ('llm',         'Claude',      'Extracted by Claude from unstructured text'),
    ('manual',      'Manual',      'Entered by hand');
