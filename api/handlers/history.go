package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// HistoryReader reads the watch history of the account whose cookies a jar
// holds, as *ytdlp.Reader does, returning the jar written back after the read.
type HistoryReader interface {
	History(ctx context.Context, jar string, entries int) ([]ytdlp.HistoryEntry, string, error)
}

// historyReads is what the watch history is read through, and the latest read
// kept for historyReuse so a picker opened twice in a row costs one read.
// YouTube may ban an account it sees read too often, so reads are one at a
// time and only on request.
type historyReads struct {
	reader HistoryReader
	mu     sync.Mutex
	at     time.Time
	last   []ytdlp.HistoryEntry
}

// historyReuse is how long one read of the watch history answers for.
const historyReuse = 30 * time.Second

// historyDepth is how many history entries a read asks for, of which those the
// library holds are answered. A video watched outside the library is skipped,
// so the read goes deeper than the answer.
const historyDepth = 25

// historyDuration bounds one read of the watch history.
const historyDuration = 60 * time.Second

// maxCookieBody bounds the body of PUT /api/v1/youtube/session, many times the
// size of a jar of YouTube's cookies.
const maxCookieBody = 256 << 10

// ReadHistoryWith sets what the watch history is read through. Without it the
// history and session routes answer 503.
func (h *Handlers) ReadHistoryWith(reader HistoryReader) {
	h.history = &historyReads{reader: reader}
}

// historyEntry is a library video the watch history holds, at its place in the
// history, 1 being the newest.
type historyEntry struct {
	Position int64       `json:"position"`
	Video    playedVideo `json:"video"`
}

// youtubeSession is where the signed-in session stands: when its jar was set,
// and the latest read YouTube answered signed in and signed out. A refusal
// later than the latest ok read means the jar needs exporting again.
type youtubeSession struct {
	StoredTs      string  `json:"stored_ts"`
	LastOKTs      *string `json:"last_ok_ts"`
	LastRefusedTs *string `json:"last_refused_ts"`
	LastError     *string `json:"last_error"`
}

// newSession is the body of PUT /api/v1/youtube/session: a Netscape cookie
// jar holding youtube.com's cookies.
type newSession struct {
	CookieJar string `json:"cookie_jar"`
}

// listHistory answers the newest library videos in the watch history, up to
// limit, newest first.
func (h *Handlers) listHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.history == nil {
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeHistoryUnavailable, "this server reads no watch history: it has no yt-dlp")
		return
	}
	limit, ok := limitParam(w, r, historyPage)
	if !ok {
		return
	}
	entries, err := h.readHistory(ctx)
	if h.refuseHistory(w, r, err) {
		return
	}
	var answer []historyEntry
	err = h.store.InReadTx(ctx, func(q *generated.Queries) error {
		for i, entry := range entries {
			if int64(len(answer)) == limit {
				return nil
			}
			video, err := q.GetVideo(ctx, entry.ID)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && video.IsUnavailable) {
				continue
			}
			if err != nil {
				return err
			}
			answer = append(answer, historyEntry{
				Position: int64(i + 1),
				Video:    playedVideo{ID: video.VideoID, Title: video.Title, ChannelTitle: video.ChannelTitle},
			})
		}
		return nil
	})
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	wire.JSON(w, http.StatusOK, orEmpty(answer))
}

// readHistory is the watch history, read with the stored jar or reused from a
// read within historyReuse. Each read records how YouTube answered, and keeps
// the jar yt-dlp wrote back.
func (h *Handlers) readHistory(ctx context.Context) ([]ytdlp.HistoryEntry, error) {
	h.history.mu.Lock()
	defer h.history.mu.Unlock()
	if h.history.last != nil && h.now().Sub(h.history.at) < historyReuse {
		return h.history.last, nil
	}
	var session generated.GetYouTubeSessionRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		session, err = q.GetYouTubeSession(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, err
	}
	entries, jar, err := h.readWith(ctx, session.CookieJar)
	if err != nil {
		return nil, err
	}
	h.history.at, h.history.last = h.now(), entries
	return entries, h.store.InTx(ctx, func(tx *store.Tx) error {
		return tx.RecordYouTubeSessionOK(ctx, generated.RecordYouTubeSessionOKParams{
			CookieJar: jar, ReadTs: sql.NullString{String: store.Timestamp(h.now()), Valid: true},
		})
	})
}

// readWith reads the history with jar, recording a signed-out answer against
// the stored session.
func (h *Handlers) readWith(ctx context.Context, jar string) ([]ytdlp.HistoryEntry, string, error) {
	readCtx, cancel := context.WithTimeout(ctx, historyDuration)
	defer cancel()
	entries, written, err := h.history.reader.History(readCtx, jar, historyDepth)
	if errors.Is(err, ytdlp.ErrSignedOut) {
		recorded := h.store.InTx(ctx, func(tx *store.Tx) error {
			return tx.RecordYouTubeSessionRefused(ctx, generated.RecordYouTubeSessionRefusedParams{
				ReadTs:    sql.NullString{String: store.Timestamp(h.now()), Valid: true},
				LastError: sql.NullString{String: err.Error(), Valid: true},
			})
		})
		return nil, "", errors.Join(err, recorded)
	}
	return entries, written, err
}

var errNoSession = errors.New("no YouTube session")

// refuseHistory answers a failed read of the watch history, and reports
// whether it answered.
func (h *Handlers) refuseHistory(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNoSession):
		wire.Refuse(w, http.StatusConflict, wire.CodeYouTubeSessionMissing, "the server holds no YouTube session; export a cookie jar and send it with `ypl server cookies set`")
	case errors.Is(err, ytdlp.ErrSignedOut):
		wire.Refuse(w, http.StatusBadGateway, wire.CodeYouTubeSignedOut, "YouTube answered the watch history signed out; export the cookie jar again and send it with `ypl server cookies set`")
	case errors.Is(err, ytdlp.ErrRateLimited):
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeRateLimited, "YouTube is refusing reads from the server for now")
	default:
		h.refuseAndLog(w, r, slog.LevelWarn, http.StatusBadGateway, wire.CodeYouTubeReadFailed, err, "the watch history could not be read")
	}
	return true
}

// setSession replaces the YouTube session with the jar in the body, once a
// read of the watch history with it answers signed in. A jar that reads signed
// out is refused and the session already held is kept.
func (h *Handlers) setSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.history == nil {
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeHistoryUnavailable, "this server reads no watch history: it has no yt-dlp")
		return
	}
	body, ok := decodeJSON[newSession](w, r, maxCookieBody, "a cookie jar")
	if !ok {
		return
	}
	if !holdsYouTubeCookies(body.CookieJar) {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidCookieJar, "cookie_jar is not a Netscape cookie jar holding youtube.com cookies")
		return
	}
	h.history.mu.Lock()
	defer h.history.mu.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, historyDuration)
	defer cancel()
	entries, jar, err := h.history.reader.History(readCtx, body.CookieJar, historyDepth)
	if errors.Is(err, ytdlp.ErrSignedOut) {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeYouTubeSignedOut, "YouTube reads the watch history signed out with this jar, so the session held is kept; export it again from a private window")
		return
	}
	if h.refuseHistory(w, r, err) {
		return
	}
	storedTs := store.Timestamp(h.now())
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		return tx.SetYouTubeSession(ctx, generated.SetYouTubeSessionParams{CookieJar: jar, StoredTs: storedTs})
	})
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	h.history.at, h.history.last = h.now(), entries
	wire.JSON(w, http.StatusOK, youtubeSession{StoredTs: storedTs, LastOKTs: &storedTs})
}

// holdsYouTubeCookies reports whether jar has at least one Netscape cookie line
// for youtube.com: seven tab-separated fields, the first a domain.
func holdsYouTubeCookies(jar string) bool {
	for line := range strings.Lines(jar) {
		line = strings.TrimPrefix(strings.TrimSpace(line), "#HttpOnly_")
		fields := strings.Split(line, "\t")
		if len(fields) == 7 && strings.HasSuffix(fields[0], "youtube.com") {
			return true
		}
	}
	return false
}

// sessionState is the YouTube session the store holds, and nil without one.
func sessionState(ctx context.Context, q *generated.Queries) (*youtubeSession, error) {
	row, err := q.GetYouTubeSession(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &youtubeSession{
		StoredTs:      row.StoredTs,
		LastOKTs:      nullableText(row.LastOkTs),
		LastRefusedTs: nullableText(row.LastRefusedTs),
		LastError:     nullableText(row.LastError),
	}, nil
}
