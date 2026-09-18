// Package youtube turns a YouTube address into a video id and back.
//
// Both directions live together because they are one round trip, and because
// every caller that has one of them eventually wants the other. The callers
// have nothing else in common: a playlist edit buffer reads a pasted link, `ypl
// now` reads the path mpv has loaded, `ypl plays add` reads a command-line
// argument, and `ypl next` writes an address out.
//
// Nothing here reaches YouTube. An address is a fact about how YouTube spells
// things rather than about what it holds, which is why the store never sends
// one.
package youtube

import (
	"net/url"
	"strings"
)

// idLength is how many characters a YouTube video id has, and idAlphabet what
// they are drawn from.
const idLength = 11

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// watchHosts serve a video at /watch?v=; shortHosts serve one at /<id>.
var watchHosts = map[string]bool{
	"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true, "music.youtube.com": true,
}

var shortHosts = map[string]bool{"youtu.be": true}

// pathPrefixes are the paths that carry an id as their next segment.
var pathPrefixes = []string{"/shorts/", "/embed/", "/v/", "/live/"}

// WatchURL is where a video is played from. It is built here rather than sent
// by the server, since it is a fact about YouTube rather than about the store.
func WatchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// VideoID is the video a token names: a bare id, or the id inside a YouTube
// URL. It is "" for anything else.
//
// A URL is read structurally rather than matched, so one carrying extra query
// parameters — `&list=`, `&t=`, the tracking ones YouTube's share button
// appends — yields the same id as the bare link. What a URL names is taken as
// given, since `v=` says what it is; only a bare token has to look like an id,
// which is what keeps a line of prose from being filed as a video.
func VideoID(token string) string {
	candidate := strings.TrimSpace(token)
	switch {
	case candidate == "":
		return ""
	case !strings.Contains(candidate, "/"):
		if IsVideoID(candidate) {
			return candidate
		}
		return ""
	}
	// A URL written without a scheme parses as a path, and then the host this
	// reads is empty. The `//` makes it an authority either way.
	if !strings.Contains(candidate, "//") {
		candidate = "//" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	if shortHosts[host] {
		return firstSegment(parsed.Path)
	}
	if !watchHosts[host] {
		return ""
	}
	if named := parsed.Query()["v"]; len(named) > 0 {
		return named[0]
	}
	for _, prefix := range pathPrefixes {
		if strings.HasPrefix(parsed.Path, prefix) {
			return firstSegment(strings.TrimPrefix(parsed.Path, prefix))
		}
	}
	return ""
}

// IsVideoID reports whether text is an id as YouTube writes one.
func IsVideoID(text string) bool {
	if len(text) != idLength {
		return false
	}
	return strings.IndexFunc(text, func(r rune) bool {
		return !strings.ContainsRune(idAlphabet, r)
	}) < 0
}

// firstSegment is the first path segment of path, and "" where there is none.
func firstSegment(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return segment
}
