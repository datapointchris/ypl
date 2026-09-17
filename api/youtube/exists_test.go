package youtube

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func TestExistingPlaylistsLeavesOutAnIDYouTubeHasNothingFor(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA"), fakePlaylist(t, "PLB")}
	channel := api.channel()

	found, err := channel.ExistingPlaylists(context.Background(), []PlaylistID{"PLA", "PLgone", "PLB"})
	if err != nil {
		t.Fatalf("ExistingPlaylists: %v", err)
	}
	if !slices.Equal(found, []PlaylistID{"PLA", "PLB"}) || channel.Requests() != 1 {
		t.Fatalf("found %v in %d requests, want PLA and PLB in 1", found, channel.Requests())
	}
}

func TestExistingItemsReadsFiftyIDsARequest(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 120)
	channel := api.channel()
	var ids []ItemID
	for i := range 120 {
		ids = append(ids, ItemID(fmt.Sprintf("PLA-item-%03d", i)))
	}
	asked := append(slices.Clone(ids), "gone-1", "gone-2", "gone-3", "gone-4", "gone-5")

	found, err := channel.ExistingItems(context.Background(), asked)
	if err != nil {
		t.Fatalf("ExistingItems: %v", err)
	}
	if !slices.Equal(found, ids) {
		t.Fatalf("found %d ids, want the 120 that exist", len(found))
	}
	if requests, units := channel.Requests(), channel.Units(); requests != 3 || units != 3*ReadUnits {
		t.Fatalf("counted %d requests and %d units, want 125 ids in 3 requests of 50", requests, units)
	}
}

func TestNoIDsMakeNoRequest(t *testing.T) {
	api := newFakeAPI(t)
	channel := api.channel()

	found, err := channel.ExistingItems(context.Background(), nil)
	if err != nil || len(found) != 0 || channel.Requests() != 0 {
		t.Fatalf("ExistingItems of no ids = %v, %v after %d requests, want nothing and no request", found, err, channel.Requests())
	}
}
