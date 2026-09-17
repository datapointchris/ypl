-- +goose Up

-- The playlists the channel owns, keyed by YouTube's own playlist id, with the
-- title, description and privacy YouTube last reported. is_sorted_manually is 0
-- once YouTube refuses a write naming a position in the playlist, and a push then
-- appends and makes no move.
CREATE TABLE playlists (
    playlist_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    privacy TEXT NOT NULL,
    is_sorted_manually BOOLEAN NOT NULL DEFAULT 1 CHECK (is_sorted_manually IN (0, 1))
);

-- The server's order of each playlist. A playlist's rows are replaced together,
-- so a position is never renumbered in place and UNIQUE holds at every
-- statement.
CREATE TABLE playlist_items (
    playlist_item_id INTEGER PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    UNIQUE (playlist_id, position)
);

-- What YouTube held in each playlist after the last reconcile or push: the base
-- of the three-way merge. Keyed by YouTube's playlistItem id, which every write
-- to a slot names. A playlist's rows are replaced together.
CREATE TABLE base_items (
    item_id TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    UNIQUE (playlist_id, position)
);

-- A video YouTube refused to add to a playlist, because it has no such video or
-- will not add it. A push leaves it out while this row stands.
CREATE TABLE push_refusals (
    playlist_id TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    video_id TEXT NOT NULL,
    refused_ts TEXT NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY (playlist_id, video_id)
);

-- How a sync run ended.
CREATE TABLE sync_outcomes (
    outcome TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- One row per sync run, written when it ends. The server writes these about its
-- own work, so they key on the integer rowid. quota_date is the Pacific date the
-- run's units count against, and read_units and write_units are what the run's
-- own requests cost.
CREATE TABLE sync_runs (
    run_id INTEGER PRIMARY KEY,
    started_ts TEXT NOT NULL,
    finished_ts TEXT NOT NULL,
    quota_date TEXT NOT NULL,
    outcome TEXT NOT NULL REFERENCES sync_outcomes (outcome),
    playlists INTEGER NOT NULL,
    playlists_deleted INTEGER NOT NULL,
    playlists_skipped INTEGER NOT NULL,
    pulled_in INTEGER NOT NULL,
    pulled_out INTEGER NOT NULL,
    writes INTEGER NOT NULL,
    requests INTEGER NOT NULL,
    read_units INTEGER NOT NULL,
    write_units INTEGER NOT NULL
);

CREATE INDEX sync_runs_by_quota_date ON sync_runs (quota_date);

-- What went wrong in a run. playlist_id is NULL for a failure of the whole run,
-- and names no foreign key, since the playlist may be deleted afterwards.
CREATE TABLE sync_failures (
    sync_failure_id INTEGER PRIMARY KEY,
    run_id INTEGER NOT NULL REFERENCES sync_runs (run_id) ON DELETE CASCADE,
    playlist_id TEXT,
    error TEXT NOT NULL
);

-- +goose Down

DROP TABLE sync_failures;
DROP INDEX sync_runs_by_quota_date;
DROP TABLE sync_runs;
DROP TABLE sync_outcomes;
DROP TABLE push_refusals;
DROP TABLE base_items;
DROP TABLE playlist_items;
DROP TABLE playlists;
