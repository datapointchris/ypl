-- +goose Up

-- When a sync last took up a read of a playlist's items, whatever the read did,
-- and the count the channel's listing gave the playlist before the last read
-- that merged. The sweep takes the playlist whose read_ts is oldest, so a read
-- that keeps failing moves on like any other. A probe lists every playlist for
-- one unit and reads again only those whose count moved from read_item_count,
-- so the count is compared with YouTube's own and never with the rows a read
-- returned, which YouTube may count differently. Both are NULL until the first
-- read.
ALTER TABLE playlists ADD COLUMN read_ts TEXT;
ALTER TABLE playlists ADD COLUMN read_item_count INTEGER;

-- How many playlists a pass read on its sweep and found changed on YouTube
-- although the probe had not flagged them: an addition or removal the listing's
-- count did not show.
ALTER TABLE sync_runs ADD COLUMN probe_misses INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE sync_runs DROP COLUMN probe_misses;
ALTER TABLE playlists DROP COLUMN read_item_count;
ALTER TABLE playlists DROP COLUMN read_ts;
