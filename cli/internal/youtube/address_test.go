package youtube

import "testing"

// The parser is the binary's only reader of a YouTube address, and every caller
// hands it something a person typed or pasted.
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
