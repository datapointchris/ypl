// Package wire writes the JSON bodies the API answers with, and names every
// refusal it can make.
//
// A refusal is {"error": "<sentence>", "code": "<code>"}. The sentence is for a
// person and may change. The code is for a client to branch on and does not.
package wire

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Code names one refusal.
type Code string

// Every refusal the API makes, by the status it answers with.
const (
	// 400
	CodeInvalidLimit       Code = "invalid_limit"
	CodeInvalidParameter   Code = "invalid_parameter"
	CodeUnknownReference   Code = "unknown_reference"
	CodeAmbiguousReference Code = "ambiguous_reference"
	CodeInvalidBody        Code = "invalid_body"

	// 401
	CodeMissingToken Code = "missing_token"
	CodeInvalidToken Code = "invalid_token"

	// 404
	CodeNotFound      Code = "not_found"
	CodeRouteNotFound Code = "route_not_found"

	// 405
	CodeMethodNotAllowed Code = "method_not_allowed"

	// 409
	CodePlayConflict Code = "play_conflict"

	// 422
	CodeTitleRequired       Code = "title_required"
	CodeNoChanges           Code = "no_changes"
	CodeInvalidPlayID       Code = "invalid_play_id"
	CodeVideoIDRequired     Code = "video_id_required"
	CodeVideoNotStored      Code = "video_not_stored"
	CodeInvalidPlayedTs     Code = "invalid_played_ts"
	CodePlayedTsOutOfRange  Code = "played_ts_out_of_range"
	CodePlayedTsInTheFuture Code = "played_ts_in_the_future"

	// 500
	CodeInternal Code = "internal"

	// 502
	CodeYouTubeWriteFailed Code = "youtube_write_failed"

	// 503
	CodeIdentityProviderUnavailable Code = "identity_provider_unavailable"
	CodeYouTubeQuotaSpent           Code = "youtube_quota_spent"
)

// Refusal is the body of every answer that refuses a request.
type Refusal struct {
	Error string `json:"error"`
	Code  Code   `json:"code"`
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
	JSON(w, status, Refusal{Error: fmt.Sprintf(format, args...), Code: code})
}
