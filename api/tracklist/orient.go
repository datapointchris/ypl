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

// MinimumAnnotatedArtists is the fewest of a tracklist's artists that must
// carry a mix annotation before it is read as written title first on that
// evidence alone, with no other tracklist consulted. Over a library of 501
// tracklists, 281 carried an annotation on their titles and none on their
// artists, 15 the other way round, 5 on both and 200 on neither. Two, against
// twice as many annotated titles, exchanges 13 of those 20 and reads every one
// of the 13 right.
const MinimumAnnotatedArtists = 2

// mixAnnotation is a bracketed note ending a side and naming a mix: "(Original
// Mix)", "(El Búho Remix)", "[Extended Version]". A person writes one about a
// recording, never about a performer, so an artist carrying one is really a
// title. It is narrower than trailingAnnotation, which is any bracketed note
// at all and includes the labels and dates that do end an artist.
var mixAnnotation = regexp.MustCompile(`(?i)[(\[][^()\[\]]*\b(?:remix|mix|edit|version|rework|bootleg|vip|dub|instrumental|reprise)\b[^()\[\]]*[)\]]\s*$`)

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
// whole, on two kinds of evidence.
//
// A mix annotation is the first, and it is read from the one tracklist. A
// person writes "(Original Mix)" about a recording, never about a performer,
// so an artist carrying one is really a title. A list carrying an annotation
// on at least MinimumAnnotatedArtists of its artists, and on at least twice as
// many artists as titles, is written title first. This reaches a mix whose
// artists the rest of the library never names, which corroboration cannot.
//
// The artists the other tracklists credit are the second. A list whose titles
// name such an artist at least MinimumTitleFirst times, and at least twice as
// often as its artists do, is written title first.
//
// Each kind is judged one list at a time, the clearest first, and every
// exchange changes what the next is judged against, because a tracklist read
// the wrong way round credits its titles as artists until it is exchanged.
// Annotations are read first for that reason: they need no corroboration and
// they leave correct credits behind for the pass that does.
//
// Within a list being exchanged, a line whose artist names a known artist and
// whose title does not stays as written, since a tracklist can mix the two
// orders. An annotated artist is turned round whatever is credited, because
// the annotation says that side is a title and a credit only suggests it is
// not.
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

	// apply reads video's tracklist the other way round, leaving as written
	// any line whose artist names a known artist and whose title does not,
	// since a tracklist can mix the two orders. An artist carrying a mix
	// annotation is turned round whatever is credited, because stripping the
	// annotation off "Believe (Album Mix)" leaves a title that collides with
	// somebody's name. It then credits what the list now names, so the lists
	// judged after it are judged against the right side.
	apply := func(video string) {
		tracks := lists[video]
		stays := make([]bool, len(tracks))
		for i, track := range tracks {
			stays[i] = track.Artist == "" ||
				!mixAnnotation.MatchString(track.Artist) &&
					known(video, track.Artist) && !known(video, track.Title)
		}
		for key := range own[video] {
			credits[key]--
		}
		for i := range tracks {
			if !stays[i] {
				tracks[i] = exchange(tracks[i])
			}
		}
		count(video, 1)
		exchanged = append(exchanged, video)
	}

	// The annotated pass first, because it consults no other tracklist and so
	// can read a mix whose artists the rest of the library never names. Every
	// list it exchanges credits its artists correctly from then on, which is
	// evidence the corroborated pass would otherwise not have.
	for {
		clearest, margin := "", 0
		for _, video := range videos {
			if slices.Contains(exchanged, video) {
				continue
			}
			titles, artists := annotated(lists[video])
			if artists >= MinimumAnnotatedArtists && artists >= 2*titles && artists-titles > margin {
				clearest, margin = video, artists-titles
			}
		}
		if clearest == "" {
			break
		}
		apply(clearest)
	}

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
		apply(clearest)
	}
}

// annotated is how many of tracks carry a mix annotation on the title and how
// many carry one on the artist. A line carrying one on both sides says nothing
// about which way round it is written and counts for neither.
//
// The two counts are not the same evidence. An annotation on the artist can
// only be a title's, so it says the line is written title first. One on the
// title may be that line's second name annotated, or it may be the annotation
// that ended the whole line, which lands on the second side whichever way
// round the line is: "Flicker - Ben Böhmer (Extended Mix)" is written title
// first and carries its annotation on the title. So the artists are counted as
// evidence and the titles only as a brake on it.
func annotated(tracks []Track) (titles, artists int) {
	for _, track := range tracks {
		if track.Artist == "" {
			continue
		}
		onTitle, onArtist := mixAnnotation.MatchString(track.Title), mixAnnotation.MatchString(track.Artist)
		switch {
		case onTitle && !onArtist:
			titles++
		case onArtist && !onTitle:
			artists++
		}
	}
	return titles, artists
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
