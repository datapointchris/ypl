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

// VideoReader reads YouTube through the server's one yt-dlp door, as
// *ytdlp.Door does: a video's captions, transcript and audio, a person's own
// run of yt-dlp, and a lease for a read that a desk makes itself.
type VideoReader interface {
	Captions(ctx context.Context, class ytdlp.Class, id string) (ytdlp.Captions, error)
	Transcript(ctx context.Context, class ytdlp.Class, id, lang string) (ytdlp.Transcript, error)
	Audio(ctx context.Context, class ytdlp.Class, id string, maxBytes int64) (*ytdlp.Audio, error)
	Run(ctx context.Context, class ytdlp.Class, args ytdlp.RunArguments, maxBytes int64) (*ytdlp.Run, error)
	Lease(ctx context.Context, class ytdlp.Class) (time.Time, error)
	PausedUntil() (ytdlp.Pause, bool)
	Pace() ytdlp.Pace
}

// videoReads is the door the caption, transcript and audio routes read
// through, the host a request names when it reaches the server through its
// public address, how long a read for such a request may take, and the most a
// file read answers it with.
type videoReads struct {
	door       VideoReader
	publicHost string
	budget     time.Duration
	maxPublic  int64
}

// The limits of the reverse proxy in front of PUBLIC_URL, which a request
// through the public address meets.
const (
	// maxPublicAnswer is the most a file read sends to such a request, the
	// audio route's file and a run's streams and files alike: the largest
	// answer the proxy passes, as Cloudflare's does by default.
	maxPublicAnswer = 100_000_000
	// publicReadBudget is the longest a caption, transcript or audio read
	// takes for such a request, its wait at the door included. The proxy
	// drops a request whose answer has not begun within its read timeout,
	// which on Cloudflare is longer than this, so the server answers first.
	publicReadBudget = 90 * time.Second
)

// errPastPublicBudget ends a read for a request through the public address
// once publicReadBudget has passed.
var errPastPublicBudget = errors.New("the read ran past the time a request through the public address may take")

// ReadVideosWith sets the door the caption, transcript and audio routes read
// through. publicHost is the host of the server's public address, such as
// ypl.example.com. A request naming it gets at most maxPublicAnswer of audio,
// and a read for it ends at publicReadBudget. Without a door those routes
// answer 503.
func (h *Handlers) ReadVideosWith(door VideoReader, publicHost string) {
	h.videos = &videoReads{door: door, publicHost: strings.ToLower(publicHost), budget: publicReadBudget, maxPublic: maxPublicAnswer}
}

// public is whether r reached the server through its public address.
func (v *videoReads) public(r *http.Request) bool {
	return v.publicHost != "" && requestHost(r) == v.publicHost
}

// turnAfter is the longest gap the door leaves after a read, which is when to
// ask again for a turn that did not come.
func (v *videoReads) turnAfter() time.Duration {
	pace := v.door.Pace()
	return pace.Gap + pace.Jitter
}

// readContext is what a read for r runs on: r's context, ended at the budget
// where r came through the public address.
func (v *videoReads) readContext(r *http.Request) (context.Context, context.CancelFunc) {
	if !v.public(r) {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeoutCause(r.Context(), v.budget, errPastPublicBudget)
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
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	read, err := h.videos.door.Captions(ctx, classOf(r), id)
	if h.refuseVideoRead(ctx, w, r, err) {
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
// line: its manual track, else its automatic one. A lang that is no language
// is refused before the door.
func (h *Handlers) showTranscript(w http.ResponseWriter, r *http.Request) {
	id, ok := h.videoToRead(w, r)
	if !ok {
		return
	}
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "en"
	}
	if !ytdlp.ValidLanguage(lang) {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "lang %q is not a language, such as en or pt-BR", lang)
		return
	}
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	read, err := h.videos.door.Transcript(ctx, classOf(r), id, lang)
	if h.refuseVideoRead(ctx, w, r, err) {
		return
	}
	wire.JSON(w, http.StatusOK, transcriptAnswer{VideoID: read.VideoID, Language: read.Track.Language, Kind: string(read.Track.Kind), Text: read.Text})
}

// streamAudio answers the video's best audio, from a file removed once the
// answer is sent. A request that reached the server through its public
// address gets at most maxPublicAnswer bytes, and audio larger than that is
// refused before any of it is fetched.
func (h *Handlers) streamAudio(w http.ResponseWriter, r *http.Request) {
	id, ok := h.videoToRead(w, r)
	if !ok {
		return
	}
	var most int64
	if h.videos.public(r) {
		most = h.videos.maxPublic
	}
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	audio, err := h.videos.door.Audio(ctx, classOf(r), id, most)
	if h.refuseVideoRead(ctx, w, r, err) {
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

// refuseVideoRead answers a read through the door on ctx that failed with
// err, and reports whether it answered.
func (h *Handlers) refuseVideoRead(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case h.refuseAtTheDoor(w, r, err):
	case errors.Is(context.Cause(ctx), errPastPublicBudget):
		retryAfter(w, h.videos.turnAfter())
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsBusy,
			"the read did not finish within the %v a request through the public address may take; send it again, or ask the server's own address", h.videos.budget)
	case errors.Is(err, ytdlp.ErrNotStarted):
		wire.RefuseVideos(w, http.StatusConflict, wire.CodeVideoNotStarted, []string{r.PathValue("id")}, "the video is a premiere or a live stream that has not begun")
	case errors.Is(err, ytdlp.ErrNoCaptions):
		wire.Refuse(w, http.StatusNotFound, wire.CodeNoCaptions, "the video has no caption track in that language")
	case errors.Is(err, ytdlp.ErrTooLarge):
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeAudioTooLarge,
			"the video's audio is larger than %d bytes, the most this server sends through its public address; ask its own address instead", h.videos.maxPublic)
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
// YouTube refused the address. Its Retry-After says when to ask again. A read
// that YouTube refused answers as every read after it does, with the pause
// it began. It reports whether it answered.
func (h *Handlers) refuseAtTheDoor(w http.ResponseWriter, r *http.Request, err error) bool {
	pause, paused := ytdlp.PauseOf(err)
	switch {
	case paused:
		retryAfter(w, pause.Until.Sub(h.now()))
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsPaused,
			"the server makes no read of YouTube until %s, after %s reads", store.Timestamp(pause.Until), pause.Cause)
	case errors.Is(err, ytdlp.ErrBusy):
		retryAfter(w, h.videos.turnAfter())
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsBusy, "the server's yt-dlp stayed busy with other reads; send the request again")
	case errors.Is(err, ytdlp.ErrClosed):
		h.refuseAndLog(w, r, slog.LevelInfo, http.StatusServiceUnavailable, wire.CodeShuttingDown, err,
			"the server is shutting down, so it stopped or made no read; send the request again")
	default:
		return false
	}
	return true
}

// retryAfter sets Retry-After to wait, in whole seconds rounded up.
func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(math.Ceil(wait.Seconds()))), 10))
}
