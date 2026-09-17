-- +goose Up

-- attempts counts the reads of a video that stored no tracklist, and retry_ts
-- is when the video is read again. A NULL retry_ts stops the reading, which
-- takes either YouTube answering that no signed-out read will return the video
-- or enough attempts. An import of a Python mirror decides per failure which of
-- those its recorded message says, since the mirror read a rate limit as a
-- video closed for good.
ALTER TABLE enrich_failures ADD COLUMN attempts INTEGER NOT NULL DEFAULT 1 CHECK (attempts > 0);
ALTER TABLE enrich_failures ADD COLUMN retry_ts TEXT CHECK (
    retry_ts GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
);

-- What a run's enrichment did: how many videos it read with yt-dlp, how many of
-- those it stored a tracklist search for and how many tracks those held, how
-- many it found closed to reading for good, and whether YouTube refused its
-- reads for now.
ALTER TABLE sync_runs ADD COLUMN video_reads INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN videos_enriched INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN tracks_found INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN videos_unreadable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_runs ADD COLUMN is_rate_limited BOOLEAN NOT NULL DEFAULT 0 CHECK (is_rate_limited IN (0, 1));

-- The video a failure is about, when it is one video's. It names no foreign
-- key, since the video may be deleted afterwards.
ALTER TABLE sync_failures ADD COLUMN video_id TEXT;

-- +goose Down

ALTER TABLE sync_failures DROP COLUMN video_id;
ALTER TABLE sync_runs DROP COLUMN is_rate_limited;
ALTER TABLE sync_runs DROP COLUMN videos_unreadable;
ALTER TABLE sync_runs DROP COLUMN tracks_found;
ALTER TABLE sync_runs DROP COLUMN videos_enriched;
ALTER TABLE sync_runs DROP COLUMN video_reads;
ALTER TABLE enrich_failures DROP COLUMN retry_ts;
ALTER TABLE enrich_failures DROP COLUMN attempts;
