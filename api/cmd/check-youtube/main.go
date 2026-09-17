// Command check-youtube reads every playlist the channel owns, and every item in
// each, through the Data API with the credentials in the environment, charging
// the quota ledger in the API's database. It prints what it read and what the
// read cost as JSON on stdout.
//
//	DATABASE_PATH=/data/api.db YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=… YOUTUBE_REFRESH_TOKEN=… go run ./cmd/check-youtube
//
// It exits 0 when every playlist reads consistently, and 1 otherwise.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// report is what one check read and spent.
type report struct {
	Playlists       int   `json:"playlists"`
	Items           int   `json:"items"`
	Unavailable     int   `json:"unavailable"`
	UnitsSpent      int64 `json:"units_spent"`
	UnitsSpentToday int64 `json:"units_spent_today"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(context.Background(), os.Stdout); err != nil {
		slog.Error("check youtube", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout io.Writer) error {
	creds, err := youtube.CredentialsFromEnv()
	if err != nil {
		return err
	}
	path, err := store.Path()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	service, err := youtube.NewService(ctx, creds)
	if err != nil {
		return err
	}
	ledger := youtube.NewLedger(st.Queries, youtube.DailyQuota)
	reader := youtube.NewReader(service, ledger)
	started := time.Now()

	var r report
	playlists, err := reader.Playlists(ctx)
	if err != nil {
		return err
	}
	r.Playlists = len(playlists)
	for _, playlist := range playlists {
		items, err := reader.Items(ctx, playlist)
		if err != nil {
			return err
		}
		r.Items += len(items)
		for _, item := range items {
			if item.Unavailable {
				r.Unavailable++
			}
		}
	}

	if r.UnitsSpent, err = ledger.SpentSince(ctx, started); err != nil {
		return err
	}
	if r.UnitsSpentToday, err = ledger.Spent(ctx); err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}
