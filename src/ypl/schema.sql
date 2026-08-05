-- Applied only to a fresh database. Anything added here never reaches an
-- existing one, so a change that must land on a populated mirror belongs in
-- indexes.sql or a hand-run statement. The mirror rebuilds from `ypl sync` at
-- no API cost, which is what makes that an acceptable trade rather than a
-- migration system.

CREATE TABLE playlists (
    playlist_id TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    channel     TEXT NOT NULL DEFAULT '',
    item_count  INTEGER NOT NULL DEFAULT 0,
    synced_ts   TEXT NOT NULL
);

CREATE TABLE videos (
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
CREATE TABLE playlist_videos (
    playlist_video_id INTEGER PRIMARY KEY,
    playlist_id       TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    video_id          TEXT NOT NULL REFERENCES videos (video_id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    UNIQUE (playlist_id, position)
);

CREATE TABLE track_sources (
    source      TEXT PRIMARY KEY,
    label       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT ''
);

-- One row per track inside a video. For a DJ mix that is the tracklist; for a
-- single song it is one row or none.
CREATE TABLE tracks (
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

INSERT INTO track_sources (source, label, description) VALUES
    ('chapter',     'Chapter',     'YouTube chapter marker, carries real timestamps'),
    ('description', 'Description', 'Parsed from the video description'),
    ('llm',         'Claude',      'Extracted by Claude from unstructured text'),
    ('manual',      'Manual',      'Entered by hand');
