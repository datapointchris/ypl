-- +goose Up

-- A deleted play keeps its id and its handle here, so neither names another
-- play later. A handle is what a person types, and one given out again would
-- make an old command reach a different listen. An id sent again after its
-- play was deleted is refused rather than stored a second time.
--
-- handle is not unique. A binary older than this table numbers handles from
-- plays alone, so while one runs it can give a deleted play's handle to a new
-- play, and deleting that play later records the handle a second time. Nothing
-- reads the column but the next handle's max() and the lookup of a deleted
-- play, and a second row changes neither answer.
CREATE TABLE deleted_plays (
    play_id TEXT PRIMARY KEY,
    handle INTEGER NOT NULL CHECK (handle > 0)
);

-- +goose Down

DROP TABLE deleted_plays;
