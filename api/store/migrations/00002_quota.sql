-- +goose Up

-- The YouTube Data API methods this service calls, with the quota units each
-- call costs.
CREATE TABLE quota_methods (
    method TEXT PRIMARY KEY,
    units INTEGER NOT NULL CHECK (units > 0)
);

-- One row per Data API call, charged before the call is made, because YouTube
-- charges a request even when it fails. quota_date is the Pacific date the call
-- counts against: the daily quota resets at midnight Pacific Time. units is the
-- method's cost at the time of the call.
CREATE TABLE quota_spends (
    spend_id INTEGER PRIMARY KEY,
    quota_date TEXT NOT NULL,
    method TEXT NOT NULL REFERENCES quota_methods (method),
    units INTEGER NOT NULL CHECK (units > 0),
    spent_ts TEXT NOT NULL
);

CREATE INDEX quota_spends_by_date ON quota_spends (quota_date);

-- +goose Down

DROP INDEX quota_spends_by_date;
DROP TABLE quota_spends;
DROP TABLE quota_methods;
