// Package wire writes the JSON bodies the API answers with, and names every
// refusal it can make.
//
// A refusal is {"error": "<status text>", "message": "<sentence>", "code":
// "<code>"}. The sentence is for a person and may change. The code is for a
// client to branch on and does not. A refusal about particular videos names
// them in "video_ids" too.
package wire

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/datapointchris/goapiauth"
)

// Code names one refusal.
type Code string

// Every refusal the API makes, by the status it answers with.
const (
	// 400
	CodeInvalidLimit        Code = "invalid_limit"
	CodeInvalidParameter    Code = "invalid_parameter"
	CodeUnknownReference    Code = "unknown_reference"
	CodeAmbiguousReference  Code = "ambiguous_reference"
	CodeInvalidBody         Code = "invalid_body"
	CodeInvalidPrecondition Code = "invalid_precondition"

	// 401
	CodeMissingToken Code = goapiauth.CodeMissingToken
	CodeInvalidToken Code = goapiauth.CodeInvalidToken

	// 403
	CodeOutsideServiceScope Code = goapiauth.CodeOutsideServiceScope

	// 404
	CodeNotFound      Code = "not_found"
	CodeRouteNotFound Code = "route_not_found"

	// 405
	CodeMethodNotAllowed Code = "method_not_allowed"

	// 409
	CodePlayConflict          Code = "play_conflict"
	CodeYouTubeSessionMissing Code = "youtube_session_missing"

	// 410
	CodePlayDeleted Code = "play_deleted"

	// 412
	CodePreconditionFailed Code = "precondition_failed"

	// 422
	CodeTitleRequired       Code = "title_required"
	CodeTitleTooLong        Code = "title_too_long"
	CodeDescriptionTooLong  Code = "description_too_long"
	CodeNoChanges           Code = "no_changes"
	CodeYouTubeRefused      Code = "youtube_refused"
	CodeInvalidPlayID       Code = "invalid_play_id"
	CodeVideoIDRequired     Code = "video_id_required"
	CodeVideoNotStored      Code = "video_not_stored"
	CodeInvalidPlayedTs     Code = "invalid_played_ts"
	CodePlayedTsOutOfRange  Code = "played_ts_out_of_range"
	CodePlayedTsInTheFuture Code = "played_ts_in_the_future"
	CodeVideoIDsRequired    Code = "video_ids_required"
	CodeTooManyVideos       Code = "too_many_videos"
	CodeInvalidVideoID      Code = "invalid_video_id"
	CodeVideoUnavailable    Code = "video_unavailable"
	CodeInvalidRatingID     Code = "invalid_rating_id"
	CodeInvalidVerdict      Code = "invalid_verdict"
	CodeNoteTooLong         Code = "note_too_long"
	CodeTagTooLong          Code = "tag_too_long"
	CodeTooManyTags         Code = "too_many_tags"
	CodeRatingEmpty         Code = "rating_empty"
	CodeInvalidCookieJar    Code = "invalid_cookie_jar"

	// 428
	CodePreconditionRequired Code = "precondition_required"

	// 500
	CodeInternal               Code = "internal"
	CodeYouTubeWriteUnrecorded Code = "youtube_write_unrecorded"

	// 502
	CodeYouTubeWriteFailed Code = "youtube_write_failed"
	CodeYouTubeReadFailed  Code = "youtube_read_failed"
	CodeYouTubeSignedOut   Code = "youtube_signed_out"

	// 503
	CodeIdentityProviderUnavailable Code = goapiauth.CodeIdentityProviderUnavailable
	CodeYouTubeQuotaSpent           Code = "youtube_quota_spent"
	CodeShuttingDown                Code = "shutting_down"
	CodeHistoryUnavailable          Code = "history_unavailable"
	CodeYouTubeRateLimited          Code = "youtube_rate_limited"
)

// Refusal is the body of every answer that refuses a request.
type Refusal struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Code    Code   `json:"code"`
}

func refusal(status int, code Code, format string, args ...any) Refusal {
	return Refusal{Error: http.StatusText(status), Message: fmt.Sprintf(format, args...), Code: code}
}

// JSON answers status with body encoded as JSON.
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Refuse answers status with a Refusal carrying code and the sentence format
// and args make.
func Refuse(w http.ResponseWriter, status int, code Code, format string, args ...any) {
	JSON(w, status, refusal(status, code, format, args...))
}

// VideoRefusal is the body of a refusal about particular videos, naming each.
type VideoRefusal struct {
	Refusal
	VideoIDs []string `json:"video_ids"`
}

// RefuseVideos answers status with a VideoRefusal carrying code, the sentence
// format and args make, and videoIDs.
func RefuseVideos(w http.ResponseWriter, status int, code Code, videoIDs []string, format string, args ...any) {
	JSON(w, status, VideoRefusal{Refusal: refusal(status, code, format, args...), VideoIDs: videoIDs})
}
