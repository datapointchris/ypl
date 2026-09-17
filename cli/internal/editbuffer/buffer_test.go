package editbuffer

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// rendered is a buffer of three videos, with the labels and lengths the CLI
// puts on the lines.
func rendered() (string, []string) {
	videoIDs := []string{"dQw4w9WgXcQ", "aBcDeFgHiJk", "_-123456789"}
	return Render("Sunday Morning", []Row{
		{VideoID: videoIDs[0], Label: "Some Channel - A Six Hour Mix", Length: "6:00:00"},
		{VideoID: videoIDs[1], Label: "Another - Deep # House", Length: "1:12:30"},
		{VideoID: videoIDs[2], Label: "", Length: ""},
	}), videoIDs
}

// A buffer the tool wrote has to be one the tool accepts, or the first save of
// an untouched buffer is refused with the tool's own output as the reason.
func TestABufferParsesBackToTheIdsItWasRenderedFrom(t *testing.T) {
	buffer, videoIDs := rendered()

	got, err := Parse(buffer, held(videoIDs))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(got, videoIDs) {
		t.Fatalf("parsed %v, want %v", got, videoIDs)
	}
}

func TestRenderPutsTheIdFirstOnEveryVideoLine(t *testing.T) {
	buffer, videoIDs := rendered()

	var lines []string
	for _, line := range strings.Split(strings.TrimRight(buffer, "\n"), "\n") {
		if !strings.HasPrefix(line, comment) {
			lines = append(lines, line)
		}
	}
	if len(lines) != len(videoIDs) {
		t.Fatalf("rendered %d video lines, want %d: %q", len(lines), len(videoIDs), buffer)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, videoIDs[i]) {
			t.Errorf("line %d is %q, want it to start with %q", i, line, videoIDs[i])
		}
	}
}

func TestParseIgnoresCommentsAndBlankLines(t *testing.T) {
	got, err := Parse("# a heading\n\n   \ndQw4w9WgXcQ  Some - Mix\n#dQw4w9WgXcQ commented out\n", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(got, []string{"dQw4w9WgXcQ"}) {
		t.Fatalf("parsed %v, want the one uncommented line", got)
	}
}

// An id that does not match the eleven-character rule still came out of this
// buffer, and rejecting it on the way back in is the tool refusing its own
// output.
func TestParseTakesAnIdItRenderedHoweverOddItLooks(t *testing.T) {
	got, err := Parse("odd  A Mix\n", held([]string{"odd"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(got, []string{"odd"}) {
		t.Fatalf("parsed %v, want the odd id taken as given", got)
	}
	if _, err := Parse("odd  A Mix\n", nil); err == nil {
		t.Fatal("an odd token the buffer never held was accepted, so prose would file as a video")
	}
}

// Which line is the whole difference between fixing it and reopening the editor
// to hunt for it.
func TestParseNamesTheLineThatIsNotAVideo(t *testing.T) {
	_, err := Parse("# a heading\ndQw4w9WgXcQ  A Mix\nremember to add the other one\n", held([]string{"dQw4w9WgXcQ"}))

	var line *LineError
	if !errors.As(err, &line) {
		t.Fatalf("Parse gave %v, want a LineError", err)
	}
	if line.Number != 3 {
		t.Errorf("named line %d, want 3", line.Number)
	}
	if !strings.Contains(line.Error(), "remember to add the other one") {
		t.Errorf("the sentence is %q, want it to carry the line", line.Error())
	}
}

// A playlist may legitimately hold the same mix twice, and the buffer is the
// person saying what they want.
func TestParseKeepsADuplicate(t *testing.T) {
	got, err := Parse("dQw4w9WgXcQ\ndQw4w9WgXcQ\n", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %v, want both slots", got)
	}
}

func TestVideoIDReadsEveryShapeOfAddressAndRefusesTheRest(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	for _, c := range []struct {
		token string
		want  string
	}{
		{id, id},
		{"  " + id + "  ", id},
		{"https://www.youtube.com/watch?v=" + id, id},
		{"https://www.youtube.com/watch?v=" + id + "&list=PLabc&t=42s", id},
		{"https://m.youtube.com/watch?v=" + id, id},
		{"https://music.youtube.com/watch?v=" + id, id},
		{"WWW.YOUTUBE.COM/watch?v=" + id, id},
		{"https://youtu.be/" + id, id},
		{"https://youtu.be/" + id + "?t=42", id},
		{"youtu.be/" + id, id},
		{"https://www.youtube.com/shorts/" + id, id},
		{"https://www.youtube.com/embed/" + id, id},
		{"https://www.youtube.com/live/" + id, id},
		{"https://www.youtube.com/v/" + id, id},
		{"", ""},
		{"short", ""},
		{"this is not an id at all", ""},
		{"https://vimeo.com/123456789", ""},
		// The host decides, not the shape of what follows it. Somewhere else's
		// address in YouTube's shape is somewhere else's video.
		{"https://example.com/watch?v=" + id, ""},
		{"https://notyoutube.com/shorts/" + id, ""},
		{"https://www.youtube.com/playlist?list=PLabc", ""},
		{"https://www.youtube.com/", ""},
	} {
		if got := VideoID(c.token); got != c.want {
			t.Errorf("VideoID(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}

func TestCommandPrefersVisualAndFallsBackToVi(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := Command(); !slices.Equal(got, []string{defaultEditor}) {
		t.Errorf("with neither variable set the editor is %v, want %v", got, []string{defaultEditor})
	}

	t.Setenv("EDITOR", "ed")
	if got := Command(); !slices.Equal(got, []string{"ed"}) {
		t.Errorf("with EDITOR set the editor is %v, want ed", got)
	}

	// $EDITOR may be a line editor for dumb terminals, and a playlist is not
	// something to rearrange in ed.
	t.Setenv("VISUAL", "code --wait")
	if got := Command(); !slices.Equal(got, []string{"code", "--wait"}) {
		t.Errorf("the editor is %v, want VISUAL split into its arguments", got)
	}
}

// held is the set Parse takes as given, built the way the command layer builds
// it.
func held(videoIDs []string) map[string]bool {
	known := map[string]bool{}
	for _, videoID := range videoIDs {
		known[videoID] = true
	}
	return known
}
