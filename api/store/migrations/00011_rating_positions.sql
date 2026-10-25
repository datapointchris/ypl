-- +goose Up

-- Where in the mix a rating was left, in seconds from its start, and NULL where
-- nobody said. position_estimated is 1 when the position came from YouTube's
-- resume point for the video, which is a whole percentage of its length and so
-- approximate, and 0 when a person typed it.
ALTER TABLE ratings ADD COLUMN position_seconds INTEGER
CHECK (position_seconds IS NULL OR position_seconds >= 0);

ALTER TABLE ratings ADD COLUMN position_estimated INTEGER NOT NULL DEFAULT 0
CHECK (position_estimated IN (0, 1));

-- +goose Down

ALTER TABLE ratings DROP COLUMN position_estimated;
ALTER TABLE ratings DROP COLUMN position_seconds;
