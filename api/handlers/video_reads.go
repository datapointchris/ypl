package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/goapiauth"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// VideoReader reads a video through the server's one yt-dlp door, as
// *ytdlp.Door does.
type VideoReader interface {
	Captions(ctx context.Context, class ytdlp.Class, id string) (ytdlp.Captions, error)
	Transcript(ctx context.Context, class ytdlp.Class, id, lang string) (ytdlp.Transcript, error)
	Audio(ctx context.Context, class ytdlp.Class, id string, maxBytes int64) (*ytdlp.Audio, error)
	PausedUntil() (ytdlp.Pause, bool)
}

// videoReads is the door the caption, transcript and audio routes read
// through, and the host a request names when it reaches the server through
// its public address.
type videoReads struct {
	door       VideoReader
	publicHost string
}

// maxPublicAudio is the most audio the audio route sends to a request that
// reached the server through its public address. The proxy in front of that
// address refuses an answer of more than 100 MB.
const maxPublicAudio = 100_000_000

// ReadVideosWith sets the door the caption, transcript and audio routes read
// through. publicHost is the host of the server's public address, such as
// ypl.example.com, and an audio answer to a request naming it is capped at
// maxPublicAudio. Without a door those routes answer 503.
func (h *Handlers) ReadVideosWith(door VideoReader, publicHost string) {
	h.videos = &videoReads{door: door, publicHost: strings.ToLower(publicHost)}
}

// captionsAnswer is the body of GET /api/v1/youtube/videos/{id}/captions.
// Track is null and Segments empty where the read finds no manual track in the
// video's language or English, and no automatic one in its original language
// or English.
type captionsAnswer struct {
	VideoID         string           `json:"video_id"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	Language        *string          `json:"language"`
	DurationSeconds *int64           `json:"duration_seconds"`
	Track           *captionTrack    `json:"track"`
	Segments        []captionSegment `json:"segments"`
	Chapters        []videoChapter   `json:"chapters"`
}

// captionTrack is the track a read used: its language as YouTube files it,
// and manual or automatic.
type captionTrack struct {
	Language string `json:"language"`
	Kind     string `json:"kind"`
}

type captionSegment struct {
	StartMs    int64  `json:"start_ms"`
	DurationMs int64  `json:"duration_ms"`
	Text       string `json:"text"`
}

type videoChapter struct {
	StartSeconds int64  `json:"start_seconds"`
	EndSeconds   int64  `json:"end_seconds"`
	Title        string `json:"title"`
}

// transcriptAnswer is the body of GET /api/v1/youtube/videos/{id}/transcript:
// the track's segments as one line of text.
type transcriptAnswer struct {
	VideoID  string `json:"video_id"`
	Language string `json:"language"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

// showCaptions answers the video's captions: the segments of the track the
// door's read picks, its chapters and its details.
func (h *Handlers) showCaptions(w http.ResponseWriter, r *http.Request) {
	id, ok := h.videoToRead(w, r)
	if !ok {
		return
	}
	read, err := h.videos.door.Captions(r.Context(), classOf(r), id)
	if h.refuseVideoRead(w, r, err) {
		return
	}
	answer := captionsAnswer{
		VideoID:         read.VideoID,
		Title:           read.Title,
		Description:     read.Description,
		DurationSeconds: read.DurationSeconds,
		Segments:        make([]captionSegment, len(read.Segments)),
		Chapters:        make([]videoChapter, len(read.Chapters)),
	}
	if read.Language != "" {
		answer.Language = &read.Language
	}
	if read.Track != nil {
		answer.Track = &captionTrack{Language: read.Track.Language, Kind: string(read.Track.Kind)}
	}
	for i, segment := range read.Segments {
		answer.Segments[i] = captionSegment{StartMs: segment.StartMs, DurationMs: segment.DurationMs, Text: segment.Text}
	}
	for i, chapter := range read.Chapters {
		answer.Chapters[i] = videoChapter{StartSeconds: chapter.StartSeconds, EndSeconds: chapter.EndSeconds, Title: chapter.Title}
	}
	wire.JSON(w, http.StatusOK, answer)
}

// showTranscript answers the video's captions in lang, en when absent, as one
// line: its manual track, else its automatic one.
func (h *Handlers) showTranscript(w http.ResponseWriter, r *http.Request) {
	id, ok := h.videoToRead(w, r)
	if !ok {
		return
	}
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "en"
	}
	read, err := h.videos.door.Transcript(r.Context(), classOf(r), id, lang)
	if h.refuseVideoRead(w, r, err) {
		return
	}
	wire.JSON(w, http.StatusOK, transcriptAnswer{VideoID: read.VideoID, Language: read.Track.Language, Kind: string(read.Track.Kind), Text: read.Text})
}

// streamAudio answers the video's best audio, from a file removed once the
// answer is sent. A request that reached the server through its public
// address gets at most maxPublicAudio bytes, and audio larger than that is
// refused before any of it is fetched.
func (h *Handlers) streamAudio(w http.ResponseWriter, r *http.Request) {
	id, ok := h.videoToRead(w, r)
	if !ok {
		return
	}
	var most int64
	if h.videos.publicHost != "" && requestHost(r) == h.videos.publicHost {
		most = maxPublicAudio
	}
	audio, err := h.videos.door.Audio(r.Context(), classOf(r), id, most)
	if h.refuseVideoRead(w, r, err) {
		return
	}
	defer func() { _ = audio.Close() }()
	file, err := os.Open(audio.Path)
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", audioType(audio.Ext))
	w.Header().Set("Content-Length", strconv.FormatInt(audio.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+"."+audio.Ext))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, file); err != nil {
		h.log.InfoContext(r.Context(), "the audio answer ended early", "video_id", id, "err", err)
	}
}

// audioType is the media type of an audio file yt-dlp names by ext.
func audioType(ext string) string {
	switch ext {
	case "webm":
		return "audio/webm"
	case "m4a", "mp4":
		return "audio/mp4"
	case "mp3":
		return "audio/mpeg"
	case "opus", "ogg":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

// requestHost is the host r names, lowercased and without a port.
func requestHost(r *http.Request) string {
	host := r.Host
	if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	return strings.ToLower(host)
}

// classOf is the door's class for r: a service client's read is a service's,
// and every other a person's.
func classOf(r *http.Request) ytdlp.Class {
	if identity, ok := goapiauth.IdentityFrom(r.Context()); ok && identity.Service {
		return ytdlp.Service
	}
	return ytdlp.Person
}

// videoToRead is the video id the path names, once the server is found to
// have a door to read it through. ok is false once it has answered.
func (h *Handlers) videoToRead(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.videos == nil {
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsUnavailable, "this server reads no video through yt-dlp: it has no yt-dlp")
		return "", false
	}
	id := r.PathValue("id")
	if !videoID.MatchString(id) {
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, []string{id}, "%q is not a YouTube video id", id)
		return "", false
	}
	return id, true
}

// refuseVideoRead answers a read through the door that failed with err, and
// reports whether it answered.
func (h *Handlers) refuseVideoRead(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case h.refuseAtTheDoor(w, r, err):
	case errors.Is(err, ytdlp.ErrNoCaptions):
		wire.Refuse(w, http.StatusNotFound, wire.CodeNoCaptions, "the video has no caption track in that language")
	case errors.Is(err, ytdlp.ErrTooLarge):
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeAudioTooLarge,
			"the video's audio is larger than %d bytes, the most this server sends through its public address; ask its own address instead", maxPublicAudio)
	case errors.Is(err, ytdlp.ErrUnreadable):
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeVideoUnavailable, []string{r.PathValue("id")}, "YouTube lets no signed-out read reach this video")
	case r.Context().Err() != nil:
		h.log.InfoContext(r.Context(), "the caller left before the read of YouTube ended", "path", r.URL.Path)
	default:
		h.refuseAndLog(w, r, slog.LevelWarn, http.StatusBadGateway, wire.CodeYouTubeReadFailed, err, "the read of YouTube failed")
	}
	return true
}

// refuseAtTheDoor answers with a 503 where the door turned the read away or
// YouTube refused the address. Its Retry-After says when to ask again. It
// reports whether it answered.
func (h *Handlers) refuseAtTheDoor(w http.ResponseWriter, r *http.Request, err error) bool {
	var paused *ytdlp.PausedError
	switch {
	case errors.As(err, &paused):
		retryAfter(w, paused.Pause.Until.Sub(h.now()))
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsPaused,
			"the server makes no read of YouTube until %s, after %s reads", store.Timestamp(paused.Pause.Until), paused.Pause.Cause)
	case errors.Is(err, ytdlp.ErrRateLimited):
		retryAfter(w, ytdlp.RefusedPause)
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeRateLimited, "YouTube refused the server's read, so it makes none for a day")
	case errors.Is(err, ytdlp.ErrBusy):
		retryAfter(w, ytdlp.Gap+ytdlp.Jitter)
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsBusy, "the server's yt-dlp stayed busy with other reads; send the request again")
	case errors.Is(err, ytdlp.ErrClosed):
		h.refuseAndLog(w, r, slog.LevelInfo, http.StatusServiceUnavailable, wire.CodeShuttingDown, err,
			"the server is shutting down, so it made no read; send the request again")
	default:
		return false
	}
	return true
}

// retryAfter sets Retry-After to wait, in whole seconds rounded up.
func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(math.Ceil(wait.Seconds()))), 10))
}
