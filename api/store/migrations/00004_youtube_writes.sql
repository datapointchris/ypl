-- +goose Up

-- The YouTube methods a write calls, the vocabulary youtube_writes.method draws
-- from.
CREATE TABLE youtube_write_methods (
    method TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- How a write sent to YouTube ended, the vocabulary youtube_writes.outcome
-- draws from.
CREATE TABLE youtube_write_outcomes (
    outcome TEXT PRIMARY KEY,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- Every write the server sends YouTube. A row is inserted pending before its
-- write is sent, and settled with YouTube's answer in the transaction that
-- stores what the answer changed, so a row left pending is a write whose answer
-- was never recorded. The server writes these about its own work, so they key
-- on the integer rowid.
--
-- playlist_id names no foreign key, since a write can delete its playlist, and
-- is NULL for a create YouTube has not answered with an id. quota_date is the
-- Pacific date the write was sent on. settled_ts, requests and units are when
-- the write settled and what its attempts cost, and error is why it did not
-- apply.
CREATE TABLE youtube_writes (
    write_id INTEGER PRIMARY KEY,
    method TEXT NOT NULL REFERENCES youtube_write_methods (method),
    playlist_id TEXT,
    sent_ts TEXT NOT NULL,
    quota_date TEXT NOT NULL,
    outcome TEXT NOT NULL REFERENCES youtube_write_outcomes (outcome),
    settled_ts TEXT,
    requests INTEGER,
    units INTEGER,
    error TEXT,
    CHECK (sent_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (settled_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (quota_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    CHECK ((outcome = 'pending') = (settled_ts IS NULL)),
    CHECK ((outcome = 'pending') = (requests IS NULL)),
    CHECK ((outcome = 'pending') = (units IS NULL)),
    CHECK ((outcome IN ('pending', 'applied')) = (error IS NULL))
);

CREATE INDEX youtube_writes_by_playlist ON youtube_writes (playlist_id, settled_ts);

-- +goose Down

DROP INDEX youtube_writes_by_playlist;
DROP TABLE youtube_writes;
DROP TABLE youtube_write_outcomes;
DROP TABLE youtube_write_methods;
