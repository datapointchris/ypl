# ypl

Organize YouTube playlists of long DJ mixes. `README.md` says what the parts are, and
`api/README.md` how the server and its sync work; this file covers what someone changing the code
has to hold in mind.

## Every way out of a process is named

Everything either program reaches outside itself goes through one package, and nothing else opens
that kind of connection. An undocumented boundary is one a later change walks straight past, so
each is written here with what it is allowed to do.

The server, on the machine it is deployed to:

| Door | Package | Reaches | Costs |
| --- | --- | --- | --- |
| The Data API | `api/youtube` | YouTube, over HTTPS, signed in as the channel with an OAuth refresh token: for the sync, and live for each `/api/v1/youtube/` route that calls it | Quota units, 10,000 a day, each route's call charged to the client that made it |
| yt-dlp | `api/ytdlp` | YouTube, over HTTPS, through a subprocess, one at a time through the door: signed in as nobody for a video's tracklist, captions, transcript or audio and for a person's own command line, and with the stored cookie jar for the watch history | Nothing, the address's standing with YouTube, and the account's for history reads |
| The trace exporter | `api/tracing` | The OTLP/HTTP endpoint `OTEL_EXPORTER_OTLP_ENDPOINT` names, only where it is set | Nothing |

A request's span is what crosses to the trace exporter. It holds the method, the route, the status,
the reference the path names (a playlist's title or slug, a video's id), the caller's subject and
client id, and every baggage member the request carried. No token, no body and no other part of the
store crosses. The server's own calls to YouTube start no span and send no trace header, so a
caller's trace and baggage stop at the server.

The CLI, on every workstation:

| Door | Package | Reaches | Costs |
| --- | --- | --- | --- |
| The ypl server | `cli/internal/api`; `goclilogin` for the metadata | The configured server, at each address `api_tried_first` names in order and then `api`'s, over HTTPS or the HTTP an address names: the API with a bearer token and the trace and baggage `TRACEPARENT`, `TRACESTATE` and `BAGGAGE` handed the CLI, and its RFC 9728 metadata with none, during `ypl auth login <url>` and for an `--api` other than the file's | Nothing |
| The identity provider | `goclilogin` | The configured issuer, for discovery and the device grant, or the client-credentials grant where `YPL_CLIENT_SECRET` is set | Nothing, and it is reached on every command |
| A token command | `goclilogin` | The argv the config file's `token_command` names, once its last token has expired, in place of the identity provider | A subprocess |
| This machine's keychain | `goclilogin` | The OS keychain, or a mode-600 file where there is none | The refresh token lives there |
| A browser | `pkg/browser` | Whatever `xdg-open` or `open` resolves to, once, during `ypl auth login` | A subprocess |
| An editor | `cli/internal/editbuffer` | Whatever `$VISUAL` or `$EDITOR` names, holding the terminal, during `ypl playlists edit` | A subprocess, and the terminal until it exits |
| mpv | `cli/internal/mpv` | mpv on `PATH`, holding the terminal, during `ypl play`; its IPC socket at `$XDG_STATE_HOME/ypl/mpv.sock` during `ypl play` itself and `ypl now`; and a SIGTERM from the stand-in once the server's door refuses a turn | A subprocess, the terminal until it exits, and whatever mpv itself reaches |
| fzf | `cli/internal/cli` | fzf on `PATH`, drawing on the terminal, during `ypl rate` with no video named | A subprocess, and the terminal until a mix is picked |
| yt-dlp | `cli/internal/cli` | The first yt-dlp on `PATH` that is neither this binary nor a script running `ypl ytdlp`, started by the stand-in each time mpv resolves a stream during `ypl play` | A subprocess of mpv's, whatever yt-dlp itself reaches, and this machine's standing with YouTube |

mpv makes its own network requests, as yt-dlp does. What crosses to it is the watch URLs, the
socket path, `--no-video` under `--audio`, the stand-in's path, `try_ytdl_first`, and whatever
`--mpv` was given. Its environment adds the real yt-dlp's path and any `--api`. No credential, no token and no part of the
store crosses. mpv opens the socket itself, from the flag `ypl play` passes it. `ypl play`
reads it while mpv runs, which is how `cli/internal/cli/listen.go` records a play, and `ypl now`
reads it when asked. The only thing any of them writes there is `get_property`,
which is how mpv's IPC is asked anything and changes nothing about what is playing. A play is
posted through the server door while mpv is still running, under an id made once, so a retry is
stored once.

The stand-in is this binary, linked as `mpv-yt-dlp` beside the socket, and handed to mpv's
ytdl_hook as `ytdl_path`. This machine and the server often reach YouTube from one address, so mpv
resolves no stream before the server's door has leased it a turn. mpv is told to resolve before it
opens a URL itself, since its own open of a watch URL is a request to YouTube outside the door.
`ypl play` takes the first stream's turn before mpv starts, and leaves it in a file that the first
resolve removes. Each later resolve takes its own. The stand-in then runs the real yt-dlp with
mpv's arguments. It passes over a yt-dlp that runs `ypl ytdlp`, because the server's address is
the only one a stream that the server resolved plays at.

A resolve that fails sends the server its exit code and the tail of its stderr, so a refusal of the
address that this machine met pauses the server's reads too. Where the door is paused, or busy
through every attempt, the stand-in leaves the reason in `mpv-stopped` beside the socket and sends
mpv SIGTERM. A failed resolve would send mpv to open the watch URL itself and on to the next one.
`ypl play` says the reason once mpv has gone. Where the server cannot be reached or is restarting,
the resolve goes ahead with no turn, as every resolve did before the door.

In the server, only `api/ytdlp` starts a process, and anything that needs more of what yt-dlp
knows extends that package rather than running the binary somewhere else. In the CLI, these start
one: `ypl auth login`, `ypl playlists edit`, `ypl play`, `ypl rate` and the stand-in. Each child
gets the streams its interface needs. The browser launcher is given `os.DevNull`, because a pipe would keep
the login blocked until the browser exits. The editor and mpv are given this process's own
streams, because each draws an interface and takes keys, and a captured stream turns that
interface into a string. fzf, which `ypl rate` picks a mix with, draws on the terminal itself, so
its stdin carries the rows and its stdout the one chosen. The stand-in gives yt-dlp mpv's own
streams, since mpv reads the stream's address from yt-dlp's stdout.

The CLI is given no credential of the server's and no part of the store. What it holds is a token
for one machine, revocable on its own without touching any other. A service running the CLI holds
its own client's secret instead, in its environment, and a token that lives only for the run.

## Why yt-dlp is a dependency, and what it is allowed to reach

The Data API reports neither a video's chapters nor its comments, under any part or field
combination, and the tracklist of a mix is in one or the other. So reading tracklists means driving
yt-dlp. Nothing safer gives the same answer: there is no API to ask, and parsing the watch page
directly would be the same scraping with none of yt-dlp's maintenance behind it.

It is a third-party binary the server executes, and it makes its own network requests. A read is
therefore treated as untrusted input and given as little of the host as possible:

- **`--ignore-config` and `--no-plugin-dirs`.** These are separate settings, and either one left
  on would let whatever is installed beside yt-dlp change what a read does. A plugin can replace
  the extractor outright.
- **`--skip-download` and `--dump-json` for a tracklist.** A tracklist read wants one JSON
  document, so nothing is written to disk and no media is fetched. A caption read writes the one
  track it picks, and an audio read the one file, each to a directory removed when the answer ends.
- **One process at a time, through the door.** `api/ytdlp.Door` starts the next read at least
  `YTDLP_GAP`, plus up to `YTDLP_JITTER` at random, after the last ended: a person's first, then
  a service's, then enrichment's. Inside a read, the sleeps `YTDLP_SLEEPS` gives pace yt-dlp's own
  requests, passed by `paced()`. A lease holds the door for `LeaseHold` while a desk's own yt-dlp
  reads, or until the desk tells the door how the read ended. One refusal of the address pauses
  every class for `RefusedPause`, a desk's included, and failures of `MaxFailuresInARow` different
  videos in a row pause it for `FailingPause`. A person's runs keep that count apart, and pause runs
  alone. The store keeps the door's pause, so a restart does not reopen the door. `Close` stops every read under way but the
  history's, because cutting that one short loses the cookies rotated during it.
- **yt-dlp starts deno.** yt-dlp's release executable bundles yt-dlp-ejs. That script answers
  YouTube's player challenges by running YouTube's own script in deno. The image pins yt-dlp and deno, and
  its build check fails on any other version of either, or of yt-dlp-ejs.
- **Each process is bounded by the caller's context, with `WaitDelay` set.** A process yt-dlp
  itself starts can hold the output pipe open after yt-dlp is gone, so the deadline alone does not
  end a read.
- **Only the fields a read keeps are decoded.** `api/ytdlp.info` names them; everything else in
  yt-dlp's answer is dropped rather than carried around.

If yt-dlp egressed something it should not during a video read, what it holds is the video ids the
server is reading and the address it reads from. A video read is given no credential, no cookie and
no part of the store.

A person's run, `POST /api/v1/ytdlp/runs`, hands yt-dlp a command line that the person wrote, so
`api/ytdlp.ReadRunArguments` reads it as yt-dlp would before anything runs. It passes only the
options `passedOptions` lists, each in its one long spelling. It refuses the rest with the reason
`refusedOptions` gives, or as not weighed. It also refuses an output template that leaves the run's
directory, an abbreviation, and an input that is not YouTube's. yt-dlp is then handed `paced()`
with the door's sleeps, or the person's where those are longer, then `runOnly`, then the inputs
after `--`. `runOnly` holds `--color=never`, because the door reads plain `ERROR:` lines for
YouTube's refusal. `--ignore-no-formats-error` is refused for the same reason, since it turns that
refusal into a warning. A run stops once its disk has less than `DiskFloor` free. The run writes in a directory
of its own, removed once the answer is sent, and is given no credential, cookie or part of the
store. Adding an option to `passedOptions` is a judgment that it reaches nothing on the server,
and `TestEveryPassedOptionIsOneTheRealYtdlpTakesWithItsValue` holds the list to the installed
yt-dlp's help.

A watch-history read is the exception, and the only one. It is given the channel account's cookie
jar, because the history exists only for a signed-in session. So a compromised yt-dlp could carry
that session away. The jar is written to a mode-600 file in a directory removed when the read
ends. The read keeps config and plugins out the way a video read does, and adds `--no-cache-dir`,
so yt-dlp keeps nothing on disk between reads. It also adds `--write-pages`, which leaves the
history page in that directory, and `api/ytdlp` reads each entry's resume point from the page's
`ytInitialData` before the directory goes. That parse is ours rather than yt-dlp's, so a page YouTube
reshapes loses the resume points, never the history. Reads happen only when a person asks for the
history, and none is made while the door's pause holds. Nothing else
is handed to yt-dlp with the jar, and a video read never sees it. The store holds the jar sealed
by `api/jarseal` under `YOUTUBE_COOKIE_KEY`, which reaches only the server's environment, so a
backup of the store carries no usable session.

The server needs yt-dlp only when it is configured to read videos. `ENRICH_PACE=off` is a
supported configuration and starts without one. The server looks for yt-dlp at start whatever
`ENRICH_PACE` says, and the history, video-read, run and lease routes answer 503 when it found
none. `ypl play` reads that 503 from a lease as a server with no door, and plays without one. It
reads a 404 the same way, since a server older than the lease route has no door to lease either.
So does a server it cannot reach, and one that answers neither a turn nor the door's own refusal.

## Classifying another program's prose is the fragile part

`api/ytdlp` decides from yt-dlp's error text whether YouTube refused the address or refused the
video, and the two have opposite consequences. Reading on into a refusal of the address is what
turns a pause into a block, so a rate limit is looked for first and a bare "Video unavailable" is
read as neither — YouTube words its rate limit beginning with that sentence.

The marker lists are a judgment about wording nobody here controls, so nothing is allowed to
depend on them alone. The door in `api/ytdlp` stops every read for an hour after three reads fail
in a row whatever they failed with, which holds however YouTube words the next refusal.

## A verdict about a video always has a way out

Enrichment's marks are derived from foreign text and from one bounded observation, so none of them
is permanent without a route back. A video is queued on whether it holds tracks, never on whether
a read has reached it, and `api/cmd/reset-enrichment` shows every video enrichment has stopped
reading and puts them back. Anything added that excludes a video from future work ships the same.

A play is the same kind of verdict once `ypl play` infers it from playback: it sinks a mix in the
draw on one bounded observation. `ypl plays delete` takes one back, and `deleted_plays` keeps its
handle from naming another play and its id from being stored again. Every door that names a play
consults that table, because a door that did not would answer a deleted play as one never held.

## The two modules never import each other

`cli/internal/api` carries its own copy of the JSON shapes, and `cli/internal/config` reads every
deployment value from the machine rather than from a constant. Both package docs say why.

What neither says, because it is a property of the pair rather than of either: the wire contract is
the only thing holding them together, and nothing in the compiler checks it. `api/handlers`
writes what it answers to `testdata/wire`, and `cli/internal/api` decodes those documents and
requires every field it declares to arrive. Renaming a response field without regenerating is what
that catches, and it is the one mistake a green build on both sides would otherwise hide.

A value the server *enforces* is the harder half. A shape the client has wrong degrades, because an
unknown field is ignored. A value it has wrong is a refusal the client reports as a failure. These
are pinned:

- The slug Tab offers a playlist by, a derivation the server owns. `api/handlers` writes its own
  slug of a set of titles to `testdata/wire/slugs.json`, and the completion test holds the client's
  to every pair.
- The service client's scope. `api/handlers` writes `service-scopes.json` beside it, and the CLI's
  service test requires the scope it requests to reach the route `ypl server status` calls.
- The kinds of rating. `rating_kinds.json` holds the server's, and the CLI's wire test requires
  `RatingKinds` to match.
- The shape of a transcript's language. `language_pattern.json` holds the server's, and the CLI's
  wire test requires `LanguagePattern` to match.
- The refusal codes. `refusal-codes.json` holds every code `wire.go` spells, and the CLI's wire
  test requires each `Code` constant of `cli/internal/api` to be among them. The CLI waits, fails
  or plays on by some of them.

`PageSize` and `VideoSorts` are unpinned copies of a number and words the server owns, with no door
to read them through. So are `leaseAnswerWithin` and `runAnswerWithin`, each chosen longer than the
server's own bound on the same wait.
