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
	ProbeMisses       int64         `json:"probe_misses"`
	Failures          []SyncFailure `json:"failures"`
}

// SyncFailure is one thing a run could not do, and the stage it was in.
type SyncFailure struct {
	Stage      string  `json:"stage"`
	PlaylistID *string `json:"playlist_id"`
	VideoID    *string `json:"video_id"`
	Error      string  `json:"error"`
}

// Status is what the store holds, the latest run, the latest run that ended
// ok, and what the sync spends and has waiting. Either run is null before
// there has been one.
type Status struct {
	Library   Library   `json:"library"`
	LastRun   *SyncRun  `json:"last_run"`
	LastOKRun *SyncRun  `json:"last_ok_run"`
	Sync      SyncState `json:"sync"`
}

// SyncState is how the sync runs and what it has waiting: the mean wait
// between its ticks, the day's quota, each playlist whose order YouTube does
// not yet hold, the tracklist reads of the last hour, and the changes its
// sweeps found on YouTube in the last day that its probe missed.
type SyncState struct {
	IntervalSeconds        int64         `json:"interval_seconds"`
	Quota                  Quota         `json:"quota"`
	Pushes                 []PendingPush `json:"pushes"`
	TracklistReadsLastHour int64         `json:"tracklist_reads_last_hour"`
	ProbeMissesLastDay     int64         `json:"probe_misses_last_day"`
}

// Quota is the Pacific day's YouTube quota: the units spent, the most it
// allows, and when it resets, in RFC 3339.
type Quota struct {
	Date       string `json:"date"`
	UnitsSpent int64  `json:"units_spent"`
	UnitsLimit int64  `json:"units_limit"`
	ResetsAt   string `json:"resets_at"`
}

// PendingPush is a playlist whose order YouTube does not yet hold, and the
// writes a push of it plans. Held says the push waits on a write YouTube
// refused today.
type PendingPush struct {
	PlaylistID string `json:"playlist_id"`
	Title      string `json:"title"`
	Writes     int64  `json:"writes"`
	Held       bool   `json:"held"`
}

// Library counts what the store holds. Videos are the ones some playlist holds,
// and the unavailable, enriched and with-a-tracklist counts are among them. A
// read can find no track, so the last two differ. VideosWithTracklist is nil
// from a server that does not count it, which a zero would misreport as none.
type Library struct {
	Playlists           int64  `json:"playlists"`
	Videos              int64  `json:"videos"`
	UnavailableVideos   int64  `json:"unavailable_videos"`
	EnrichedVideos      int64  `json:"enriched_videos"`
	VideosWithTracklist *int64 `json:"videos_with_tracklist"`
	Tracks              int64  `json:"tracks"`
	Plays               int64  `json:"plays"`
}

// ListSyncRuns is the newest limit runs, newest first, reading as many pages as
// that takes.
func (c *Client) ListSyncRuns(ctx context.Context, limit int) (Page[SyncRun], error) {
	return collect(ctx, c, "/api/v1/sync/runs", nil, limit, func(r SyncRun) string { return strconv.FormatInt(r.ID, 10) })
}

// GetStatus is what the store holds and how the sync is faring.
func (c *Client) GetStatus(ctx context.Context) (Status, error) {
	var status Status
	err := c.Get(ctx, "/api/v1/status", &status)
	return status, err
}
