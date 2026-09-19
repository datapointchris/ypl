# The ypl server

The server keeps the channel's playlists mirrored, reads a tracklist for each mix, and answers the
HTTP API the CLI speaks. The repository's own [README](../README.md) says what the other parts are.

The server reads playlists through the Data API rather than through `yt-dlp`. It has no browser to
read cookies from, and a private playlist read through `yt-dlp` needs a signed-in session. The Data API signs in with an OAuth refresh token, and `api/youtube`'s package
documentation says how to get one. Each request is a page of up to 50 playlists or 50 items and
costs 1 unit, so reading every playlist costs one unit per page of playlists plus one per page of
each playlist's items, with an empty playlist still taking one.

The server reads its settings from the environment, and refuses to start on one it cannot use.
`YOUTUBE_CLIENT_ID` and `YOUTUBE_CLIENT_SECRET` name the OAuth client, and `YOUTUBE_REFRESH_TOKEN`
is the channel's refresh token. All three are required. The server listens on `PORT`, 8080 when
unset. It keeps its store at `DATABASE_PATH`, or at `ypl/api.db` under `$XDG_DATA_HOME` when that
is unset. The image sets `DATABASE_PATH` to `/var/db/ypl/ypl.db`, in a directory for the host to
mount, since a store inside the container goes with the container. It also sets `TZ` to UTC, the
zone the log lines are written in.

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

A run then reads, through the Data API, the length of each video a playlist holds that it holds no
length for, the newest first, up to 1,000 a run at a unit per 50. So a filter or an order by length
reaches a video before its tracklist is read. Once the library is measured, a run reads the videos
new since the last and any YouTube reports no length for, which are live and upcoming streams.

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
from the description the same way it reports chapters YouTube marked. Each track keeps the line it
was read from, and the store derives every track's artist and title from that line again each time
it opens, so a parser that learns a shape of line corrects the tracks read before it.

A line says which of its two sides is the artist only by where it puts it, and some tracklists put
the title first. So a tracklist is read the other way round when at least 4 of its titles name an
artist another tracklist credits, and at least twice as many as its artists do. A line in such a
tracklist whose artist is a known one and whose title is not stays as written. The store judges
every tracklist that way whenever it opens and whenever a run stores one, the clearest first, so
each exchange counts toward the next.

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
| `GET /api/v1/playlists` | Playlists by title, with how many items each holds and how many of their videos are unavailable or enriched, a page at a time |
| `POST /api/v1/playlists` | The private playlist it creates on YouTube, from `{"title", "description"}` |
| `GET /api/v1/playlists/{id}` | One playlist and its items in order |
| `PATCH /api/v1/playlists/{id}` | The playlist with the `title` or `description` it sets on YouTube |
| `DELETE /api/v1/playlists/{id}` | Nothing, once it has deleted the playlist on YouTube and from the store |
| `GET /api/v1/playlists/{id}/items` | The playlist's order as `{"video_ids"}`, with its `ETag` |
| `PUT /api/v1/playlists/{id}/items` | The order it sets, from `{"video_ids"}`, when `If-Match` matches the order's `ETag` |
| `GET /api/v1/videos` | The available videos some playlist holds, with their artists and playlists, a page at a time |
| `GET /api/v1/videos/{id}` | One video with its description and tracklist |
| `POST /api/v1/plays` | The play it records, from `{"id", "video_id", "played_ts"}` |
| `GET /api/v1/plays` | Plays newest first, a page at a time |
| `GET /api/v1/plays/{id}` | One play |
| `DELETE /api/v1/plays/{id}` | Nothing, once it has deleted the play |
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

`GET /api/v1/videos/{id}` names a video the way a read names a playlist, through the same resolver:
by its id, its title, or part of its title. A name matching several is refused naming the first 10,
for a video or a playlist alike, since part of a title can match hundreds of mixes.

`GET /api/v1/videos` narrows by `playlist`, `min_seconds`, `max_seconds` and `artist`, which
matches part of an artist's name ignoring case and accents. `sort` is one of `longest`, `shortest`,
`newest`, `oldest`, `title` or `random`. The first is the order when `sort` is absent. A random
order comes out new on every request, so it answers one page, of `limit` videos or every one, and
has no next.
`GET /api/v1/suggestions` takes `playlist`, and a `limit` of up to 100 that is one when absent.

A play's `id` is a UUIDv7 the client generates, written lowercase with hyphens, so sending the
same play again records it once. The server gives each play a `handle`, a short number.
`GET /api/v1/plays/{id}`, `DELETE /api/v1/plays/{id}` and `starting_after` take the id, the
handle, or the id's last eight characters. `played_ts` is an RFC 3339 timestamp, stored in UTC to
the second, and the time the request arrives when it is absent. A time more than five minutes after
the request arrives is refused.

Deleting is the one correction a stored play takes. A deleted play's handle is never given to
another play, so a handle typed from an old listing finds nothing rather than a different listen.
A deleted play answers as deleted by every name it had. `GET /api/v1/plays/{id}` answers 410 with
`play_deleted`. `DELETE` answers 204 again, since the play is gone as asked. Its id sent to
`POST /api/v1/plays` again answers 410 with `play_deleted`, so a retry arriving after the delete
cannot store the play a second time. A name no play ever had answers 404.

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
`starting_after` set to the last id on this one. `limit` sets the page size, at most 100. When it is
absent, videos and playlists answer the whole list as one page, and plays and sync runs answer 20. A
`starting_after` naming no row of the list is refused, since the list has changed since the page
before.

A refused request answers `{"error": "<sentence>", "code": "<code>"}`. The sentence is for a person
and can change. The code is for a client to branch on, and `api/wire` lists every one.
