// Package tracklist turns what a video says about its music into a tracklist:
// its chapters, or the timestamped lines of its description or of one of its
// top comments. It reads no store and makes no request.
//
// A chapter's start and end arrive as numbers and are used as numbers. Only a
// chapter's title and a line of free text, which a person typed, are parsed.
package tracklist

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Where a track's text came from, as tracks.source holds it.
const (
	SourceChapter     = "chapter"
	SourceDescription = "description"
	SourceComment     = "comment"
)

// MinimumTracks is the fewest timestamped lines a description or a comment
// holds for them to be a tracklist. A comment naming one or two times points at
// moments of the set, "16:57 Guy Gerber - What to do", rather than listing it.
const MinimumTracks = 3

// Chapter is one chapter of a video: where it starts and ends, in seconds, and
// its title.
type Chapter struct {
	StartSeconds int64
	EndSeconds   int64
	Title        string
}

// Track is one track of a video's tracklist: its position from 1, where it
// starts, and where it ends when the source says. A track from a line of text
// ends where the next begins, and the last one has no end, since text cannot
// say where the video ends. Artist is empty when the text carried no artist to
// split from the title, since a split never yields an empty name. RawText is
// the text the track was parsed from, so a wrong split can be read again.
type Track struct {
	Position     int64
	StartSeconds int64
	EndSeconds   int64
	HasEnd       bool
	Artist       string
	Title        string
	RawText      string
	Source       string
}

// artistTitleSeparator is a hyphen, en dash, em dash or tilde with whitespace
// on both sides. The whitespace is what keeps "Jay-Z" whole.
var artistTitleSeparator = regexp.MustCompile(`\s+[-–—~]\s+`)

// leadingTrackNumber is a track number opening a text: "1.", "01)", "3:" or
// "#3".
var leadingTrackNumber = regexp.MustCompile(`^\s*#?\d{1,3}\s*[.):]\s*|^\s*#\d{1,3}\s+`)

// timestampFirst is a line opening with a timestamp, after an optional track
// number, then whitespace or a dash, then text: "0:00 Artist - Title",
// "[4:20] Artist - Title", "01. 00:00 - “Title”". A timestamp is m:ss or
// h:mm:ss, bracketed or not, and its groups are the hours, minutes and seconds.
var timestampFirst = regexp.MustCompile(`^\s*(?:#?\d{1,3}[.)]\s+)?[\[(]?(?:(\d{1,2}):)?(\d{1,3}):(\d{2})[\])]?(?:\s*[-–—|]\s*|\s+)(\S.*)$`)

// timestampLast is a line closing with a timestamp after its text, set off by
// whitespace, a dash or a bracket: "Title (00:05:48)", "Artist - Title 4:20".
// The text is the shortest that leaves a whole timestamp, so "Title 14:20" ends
// at 14 minutes and not at 4.
var timestampLast = regexp.MustCompile(`^\s*(\S.*?)\s*(?:[-–—|]\s*)?[\[(]?(?:(\d{1,2}):)?(\d{1,3}):(\d{2})[\])]?\s*$`)

// unknownArtists is the DJ convention for a track nobody has identified.
// Recorded as an artist, it would make every unidentified track in the library
// the work of one artist called ID.
var unknownArtists = map[string]bool{"id": true, "?": true, "unknown": true, "n/a": true}

// SplitArtistAndTitle splits "Artist - Title" at its first separator, so
// "Bicep - Glue - Extended Mix" keeps its version on the title. A leading
// track number and quotes around the text are dropped first. The artist is
// empty when the text holds no separator with words on both sides, or names an
// artist nobody has identified.
func SplitArtistAndTitle(text string) (artist, title string) {
	cleaned := unquoted(strings.TrimSpace(leadingTrackNumber.ReplaceAllString(text, "")))
	parts := artistTitleSeparator.Split(cleaned, 2)
	if len(parts) != 2 {
		return "", cleaned
	}
	artist, title = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	switch {
	case artist == "" || title == "":
		return "", cleaned
	case unknownArtists[strings.ToLower(artist)]:
		return "", title
	}
	return artist, title
}

// unquoted is text without the quotes wrapping it, when it opens with one: a
// straight, curly or angle quote, closed by any of them or by none.
func unquoted(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if !strings.ContainsRune(`"“”«'‘`, first) {
		return text
	}
	inner := text[size:]
	last, size := utf8.DecodeLastRuneInString(inner)
	if strings.ContainsRune(`"“”»'’`, last) {
		inner = inner[:len(inner)-size]
	}
	return strings.TrimSpace(inner)
}

// FromChapters is a track for each chapter, with the chapter's start and end.
func FromChapters(chapters []Chapter) []Track {
	tracks := make([]Track, len(chapters))
	for i, chapter := range chapters {
		artist, title := SplitArtistAndTitle(chapter.Title)
		tracks[i] = Track{
			Position:     int64(i + 1),
			StartSeconds: chapter.StartSeconds,
			EndSeconds:   chapter.EndSeconds,
			HasEnd:       true,
			Artist:       artist,
			Title:        title,
			RawText:      chapter.Title,
			Source:       SourceChapter,
		}
	}
	return tracks
}

// FromText is a track for each timestamped line of text, from source, each
// ending where the next begins. It is nil unless text holds at least
// MinimumTracks such lines and each starts later than the one before, since a
// text whose times do not run forward is not listing a set in order. A line
// with no text beside its timestamp is not a track.
func FromText(text, source string) []Track {
	var tracks []Track
	for line := range strings.Lines(text) {
		start, rest, ok := timestamped(line)
		if !ok {
			continue
		}
		artist, title := SplitArtistAndTitle(rest)
		if title == "" {
			continue
		}
		tracks = append(tracks, Track{
			Position:     int64(len(tracks) + 1),
			StartSeconds: start,
			Artist:       artist,
			Title:        title,
			RawText:      strings.TrimSpace(line),
			Source:       source,
		})
	}
	if len(tracks) < MinimumTracks {
		return nil
	}
	for i := 1; i < len(tracks); i++ {
		if tracks[i].StartSeconds <= tracks[i-1].StartSeconds {
			return nil
		}
		tracks[i-1].EndSeconds, tracks[i-1].HasEnd = tracks[i].StartSeconds, true
	}
	return tracks
}

// timestamped is where line says a track starts and the text beside its
// timestamp, and false for a line holding neither shape of timestamped line, or
// a timestamp whose minutes or seconds do not fit a clock.
func timestamped(line string) (int64, string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if match := timestampFirst.FindStringSubmatch(line); match != nil {
		start, ok := seconds(match[1], match[2], match[3])
		return start, match[4], ok
	}
	if match := timestampLast.FindStringSubmatch(line); match != nil {
		start, ok := seconds(match[2], match[3], match[4])
		return start, match[1], ok
	}
	return 0, "", false
}

// seconds is the timestamp hours:minutes:seconds in seconds, with hours empty
// for m:ss. Minutes past 59 fit only a timestamp with no hours.
func seconds(hours, minutes, secs string) (int64, bool) {
	h, _ := strconv.ParseInt(hours, 10, 64)
	m, _ := strconv.ParseInt(minutes, 10, 64)
	s, _ := strconv.ParseInt(secs, 10, 64)
	if s > 59 || hours != "" && m > 59 {
		return 0, false
	}
	return h*3600 + m*60 + s, true
}

// Best is the tracklist a video's chapters make, or failing them the one its
// description makes, or failing that the one the first of its top comments to
// hold a tracklist makes, and nil when none does.
func Best(chapters []Chapter, description string, comments []string) []Track {
	if len(chapters) > 0 {
		return FromChapters(chapters)
	}
	if tracks := FromText(description, SourceDescription); tracks != nil {
		return tracks
	}
	for _, comment := range comments {
		if tracks := FromText(comment, SourceComment); tracks != nil {
			return tracks
		}
	}
	return nil
}
