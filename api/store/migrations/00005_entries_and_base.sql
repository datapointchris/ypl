-- +goose Up

-- How YouTube orders a playlist, the vocabulary playlists.sort draws from.
CREATE TABLE playlist_sorts (
    sort TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- Whether a playlist's base is known to be what YouTube holds, the vocabulary
-- playlists.base_state draws from.
CREATE TABLE base_states (
    base_state TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- The rows existing playlists take. Every open upserts both vocabularies whole.
INSERT INTO playlist_sorts (sort, label, description) VALUES ('manual', 'Manual', '');
INSERT INTO base_states (base_state, label, description) VALUES ('current', 'Current', '');

-- The playlists the channel owns, keyed by YouTube's own playlist id, with the
-- title, description and privacy YouTube last reported. revision counts the
-- changes to the server's order of the playlist, sort is how YouTube orders it,
-- and base_state whether its base_items are what YouTube holds.
CREATE TABLE playlists_rebuilt (
    playlist_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    privacy TEXT NOT NULL REFERENCES playlist_privacies (privacy),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    sort TEXT NOT NULL DEFAULT 'manual' REFERENCES playlist_sorts (sort),
    base_state TEXT NOT NULL DEFAULT 'current' REFERENCES base_states (base_state)
);

INSERT INTO playlists_rebuilt (playlist_id, title, description, privacy)
SELECT
    playlist_id,
    title,
    description,
    privacy
FROM playlists;

-- The server's order of each playlist. An entry holds a video, and the id of
-- the YouTube playlist item holding it once YouTube has one; an entry with no
-- item id is one the push has yet to add. A playlist's rows are replaced
-- together, so a position is never renumbered in place and UNIQUE holds at
-- every statement. An entry id is never reused, since a push write names the
-- entry it adds a video for, and a new entry must not take the id of one an edit
-- removed while that write was in flight.
CREATE TABLE playlist_entries (
    entry_id INTEGER PRIMARY KEY AUTOINCREMENT,
    playlist_id TEXT NOT NULL REFERENCES playlists_rebuilt (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    item_id TEXT UNIQUE,
    UNIQUE (playlist_id, position)
);

-- What YouTube held of each playlist after the server last read or wrote it,
-- keyed by YouTube's playlistItem id.
CREATE TABLE base_items (
    item_id TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists_rebuilt (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    UNIQUE (playlist_id, position)
);

INSERT INTO playlist_entries (playlist_id, position, video_id, item_id)
SELECT
    playlist_id,
    position,
    video_id,
    item_id
FROM playlist_items
ORDER BY playlist_id, position;

INSERT INTO base_items (item_id, playlist_id, position, video_id)
SELECT
    item_id,
    playlist_id,
    position,
    video_id
FROM playlist_items;

DROP TABLE playlist_items;
DROP TABLE playlists;
ALTER TABLE playlists_rebuilt RENAME TO playlists;

-- What an item write named: the item it moved or deleted, or the item an insert
-- made once YouTube answered, the video an insert added, and the position a
-- write placed it at.
ALTER TABLE youtube_writes ADD COLUMN item_id TEXT;
ALTER TABLE youtube_writes ADD COLUMN video_id TEXT;
ALTER TABLE youtube_writes ADD COLUMN position INTEGER;

CREATE INDEX youtube_writes_by_quota_date ON youtube_writes (quota_date);

-- How many playlists a run left for the next because a write to their items
-- was newer than its read of them, how many writes it pushed, and the units of
-- those among its own.
ALTER TABLE sync_runs ADD COLUMN playlists_deferred INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN writes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN write_units INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sync_runs DROP COLUMN write_units;
ALTER TABLE sync_runs DROP COLUMN writes;
ALTER TABLE sync_runs DROP COLUMN playlists_deferred;
DROP INDEX youtube_writes_by_quota_date;
ALTER TABLE youtube_writes DROP COLUMN position;
ALTER TABLE youtube_writes DROP COLUMN video_id;
ALTER TABLE youtube_writes DROP COLUMN item_id;

CREATE TABLE playlists_rebuilt (
    playlist_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    privacy TEXT NOT NULL REFERENCES playlist_privacies (privacy)
);

INSERT INTO playlists_rebuilt (playlist_id, title, description, privacy)
SELECT
    playlist_id,
    title,
    description,
    privacy
FROM playlists;

CREATE TABLE playlist_items (
    item_id TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists_rebuilt (playlist_id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    UNIQUE (playlist_id, position)
);

INSERT INTO playlist_items (item_id, playlist_id, position, video_id)
SELECT
    item_id,
    playlist_id,
    position,
    video_id
FROM base_items;

DROP TABLE base_items;
DROP TABLE playlist_entries;
DROP TABLE playlists;
ALTER TABLE playlists_rebuilt RENAME TO playlists;
DROP TABLE base_states;
DROP TABLE playlist_sorts;
