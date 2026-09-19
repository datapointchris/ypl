package tracklist

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MinimumTitleFirst is the fewest of a tracklist's titles that must name an
// artist other tracklists credit before it is read as written title first.
// Over a library of 419 tracklists, three let through an artist-first mix
// whose titles other lists had credited as artists, and four exchanged 28
// lists, each of them written title first.
const MinimumTitleFirst = 4

// creditSeparator is what joins the artists one side of a line credits:
// "Keinemusik & Sevdaliza", "DJ Micks ft. Robin Latimore", "Yotto x Stephen
// Jolk".
var creditSeparator = regexp.MustCompile(`(?i)\s*(?:&|,|\s(?:ft|feat|featuring)\.?\s|\sx\s|\svs\.?\s)\s*`)

// trailingAnnotation is a bracketed note ending a side of a line, a remix
// credit, a label or "(Unreleased)", which belongs to the track rather than to
// whichever side the writer put it on.
var trailingAnnotation = regexp.MustCompile(`\s*[(\[].*$`)

// credited is the artists side credits, as each is compared: without case,
// accents or a trailing annotation. An artist nobody has identified is none.
func credited(side string) []string {
	var keys []string
	for _, name := range creditSeparator.Split(trailingAnnotation.ReplaceAllString(side, ""), -1) {
		var folded strings.Builder
		for _, r := range norm.NFKD.String(strings.ToLower(strings.TrimSpace(name))) {
			if !unicode.Is(unicode.Mn, r) {
				folded.WriteRune(r)
			}
		}
		key := strings.TrimSpace(folded.String())
		if key != "" && !unknownArtists[key] && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// Orient exchanges artist and title on the tracks of each tracklist in lists,
// keyed by video, that is written title first, "Laura - Monolink", and returns
// the videos whose tracklists it exchanged.
//
// One line cannot say which side names the artist, so a tracklist is judged
// whole, against the artists the others credit. One whose titles name such an
// artist at least MinimumTitleFirst times, and at least twice as often as its
// artists do, is written title first. The tracklists are judged one at a time,
// the clearest first, and each exchange changes the artists the next is judged
// against, because a tracklist read the wrong way round credits its titles as
// artists until it is exchanged. Within one exchanged, a line whose artist
// names a known artist and whose title does not stays as written, since a
// tracklist can mix the two orders.
//
// The answer depends on lists alone, so judging the lists it returns again
// exchanges nothing.
func Orient(lists map[string][]Track) []string {
	videos := make([]string, 0, len(lists))
	for video := range lists {
		videos = append(videos, video)
	}
	slices.Sort(videos)

	// credits is how many tracklists credit each artist, and own the artists
	// each one credits, so a tracklist is never judged against itself.
	credits := map[string]int{}
	own := map[string]map[string]bool{}
	count := func(video string, by int) {
		own[video] = map[string]bool{}
		for _, track := range lists[video] {
			for _, key := range credited(track.Artist) {
				own[video][key] = true
			}
		}
		for key := range own[video] {
			credits[key] += by
		}
	}
	for _, video := range videos {
		count(video, 1)
	}
	known := func(video, side string) bool {
		for _, key := range credited(side) {
			if others := credits[key]; others > 1 || others == 1 && !own[video][key] {
				return true
			}
		}
		return false
	}

	var exchanged []string
	for {
		clearest, margin := "", 0
		for _, video := range videos {
			if slices.Contains(exchanged, video) {
				continue
			}
			titles, artists := 0, 0
			for _, track := range lists[video] {
				if track.Artist == "" {
					continue
				}
				if known(video, track.Title) {
					titles++
				}
				if known(video, track.Artist) {
					artists++
				}
			}
			if titles >= MinimumTitleFirst && titles >= 2*artists && titles-artists > margin {
				clearest, margin = video, titles-artists
			}
		}
		if clearest == "" {
			return exchanged
		}
		tracks := lists[clearest]
		stays := make([]bool, len(tracks))
		for i, track := range tracks {
			stays[i] = track.Artist == "" || known(clearest, track.Artist) && !known(clearest, track.Title)
		}
		for key := range own[clearest] {
			credits[key]--
		}
		for i := range tracks {
			if !stays[i] {
				tracks[i] = exchange(tracks[i])
			}
		}
		count(clearest, 1)
		exchanged = append(exchanged, clearest)
	}
}

// exchange is track read the other way round: its title as the artist, and
// its artist as the title, carrying any annotation that trailed the title.
func exchange(track Track) Track {
	annotation := trailingAnnotation.FindString(track.Title)
	artist := strings.TrimSpace(strings.TrimSuffix(track.Title, annotation))
	if unknownArtists[strings.ToLower(artist)] {
		artist = ""
	}
	track.Artist, track.Title = artist, strings.TrimSpace(track.Artist+annotation)
	return track
}
