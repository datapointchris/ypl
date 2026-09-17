-- +goose Up

-- Who can see a playlist, as YouTube names it.
CREATE TABLE playlist_privacies (
    privacy TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- The playlists the channel owns, keyed by YouTube's own playlist id, with the
-- title, description and privacy YouTube last reported.
CREATE TABLE playlists (
    playlist_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    privacy TEXT NOT NULL REFERENCES playlist_privacies (privacy)
);

-- Each playlist's items in YouTube's order, keyed by YouTube's playlistItem id.
-- A playlist's rows are replaced together, so a position is never renumbered in
-- place and UNIQUE holds at every statement.
CREATE TABLE playlist_items (
    item_id TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    UNIQUE (playlist_id, position)
);

-- How a sync run ended.
CREATE TABLE sync_outcomes (
    outcome TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- One row per sync run, written when it ends. The server writes these about its
-- own work, so they key on the integer rowid. quota_date is the Pacific date the
-- run started on, which its units count against, and requests and units are what
-- the run's own requests cost.
CREATE TABLE sync_runs (
    run_id INTEGER PRIMARY KEY,
    started_ts TEXT NOT NULL,
    finished_ts TEXT NOT NULL,
    quota_date TEXT NOT NULL,
    outcome TEXT NOT NULL REFERENCES sync_outcomes (outcome),
    playlists INTEGER NOT NULL,
    playlists_deleted INTEGER NOT NULL,
    playlists_skipped INTEGER NOT NULL,
    items_added INTEGER NOT NULL,
    items_removed INTEGER NOT NULL,
    requests INTEGER NOT NULL,
    units INTEGER NOT NULL
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
DROP TABLE playlist_items;
DROP TABLE playlists;
DROP TABLE playlist_privacies;
