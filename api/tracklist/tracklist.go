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

// Source is where a track's text came from, as tracks.source holds it.
type Source string

// Where a track's text came from, as tracks.source holds it.
const (
	SourceChapter     Source = "chapter"
	SourceDescription Source = "description"
	SourceComment     Source = "comment"
)

// MinimumTracks is the fewest timestamped lines a description or a comment
// holds for them to be a tracklist, and the fewest chapters a video has for
// them to be one. A comment naming one or two times points at moments of the
// set, "16:57 Guy Gerber - What to do", rather than listing it.
const MinimumTracks = 3

// MinimumCoverage is how much of a video's length a tracklist's own tracks
// reach: a set is listed through to near its end. A comment naming a few
// moments of a set carries its timestamps in the same shapes and stops wherever
// the person writing it stopped.
const MinimumCoverage = 0.5

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
	Source       Source
}

// artistTitleSeparator is a hyphen, en dash, em dash or tilde with whitespace
// on both sides. The whitespace is what keeps "Jay-Z" whole.
var artistTitleSeparator = regexp.MustCompile(`\s+[-–—~]\s+`)

// leadingMarker is the list markers opening a text, as a commenter who writes
// a tracklist as a list puts one after each timestamp: "• Baby Run - Jimi
// Jules", "▶️ Baby Run", "🎵 Baby Run". A bullet may touch the name. Any other
// symbol or emoji is a marker only with whitespace after it, which keeps
// "*NSYNC" whole, and an emoji's presentation selector goes with it.
var leadingMarker = regexp.MustCompile(`^\s*(?:[•·●▪◦►▶]\x{FE0F}?\s*|[\p{So}\p{Sm}]\x{FE0F}?\s+)+`)

// leadingTrackNumber is a track number opening a text: "1.", "01)", "1..",
// "1)." or "#3". A
// colon does not end one, since a number before a colon is part of a title far
// more often than it numbers a track: "1:1 Sessions".
var leadingTrackNumber = regexp.MustCompile(`^\s*#?\d{1,3}\s*[.)]+\s*|^\s*#\d{1,3}\s+`)

// timestampFirst is a line opening with a timestamp, after an optional track
// number, then whitespace or a dash, then text: "0:00 Artist - Title",
// "[4:20] Artist - Title", "01. 00:00 - “Title”". A timestamp is m:ss or
// h:mm:ss, bracketed or not, and its groups are the hours, minutes and seconds.
// A line naming where its track ends as well as where it starts, "00:00 - 04:35
// Artist - Title", keeps the start and drops the end, which the next track's
// start already says.
var timestampFirst = regexp.MustCompile(`^\s*(?:#?\d{1,3}[.)]\s+)?[\[(]?(?:(\d{1,2}):)?(\d{1,3}):(\d{2})(?:\s*[-–—|]\s*(?:\d{1,2}:)?\d{1,3}:\d{2})?[\])]?(?:\s*[-–—|]\s*|\s+)(\S.*)$`)

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
// "Bicep - Glue - Extended Mix" keeps its version on the title. A leading list
// marker, a leading track number and quotes around the text are dropped first.
// The artist is empty when the text holds no separator with words on both
// sides, or names an artist nobody has identified.
func SplitArtistAndTitle(text string) (artist, title string) {
	cleaned := unquoted(strings.TrimSpace(leadingTrackNumber.ReplaceAllString(leadingMarker.ReplaceAllString(text, ""), "")))
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

// unquoted is text without the quotes wrapping it, when a straight, curly or
// angle quote opens it and any of them closes it. Text that only opens with one
// is unchanged, since an apostrophe opening a name quotes nothing: "'Til Dawn".
func unquoted(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if !strings.ContainsRune(`"“”«'‘`, first) {
		return text
	}
	inner := text[size:]
	last, lastSize := utf8.DecodeLastRuneInString(inner)
	if !strings.ContainsRune(`"“”»'’`, last) {
		return text
	}
	return strings.TrimSpace(inner[:len(inner)-lastSize])
}

// Rederive is the artist and title this parser makes of a stored track's text,
// read from source, and false when the parser makes no track of it. A
// chapter's text is its title; a line of text carries its timestamp, which is
// dropped.
func Rederive(raw string, source Source) (artist, title string, ok bool) {
	text := raw
	if source != SourceChapter {
		_, rest, found := timestamped(raw)
		if !found {
			return "", "", false
		}
		text = rest
	}
	artist, title = SplitArtistAndTitle(text)
	return artist, title, title != ""
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
// ending where the next one to start later begins. It is nil unless text holds
// at least MinimumTracks such lines and none starts earlier than the one before
// it, since a text whose times run backwards is not listing a set in order. Two
// tracks may start at one time, which is how a mashup or a segue is listed, and
// neither of them ends before the other. A line with no text beside its
// timestamp is not a track.
func FromText(text string, source Source) []Track {
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
		if tracks[i].StartSeconds < tracks[i-1].StartSeconds {
			return nil
		}
	}
	for i := range tracks {
		for _, later := range tracks[i+1:] {
			if later.StartSeconds > tracks[i].StartSeconds {
				tracks[i].EndSeconds, tracks[i].HasEnd = later.StartSeconds, true
				break
			}
		}
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

// untitledChapter is the title yt-dlp gives a chapter that has none, including
// the one it inserts at the start of a video whose first chapter begins later.
var untitledChapter = regexp.MustCompile(`^<Untitled Chapter \d+>$`)

// titled is chapters less those yt-dlp titled itself, which name no track.
func titled(chapters []Chapter) []Chapter {
	kept := make([]Chapter, 0, len(chapters))
	for _, chapter := range chapters {
		if !untitledChapter.MatchString(strings.TrimSpace(chapter.Title)) {
			kept = append(kept, chapter)
		}
	}
	return kept
}

// spans is whether tracks reach far enough through a video durationSeconds long
// to be its tracklist rather than a few of its moments, by MinimumCoverage. A
// video of unknown length is judged on its tracks alone.
func spans(tracks []Track, durationSeconds int64) bool {
	switch {
	case len(tracks) == 0:
		return false
	case durationSeconds <= 0:
		return true
	}
	return float64(tracks[len(tracks)-1].StartSeconds) >= MinimumCoverage*float64(durationSeconds)
}

// Best is the tracklist a video durationSeconds long makes from its chapters,
// or failing them from its description, or failing that from the first of its
// top comments to hold one, and nil when none does.
//
// yt-dlp reports chapters it read from the video and chapters it derived from
// the description alike, and says which it did for neither. So chapters are
// held to the same fewest tracks as a text, and one yt-dlp titled itself is
// dropped.
func Best(chapters []Chapter, durationSeconds int64, description string, comments []string) []Track {
	if named := titled(chapters); len(named) >= MinimumTracks {
		return FromChapters(named)
	}
	if tracks := FromText(description, SourceDescription); spans(tracks, durationSeconds) {
		return tracks
	}
	for _, comment := range comments {
		if tracks := FromText(comment, SourceComment); spans(tracks, durationSeconds) {
			return tracks
		}
	}
	return nil
}
