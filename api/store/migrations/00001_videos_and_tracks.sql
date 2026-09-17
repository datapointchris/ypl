-- +goose Up

-- Where a track's text came from. A lookup rather than a CHECK, so a new source
-- is an insert and has somewhere to keep its label.
CREATE TABLE track_sources (
    source TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- Keyed by YouTube's own video id, which is immutable and is the id every
-- request and response names. description, upload_date and enriched_ts are
-- NULL until enrichment has read the video, so a NULL description is "not read"
-- and '' is "read, and empty". upload_date is an ISO date.
CREATE TABLE videos (
    video_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    channel_title TEXT NOT NULL,
    duration_seconds INTEGER,
    description TEXT,
    upload_date TEXT,
    is_unavailable BOOLEAN NOT NULL DEFAULT 0 CHECK (is_unavailable IN (0, 1)),
    enriched_ts TEXT
);

-- One row per track inside a video: a mix's tracklist, or one row for a single
-- song. artist is NULL when the source text carried no separable artist, which
-- is a different fact from an empty name. A video's tracks are replaced
-- together, never reordered in place.
CREATE TABLE tracks (
    track_id INTEGER PRIMARY KEY,
    video_id TEXT NOT NULL REFERENCES videos (video_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    start_seconds INTEGER,
    end_seconds INTEGER,
    artist TEXT,
    title TEXT NOT NULL,
    raw_text TEXT NOT NULL,
    source TEXT NOT NULL REFERENCES track_sources (source),
    UNIQUE (video_id, position)
);

-- Videos a full extraction did not store a tracklist for, with why and when to
-- read them again. Most are read again: a tracklist can be posted after the
-- video is, and a read can fail for a reason the next one does not meet. Kept
-- apart from videos.is_unavailable, which is what a playlist listing reports
-- and which every read rewrites.
CREATE TABLE enrich_failures (
    video_id TEXT PRIMARY KEY REFERENCES videos (video_id) ON DELETE CASCADE,
    attempted_ts TEXT NOT NULL,
    reason TEXT NOT NULL
);

-- +goose Down

DROP TABLE enrich_failures;
DROP TABLE tracks;
DROP TABLE videos;
DROP TABLE track_sources;
