-- Indexes only. Both this and schema.sql run on every open, so a new *table*
-- belongs there rather than here.

CREATE INDEX IF NOT EXISTS idx_playlist_videos_playlist ON playlist_videos (playlist_id, position);
CREATE INDEX IF NOT EXISTS idx_playlist_videos_video ON playlist_videos (video_id);
CREATE INDEX IF NOT EXISTS idx_videos_enriched ON videos (enriched_ts);
CREATE INDEX IF NOT EXISTS idx_tracks_video ON tracks (video_id, position);

-- The artist index is what makes similarity work: "which other videos share an
-- artist with this one" is the query behind mix-to-mix comparison, and it runs
-- against every track row in the library.
CREATE INDEX IF NOT EXISTS idx_tracks_artist ON tracks (artist);
