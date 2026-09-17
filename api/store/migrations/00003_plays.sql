-- +goose Up

-- One row per listen. A play is a user's log entry, keyed by a UUIDv7 the
-- client generates, so a client that sends the same play twice records it once.
-- played_ts is an RFC 3339 timestamp in UTC to the second, the one form whose
-- text sorts in time order. The CHECK holds that shape; it does not check the
-- calendar.
CREATE TABLE plays (
    play_id TEXT PRIMARY KEY,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    played_ts TEXT NOT NULL
    CHECK (played_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z')
);

CREATE INDEX plays_by_video ON plays (video_id, played_ts);

-- Plays newest first, the order they are listed and paged in.
CREATE INDEX plays_by_time ON plays (played_ts, play_id);

-- +goose Down

DROP INDEX plays_by_time;
DROP INDEX plays_by_video;
DROP TABLE plays;
