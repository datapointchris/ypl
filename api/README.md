# The ypl server

The server keeps the channel's playlists mirrored, reads a tracklist for each mix, and answers the
HTTP API the CLI speaks. The repository's own [README](../README.md) says what the other parts are.

The server reads playlists through the Data API, where the Python tool reads them with `yt-dlp`. It
has no browser to read cookies from, and a private playlist read through `yt-dlp` needs a
signed-in session. The Data API signs in with an OAuth refresh token, and `api/youtube`'s package
documentation says how to get one. Each request is a page of up to 50 playlists or 50 items and
costs 1 unit, so reading every playlist costs one unit per page of playlists plus one per page of
each playlist's items, with an empty playlist still taking one.

The server edits playlists through the Data API as well. Creating, renaming or deleting a playlist,
and inserting, moving or deleting an item, costs 50 units a request. A rename reads the playlist
first, for 1 unit more.

The server waits `SYNC_INTERVAL`, an hour when unset, between one run ending and the next
beginning, so a run's own length is added to that. Each run reads every playlist at a unit a page
and merges YouTube's order of it into the server's, then pushes the server's order back to YouTube,
one write at a time, and ends by reading tracklists. A run whose interval would read more in a day
than the quota allows is recorded as partly synced.

A merge compares each side with the base, what YouTube held after the server last read the
playlist with the server's writes since, item by item. An item YouTube removed, or one the server
removed, stays removed, and an item either side added stays. When YouTube moved an item it placed
itself among the items the base and its read share, YouTube's order wins, and otherwise the
server's does. An item the push wrote sits where the write landed, which an edit on YouTube during
the push can shift, so where it sits is no sign YouTube reordered anything. An item missing from a
read is read by id before it counts as removed, since a read spanning pages can miss one.

The push deletes what the server removed, keeps in place the longest run of items already in order,
and inserts or moves the rest. It stops for the day when a write would leave too little of the
quota for the reads of the day's remaining runs, and it stops pushing a playlist at a write that
YouTube refuses or does not answer, which the next run accounts for. A video YouTube refuses to add
leaves the server's order. A write YouTube refuses for another reason is not sent again that
Pacific day unless the playlist changes, and each run it waits is recorded as partly synced. A
playlist YouTube orders itself, which refuses a write naming a position, takes YouTube's order at
each merge and is added to at its end, until an edit of its order tries positions again. A
playlist whose items were written less than a minute before a run read them is left for the next
run.

Each run then reads a tracklist for each video the playlists hold that has none, the newest in a
playlist first, which `api/enrich` queues and paces. The Data API reports neither chapters nor
comments, so these reads go through `yt-dlp`, signed in as nobody, at `YTDLP_PATH` or on `PATH`;
`api/ytdlp` runs it, one process a read. A run reads at most `ENRICH_VIDEOS_PER_RUN` videos, 30
when unset, `ENRICH_PACE` apart, 10 seconds when unset, or up to half as long again. Those reads
happen inside the run, so they are also half the wait between two syncs: a pace and a count whose
reads cannot finish in half of `SYNC_INTERVAL` are refused at startup, and a run whose reads reach
that budget stops. A server configured to read no video needs no `yt-dlp` and starts without one.

`api/tracklist` is what reads a tracklist out of a video. It is the video's chapters, or failing
those the timestamped lines of its description, or failing that the first of its top 20 comments
holding at least 3 timestamped lines that run forward and reach at least half way through the
video. Fewer than 3 chapters are read as no chapters, since `yt-dlp` reports chapters it derived
from the description the same way it reports chapters YouTube marked.

When YouTube refuses a read for its rate limit or its bot check, the run stops reading and records
it, and no run reads for a day after. A run also stops once 3 reads in a row fail, whatever they
failed with, since only a refusal worded the way `api/ytdlp` spells it is recognized as one. A read
that stores no tracklist, and a read that fails, are both tried again 6 hours later, then 12,
doubling up to a week, for up to 6 attempts. A video YouTube will never let a signed-out read
return, because it is private, removed, members-only or age-restricted, is not read again at all.
`api/cmd/reset-enrichment` shows every video enrichment has stopped reading and puts them back.

The server answers `/api/v1` only to a request carrying an access token, which `api/auth`
verifies. The token is an RFC 9068 JWT that the identity provider `OIDC_ISSUER` signed for a client
whose id starts with `CLI_CLIENT_ID_PREFIX`, which is `ypl-cli-` when unset. The server reads the
provider beside the sync, retrying while it is down, and `/ready` answers 200 once it has.
`/health` and `/ready` answer without a token.

| Request | Answers with |
| --- | --- |
| `GET /api/v1/playlists` | Every playlist, with how many items it holds and how many of their videos are unavailable or enriched |
| `POST /api/v1/playlists` | The private playlist it creates on YouTube, from `{"title", "description"}` |
| `GET /api/v1/playlists/{id}` | One playlist and its items in order |
| `PATCH /api/v1/playlists/{id}` | The playlist with the `title` or `description` it sets on YouTube |
| `DELETE /api/v1/playlists/{id}` | Nothing, once it has deleted the playlist on YouTube and from the store |
| `GET /api/v1/playlists/{id}/items` | The playlist's order as `{"video_ids"}`, with its `ETag` |
| `PUT /api/v1/playlists/{id}/items` | The order it sets, from `{"video_ids"}`, when `If-Match` matches the order's `ETag` |
| `GET /api/v1/videos` | Every available video some playlist holds, with its artists and playlists |
| `GET /api/v1/videos/{id}` | One video with its description and tracklist |
| `POST /api/v1/plays` | The play it records, from `{"id", "video_id", "played_ts"}` |
| `GET /api/v1/plays` | Plays newest first, a page at a time |
| `GET /api/v1/plays/{id}` | One play |
| `GET /api/v1/suggestions` | What to play next: never-played videos first, then the least recently played |
| `GET /api/v1/sync/runs` | Sync runs newest first with their failures, a page at a time |
| `GET /api/v1/status` | What the store holds, the latest run, and the latest run that ended ok |

A playlist is named by its YouTube id or by its title, wherever one is named — in the path, and in
the `playlist` parameter of videos and suggestions. Titles are matched with their case, spacing and
punctuation removed, so `deep house` and `Deep / House!` both reach the same playlist, and a title
matching the whole of what was sent beats one merely holding it. A name matching two playlists is
refused naming each, rather than answered with one of them.

Part of a title reaches a playlist, its videos and its suggestions. It reaches nothing else: a
rename, a delete, and both the read and the write of an order take the whole title or the id. The
order is read as narrowly as it is written because that read is what an edit is made from, and a
reference the write would refuse buys an editing session that is then thrown away.

`GET /api/v1/videos` narrows by `playlist`, `min_seconds`, `max_seconds` and `artist`, which
matches part of an artist's name ignoring case and accents. `sort` is one of `longest`, `shortest`,
`newest`, `oldest`, `title` or `random`. The first is the order when `sort` is absent.
`GET /api/v1/suggestions` takes `playlist`, and a `limit` of up to 100 that is one when absent.

A play's `id` is a UUIDv7 the client generates, written lowercase with hyphens, so sending the
same play again records it once. The server gives each play a `handle`, a short number.
`GET /api/v1/plays/{id}` and `starting_after` take the id, the handle, or the id's last eight
characters. `played_ts` is an RFC 3339 timestamp, stored in UTC to the second, and the time the
request arrives when it is absent. A time more than five minutes after the request arrives is
refused.

Creating, renaming and deleting a playlist write to YouTube in the request, one request at a time.
The server records each write before sending it and settles it with YouTube's answer, and the store
changes only once YouTube has answered. The store keeps a title and description as YouTube's answer
reports them, and YouTube trims the whitespace around both. A rename sets the field it leaves out
as YouTube holds it, read just before the write.

YouTube keeps a title of at most 150 characters and a description of at most 5,000, and a longer
one is refused before it is sent. A write YouTube refuses answers 422 `youtube_refused` with
YouTube's reason, and nothing changed. A write YouTube did not answer answers 502 and may still
have landed, and the next sync stores what YouTube holds. A write YouTube made that the server
could not record answers 500 `youtube_write_unrecorded`, and sending it again would make it twice.
A write refused because the day's quota is spent answers 503 with a `Retry-After` of the seconds
until midnight Pacific.

A read sent within a minute of a write YouTube answered can show the playlist as it was before, so
a sync run keeps what the API wrote until its reads follow the write by that minute.

`GET /api/v1/playlists/{id}/items` answers the server's order of a playlist as
`{"video_ids": [...]}`, with an `ETag` that changes whenever the order of the videos does, a sync's
merge included. `PUT /api/v1/playlists/{id}/items` takes the whole new order in the same shape,
repeats allowed, up to the 5,000 videos a YouTube playlist holds. It needs `If-Match` holding that
`ETag`, or `*`: 428 `precondition_required` without it, 400 `invalid_precondition` for a field that
is not a list of entity tags, and 412 `precondition_failed` once the order has moved on.

Each video takes the earliest slot holding it that no earlier video took, keeping the YouTube item
in it, and a video no slot is left for takes a new one, whose `id` in `GET /api/v1/playlists/{id}`
is null until the sync adds it to YouTube on its next run. A video the server has never seen is
read from YouTube. An order adding a video YouTube returns no public or unlisted video for, or a new
slot for a video the server knows is private or deleted, answers 422 `video_unavailable` naming each
such video in `video_ids`, as 422 `invalid_video_id` names each malformed id.

On SIGTERM the server begins no new playlist write, answering 503 `shutting_down`, and gives
requests in flight up to 30 seconds to finish. A container's stop timeout has to be longer.

A paged list answers `{"data": [...], "has_more": true}`. The next page is the same request with
`starting_after` set to the last id on this one. `limit` sets the page size, 20 when absent and at
most 100.

A refused request answers `{"error": "<sentence>", "code": "<code>"}`. The sentence is for a person
and can change. The code is for a client to branch on, and `api/wire` lists every one.
