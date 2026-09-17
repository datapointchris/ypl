-- +goose Up

-- Which half of a run a failure is from, the vocabulary sync_failures.stage
-- draws on.
CREATE TABLE sync_stages (
    stage TEXT PRIMARY KEY,
    description TEXT NOT NULL
);

INSERT INTO sync_stages (stage, description) VALUES
('sync', 'Reading the channel''s playlists and writing back what an edit changed'),
('enrichment', 'Reading tracklists for the videos those playlists hold');

-- A failure of the sync as a whole and a failure of its enrichment as a whole
-- both name no playlist and no video, so without this a reader tells them apart
-- only by reading the error prose.
ALTER TABLE sync_failures ADD COLUMN stage TEXT NOT NULL DEFAULT 'sync' REFERENCES sync_stages (stage);

-- Whether the run made no read because YouTube refused one within the pause
-- enrichment keeps after a refusal. It is kept apart from is_rate_limited,
-- which marks the run whose own read drew the refusal and which is what the
-- pause is counted from. A run that recorded the pause as a failure would be
-- partial for every run of the day after one refusal, which costs outcome its
-- meaning for that day.
ALTER TABLE sync_runs ADD COLUMN enrichment_paused BOOLEAN NOT NULL DEFAULT 0 CHECK (
    enrichment_paused IN (0, 1)
);

-- +goose Down

ALTER TABLE sync_runs DROP COLUMN enrichment_paused;
ALTER TABLE sync_failures DROP COLUMN stage;
DROP TABLE sync_stages;
