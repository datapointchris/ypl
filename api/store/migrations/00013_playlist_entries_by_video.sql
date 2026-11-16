-- +goose Up

-- The entries holding one video, which is how a video is shown with the
-- playlists it is in. Every other index on the table leads with the playlist.
CREATE INDEX playlist_entries_by_video ON playlist_entries (video_id);

-- +goose Down

DROP INDEX playlist_entries_by_video;
