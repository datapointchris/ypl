# ypl

Organize YouTube playlists of long DJ mixes. `README.md` says what the parts are and how the sync
works; this file covers what someone changing the code has to hold in mind.

## Every way out of a process is named

Everything either program reaches outside itself goes through one package, and nothing else opens
that kind of connection. An undocumented boundary is one a later change walks straight past, so
each is written here with what it is allowed to do.

The server, on the machine it is deployed to:

| Door | Package | Reaches | Costs |
| --- | --- | --- | --- |
| The Data API | `api/youtube` | YouTube, over HTTPS, signed in as the channel with an OAuth refresh token | Quota units, 10,000 a day |
| yt-dlp | `api/ytdlp` | YouTube, over HTTPS, signed in as nobody, through a subprocess | Nothing, and the address's standing with YouTube |

The CLI, on every workstation:

| Door | Package | Reaches | Costs |
| --- | --- | --- | --- |
| The ypl server | `cli/internal/api` | The configured server, over HTTPS, with a bearer token | Nothing |
| The identity provider | `goclilogin` | The configured issuer, for discovery and the device grant | Nothing, and it is reached on every command |
| This machine's keychain | `goclilogin` | The OS keychain, or a mode-600 file where there is none | The refresh token lives there |
| A browser | `pkg/browser` | Whatever `xdg-open` or `open` resolves to, once, during `ypl auth login` | A subprocess |
| An editor | `cli/internal/editbuffer` | Whatever `$VISUAL` or `$EDITOR` names, holding the terminal, during `ypl playlists edit` | A subprocess, and the terminal until it exits |
| mpv | `cli/internal/mpv` | mpv on `PATH`, holding the terminal, during `ypl play`; and its IPC socket at `$XDG_STATE_HOME/ypl/mpv.sock` during `ypl now` | A subprocess, the terminal until it exits, and whatever mpv itself reaches |

mpv makes its own network requests, as yt-dlp does. It is given the watch URLs of the videos being
played and nothing else — no credential, no token, no part of the store. `ypl now` only reads from
the socket it opened, and sends mpv no command.

`api/ytdlp.Reader.Video` is the only place in the server that starts a process. In the CLI there
are three, `ypl auth login`, `ypl playlists edit` and `ypl play`, and the first hands its
subprocess the opposite of what the other two hand theirs. The browser launcher is given
`os.DevNull`, because a pipe would keep the login blocked until the browser exits. The editor and
mpv are given this process's own streams, because each draws an interface and takes keys, and a
captured stream turns that interface into a string. Anything that needs more of what yt-dlp knows
extends that package rather than running the binary somewhere else.

The CLI is given no credential of the server's and no part of the store. What it holds is a token
for one machine, revocable on its own without touching any other.

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
- **`--skip-download` and `--dump-json`.** A read wants one JSON document. Nothing is written to
  disk and no media is fetched.
- **One process a read, bounded by the caller's context, with `WaitDelay` set.** A process yt-dlp
  itself starts can hold the output pipe open after yt-dlp is gone, so the deadline alone does not
  end a read.
- **Only the fields a read keeps are decoded.** `api/ytdlp.info` names them; everything else in
  yt-dlp's answer is dropped rather than carried around.

If yt-dlp egressed something it should not, what it holds is the video ids the server is reading
and the address it reads from. It is given no credential, no cookie and no part of the store.

The server needs yt-dlp only when it is configured to read videos. `ENRICH_VIDEOS_PER_RUN=0` is a
supported configuration and starts without one.

## Classifying another program's prose is the fragile part

`api/ytdlp` decides from yt-dlp's error text whether YouTube refused the address or refused the
video, and the two have opposite consequences. Reading on into a refusal of the address is what
turns a pause into a block, so a rate limit is looked for first and a bare "Video unavailable" is
read as neither — YouTube words its rate limit beginning with that sentence.

The marker lists are a judgment about wording nobody here controls, so nothing is allowed to
depend on them alone. `api/enrich` stops a run after three reads fail in a row whatever they
failed with, which holds however YouTube words the next refusal.

## A verdict about a video always has a way out

Enrichment's marks are derived from foreign text and from one bounded observation, so none of them
is permanent without a route back. A video is queued on whether it holds tracks, never on whether
a read has reached it, and `api/cmd/reset-enrichment` shows every video enrichment has stopped
reading and puts them back. Anything added that excludes a video from future work ships the same.

## The two modules never import each other

`cli/internal/api` carries its own copy of the JSON shapes, and `cli/internal/config` reads every
deployment value from the machine rather than from a constant. Both package docs say why.

What neither says, because it is a property of the pair rather than of either: the wire contract is
the only thing holding them together, and nothing in the compiler checks it. `api/handlers`
writes what it answers to `testdata/wire`, and `cli/internal/api` decodes those documents and
requires every field it declares to arrive. Renaming a response field without regenerating is what
that catches, and it is the one mistake a green build on both sides would otherwise hide.

A value the server *enforces* is the harder half and is not solved. `PageSize` and `VideoSorts` are
copies of numbers and words the server owns, with no door to read them through. A shape the client
has wrong degrades — an unknown field is ignored — and a value it has wrong is a refusal the client
reports as a failure.

## Where the Python tool fits

`src/` and `tests/` are the original single-user Python tool. It is the source of the library the
server imports once, through `api/cmd/import-python-mirror`, and it is not part of the server. Its
release is gated on its own paths, so a commit that changes only `api/**` publishes nothing:
keep a change to `README.md` or `pyproject.toml` in its own `docs:` or `chore:` commit rather than
folding it into a `feat(api):` one, or the filter is defeated and a release is cut for a Python
tool that did not change.
