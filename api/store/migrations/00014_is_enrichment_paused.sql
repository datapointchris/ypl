-- +goose Up

-- A boolean column starts is_ or has_, so its name reads as a flag wherever
-- the schema is not in front of the reader.
ALTER TABLE sync_runs RENAME COLUMN enrichment_paused TO is_enrichment_paused;

-- +goose Down

ALTER TABLE sync_runs RENAME COLUMN is_enrichment_paused TO enrichment_paused;
