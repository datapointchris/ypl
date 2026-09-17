package tracklist

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// show is each track as "position start-end artist|title" with "-" for no end,
// so a table compares whole tracks.
func show(tracks []Track) []string {
	var shown []string
	for _, track := range tracks {
		end := "-"
		if track.HasEnd {
			end = fmt.Sprint(track.EndSeconds)
		}
		shown = append(shown, fmt.Sprintf("%d %d-%s %s|%s", track.Position, track.StartSeconds, end, track.Artist, track.Title))
	}
	return shown
}

func TestSplitArtistAndTitle(t *testing.T) {
	cases := []struct {
		text, artist, title string
	}{
		{"Totally Enormous Extinct Dinosaurs - Islas Canarias", "Totally Enormous Extinct Dinosaurs", "Islas Canarias"},
		{"Disclosure ft. Fatoumata Diawara - Douha (Mali Mali)", "Disclosure ft. Fatoumata Diawara", "Douha (Mali Mali)"},
		{"Bonobo – Kerala", "Bonobo", "Kerala"},
		{"Four Tet — Baby", "Four Tet", "Baby"},
		{"Overmono ~ Gunk", "Overmono", "Gunk"},
		{"Jay-Z", "", "Jay-Z"},
		{"Jay-Z - Dirt Off Your Shoulder", "Jay-Z", "Dirt Off Your Shoulder"},
		{"Bicep - Glue - Extended Mix", "Bicep", "Glue - Extended Mix"},
		{"1. Caribou - Odessa", "Caribou", "Odessa"},
		{"01) Caribou - Odessa", "Caribou", "Odessa"},
		{"#3 Caribou - Odessa", "Caribou", "Odessa"},
		{"Intro", "", "Intro"},
		{"ID - ID", "", "ID"},
		{"? - Something", "", "Something"},
		{"Unknown - Track", "", "Track"},
		{"Bonobo - ", "", "Bonobo -"},
		{"“Christian Löffler - A Life”", "Christian Löffler", "A Life"},
		{`"Shadows”`, "", "Shadows"},
		{"“Valentin’s Blood Flows”", "", "Valentin’s Blood Flows"},
		{"13. Revival Agents, Korolova - Iris", "Revival Agents, Korolova", "Iris"},
	}
	for _, c := range cases {
		if artist, title := SplitArtistAndTitle(c.text); artist != c.artist || title != c.title {
			t.Errorf("SplitArtistAndTitle(%q) = %q, %q; want %q, %q", c.text, artist, title, c.artist, c.title)
		}
	}
}

func TestChaptersCarryTheirOwnStartsAndEnds(t *testing.T) {
	chapters := []Chapter{
		{StartSeconds: 0, EndSeconds: 251, Title: "San Rossore"},
		{StartSeconds: 251, EndSeconds: 454, Title: "Tennyson & Mr. Carmack - Tuesday"},
		{StartSeconds: 454, EndSeconds: 754, Title: "weird -- formatting"},
	}
	tracks := FromChapters(chapters)
	want := []string{"1 0-251 |San Rossore", "2 251-454 Tennyson & Mr. Carmack|Tuesday", "3 454-754 |weird -- formatting"}
	if !slices.Equal(show(tracks), want) || tracks[2].RawText != "weird -- formatting" || tracks[0].Source != SourceChapter {
		t.Fatalf("FromChapters = %v, raw %q, source %q; want %v from chapters with the raw title kept", show(tracks), tracks[2].RawText, tracks[0].Source, want)
	}
}

// Each case is a real shape from a mix in the library or a top comment on one,
// with the tracks it makes.
func TestTimestampedLinesInEveryShapeTheLibraryHolds(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{
			name: "a timestamp first",
			text: "0:00 Bonobo - Kerala\n12:34 Four Tet - Baby\n1:02:03 Caribou - Odessa",
			want: []string{"1 0-754 Bonobo|Kerala", "2 754-3723 Four Tet|Baby", "3 3723-- Caribou|Odessa"},
		},
		{
			name: "bracketed timestamps with the label after the title",
			text: "Complete Tracklist: 1001.tl/256cvzr1\n\n[00:00] Arina Mur - Moon [AKBAL]\n[04:00] Dominik Eulberg - Goldene Acht (Hunter/Game Remix) [!K7]\n[07:00] Tim Engelhardt - Light The Fire [POKER FLAT]",
			want: []string{"1 0-240 Arina Mur|Moon [AKBAL]", "2 240-420 Dominik Eulberg|Goldene Acht (Hunter/Game Remix) [!K7]", "3 420-- Tim Engelhardt|Light The Fire [POKER FLAT]"},
		},
		{
			name: "a parenthesized timestamp and a dash before the text",
			text: "(4:20) - Bonobo - Kerala\n(5:00) - Four Tet - Baby\n(6:00) - Caribou - Odessa",
			want: []string{"1 260-300 Bonobo|Kerala", "2 300-360 Four Tet|Baby", "3 360-- Caribou|Odessa"},
		},
		{
			name: "a track number before the timestamp and a quoted title",
			text: "01. 00:00 - “Christian Löffler - A Life”\n02. 04:56 - “Nikola Melnikov - Sphinx”\n03. 13:30 - “Boxer - Blue Planet”",
			want: []string{"1 0-296 Christian Löffler|A Life", "2 296-810 Nikola Melnikov|Sphinx", "3 810-- Boxer|Blue Planet"},
		},
		{
			name: "a track number padded with spaces and mixed quotes",
			text: "1.   00:00 - “Supersede” \n2. 1:48 - \"Shadows”\n3. 6:32 - “Rosewood”",
			want: []string{"1 0-108 |Supersede", "2 108-392 |Shadows", "3 392-- |Rosewood"},
		},
		{
			name: "a track number after the timestamp",
			text: "Track list \n00:02 1. Alegant & Tube & Berger - Get Down \n04:30 2. Frost - Overtones (PROFF Remix) \n01:03:55 13. Revival Agents, Korolova - Iris",
			want: []string{"1 2-270 Alegant & Tube & Berger|Get Down", "2 270-3835 Frost|Overtones (PROFF Remix)", "3 3835-- Revival Agents, Korolova|Iris"},
		},
		{
			name: "a timestamp last",
			text: "The One  (00:00:27)\nSticky Fingers (Lane 8 Remix)  (00:11:35)\nAba (00:16:45)",
			want: []string{"1 27-695 |The One", "2 695-1005 |Sticky Fingers (Lane 8 Remix)", "3 1005-- |Aba"},
		},
		{
			name: "minutes past an hour with no hours",
			text: "58:00 Beije - Waiting\n75:30 Fejká - Moonlight\n120:00 DBRA - A Dolphin's Tale",
			want: []string{"1 3480-4530 Beije|Waiting", "2 4530-7200 Fejká|Moonlight", "3 7200-- DBRA|A Dolphin's Tale"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := show(FromText(c.text, SourceComment)); !slices.Equal(got, c.want) {
				t.Fatalf("FromText = %q, want %q", got, c.want)
			}
		})
	}
}

// Each case is text that names times without listing a set: prose, links,
// verses and comments pointing at a moment, all from the library's videos and
// the comments on them.
func TestTextThatIsNotATracklistMakesNoTracks(t *testing.T) {
	cases := map[string]string{
		"fewer lines than a tracklist": "16:57 Guy Gerber - What to do\n20:00 Another - One",
		"a timestamp inside a sentence": "Ty! Was glad to hear this track especially 15:56 4. Motorcycle - As The Rush Comes\n" +
			"We cruise past a minbrot at a depth of 2e68 (07:30).\nand dense areas as a result (like 21:15).",
		"a verse and links":              "Holy Bible: New Revised Standard Version, Luke 15:11-32.\nhttps://example.com:8080/a\nhttps://bit.ly/33WfpXA",
		"a tracklist with no timestamps": "Thanks guys.\n\nReflekt ft. Delline Bass - Need To Feel Loved (Richie Blacker edit)\nChicola - La Niña del Mar\nRichie blacker - Tomb Raver",
		"times that run backwards":       "0:00 A - One\n5:00 B - Two\n2:00 C - Three",
		"a timestamp with no text":       "0:00\n1:00 -\n[2:00]\n3:00 “”",
		"seconds past a minute":          "0:75 A - One\n1:80 B - Two\n2:99 C - Three",
		"minutes past an hour's worth":   "1:75:00 A - One\n2:61:00 B - Two\n3:99:00 C - Three",
	}
	for name, text := range cases {
		if tracks := FromText(text, SourceDescription); tracks != nil {
			t.Errorf("%s: FromText = %q, want no tracks", name, show(tracks))
		}
	}
}

func TestTheBestTracklistPrefersChaptersThenTheDescriptionThenTheFirstCommentHoldingOne(t *testing.T) {
	chapters := []Chapter{{StartSeconds: 0, EndSeconds: 60, Title: "From - Chapters"}}
	listing := func(from string) string {
		return strings.Join([]string{"0:00 " + from + " - One", "1:00 " + from + " - Two", "2:00 " + from + " - Three"}, "\n")
	}
	comments := []string{"Gorgeous set", "16:57 Guy Gerber - What to do", listing("First"), listing("Second")}
	cases := []struct {
		name        string
		chapters    []Chapter
		description string
		comments    []string
		artist      string
		source      string
	}{
		{name: "chapters", chapters: chapters, description: listing("Description"), comments: comments, artist: "From", source: SourceChapter},
		{name: "the description", description: listing("Description"), comments: comments, artist: "Description", source: SourceDescription},
		{name: "the first comment holding a tracklist", description: "Follow us", comments: comments, artist: "First", source: SourceComment},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tracks := Best(c.chapters, c.description, c.comments)
			if len(tracks) == 0 || tracks[0].Artist != c.artist || tracks[0].Source != c.source {
				t.Fatalf("Best = %q from %v, want %s's from %s", show(tracks), tracks, c.artist, c.source)
			}
		})
	}
	if tracks := Best(nil, "Follow us", []string{"Gorgeous set"}); tracks != nil {
		t.Fatalf("Best of nothing parseable = %q, want no tracks", show(tracks))
	}
}
