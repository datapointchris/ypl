package tracklist

import (
	"slices"
	"strings"
	"testing"
)

// listed is a tracklist written as "artist|title" a line, which is how each
// case below states both what it holds and what Orient should make of it.
func listed(lines ...string) []Track {
	tracks := make([]Track, len(lines))
	for i, line := range lines {
		artist, title, _ := strings.Cut(line, "|")
		tracks[i] = Track{Position: int64(i + 1), Artist: artist, Title: title}
	}
	return tracks
}

// oriented is a tracklist back in the shape listed takes.
func oriented(tracks []Track) []string {
	lines := make([]string, len(tracks))
	for i, track := range tracks {
		lines[i] = track.Artist + "|" + track.Title
	}
	return lines
}

func requireOriented(t *testing.T, lists map[string][]Track, video string, want ...string) {
	t.Helper()
	if got := oriented(lists[video]); !slices.Equal(got, want) {
		t.Errorf("%s reads\n  %v\nwant\n  %v", video, got, want)
	}
}

func TestAMixNoOtherTracklistCorroboratesIsReadFromItsOwnAnnotations(t *testing.T) {
	lists := map[string][]Track{
		"fireplace": listed(
			"Deity|Mark Almost",
			"Love Lost|Linkwood",
			"Ninsei (Original Mix)|Andreiclv",
			"Alignment (Brightly Aligned Remix)|Alveol",
		),
	}
	if exchanged := Orient(lists); !slices.Equal(exchanged, []string{"fireplace"}) {
		t.Fatalf("Orient exchanged %v, want [fireplace]", exchanged)
	}
	requireOriented(t, lists, "fireplace",
		"Mark Almost|Deity",
		"Linkwood|Love Lost",
		"Andreiclv|Ninsei (Original Mix)",
		"Alveol|Alignment (Brightly Aligned Remix)",
	)
}

func TestATracklistWhoseAnnotationsSitOnItsTitlesIsLeftAsWritten(t *testing.T) {
	lists := map[string][]Track{
		"groovy": listed(
			"Nathalie Duchene|Praia (Yuksek Remix)",
			"Prospa|Guitar Anthem",
			"Oliver Dollar|School Daze (Extended Mix)",
		),
	}
	if exchanged := Orient(lists); len(exchanged) != 0 {
		t.Fatalf("Orient exchanged %v, want none", exchanged)
	}
	requireOriented(t, lists, "groovy",
		"Nathalie Duchene|Praia (Yuksek Remix)",
		"Prospa|Guitar Anthem",
		"Oliver Dollar|School Daze (Extended Mix)",
	)
}

func TestOneAnnotatedArtistDoesNotTurnATracklistRound(t *testing.T) {
	lists := map[string][]Track{
		"single": listed(
			"Ninsei (Original Mix)|Andreiclv",
			"Deity|Mark Almost",
			"Love Lost|Linkwood",
		),
	}
	if exchanged := Orient(lists); len(exchanged) != 0 {
		t.Fatalf("Orient exchanged %v, want none", exchanged)
	}
}

func TestALabelOrAYearEndingAnArtistIsNotAMixAnnotation(t *testing.T) {
	lists := map[string][]Track{
		"labeled": listed(
			"Dosem (Anjunadeep)|All Locations",
			"Quivver (2019)|Out of Reach",
			"Lane 8 (Anjunadeep)|Brightest Lights",
		),
	}
	if exchanged := Orient(lists); len(exchanged) != 0 {
		t.Fatalf("Orient exchanged %v, want none", exchanged)
	}
}

func TestAnAnnotatedArtistIsTurnedRoundThoughItsNameIsCreditedElsewhere(t *testing.T) {
	lists := map[string][]Track{
		"funky": listed(
			"Discobionic (Main Mix)|Walterino",
			"Believe (Album Mix)|Jamie Lewis",
			"Spread Your Love (Original Extended Mix)|Sea N Soul",
		),
		"reference": listed(
			"Believe|Hollow Coves",
			"Dosem|All Locations",
			"Quivver|Out of Reach",
		),
	}
	if exchanged := Orient(lists); !slices.Equal(exchanged, []string{"funky"}) {
		t.Fatalf("Orient exchanged %v, want [funky]", exchanged)
	}
	requireOriented(t, lists, "funky",
		"Walterino|Discobionic (Main Mix)",
		"Jamie Lewis|Believe (Album Mix)",
		"Sea N Soul|Spread Your Love (Original Extended Mix)",
	)
}

func TestAnAnnotationEndingATitleFirstLineDoesNotHoldItAsWritten(t *testing.T) {
	lists := map[string][]Track{
		"trailing": listed(
			"Deity|Mark Almost",
			"Ninsei (Original Mix)|Andreiclv",
			"Alignment (Brightly Aligned Remix)|Alveol",
			"Flicker|Ben Böhmer (Extended Mix)",
		),
	}
	if exchanged := Orient(lists); !slices.Equal(exchanged, []string{"trailing"}) {
		t.Fatalf("Orient exchanged %v, want [trailing]", exchanged)
	}
	requireOriented(t, lists, "trailing",
		"Mark Almost|Deity",
		"Andreiclv|Ninsei (Original Mix)",
		"Alveol|Alignment (Brightly Aligned Remix)",
		"Ben Böhmer|Flicker (Extended Mix)",
	)
}

func TestATracklistMixingBothOrdersKeepsTheLinesAlreadyWrittenArtistFirst(t *testing.T) {
	lists := map[string][]Track{
		"mixed": listed(
			"Sébastien Léger|Gaufrette",
			"Metanoia|Nicolas Benedetti",
			"Mauna Loa (Extended Mix)|Benja Molina",
			"Arctic Skies (Agustin Pietrocola Remix)|TAYLAN",
			"Lane 8|The Little Mushroom (Extended Mix)",
		),
		"reference": listed(
			"Sébastien Léger|Koi Fish",
			"Lane 8|Brightest Lights",
			"Dosem|All Locations",
			"Quivver|Out of Reach",
		),
	}
	if exchanged := Orient(lists); !slices.Equal(exchanged, []string{"mixed"}) {
		t.Fatalf("Orient exchanged %v, want [mixed]", exchanged)
	}
	requireOriented(t, lists, "mixed",
		"Sébastien Léger|Gaufrette",
		"Nicolas Benedetti|Metanoia",
		"Benja Molina|Mauna Loa (Extended Mix)",
		"TAYLAN|Arctic Skies (Agustin Pietrocola Remix)",
		"Lane 8|The Little Mushroom (Extended Mix)",
	)
	requireOriented(t, lists, "reference",
		"Sébastien Léger|Koi Fish",
		"Lane 8|Brightest Lights",
		"Dosem|All Locations",
		"Quivver|Out of Reach",
	)
}

func TestATracklistWithNoAnnotationsIsJudgedAgainstWhatTheOthersCredit(t *testing.T) {
	lists := map[string][]Track{
		"backwards": listed(
			"Odessa|Caribou",
			"Kerala|Bonobo",
			"Baby|Four Tet",
			"Gunk|Overmono",
		),
		"reference": listed(
			"Caribou|Sun",
			"Bonobo|Cirrus",
			"Four Tet|Angel Echoes",
			"Overmono|Le Tigre",
		),
	}
	if exchanged := Orient(lists); !slices.Equal(exchanged, []string{"backwards"}) {
		t.Fatalf("Orient exchanged %v, want [backwards]", exchanged)
	}
	requireOriented(t, lists, "backwards",
		"Caribou|Odessa",
		"Bonobo|Kerala",
		"Four Tet|Baby",
		"Overmono|Gunk",
	)
}

func TestJudgingTheTracklistsOrientReturnsExchangesNothing(t *testing.T) {
	lists := map[string][]Track{
		"fireplace": listed(
			"Deity|Mark Almost",
			"Love Lost|Linkwood",
			"Ninsei (Original Mix)|Andreiclv",
			"Alignment (Brightly Aligned Remix)|Alveol",
		),
		"backwards": listed(
			"Odessa|Caribou",
			"Kerala|Bonobo",
			"Baby|Four Tet",
			"Gunk|Overmono",
		),
		"reference": listed(
			"Caribou|Sun",
			"Bonobo|Cirrus",
			"Four Tet|Angel Echoes",
			"Overmono|Le Tigre",
		),
	}
	first := Orient(lists)
	slices.Sort(first)
	if !slices.Equal(first, []string{"backwards", "fireplace"}) {
		t.Fatalf("Orient exchanged %v, want [backwards fireplace]", first)
	}
	if again := Orient(lists); len(again) != 0 {
		t.Errorf("judging the answer again exchanged %v, want none", again)
	}
}
