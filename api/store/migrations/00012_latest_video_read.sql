-- +goose Up

-- When enrichment last began a read of a video through yt-dlp, and which video.
-- One row, written before the read is sent, so a read a restart cut short still
-- counts. The reading that starts after a restart waits out its pace from here.
CREATE TABLE latest_video_read (
    latest_id INTEGER PRIMARY KEY CHECK (latest_id = 1),
    video_id TEXT NOT NULL,
    started_ts TEXT NOT NULL CHECK (
        started_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    )
);

-- +goose Down

DROP TABLE latest_video_read;
