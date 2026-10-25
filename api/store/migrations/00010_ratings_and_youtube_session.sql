-- +goose Up

-- One row per like, dislike or note a person left on a mix, as many as they
-- like. Rows are appended and never edited, and nothing is derived from them
-- but counts: the listing is the record. A like or a dislike may carry a
-- message, and a note always does. rating_id is a UUIDv7 the client generates,
-- so an entry sent twice is stored once.
--
-- rated_ts is an RFC 3339 timestamp in UTC to the second, as plays.played_ts.
CREATE TABLE ratings (
    rating_id TEXT PRIMARY KEY,
    video_id TEXT NOT NULL REFERENCES videos (video_id),
    kind TEXT NOT NULL CHECK (kind IN ('like', 'dislike', 'note')),
    message TEXT CHECK (message IS NULL OR length(message) > 0),
    rated_ts TEXT NOT NULL
    CHECK (rated_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (kind != 'note' OR message IS NOT NULL)
);

CREATE INDEX ratings_by_video ON ratings (video_id, rated_ts);

-- Ratings newest first, the order they are listed and paged in.
CREATE INDEX ratings_by_time ON ratings (rated_ts, rating_id);

-- Free-form tags a rating carried, lowercased and trimmed by the server.
CREATE TABLE rating_tags (
    rating_id TEXT NOT NULL REFERENCES ratings (rating_id) ON DELETE CASCADE,
    tag TEXT NOT NULL CHECK (length(tag) > 0),
    PRIMARY KEY (rating_id, tag)
);

CREATE INDEX rating_tags_by_tag ON rating_tags (tag);

-- The signed-in YouTube session the watch history is read with, as a Netscape
-- cookie jar. One row at most. yt-dlp writes the jar back after each read with
-- the cookies YouTube rotated, and the server stores that version, so the
-- session lasts as long as nobody opens it in a browser.
--
-- last_ok_ts and last_refused_ts are the latest read YouTube answered signed
-- in and the latest it answered signed out; last_error is the refusal's line.
CREATE TABLE youtube_session (
    session_id INTEGER PRIMARY KEY CHECK (session_id = 1),
    cookie_jar TEXT NOT NULL,
    stored_ts TEXT NOT NULL,
    last_ok_ts TEXT,
    last_refused_ts TEXT,
    last_error TEXT
);

-- +goose Down

DROP TABLE youtube_session;
DROP INDEX rating_tags_by_tag;
DROP TABLE rating_tags;
DROP INDEX ratings_by_time;
DROP INDEX ratings_by_video;
DROP TABLE ratings;
