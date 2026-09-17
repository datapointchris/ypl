package api

import (
	"context"
	"strconv"
)

// SyncRun is one run of the sync, with every failure it recorded.
type SyncRun struct {
	ID                int64         `json:"id"`
	StartedTs         string        `json:"started_ts"`
	FinishedTs        string        `json:"finished_ts"`
	QuotaDate         string        `json:"quota_date"`
	Outcome           string        `json:"outcome"`
	Playlists         int64         `json:"playlists"`
	PlaylistsDeleted  int64         `json:"playlists_deleted"`
	PlaylistsSkipped  int64         `json:"playlists_skipped"`
	PlaylistsDeferred int64         `json:"playlists_deferred"`
	ItemsAdded        int64         `json:"items_added"`
	ItemsRemoved      int64         `json:"items_removed"`
	Writes            int64         `json:"writes"`
	Requests          int64         `json:"requests"`
	Units             int64         `json:"units"`
	WriteUnits        int64         `json:"write_units"`
	VideoReads        int64         `json:"video_reads"`
	VideosEnriched    int64         `json:"videos_enriched"`
	TracksFound       int64         `json:"tracks_found"`
	VideosUnreadable  int64         `json:"videos_unreadable"`
	IsRateLimited     bool          `json:"is_rate_limited"`
	EnrichmentPaused  bool          `json:"enrichment_paused"`
	Failures          []SyncFailure `json:"failures"`
}

// SyncFailure is one thing a run could not do, and the stage it was in.
type SyncFailure struct {
	Stage      string  `json:"stage"`
	PlaylistID *string `json:"playlist_id"`
	VideoID    *string `json:"video_id"`
	Error      string  `json:"error"`
}

// Status is what the store holds, the latest run, and the latest run that ended
// ok. Either run is null before there has been one.
type Status struct {
	Library   Library  `json:"library"`
	LastRun   *SyncRun `json:"last_run"`
	LastOKRun *SyncRun `json:"last_ok_run"`
}

// Library counts what the store holds. Videos are the ones some playlist holds,
// and the unavailable and enriched counts are among them.
type Library struct {
	Playlists         int64 `json:"playlists"`
	Videos            int64 `json:"videos"`
	UnavailableVideos int64 `json:"unavailable_videos"`
	EnrichedVideos    int64 `json:"enriched_videos"`
	Tracks            int64 `json:"tracks"`
	Plays             int64 `json:"plays"`
}

// ListSyncRuns is the newest limit runs, newest first, reading as many pages as
// that takes.
func (c *Client) ListSyncRuns(ctx context.Context, limit int) (Page[SyncRun], error) {
	return collect(ctx, c, "/api/v1/sync/runs", limit, func(r SyncRun) string { return strconv.FormatInt(r.ID, 10) })
}

// GetStatus is what the store holds and how the sync is faring.
func (c *Client) GetStatus(ctx context.Context) (Status, error) {
	var status Status
	err := c.Get(ctx, "/api/v1/status", &status)
	return status, err
}
