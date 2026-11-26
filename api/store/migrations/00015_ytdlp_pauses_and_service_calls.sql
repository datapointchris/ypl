-- +goose Up

-- Every pause of the server's reads of YouTube through yt-dlp: when it began,
-- when it ends, why, and the failure of the read that began it. The server
-- takes up the latest one as it starts, so a restart does not read into a
-- refusal that still holds.
CREATE TABLE ytdlp_pauses (
    pause_id INTEGER PRIMARY KEY,
    paused_ts TEXT NOT NULL,
    until_ts TEXT NOT NULL,
    cause TEXT NOT NULL,
    error TEXT NOT NULL,
    CHECK (paused_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (until_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (cause IN ('refused', 'failing'))
);

-- The latest sync run that YouTube rate limited begins a pause of a day, so a
-- refusal recorded before this table existed still holds.
INSERT INTO ytdlp_pauses (paused_ts, until_ts, cause, error)
SELECT
    finished_ts,
    strftime('%Y-%m-%dT%H:%M:%SZ', finished_ts, '+1 day'),
    'refused',
    'YouTube refused a read of a video during sync run ' || run_id
FROM sync_runs
WHERE is_rate_limited = 1
ORDER BY finished_ts DESC
LIMIT 1;

-- Every request a service client's call sent to the Data API: the client, the
-- method, when it was sent and the Pacific date whose quota it counts against,
-- what its attempts cost, and why it failed where it did. The day's quota is
-- this beside youtube_writes and the sync runs' reads.
CREATE TABLE youtube_service_calls (
    call_id INTEGER PRIMARY KEY,
    client_id TEXT NOT NULL,
    method TEXT NOT NULL,
    sent_ts TEXT NOT NULL,
    quota_date TEXT NOT NULL,
    requests INTEGER NOT NULL,
    units INTEGER NOT NULL,
    error TEXT,
    CHECK (sent_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'),
    CHECK (quota_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    CHECK (requests >= 0 AND units >= 0)
);

CREATE INDEX youtube_service_calls_by_date ON youtube_service_calls (quota_date, client_id);

-- +goose Down

DROP INDEX youtube_service_calls_by_date;
DROP TABLE youtube_service_calls;
DROP TABLE ytdlp_pauses;
