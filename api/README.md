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
is the channel's refresh token. All three are required. `YOUTUBE_COOKIE_KEY`, the base64 of 32
random bytes (`openssl rand -base64 32`), is what the watch-history cookie jar is sealed under in
the store. Without it the history routes answer 503, and a value that is not such a key stops the
server. The server listens on `PORT`, 8080 when unset. It keeps its store at `DATABASE_PATH`, or at
`ypl/api.db` under `$XDG_DATA_HOME` when that is unset. The image sets `DATABASE_PATH` to
`/var/db/ypl/ypl.db`, in a directory for the host to mount, since a store inside the container goes
with the container. It also sets `TZ` to UTC, the zone the log lines are written in.

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, the server exports a span for each API request over
OTLP/HTTP, and the other standard `OTEL_*` variables configure the exporter. A request carrying a
W3C `traceparent` gets its span as that caller's child, and every member of its `baggage` is copied
onto the span. Each span is named by its route, such as `GET /api/v1/playlists/{id}`, and carries
the reference the path names and the caller's client id. `/health` and `/ready` are never traced.
With the variable unset, the server installs no exporter.

One server syncs a store at a time. Before it opens the database, the server takes an exclusive
lock on `<DATABASE_PATH>.server.lock`, and holds it until it exits. A second server started on the
same store binds its port and answers `/health`, and `/ready` answers 503 naming the server holding
the lock. It holds every other request until the lock frees, then opens the store and answers them.
A SIGTERM while it waits answers each held request 503 `shutting_down`. It waits a minute past the
shutdown grace, then exits naming the holder. The lock is a `flock`, which keeps two servers apart
only on one machine, so the store's disk is mounted by one machine at a time.

The server edits playlists through the Data API as well. Creating, renaming or deleting a playlist,
and inserting, moving or deleting an item, costs 50 units a request. A rename reads the playlist
first, for 1 unit more.

The sync works in passes, and each pass works through a queue of jobs, earliest first:

| Priority | Job | What it reads |
| --- | --- | --- |
| 1 | Push a playlist an edit changed | That playlist |
| 2 | Probe: list every playlist, and sync each whose count moved | 1 unit, then each changed playlist |
| 3 | Finish a push that stopped before YouTube held the server's order | That playlist |
| 4 | Sweep: sync the playlist read longest ago | That playlist |
| 5 | Read the lengths of new videos | A unit per 50 videos |

The server makes a full pass as it starts, syncing every playlist whatever its count. It then
ticks every `SYNC_INTERVAL`, 5 minutes when unset and at least a minute, give or take a fifth at
random, and each tick queues the probe, the sweep, every unfinished push, and hourly the lengths.
An edit wakes a pass at once, and a push made for anything else stops at its next write when one
arrives, so the edit goes next and a later tick carries on. Syncing a playlist reads its items at a
unit a page, merges YouTube's order of it into the server's, and pushes the server's order back,
one write at a time.

The probe compares each playlist's count in the listing with the count the listing gave it before
its last read, rather than with the items that read returned, which YouTube may count differently.
A change that leaves the count alone, such as a reorder made on YouTube, reaches the server by the
sweep, which reads every playlist once in as many ticks as there are playlists. A sweep that finds
an addition or removal the probe did not flag counts it as a probe miss on the pass.

A merge compares each side with the base, what YouTube held after the server last read the
playlist with the server's writes since, item by item. An item YouTube removed, or one the server
removed, stays removed, and an item either side added stays. When YouTube moved an item it placed
itself among the items the base and its read share, YouTube's order wins, and otherwise the
server's does. An item the push wrote sits where the write landed, which an edit on YouTube during
the push can shift, so where it sits is no sign YouTube reordered anything. An item missing from a
read is read by id before it counts as removed, since a read spanning pages can miss one.

The push deletes what the server removed, keeps in place the longest run of items already in order,
and inserts or moves the rest. Before each write it asks whether the day's quota has room: what the
day has spent, the write, and the reads of every tick left before midnight Pacific at the listing
and one average playlist each. Anything but an edit's push also leaves a reserve of units
untouched, `EditReserve` in `api/store`, so a long push in the background leaves room for the
next edit. As the reset nears the ticks left fall,
so a push that stopped for the day comes back, and an unfinished push is not even read until the
quota has room for one of its writes. A push stops at a write that YouTube refuses or does not
answer, which a later pass accounts for. A video YouTube refuses to add leaves the server's order.
A write YouTube refuses for another reason is not sent again that Pacific day unless the playlist
changes, and each pass it waits is recorded as partly synced. A playlist YouTube orders itself,
which refuses a write naming a position, takes YouTube's order at each merge and is added to at its
end, until an edit of its order tries positions again. A playlist whose items were written less
than a minute before a pass read them is left for a later pass, and an edit's push waiting on that
is woken again once the minute is up.

The lengths job reads, through the Data API, the length of each video a playlist holds that it
holds no length for, the newest first, at a unit per 50. `lengthsPerPass` in `api/reconcile` caps
how many a pass reads. So a filter or an order
by length reaches a video before its tracklist is read. Once the library is measured, it reads the
videos new since the last and any YouTube reports no length for, which are live and upcoming
streams, and that is why it is hourly rather than every tick.

Beside the sync, the server reads a tracklist for each video the playlists hold that has none, the
newest in a playlist first, which `api/enrich` queues and paces. The Data API reports neither
chapters nor comments, so these reads go through `yt-dlp`, signed in as nobody, at `YTDLP_PATH` or
on `PATH`.

Every `yt-dlp` read the server makes passes through one door in `api/ytdlp`, which runs one process
at a time. The next read starts at least `YTDLP_GAP` after the last one ended, 8 seconds when unset,
plus up to `YTDLP_JITTER` more at random, also 8 seconds when unset. A person's read goes first,
then a service's read of captions or audio, then enrichment's. Reads of one kind go in the order they
arrived. A person's or a service's read that waits 2 minutes for its turn answers 503
`youtube_reads_busy`, with a `Retry-After`.

Inside one read, `yt-dlp` sleeps as `YTDLP_SLEEPS` says, written as `yt-dlp`'s own options:
`--sleep-requests`, `--sleep-subtitles`, `--sleep-interval` and `--max-sleep-interval`, in seconds.
Each one it leaves out keeps its default, which is 2 seconds between requests, 5 before each
subtitle, and 8 to 16 before each download. The server refuses to start on a value it cannot read,
and logs the gap, jitter and sleeps it uses.

Enrichment keeps its own pace on top of the door's. Its reads come at least `ENRICH_PACE` apart, 90
seconds when unset, and up to twice that at random, so they never arrive in a burst. The store keeps
when the latest read began, so a server that restarts waits out the pace from it before its first
read. Each pass records what the reads did since the pass before. `ENRICH_PACE=off` reads no
tracklist, and a server configured that way starts without `yt-dlp`. Without one, every read of a
video and the history answer 503 `youtube_reads_unavailable`.

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
every tracklist that way whenever it opens and whenever a read stores one, the clearest first, so
each exchange counts toward the next.

When YouTube refuses a read for its rate limit or its bot check, the door makes no read of any kind
for a day, and the next pass records the refusal. The door also pauses every read for an hour once
reads of 3 different videos fail in a row, whatever they failed with, since only a refusal worded
the way `api/ytdlp` spells it is recognized as one. A video failing again adds nothing, so a caller
asking for one unreadable video again and again pauses nobody else. A person's runs of `yt-dlp`
count apart, as the run route below says. YouTube's answer that a video is
a premiere or a live stream still to come is no failure, and that read answers 409
`video_not_started`. The store keeps each pause, so a server that restarts takes it up again. While a
pause holds, a read answers 503 `youtube_reads_paused` with a `Retry-After` of the seconds until it
ends, and so does the read YouTube refused. `GET /api/v1/status` answers the pause's end and its
cause under `youtube_reads`.

A read that stores no tracklist, and a read that fails, are both tried again 6 hours later, then 12,
doubling up to a week, for up to 6 attempts. A video YouTube will never let a signed-out read
return, because it is private, removed, members-only or age-restricted, is not read again at all.
`api/cmd/reset-enrichment` shows every video enrichment has stopped reading and puts them back.

The server answers `/api/v1` only to a request carrying an access token, which
[goapiauth](https://github.com/datapointchris/goapiauth) verifies. The token is an RFC 9068 JWT
that the identity provider `OIDC_ISSUER` signed. The server reads the provider beside the sync,
retrying while it is down. `/ready` answers 200 once it has and the store is open, and 503 with
what it waits on before then.
`/health` and `/ready` answer without a token.

So does `/.well-known/oauth-protected-resource`, the RFC 9728 metadata `ypl auth login <url>` reads
the provider from before it holds a token. It names `PUBLIC_URL` as the `resource` and
`OIDC_ISSUER` as the one authorization server. A CLI refuses the document unless `resource` is the
URL it fetched it for, so `PUBLIC_URL` is the address the edge serves the API at, exactly. The
server serves no metadata while `PUBLIC_URL` is unset, because only the deployment knows that
address.

The client id the token was issued to decides who it speaks for. An id starting with
`CLI_CLIENT_ID_PREFIX`, `ypl-cli-` when unset, is a person's CLI, and its token must name the person
in `sub`. An id starting with `SERVICE_CLIENT_ID_PREFIX`, `ypl-svc-` when unset, is a service that
authenticated with the client-credentials grant. Its token speaks for no person, and must carry its
scopes as a non-empty `scp` list. A token from any other client is refused with 401.

A service reaches only the routes its scopes list, and every other request it makes is refused with
403 and `outside_service_scope`. The refusal's sentence names the routes the client's scopes do
reach. The list is `ServiceRoutes` in `api/handlers/handlers.go`, and the wire test writes it to
`testdata/wire/service-scopes.json`, which the CLI's tests hold its requested scope against:

| Scope | Reaches |
| --- | --- |
| `ypl.status.read` | `GET /api/v1/status` |
| `ypl.version.read` | `GET /api/v1/version` |
| `ypl.playlists.read` | `GET /api/v1/playlists`, `GET /api/v1/youtube/playlists/{id}/items` |
| `ypl.playlist-items.write` | `POST /api/v1/youtube/playlists/{id}/items`, `DELETE /api/v1/youtube/playlists/{id}/items/{item}` |
| `ypl.channels.read` | `GET /api/v1/youtube/channels/{id}/uploads` |
| `ypl.videos.read` | `GET /api/v1/youtube/videos` |
| `ypl.comments.read` | `GET /api/v1/youtube/videos/{id}/comments` |
| `ypl.transcripts.read` | `GET /api/v1/youtube/videos/{id}/captions`, `GET /api/v1/youtube/videos/{id}/transcript` |
| `ypl.audio.read` | `GET /api/v1/youtube/videos/{id}/audio` |

Three routes belong to a person alone: `POST /api/v1/ytdlp/runs`, `POST /api/v1/ytdlp/leases` and
`PUT /api/v1/ytdlp/leases/{id}/outcome`. No scope lists them, so a service token sent to any of them
is refused with 403 and `outside_service_scope`.

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
| `POST /api/v1/ratings` | The rating it records, from the body `newRating` in `handlers/ratings.go` describes, as in `testdata/wire/ratings.json` |
| `GET /api/v1/ratings` | Ratings newest first, of one video under `video`, a page at a time |
| `DELETE /api/v1/ratings/{id}` | Nothing, once it has deleted the rating and its tags; `{id}` is the id or its last 8 or more characters |
| `GET /api/v1/tags` | Every tag a rating carried, with how many videos carry it, most first |
| `GET /api/v1/youtube/history` | The newest library videos in the account's watch history, up to `limit` |
| `PUT /api/v1/youtube/session` | The session it keeps, from `{"cookie_jar"}`, once a read of the watch history with it answers signed in |
| `GET /api/v1/youtube/playlists/{id}/items` | The playlist's items as YouTube holds them now, read live |
| `POST /api/v1/youtube/playlists/{id}/items` | The item it appends on YouTube, from `{"video_id"}` |
| `DELETE /api/v1/youtube/playlists/{id}/items/{item}` | Nothing, once YouTube has deleted the item |
| `GET /api/v1/youtube/channels/{id}/uploads` | A page of up to 50 of a channel's uploads, newest first, and the `next_page_token` that `page_token` takes for the next; `{id}` is a channel id or an `@handle` |
| `GET /api/v1/youtube/videos` | Each video `ids` names, up to 500, with its details, counts and `live_broadcast_content` |
| `GET /api/v1/youtube/videos/{id}/comments` | A video's top 100 comments by relevance, and whether its comments are off |
| `GET /api/v1/youtube/videos/{id}/captions` | A video's details, chapters, and the segments of the caption track the read picks |
| `GET /api/v1/youtube/videos/{id}/transcript` | The segments of the track in `lang`, `en` when absent, as one line of text |
| `GET /api/v1/youtube/videos/{id}/audio` | The video's best audio as a file |
| `POST /api/v1/ytdlp/runs` | One tar of what `yt-dlp` printed, the code it exited with and the files it wrote, from `{"args"}` |
| `POST /api/v1/ytdlp/leases` | `{"id", "held_until"}`, once the door has given a person's own read its turn |
| `PUT /api/v1/ytdlp/leases/{id}/outcome` | Nothing, once the door has read how the leased read ended, from `{"exit_code", "stderr"}` |
| `GET /api/v1/suggestions` | What to play next: never-played videos first, then the least recently played |
| `GET /api/v1/sync/runs` | Sync runs newest first with their failures, a page at a time |
| `GET /api/v1/status` | What the store holds, the latest run, the latest run that ended ok, the day's quota and its units by caller, each playlist waiting to be pushed, and any pause of the reads through `yt-dlp` |
| `GET /api/v1/version` | `{"version"}`: the release the server was built from, its image's `sha-<short>` tag, or `dev` for a build that named none |

A playlist is named by its YouTube id or by its title, wherever one is named — in the path, and in
the `playlist` parameter of videos and suggestions. Titles are matched as slugs: lowercased, with
every run of anything that is not a letter or a digit standing as one hyphen. So `deep house` and
`Deep / House!` both reach Deep House, and `deephouse` does not. A title matching the whole
of what was sent beats one merely holding it. A name matching two playlists is refused naming each,
rather than answered with one of them.

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

A rating is a `like`, a `dislike` or a `note` left on a mix, with a `message` and `tags`. A note
needs a message, and a like or a dislike may carry one. A mix holds every rating left on it, and
nothing is derived from them but counts. Tags are lowercased with their whitespace collapsed.
`GET /api/v1/videos` answers each video's `likes`, `dislikes` and `notes` counts and its `tags`, and
`GET /api/v1/videos/{id}` adds every rating as `ratings`, newest first. The list narrows by `rated`
(`like`, `dislike`, `note`, or `unrated` for none), `tag`, and `text`, which matches part of any
rating's message ignoring case and accents. A rating's `id` is a client-made UUIDv7, as a play's is.
`position_seconds` is where in the mix it was left, and null where nobody said;
`position_estimated` is true when it came from YouTube's resume point rather than from a person.
A position before the start or past the mix's known length answers 422 `invalid_position`, as
does `position_estimated` without a position.

The watch history is read through `yt-dlp` with a signed-in session, which is the one read made
signed in. Every player signed into the channel's account writes to that history, so the newest
library video in it is what is most likely playing, wherever it plays. The history says what was
started and in what order, never when. A read asks for the newest `historyDepth` entries and
answers those the library holds. Reads are made only when asked for, one at a time, and one read is
reused for `historyReuse`, both in `handlers/history.go`, since YouTube may suspend an account it
sees read through `yt-dlp` too often.

Each entry carries `resume_seconds`, where YouTube's resume point sits in the video, and null where
the page drew none, the video was watched to the end, or its length is unknown. YouTube draws it
as a whole percentage of the length, so it is approximate by up to a hundredth of the video, which
is nearly two minutes on a three-hour mix. As of 2026-10-24, YouTube saves it while the video
plays, so for the newest entry it is usually near where the player is. `ypl rate` offers it in its
form as the rating's position, marked approximate. It is read from the history page `yt-dlp` saves
with `--write-pages` during the same read, so it costs no second request.

The session is a Netscape cookie jar, kept in the store sealed with AES-256-GCM under
`YOUTUBE_COOKIE_KEY`, so a copy of the store carries no usable session. A jar sealed under another
key answers 409 `youtube_session_missing`, and is replaced by sending one again. YouTube rotates
the cookies of a session open in a browser, which invalidates an exported copy. So the jar is
exported from a private window that is then closed, and no browser opens the session again.
`ypl server cookies set --help` gives the steps.

`yt-dlp` writes the jar back after each read with whatever YouTube rotated, and the server keeps
that version, so the session lasts until YouTube ends it. A jar that reads the history signed out
is refused with 422 `cookie_jar_signed_out`, and the session already held is kept. A read refused
signed out later answers 502 `youtube_signed_out` and is recorded, and `GET /api/v1/status`
answers the session's `last_ok_ts` and `last_refused_ts` under `youtube_session`, so a refusal
later than the last ok read is the jar to export again. With no session, the history answers 409
`youtube_session_missing`, and a server with no `yt-dlp` answers 503 `youtube_reads_unavailable`.
While the door's pause holds, the history answers 503 `youtube_reads_paused` without reading,
because a read sent into a refusal can turn it into a block on the account.

A video's captions, transcript and audio are read through the same door, signed out, for a person
or for a service with the scope. The captions route picks one track. That is the manual track in
the video's language, else the manual English one, else the automatic track YouTube recognized from
the audio, filed as `<language>-orig`, else the automatic track in the video's language, else the
automatic English one. It answers the track's segments with their times, the video's chapters, and
its title, description and length. A video with no such track answers 200 with `track` null and no
segments.

The transcript route reads the track in `lang`, `en` when absent, and answers its segments as one
line. It takes a manual track in `lang`, else an automatic one, else the automatic track YouTube
recognized from audio in that language. Within each kind, `lang` itself goes first, then its base
language, then that language filed under a region, so `en` reads an `en-GB` track where no `en` is
filed. A video with none of those answers 404 `no_captions`, and a `lang` that is no language
answers 400 `invalid_parameter` before any read. The line is each piece of the track's text followed
by a space, with every line break read as a space and every run of spaces as one.

The audio route streams the video's best audio from a file the read leaves, removed once the answer
is sent. A download missing a piece fails the read rather than answering a shorter file. A request
whose `Host` is `PUBLIC_URL`'s host gets at most 100 MB, and longer audio answers 422
`answer_too_large` before any of it is fetched. A request that reaches the server by any other name,
such as a caller on the same network, gets the whole file. An audio read holds the door for as long
as the download takes, so reads waiting behind it are the ones that turn busy.

`POST /api/v1/ytdlp/runs` runs a person's own `yt-dlp` command line through the door, so a desk
gets what `yt-dlp` would give it at the door's pace. The body's `args` are the arguments after
`yt-dlp`. The server reads them as `yt-dlp` does, abbreviations aside, and passes only the options
`passedOptions` lists in `api/ytdlp/run_arguments.go`. Every other argument answers 422
`ytdlp_argument_refused`, with the argument as `field` and why in the message, and `yt-dlp` never
runs. `refusedOptions` in the same file gives the reason for each option it knows to refuse: it
signs in, runs or reads something on the server, sends requests somewhere other than YouTube, reads
faster than the door's pace, or hides YouTube's refusal from the door. An option on neither list is
refused as not yet weighed. These are refused as well:

- An output template that is absolute, starts with `~`, names a `$` variable, or holds `..`.
- An abbreviated option, and an input that is not a YouTube address, a video id or a `ytsearch:`.

`yt-dlp` is handed each option in its one long spelling, then `--ignore-config`,
`--no-plugin-dirs`, the door's sleeps, `--use-extractors=youtube.*` and `--color=never`, then the
inputs after `--`. Where the person asked for a longer sleep than the door's, theirs is kept. Every
error line reaches the door plain, whatever color the person asked for. The run writes in a
directory of its own, which is removed once the answer is sent. The answer is one
`application/x-tar` holding `stdout`, `stderr`, `exit-code`, each file under `files/`, then `end`,
as `testdata/wire/ytdlp-run.json` lists. An answer that stops before `end` was cut short, and may be
missing a file. A non-zero exit is still a 200, since the code is part of the answer.

YouTube refusing the address stops the run at once, pauses the door as any read does, and answers
503 `youtube_reads_paused`. A run's other failures count apart from the door's own reads, since a
run can fail on the options it was given. Once runs of 3 different inputs fail in a row, runs alone
pause for an hour, and the server's own reads and every lease go on. That pause is kept in memory,
so a restart lets runs through again. A run stops once the disk it writes to has less than
`ytdlp.DiskFloor` free, since the store may share that disk, and answers 409 `disk_low`. A run holds
the door for as long as it runs, up to `ytdlp.RunLimit`.

Through `PUBLIC_URL`'s host, a run meets the proxy's limits as every read does. `yt-dlp` is handed
`--max-filesize` of 100 MB, and a larger answer is refused with 422 `answer_too_large`. The run, its
wait at the door included, ends at 90 seconds and answers 422 `run_past_public_budget`. Sending it
again would read YouTube from the start. A request through the server's own address meets neither
limit. A URL that a run prints, such as a stream's, is bound to the server's address, so it plays
nowhere else.

`POST /api/v1/ytdlp/leases` gives a desk's own `yt-dlp` a turn at the door without running it on the
server. It waits as a person's read waits, answers 201 with the lease's `id` and when the turn ends,
and holds the door for 15 seconds. While the door's pause holds, it answers 503
`youtube_reads_paused` as a read does. `PUT /api/v1/ytdlp/leases/{id}/outcome` takes how the read
ended, as `{"exit_code", "stderr"}`, and lets the door go at once. The server reads the tail of
`stderr` as it reads its own reads. Where YouTube refused the address, the door pauses for every
read, even once the hold has ended. Any other end counts as neither a success nor a failure, since
the read was made elsewhere, with options the server never saw. `ypl play` takes a lease before mpv
resolves each stream, and tells the door how each failed resolve ended.

A proxy in front of a public address drops a request whose answer has not begun within its read
timeout. So a caption, transcript or audio read for a request through `PUBLIC_URL`'s host takes at
most 90 seconds, its wait at the door included. Past that it answers 503 `youtube_reads_busy` with
a `Retry-After`, and audio that takes longer to fetch is read through the server's own address.

The routes under `/api/v1/youtube/` that are not the history, the session or a video read call the
Data API live, rather than answering from the store. They exist for services, and a person's token
reaches them too. Each call runs one at a time and is charged to the client that received the
token, which `GET /api/v1/status` lists under `sync.quota.by_caller` beside `ypl`, the server's own
sync. A call costs what the Data API charges for it: 1 unit a page of reads, and 50 for an append or
a delete. The channel's uploads cost 1 unit for the channel the first time the running server reads
that channel, then 1 a page. Before each call the server asks the same question a push does, with
the same reserve left untouched. A call the day cannot afford answers 503 `youtube_quota_spent` with
a `Retry-After` of the seconds until midnight Pacific, and nothing is sent. The sync counts every
such call against the day's quota.

A call waits up to 20 seconds for the one ahead of it, then answers 503 `youtube_data_api_busy`
with a `Retry-After`. A caller that leaves while its call waits has nothing sent. YouTube limiting
how fast the server calls it answers 503 `youtube_rate_limited` with a `Retry-After`. YouTube
refusing the server's own credential or its Cloud project answers 502, and the server logs it as an
error. Any other refusal is of what the call asked, and answers 422 `youtube_refused`.

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
a sync pass keeps what the API wrote until its reads follow the write by that minute.

`GET /api/v1/playlists/{id}/items` answers the server's order of a playlist as
`{"video_ids": [...]}`, with an `ETag` that changes whenever the order of the videos does, a sync's
merge included. `PUT /api/v1/playlists/{id}/items` takes the whole new order in the same shape,
repeats allowed, up to the 5,000 videos a YouTube playlist holds. It needs `If-Match` holding that
`ETag`, or `*`: 428 `precondition_required` without it, 400 `invalid_precondition` for a field that
is not a list of entity tags, and 412 `precondition_failed` once the order has moved on. An order
that changes the videos wakes the sync, which pushes it ahead of anything else it has waiting.

Each video takes the earliest slot holding it that no earlier video took, keeping the YouTube item
in it, and a video no slot is left for takes a new one, whose `id` in `GET /api/v1/playlists/{id}`
is null until the sync adds it to YouTube. A video the server has never seen is
read from YouTube. An order adding a video YouTube returns no public or unlisted video for, or a new
slot for a video the server knows is private or deleted, answers 422 `video_unavailable` naming each
such video in `video_ids`, as 422 `invalid_video_id` names each malformed id.

On SIGTERM the server begins no new playlist write, no Data API call and no read through `yt-dlp`,
answering 503 `shutting_down`. It turns away every read still waiting at the door, and stops a
caption, transcript or audio read under way, which answers 503 `shutting_down` as well. It gives
requests in flight up to 75 seconds to finish. A history read begun before the signal runs on,
because cutting it short loses the cookies YouTube rotated during it. A container's stop timeout has
to be longer.

A paged list answers `{"data": [...], "has_more": true}`. The next page is the same request with
`starting_after` set to the last id on this one. `limit` sets the page size, at most 100. When it is
absent, videos and playlists answer the whole list as one page, and plays and sync runs answer 20. A
`starting_after` naming no row of the list is refused, since the list has changed since the page
before.

A refused request answers `{"error": "<status text>", "message": "<sentence>", "code": "<code>"}`.
The sentence is for a person and can change. The code is for a client to branch on, and `api/wire`
lists every one.

A body holding a field its route does not take is refused before anything is written, with 422
`unknown_field` and the field's name in `field`. Any other body that is not one JSON value of the
route's shape answers 400 `invalid_body`.
