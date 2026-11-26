package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/datapointchris/ypl/api/tracklist"
)

// ErrNoCaptions is a video with no caption track in the language a read asked
// for.
var ErrNoCaptions = errors.New("the video has no caption track in that language")

// captionLimit bounds one read of a video's captions: the read of what tracks
// it has, then the read of the one picked.
const captionLimit = 3 * time.Minute

// TrackKind says who wrote a caption track.
type TrackKind string

const (
	// Manual is a track written or uploaded by a person.
	Manual TrackKind = "manual"
	// Automatic is YouTube's speech recognition, or its translation of it.
	Automatic TrackKind = "automatic"
)

// Track is one caption track: its language as YouTube files it, such as en,
// en-US or de-orig, and who wrote it.
type Track struct {
	Language string
	Kind     TrackKind
}

// Segment is one caption event: when it starts and how long it shows, in
// milliseconds, and its text on one line.
type Segment struct {
	StartMs    int64
	DurationMs int64
	Text       string
}

// Captions is a read of a video's captions. Track is the one the read picked,
// and nil where the video has none, which leaves Segments empty. Language is
// the video's own as YouTube reports it, and "" where it reports none.
// DurationSeconds is nil for a video with no length, such as a live stream.
type Captions struct {
	VideoID         string
	Title           string
	Description     string
	DurationSeconds *int64
	Language        string
	Track           *Track
	Segments        []Segment
	Chapters        []tracklist.Chapter
}

// Transcript is a video's captions in one language as one line of text, the
// segments joined by single spaces.
type Transcript struct {
	VideoID string
	Track   Track
	Text    string
}

// Captions reads the video id's captions for class: its details, its chapters,
// and the segments of the one track it picks. That is the manual track in the
// video's language, else the manual English track, else the automatic track in
// the video's original language, else the automatic English one.
func (d *Door) Captions(ctx context.Context, class Class, id string) (Captions, error) {
	if !videoID.MatchString(id) {
		return Captions{}, fmt.Errorf("read the captions of %q: not a YouTube video id", id)
	}
	read, err := through(ctx, d, class, captionLimit, func(ctx context.Context) (captionsWithText, error) {
		return d.binary.captions(ctx, id, func(read captionInfo) (Track, bool) {
			return pickTrack(read.language(), read.Subtitles.json3(), read.AutomaticCaptions.json3())
		})
	})
	if err != nil {
		return Captions{}, fmt.Errorf("read the captions of %s: %w", id, err)
	}
	return read.Captions, nil
}

// Transcript reads the video id's captions in lang for class as one line: its
// manual track in lang, else its automatic one, each named exactly. The error
// wraps ErrNoCaptions where the video has neither.
func (d *Door) Transcript(ctx context.Context, class Class, id, lang string) (Transcript, error) {
	if !videoID.MatchString(id) {
		return Transcript{}, fmt.Errorf("read the transcript of %q: not a YouTube video id", id)
	}
	if !language.MatchString(lang) {
		return Transcript{}, fmt.Errorf("read the transcript of %s: %q is not a language, such as en or pt-BR", id, lang)
	}
	read, err := through(ctx, d, class, captionLimit, func(ctx context.Context) (captionsWithText, error) {
		return d.binary.captions(ctx, id, func(read captionInfo) (Track, bool) {
			switch {
			case slices.Contains(read.Subtitles.json3(), lang):
				return Track{Language: lang, Kind: Manual}, true
			case slices.Contains(read.AutomaticCaptions.json3(), lang):
				return Track{Language: lang, Kind: Automatic}, true
			}
			return Track{}, false
		})
	})
	if err == nil && read.Track == nil {
		err = fmt.Errorf("%w: %s", ErrNoCaptions, lang)
	}
	if err != nil {
		return Transcript{}, fmt.Errorf("read the transcript of %s: %w", id, err)
	}
	return Transcript{VideoID: id, Track: *read.Track, Text: read.text}, nil
}

// language is the shape of a caption track's language.
var language = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)

// captionsWithText is a read of captions with its track's text as one line.
type captionsWithText struct {
	Captions
	text string
}

// captions reads the video id's details and caption tracks, then the segments
// of the track pick chooses, and keeps the read's info for the second read
// rather than reading the watch page again. The info and the track are written
// to a directory removed when the read ends.
func (b Binary) captions(ctx context.Context, id string, pick func(captionInfo) (Track, bool)) (captionsWithText, error) {
	output, err := b.run(ctx, "", infoArguments(id), refusal)
	if err != nil {
		return captionsWithText{}, err
	}
	var read captionInfo
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&read); err != nil {
		return captionsWithText{}, fmt.Errorf("decode yt-dlp's answer: %w", err)
	}
	if read.ID != id {
		return captionsWithText{}, fmt.Errorf("yt-dlp answered with video %q", read.ID)
	}
	got := captionsWithText{Captions: Captions{
		VideoID:     id,
		Title:       read.Title,
		Description: read.Description,
		Language:    read.language(),
		Chapters:    read.Chapters.read(),
		Segments:    []Segment{},
	}}
	if read.Duration != nil && *read.Duration > 0 {
		seconds := int64(*read.Duration)
		got.DurationSeconds = &seconds
	}
	track, ok := pick(read)
	if !ok {
		return got, nil
	}
	dir, err := os.MkdirTemp("", "ypl-captions-")
	if err != nil {
		return captionsWithText{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	infoPath := filepath.Join(dir, "info.json")
	if err := os.WriteFile(infoPath, output, 0o600); err != nil {
		return captionsWithText{}, err
	}
	if _, err := b.run(ctx, dir, trackArguments(infoPath, dir, track), refusal); err != nil {
		return captionsWithText{}, err
	}
	written, err := filepath.Glob(filepath.Join(dir, "*."+track.Language+".json3"))
	if err != nil || len(written) != 1 {
		return captionsWithText{}, fmt.Errorf("yt-dlp wrote %d %s caption files, want 1", len(written), track.Language)
	}
	body, err := os.ReadFile(written[0])
	if err != nil {
		return captionsWithText{}, err
	}
	events, err := decodeJSON3(body)
	if err != nil {
		return captionsWithText{}, err
	}
	got.Track = &track
	got.Segments = segments(events)
	got.text = transcriptLine(events)
	return got, nil
}

// infoArguments is what the read of a video's details and caption tracks
// passes yt-dlp.
func infoArguments(id string) []string {
	return append(paced(), "--skip-download", "--no-playlist", "--dump-json", "--", watchURL(id))
}

// trackArguments is what the read of one caption track passes yt-dlp: the info
// written by the first read, the track by its language and kind, and json3, which
// carries each event once rather than the rolling repeats of an automatic
// track's VTT.
func trackArguments(info, dir string, track Track) []string {
	write := "--write-subs"
	if track.Kind == Automatic {
		write = "--write-auto-subs"
	}
	return append(paced(),
		"--load-info-json", info,
		"--skip-download",
		write,
		"--sub-langs", regexp.QuoteMeta(track.Language),
		"--sub-format", "json3",
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"),
	)
}

// captionInfo is the part of yt-dlp's JSON for a video that a read of its
// captions keeps.
type captionInfo struct {
	ID                string       `json:"id"`
	Title             string       `json:"title"`
	Description       string       `json:"description"`
	Duration          *float64     `json:"duration"`
	Language          *string      `json:"language"`
	Chapters          chapters     `json:"chapters"`
	Subtitles         trackFormats `json:"subtitles"`
	AutomaticCaptions trackFormats `json:"automatic_captions"`
}

func (c captionInfo) language() string {
	if c.Language == nil {
		return ""
	}
	return *c.Language
}

// trackFormats is each caption language and the formats it is served in.
type trackFormats map[string][]struct {
	Ext string `json:"ext"`
}

// json3 is every language served as json3, sorted. A live stream's chat replay
// is filed among the manual tracks, and is no caption.
func (f trackFormats) json3() []string {
	var languages []string
	for lang, formats := range f {
		if lang == "live_chat" || !slices.ContainsFunc(formats, func(format struct {
			Ext string `json:"ext"`
		},
		) bool {
			return format.Ext == "json3"
		}) {
			continue
		}
		languages = append(languages, lang)
	}
	slices.Sort(languages)
	return languages
}

// pickTrack is the track a read of captions answers with, from the video's
// language and the languages of its manual and automatic tracks, and false
// where none of the four it looks for is there. YouTube files the automatic
// track it recognized from the audio as <language>-orig, and the rest of the
// automatic tracks as translations of it.
func pickTrack(videoLanguage string, manual, automatic []string) (Track, bool) {
	base, _, _ := strings.Cut(videoLanguage, "-")
	if lang, ok := matching(manual, videoLanguage, base); ok {
		return Track{Language: lang, Kind: Manual}, true
	}
	if lang, ok := matching(manual, "en", "en"); ok {
		return Track{Language: lang, Kind: Manual}, true
	}
	for _, lang := range automatic {
		if strings.HasSuffix(lang, "-orig") {
			return Track{Language: lang, Kind: Automatic}, true
		}
	}
	if lang, ok := matching(automatic, videoLanguage, base); ok {
		return Track{Language: lang, Kind: Automatic}, true
	}
	if slices.Contains(automatic, "en") {
		return Track{Language: "en", Kind: Automatic}, true
	}
	return Track{}, false
}

// matching is the language of languages that is exact, else the one that is
// base, else the first whose own base is base, and false where base is empty
// or none is.
func matching(languages []string, exact, base string) (string, bool) {
	if base == "" {
		return "", false
	}
	for _, want := range []string{exact, base} {
		if slices.Contains(languages, want) {
			return want, true
		}
	}
	for _, lang := range languages {
		if strings.HasPrefix(lang, base+"-") && !strings.HasSuffix(lang, "-orig") {
			return lang, true
		}
	}
	return "", false
}

// json3Event is one event of a json3 caption track. A segment's utf8 is a
// pointer because the track writes some segments with none.
type json3Event struct {
	StartMs    int64 `json:"tStartMs"`
	DurationMs int64 `json:"dDurationMs"`
	Segs       []struct {
		UTF8 *string `json:"utf8"`
	} `json:"segs"`
}

func decodeJSON3(body []byte) ([]json3Event, error) {
	var track struct {
		Events []json3Event `json:"events"`
	}
	if err := json.Unmarshal(body, &track); err != nil {
		return nil, fmt.Errorf("decode the json3 caption track: %w", err)
	}
	return track.Events, nil
}

// segments is each event that shows text, with its segments joined and its line
// breaks as spaces.
func segments(events []json3Event) []Segment {
	shown := []Segment{}
	for _, event := range events {
		var text strings.Builder
		for _, seg := range event.Segs {
			if seg.UTF8 != nil {
				text.WriteString(*seg.UTF8)
			}
		}
		line := strings.TrimSpace(strings.ReplaceAll(text.String(), "\n", " "))
		if line == "" {
			continue
		}
		shown = append(shown, Segment{StartMs: event.StartMs, DurationMs: event.DurationMs, Text: line})
	}
	return shown
}

// spaces is a run of spaces.
var spaces = regexp.MustCompile(` {2,}`)

// transcriptLine is the track as one line: each segment's text followed by a
// line break, every line break then a space, and every run of spaces one. That
// is `jq -r '.events[]?.segs[]?.utf8 // empty' | tr '\n' ' ' | sed 's/  */ /g'`
// byte for byte, so it ends in a space.
func transcriptLine(events []json3Event) string {
	var joined strings.Builder
	for _, event := range events {
		for _, seg := range event.Segs {
			if seg.UTF8 != nil {
				joined.WriteString(*seg.UTF8)
				joined.WriteByte('\n')
			}
		}
	}
	return spaces.ReplaceAllString(strings.ReplaceAll(joined.String(), "\n", " "), " ")
}
