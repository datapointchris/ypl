# ypl

Organize YouTube playlists of long DJ mixes. `README.md` says what the parts are and how the sync
works; this file covers what someone changing the code has to hold in mind.

## Two ways out of the process, and both are named

Everything the server reaches outside itself goes through one package, and nothing else in the
repo opens that kind of connection. An undocumented boundary is one a later change walks straight
past, so each is written here with what it is allowed to do.

| Door | Package | Reaches | Costs |
| --- | --- | --- | --- |
| The Data API | `api/youtube` | YouTube, over HTTPS, signed in as the channel with an OAuth refresh token | Quota units, 10,000 a day |
| yt-dlp | `api/ytdlp` | YouTube, over HTTPS, signed in as nobody, through a subprocess | Nothing, and the address's standing with YouTube |

`api/ytdlp.Reader.Video` is the only place in the repo that starts a process. Anything that needs
more of what yt-dlp knows extends that package rather than running the binary somewhere else.

## Why yt-dlp is a dependency, and what it is allowed to reach

The Data API reports neither a video's chapters nor its comments, under any part or field
combination, and the tracklist of a mix is in one or the other. So reading tracklists means driving
yt-dlp. Nothing safer gives the same answer: there is no API to ask, and parsing the watch page
directly would be the same scraping with none of yt-dlp's maintenance behind it.

It is a third-party binary the server executes, it makes its own network requests, and it is
installed by the host rather than pinned by this repo. A read is therefore treated as untrusted
input and given as little of the host as possible:

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

## The CLI knows the wire contract and nothing else

`cli/internal/api` holds the JSON shapes the server sends, not the server's types, and it ignores a
field the server adds. Reaching into `api/handlers` for a struct would make every server refactor a
CLI release, and a CLI is on machines the server cannot redeploy.

Nothing about a deployment is compiled in. The server's address and the identity provider each name
one installation, so both are read from config and an unset one resolves as unset. A default here
would point the CLI somewhere plausible instead of saying it was never told, which is the harder
failure to diagnose. `cli/internal/config` declares every setting in one table, and the rows
`ypl config show` prints are built from it, so a setting cannot be added without the command
learning to print it.

The two things the command tree reaches — the server and the machine's keychain — are fields on
`app`. A command that reached either directly could not be tested without a live server and the real
keychain, which is shared machine state a test may not write.

## Where the Python tool fits

`src/` and `tests/` are the original single-user Python tool. It is the source of the library the
server imports once, through `api/cmd/import-python-mirror`, and it is not part of the server. Its
release is gated on its own paths, so a commit that changes only `api/**` publishes nothing:
keep a change to `README.md` or `pyproject.toml` in its own `docs:` or `chore:` commit rather than
folding it into a `feat(api):` one, or the filter is defeated and a release is cut for a Python
tool that did not change.
