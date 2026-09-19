package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

// syncRun is one run of the sync: when it ran, the Pacific date whose quota it
// spent, how it ended, what it changed, the writes it pushed, what it cost, what
// its enrichment read, and each playlist or video it could not sync or read. A
// playlist deferred is one whose items were written too recently for the run's
// read to show. A video unreadable is one YouTube will never let a signed-out
// read return, is_rate_limited says YouTube refused the run's own reads of
// videos, and enrichment_paused says it made no read because a refusal within
// the last day still holds. A failure names the stage it happened in, so one
// with a null playlist_id and video_id says which half of the run it failed.
type syncRun struct {
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
	Failures          []syncFailure `json:"failures"`
}

type syncFailure struct {
	Stage      string  `json:"stage"`
	PlaylistID *string `json:"playlist_id"`
	VideoID    *string `json:"video_id"`
	Error      string  `json:"error"`
}

// status is where the server stands: what the store holds, the latest run, and
// the latest run that ended ok.
type status struct {
	Library   library  `json:"library"`
	LastRun   *syncRun `json:"last_run"`
	LastOKRun *syncRun `json:"last_ok_run"`
}

// library counts what the store holds. Videos are those some playlist holds,
// and unavailable_videos, enriched_videos and videos_with_tracklist count among
// them. A video enrichment read can hold no track, so the last two differ.
type library struct {
	Playlists           int64 `json:"playlists"`
	Videos              int64 `json:"videos"`
	UnavailableVideos   int64 `json:"unavailable_videos"`
	EnrichedVideos      int64 `json:"enriched_videos"`
	VideosWithTracklist int64 `json:"videos_with_tracklist"`
	Tracks              int64 `json:"tracks"`
	Plays               int64 `json:"plays"`
}

// listSyncRuns answers a page of runs, newest first.
func (h *Handlers) listSyncRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, runsPage)
	if !ok {
		return
	}
	after := r.URL.Query().Get("starting_after")
	var before int64
	if after != "" {
		id, err := strconv.ParseInt(after, 10, 64)
		if err != nil {
			wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "starting_after %q is not a run id", after)
			return
		}
		before = id
	}
	var shown page[syncRun]
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var rows []generated.SyncRun
		var err error
		if after == "" {
			rows, err = q.ListNewestSyncRuns(ctx, limit+1)
		} else {
			if _, err := q.GetSyncRun(ctx, before); err != nil {
				return paramRow(err, referenceError{name: "starting_after", value: after})
			}
			rows, err = q.ListSyncRunsBefore(ctx, generated.ListSyncRunsBeforeParams{RunID: before, MaxRows: limit + 1})
		}
		if err != nil {
			return err
		}
		runs := make([]syncRun, len(rows))
		for i, row := range rows {
			runs[i] = syncRunFrom(row)
		}
		shown = pageOf(runs, limit)
		if len(shown.Data) == 0 {
			return nil
		}
		// A page is every run between its oldest and newest, so one range read
		// holds the failures of every run on it and of no other.
		failures, err := q.ListSyncFailuresBetween(ctx, generated.ListSyncFailuresBetweenParams{
			FirstRunID: shown.Data[len(shown.Data)-1].ID,
			LastRunID:  shown.Data[0].ID,
		})
		if err != nil {
			return err
		}
		return attachFailures(shown.Data, failures)
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}
	wire.JSON(w, http.StatusOK, shown)
}

func (h *Handlers) showStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var shown status
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		counts, err := q.CountLibrary(ctx)
		if err != nil {
			return err
		}
		shown.Library = library{
			Playlists:           counts.Playlists,
			Videos:              counts.Videos,
			UnavailableVideos:   counts.UnavailableVideos,
			EnrichedVideos:      counts.EnrichedVideos,
			VideosWithTracklist: counts.VideosWithTracklist,
			Tracks:              counts.Tracks,
			Plays:               counts.Plays,
		}
		latest, err := q.ListNewestSyncRuns(ctx, 1)
		if err != nil {
			return err
		}
		if len(latest) == 1 {
			if shown.LastRun, err = runWithFailures(ctx, q, latest[0]); err != nil {
				return err
			}
		}
		lastOK, err := q.GetLatestSyncRunWithOutcome(ctx, store.OutcomeOK)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		shown.LastOKRun, err = runWithFailures(ctx, q, lastOK)
		return err
	})
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	wire.JSON(w, http.StatusOK, shown)
}

// syncRunFrom is row with no failures attached.
func syncRunFrom(row generated.SyncRun) syncRun {
	return syncRun{
		ID:                row.RunID,
		StartedTs:         row.StartedTs,
		FinishedTs:        row.FinishedTs,
		QuotaDate:         row.QuotaDate,
		Outcome:           row.Outcome,
		Playlists:         row.Playlists,
		PlaylistsDeleted:  row.PlaylistsDeleted,
		PlaylistsSkipped:  row.PlaylistsSkipped,
		PlaylistsDeferred: row.PlaylistsDeferred,
		ItemsAdded:        row.ItemsAdded,
		ItemsRemoved:      row.ItemsRemoved,
		Writes:            row.Writes,
		Requests:          row.Requests,
		Units:             row.Units,
		WriteUnits:        row.WriteUnits,
		VideoReads:        row.VideoReads,
		VideosEnriched:    row.VideosEnriched,
		TracksFound:       row.TracksFound,
		VideosUnreadable:  row.VideosUnreadable,
		IsRateLimited:     row.IsRateLimited,
		EnrichmentPaused:  row.EnrichmentPaused,
		Failures:          []syncFailure{},
	}
}

// attachFailures appends each failure to the run in runs it belongs to, and
// refuses a failure of a run runs does not hold.
func attachFailures(runs []syncRun, failures []generated.SyncFailure) error {
	byID := make(map[int64]*syncRun, len(runs))
	for i := range runs {
		byID[runs[i].ID] = &runs[i]
	}
	for _, f := range failures {
		run, ok := byID[f.RunID]
		if !ok {
			return fmt.Errorf("failure %d belongs to run %d, which is not among the runs read", f.SyncFailureID, f.RunID)
		}
		run.Failures = append(run.Failures, syncFailure{Stage: f.Stage, PlaylistID: nullableText(f.PlaylistID), VideoID: nullableText(f.VideoID), Error: f.Error})
	}
	return nil
}

// runWithFailures is row with every one of its failures.
func runWithFailures(ctx context.Context, q *generated.Queries, row generated.SyncRun) (*syncRun, error) {
	failures, err := q.ListSyncFailures(ctx, row.RunID)
	if err != nil {
		return nil, err
	}
	runs := []syncRun{syncRunFrom(row)}
	if err := attachFailures(runs, failures); err != nil {
		return nil, err
	}
	return &runs[0], nil
}
