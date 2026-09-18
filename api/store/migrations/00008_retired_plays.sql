-- +goose Up

-- A deleted play keeps its id and its handle here, so neither names another
-- play later. A handle is what a person types, and one given out again would
-- make an old command reach a different listen. An id sent again after its
-- play was deleted is refused rather than stored a second time.
CREATE TABLE retired_plays (
    play_id TEXT PRIMARY KEY,
    handle INTEGER NOT NULL UNIQUE CHECK (handle > 0)
);

-- +goose Down

DROP TABLE retired_plays;
