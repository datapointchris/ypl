package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

// maxRatingBody bounds the body of POST /api/v1/ratings.
const maxRatingBody = 32 << 10

// Bounds on what a rating carries.
const (
	maxMessageLength = 5000
	maxTagLength     = 64
	maxTags          = 20
)

// The kinds of rating, the vocabulary ratings.kind draws from.
const (
	kindLike    = "like"
	kindDislike = "dislike"
	kindNote    = "note"
)

// rating is one like, dislike or note a person left on a mix, with its
// message and tags. A mix holds as many as were left, and nothing is derived
// from them but counts.
type rating struct {
	ID      string      `json:"id"`
	RatedTs string      `json:"rated_ts"`
	Kind    string      `json:"kind"`
	Message *string     `json:"message"`
	Tags    []string    `json:"tags"`
	Video   playedVideo `json:"video"`
}

// newRating is the body of POST /api/v1/ratings. ID is a UUIDv7 the client
// generates, so sending the entry again stores it once. A note needs a
// message, and a like or dislike may carry one.
type newRating struct {
	ID      string   `json:"id"`
	VideoID string   `json:"video_id"`
	Kind    string   `json:"kind"`
	Message *string  `json:"message"`
	Tags    []string `json:"tags"`
}

// tagCount is a tag and how many videos carry it.
type tagCount struct {
	Tag    string `json:"tag"`
	Videos int64  `json:"videos"`
}

// createRating records a rating, which answers 201. The same id sent again
// answers 200 with the rating as stored.
func (h *Handlers) createRating(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, ok := decodeJSON[newRating](w, r, maxRatingBody, "a rating")
	if !ok {
		return
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id.String() != body.ID || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidRatingID, "id %q is not a version 7 UUID written lowercase with hyphens", body.ID)
		return
	}
	if body.VideoID == "" {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoIDRequired, "video_id is required")
		return
	}
	if body.Kind != kindLike && body.Kind != kindDislike && body.Kind != kindNote {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidKind, "kind %q is not like, dislike or note", body.Kind)
		return
	}
	message := sql.NullString{}
	if body.Message != nil {
		trimmed := strings.TrimSpace(*body.Message)
		if utf8.RuneCountInString(trimmed) > maxMessageLength {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeMessageTooLong, "a message holds at most %d characters", maxMessageLength)
			return
		}
		message = sql.NullString{String: trimmed, Valid: trimmed != ""}
	}
	if body.Kind == kindNote && !message.Valid {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeMessageRequired, "a note needs a message")
		return
	}
	tags, ok := normalizedTags(w, body.Tags)
	if !ok {
		return
	}

	var stored rating
	var created bool
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetVideo(ctx, body.VideoID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errVideoNotStored
			}
			return err
		}
		n, err := tx.InsertRating(ctx, generated.InsertRatingParams{
			RatingID: body.ID, VideoID: body.VideoID, Kind: body.Kind, Message: message, RatedTs: store.Timestamp(h.now()),
		})
		if err != nil {
			return err
		}
		created = n == 1
		if created {
			for _, tag := range tags {
				if err := tx.InsertRatingTag(ctx, generated.InsertRatingTagParams{RatingID: body.ID, Tag: tag}); err != nil {
					return err
				}
			}
		}
		row, err := tx.GetRating(ctx, body.ID)
		if err != nil {
			return err
		}
		tagRows, err := tx.ListRatingTags(ctx, sql.NullString{String: row.VideoID, Valid: true})
		if err != nil {
			return err
		}
		stored = ratingFrom(generated.ListRatingsRow(row), tagsByRating(tagRows))
		return nil
	})
	switch {
	case errors.Is(err, errVideoNotStored):
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoNotStored, "video %s is not in the store", body.VideoID)
		return
	case err != nil:
		h.writeInternalError(w, r, err)
		return
	}
	if created {
		wire.JSON(w, http.StatusCreated, stored)
		return
	}
	wire.JSON(w, http.StatusOK, stored)
}

// normalizedTags is tags lowercased, trimmed and without repeats, in the order
// given. ok is false once it has answered a 422.
func normalizedTags(w http.ResponseWriter, tags []string) ([]string, bool) {
	var kept []string
	for _, tag := range tags {
		tag = strings.ToLower(strings.Join(strings.Fields(tag), " "))
		if tag == "" || slices.Contains(kept, tag) {
			continue
		}
		if utf8.RuneCountInString(tag) > maxTagLength {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTagTooLong, "tag %q is longer than %d characters", tag, maxTagLength)
			return nil, false
		}
		kept = append(kept, tag)
	}
	if len(kept) > maxTags {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTooManyTags, "a rating carries at most %d tags", maxTags)
		return nil, false
	}
	return kept, true
}

// listRatings answers a page of ratings newest first, of one video when video
// names one.
func (h *Handlers) listRatings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, ratingsPage)
	if !ok {
		return
	}
	params := generated.ListRatingsParams{MaxRows: limit + 1}
	videoRef := r.URL.Query().Get("video")
	after := r.URL.Query().Get("starting_after")
	var ratings []rating
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if videoRef != "" {
			id, err := resolveVideo(ctx, q, videoRef)
			if err != nil {
				return err
			}
			params.VideoID = sql.NullString{String: id, Valid: true}
		}
		if after != "" {
			cursor, err := q.GetRating(ctx, after)
			if err != nil {
				return paramRow(err, referenceError{name: "starting_after", value: after})
			}
			params.BeforeTs = sql.NullString{String: cursor.RatedTs, Valid: true}
			params.BeforeID = sql.NullString{String: cursor.RatingID, Valid: true}
		}
		rows, err := q.ListRatings(ctx, params)
		if err != nil {
			return err
		}
		tagRows, err := q.ListRatingTags(ctx, params.VideoID)
		if err != nil {
			return err
		}
		tags := tagsByRating(tagRows)
		for _, row := range rows {
			ratings = append(ratings, ratingFrom(row, tags))
		}
		return nil
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}
	if ratings == nil {
		ratings = []rating{}
	}
	wire.JSON(w, http.StatusOK, pageOf(ratings, limit))
}

// listTags answers every tag a rating carried, with how many videos carry it,
// most carried first.
func (h *Handlers) listTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var rows []generated.ListTagCountsRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		rows, err = q.ListTagCounts(ctx)
		return err
	})
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	tags := make([]tagCount, len(rows))
	for i, row := range rows {
		tags[i] = tagCount{Tag: row.Tag, Videos: row.Videos}
	}
	wire.JSON(w, http.StatusOK, tags)
}

func tagsByRating(rows []generated.ListRatingTagsRow) map[string][]string {
	byRating := make(map[string][]string)
	for _, row := range rows {
		byRating[row.RatingID] = append(byRating[row.RatingID], row.Tag)
	}
	for _, tags := range byRating {
		slices.Sort(tags)
	}
	return byRating
}

func ratingFrom(row generated.ListRatingsRow, tags map[string][]string) rating {
	return rating{
		ID:      row.RatingID,
		RatedTs: row.RatedTs,
		Kind:    row.Kind,
		Message: nullableText(row.Message),
		Tags:    orEmpty(tags[row.RatingID]),
		Video:   playedVideo{ID: row.VideoID, Title: row.Title, ChannelTitle: row.ChannelTitle},
	}
}
