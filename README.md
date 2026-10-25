# ypl

Organize and play YouTube playlists of long DJ mixes.

Most of the library this was built for is mixed sets: one video holding twenty to forty tracks by
different artists. YouTube already carries those tracklists as chapter markers, as timestamped lines
in the description, or in one of the top comments. ypl reads them and keeps them where they can be
searched, compared and rearranged.

ypl is two programs. A server keeps the channel's playlists mirrored and reads a tracklist for each
mix. A command-line client on every machine plays from it and changes it, so several machines see
one library rather than each keeping a copy that drifts.

ypl downloads nothing and plays nothing itself. The playlists stay on YouTube. mpv plays the watch
links ypl hands it. What ypl adds is the tracklist of every mix, a record of what was heard, and the
choice of what to play next.

## Install

The client installs from this repository's `cli/v*` releases. Each carries an archive for every
platform the release builds, with a checksum file beside them.

```bash
# the newest cli/v* release, checked against its checksums before it is installed
tag=$(gh release list --repo datapointchris/ypl --json tagName \
  --jq '[.[] | select(.tagName | startswith("cli/"))][0].tagName')
platform="$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
gh release download "$tag" --repo datapointchris/ypl --pattern "ypl_*_${platform}.tar.gz" --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt && tar -xzf ypl_*_"${platform}".tar.gz ypl && install ypl ~/.local/bin/

ypl update                                   # later: move to the newest release, verified the same way
task cli:install                             # or build this checkout into ~/.local/bin, as a development build

docker pull ghcr.io/datapointchris/ypl-api   # the server's image; :sha-<short> names one build and never moves
```

A check at most once a day says when a newer release is out. `ypl update` refuses to replace a
development build.

## Starting from scratch

A machine plays from a server, so the server comes first. Skip the first group where one is already
running.

```bash
# The server, once, from a clone of this repository
export YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=…    # the channel's Google OAuth client
export YOUTUBE_REFRESH_TOKEN=$(go -C api run ./cmd/authorize-youtube | jq -r .refresh_token)  # approve as the channel
export YOUTUBE_COOKIE_KEY=$(openssl rand -base64 32)  # seals the watch-history session in the store
docker run -d --name ypl -p 8080:8080 -v ypl-db:/var/db/ypl \
  -e YOUTUBE_CLIENT_ID -e YOUTUBE_CLIENT_SECRET -e YOUTUBE_REFRESH_TOKEN -e YOUTUBE_COOKIE_KEY \
  -e OIDC_ISSUER=https://auth.example.com -e PUBLIC_URL=https://ypl.example.com \
  ghcr.io/datapointchris/ypl-api:latest               # its first pass syncs every playlist
curl -fsS https://ypl.example.com/ready               # 200 once it has read the identity provider

# Each machine that plays
ypl auth login https://ypl.example.com   # fills in the config file, then prints a code to approve in any browser
ypl auth status                          # where it is pointed, and whether it holds a token; exit 1 if not
source <(ypl completion zsh)             # Tab offers every playlist by its slug
ypl server status                        # the whole path works: the server answers with what it holds
```

The Google OAuth client and the identity provider are the two things no command here supplies.

The OAuth client belongs to a Google Cloud project with the YouTube Data API v3 enabled. Its type is
"TVs and Limited Input devices". Its app has to be published "In production", because an app in
"Testing" issues refresh tokens that expire after 7 days. `api/youtube`'s package documentation
walks through it. When `authorize-youtube` shows the account chooser, pick the channel's own
account. A brand account's playlists belong to the brand account, not to the Google account that
manages it.

The identity provider has to issue RFC 9068 JWT access tokens and offer the device grant. It has to
hold a public client for each machine that logs in: `ypl-cli-<short hostname>`, unless
`--client-id` names another. One client per machine is what lets one machine's token be revoked
without touching the others.

`PUBLIC_URL` is exactly the address the API is served at over HTTPS. The client refuses the
server's metadata unless it names the URL the client asked. The server serves no metadata while
`PUBLIC_URL` is unset, and `ypl auth login` then has no issuer to fill in.

The image runs as uid 10001, so a host directory mounted at `/var/db/ypl` has to be owned by it. A
store left inside the container goes with the container.

`ypl completion zsh --help` says where to keep the script so every new shell loads it. The same
command takes `bash`, `fish` or `powershell`.

`ypl play` is the next command. It needs mpv on `PATH`. mpv opens a YouTube link through yt-dlp, so
both come from the system's package manager.

## The grammar

A command is a noun, then a verb: `ypl playlists show` and `ypl playlists rename` differ only in
the last word. A namespace on its own, like `ypl playlists`, lists the commands under it. A bare
`ypl` prints the root help. That help lists every command as the line to type, in groups: playing
first, then one group for each thing the library holds, then the server, then setup. It is not
repeated here, because a list in markdown goes stale and `--help` cannot.

A playlist is named by its title, by the slug Tab offers, or by its YouTube id. Case and punctuation
do not have to be reproduced, but the breaks between words do. `'sunday morning'` and
`sunday-morning` both find Sunday Morning. `sundaymorning` does not. A name matching two
playlists is refused naming each, rather than answered with one of them. A playlist the server does
not recognize is answered with every playlist it does hold, in the form Tab offers.

A read also takes part of a title, so `ypl playlists show morning` finds Sunday Morning too. A
change does not, for the reason the `playlists` section gives. A video is named by its link, its id
or its title. A play is named by its number in `ypl plays list`.

Every read takes `--json`, which writes a stable shape to stdout and nothing else. A collection with
nothing in it is `[]` rather than `null`, so one filter works on every answer. A command that prints
one string, like `config path` or `auth token`, takes no `--json`.

Exit codes are 0 for success, 2 for an invocation the CLI would not accept, and 1 for a command that
ran and failed. Three commands also spend 1 on a real state, after writing their answer: `ypl now`
when nothing is playing, `ypl next` when there is nothing to suggest, and `ypl auth status` when
this machine is not logged in. A status bar can run any of them unguarded. `ypl server status`
spends 3 on a sync that has stopped keeping up, and on a YouTube session that last read the watch
history signed out.

`--no-input` forbids every prompt, so a script can take the non-interactive path from a terminal. A
delete then needs `--yes`. An edit then needs its buffer piped in.

## Which surface answers which question

| Ask | Surface | Reads |
| --- | --- | --- |
| What should I put on | `play`, `next` | the server's draw, which `play` hands to mpv |
| Which track is this, inside the mix | `now` | mpv's socket, then the video's tracklist from the server |
| What a playlist holds, and in what order | `playlists list`, `playlists show` | the server's mirror |
| Make, retitle, delete or reorder a playlist | `playlists create`, `rename`, `delete`, `edit` | the server, which writes to YouTube |
| Which mixes run three hours, or name an artist | `videos` | the server's tracklists and lengths |
| What have I heard lately | `plays` | the server's listening history |
| What I said about a mix, wherever it is playing | `rate`, `ratings`, `tags`, `videos show` | the account's watch history, then the server's ratings |
| Is the mirror keeping up with YouTube | `server` | the server's syncs and the day's quota |
| Where this machine is pointed, and whether it is logged in | `auth status`, `config show` | the config file and this machine's keychain |
| Is a newer release out | `update` | this repository's `cli/v*` releases |

## `play`, `now`, `next` — ypl chooses and records, and mpv plays

```bash
ypl play sunday-morning           # the playlist in its order, leaving out what YouTube will not serve
ypl play                          # a draw: never-played first, in a new order each time, then the least recently heard
ypl play sunday-morning --audio   # the sound alone, with no video window
ypl now                           # the track playing inside the mix, and how far in
ypl next                          # the same draw as a bare play, printed rather than played
```

`ypl play` runs mpv in the foreground, holding the terminal until mpv exits. `--mpv` passes an
argument straight through to it. `ypl play` opens mpv's IPC socket, which is what lets `ypl now`
report the track inside a two-hour mix rather than the name of the mix. `ypl now` sees only an mpv
that `ypl play` started, because the socket sits at a path `ypl play` chose.

A player that failed is exit 1. mpv's own status is written to stderr instead. mpv spends 2 on a
file it cannot open. This tool spends 2 on an invocation it would not accept, so mpv's code passed
through would say the wrong thing.

`ypl play` records what it plays. It reads mpv's socket while the player runs. Once a mix has played
for 20 minutes, or for half its length when that is shorter, it tells the server while mpv is still
playing. Only ground covered at playing speed counts: a seek forward, a rewind and a pause are not
listening. Each play is posted under an id made once, so a retry is stored once. A play the server
could not be told about is named afterwards with the `ypl plays add` line that records it. The plays
are what `ypl next` reads to stop suggesting the same mix.

## `plays` — the listening history, where every play can be taken back

```bash
ypl plays list                                     # newest first, each with its number in the PLAY column
ypl plays add 'https://youtu.be/dQw4w9WgXcQ?t=42'  # a mix heard in a browser or on a phone, recorded as heard now
ypl plays delete 41                                # asks first; the video then ranks as if that play never happened
```

A play `ypl play` inferred is a verdict from one bounded observation. It sinks the mix in the draw,
so every play has a way back. `ypl plays add` takes a video by its id, by a link it was copied from,
or by part of its title. `ypl plays delete` asks first. `--yes` answers for it. A deleted play's
number is never given to another play, so a number typed from an old listing finds nothing rather
than a different listen. Its id is never stored again either, so a retry arriving after the delete
cannot record the play a second time.

## `rate` — every like, dislike and note on a mix is kept, picked from the watch history

```bash
ypl rate                                        # pick the mix in fzf, the watch history on top, then asks
ypl rate --like -m 'great closing hour'         # pick the mix, and like it with a message
ypl rate 'mayan warrior' --note -m 'too much talk'  # a note on one mix, by part of its title
ypl videos show 'mayan warrior'                 # every rating of that mix, newest first
ypl videos list --rated dislike                 # the mixes with a dislike
ypl videos list --tag sunrise --text closing    # tags and messages are what a mix is found by later
ypl tags list                                   # every tag, with how many mixes carry it
ypl ratings delete 3f9c2a1b                     # take one back, by the ID column of ratings list
```

A rating is about the mix, never a track inside it. It is a like, a dislike or a note, with a
message and free-form tags. A note needs its message. A mix keeps every rating left on it, and
nothing is worked out from them beyond counts, so the listing is the record.

At a terminal with no mix named, `ypl rate` reads YouTube's watch history through the server. Every
player signed into the channel's account writes to it, so a mix playing in a browser on another
machine is near the top. The picker is fzf over the whole library, with the newest mixes in the
history above the rest, each marked with its place there. A history that cannot be read is said on
stderr, and the library is offered alone. Without a terminal, name the mix and give the flags.

The server reads the history with a signed-in cookie jar, which `ypl server cookies set <file>`
sends. `ypl server cookies set --help` gives the export steps. The jar has to come from a private
window that is closed afterwards, because YouTube rotates the cookies of a session a browser keeps
open. The server keeps the jar fresh itself after that, and `ypl server status` says when YouTube
stops honoring it.

## `playlists` — a change takes the whole name, and reaches YouTube before it answers

```bash
ypl playlists list                                      # every playlist, by the slug Tab offers, with its video count
ypl playlists show morning                              # a read: part of a title is enough
ypl playlists rename sunday-morning 'Sunday Mornings'   # a change: the whole title, the slug or the id
ypl playlists edit sunday-morning                       # reorder, add or remove videos in $EDITOR
```

A rename, a delete or an edit takes the id, the slug or the whole title. A fragment that happens to
match one playlist matches it unambiguously, so the refusal of two matches has nothing to catch. A
fragment sent to a change is refused by the titles it is part of, rather than by a sentence saying
nothing was found.

`ypl playlists create`, `rename` and `delete` make their change on YouTube in the request that asks
for it. `delete` asks first, and needs `--yes` where there is nobody to ask.

`ypl playlists edit` is the one changing verb that does not reach YouTube itself. It opens the order
in `$VISUAL` or `$EDITOR`, one line per video, and sets the order the server holds. The server
pushes that order to YouTube as soon as it is saved, ahead of anything else it has waiting. Each
video the push moves, adds or removes is a 50-unit write, so a large reorder can take more than a
day's quota. `ypl server status` lists what is left to push.

Where an edit is refused, the buffer is kept in a file. The refusal names the file, with the command
that applies it. By then the editor has closed, so that file is the only copy of the rearranging.

## `videos` — the library searched by what its tracklists name

```bash
ypl videos list --min-minutes 180   # three hours or more; a video of unknown length is left out
ypl videos list --artist bjork      # every mix whose tracklist names Björk
ypl videos show 'mayan warrior'     # one mix's tracklist, by its link, its id or part of its title
```

A tracklist is the video's chapters, or failing those the timestamped lines of its description, or
failing that one of its top comments that reads as a whole tracklist. The server reads it, and
[`api/README.md`](api/README.md) gives the rule. An artist matches ignoring case and accents. A
video's length is known before its tracklist is read, so a filter by length reaches a mix the server
has not read yet. A video YouTube will not serve is left out.

## `server` — whether the mirror is keeping up, and an exit code that means look

```bash
ypl server status       # what it holds, its last sync, the day's quota, and what waits to be pushed
ypl server syncs list   # the recent syncs: what each changed, read and spent
```

`ypl server status` exits 3 when the server's latest ok sync is older than an hour, or older than
twelve of its ticks where those are longer. That is a state for a person to look at rather than a
command that failed, so it has a code of its own. It exits 1 only when the server could not be
asked. A service can run it on a timer with a credential of its own, as the next section describes.

## `auth` and `config` — one file says where, and each machine holds a token of its own

```bash
ypl config path   # where the file goes, whether or not it exists
ypl config show   # each setting, and whether the file or --api set it
ypl auth status   # the same three, and whether a token is held; exit 1 if not
ypl auth token    # the access token, for a request made with curl
ypl auth logout   # forget this machine's token, before handing the machine on
```

Nothing about a deployment is built into the binary. The server's address, the identity provider
that signs its tokens, and the client this machine logs in as are read from one file,
`$XDG_CONFIG_HOME/ypl/config.toml`:

```toml
api = "https://ypl.example.com"
issuer = "https://auth.example.com"
client_id = "ypl-cli-laptop"
```

`--api` is the only thing that outranks the file. No environment variable does.

`ypl auth login <url>` fills in only what the file lacks: `api` from the URL, `issuer` from the
server's RFC 9728 metadata, and `client_id` from `--client-id` or `ypl-cli-<short hostname>`. It
prints each value and where it came from before the login starts. It writes the file once the login
succeeds. It never rewrites a value the file holds. A typed value that disagrees with the file is
refused, naming the file to edit. The file and its rules are
[goclilogin's](https://github.com/datapointchris/goclilogin).

The login is the OAuth 2.0 device authorization grant. The CLI prints a code and a URL. Approving
it in a browser anywhere logs this machine in, which is what makes it work over SSH on a machine
with no browser. The token goes in the OS keychain, or in a mode-600 file on a host that has none.
It is refreshed under a lock, so two commands at once cannot spend the same refresh token twice.

A service runs ypl with nobody present to approve a login. Its config file names its own
confidential client, `ypl-svc-<machine>`, as `client_id`. `YPL_CLIENT_SECRET` in the environment
holds that client's secret, which never goes in the file. On each run ypl requests a token through
the client-credentials grant, asking for `ypl.status.read`. It stores nothing. The server lets that
token read `GET /api/v1/status`, which is what `ypl server status` calls. Any other route answers
that token with 403. `ypl auth login` and `ypl auth logout` refuse in that mode.

`ypl auth status --json` names the credential it checked in `type`: `authorized_user` for a person's
login, or `service_account` for a service, as Google's credential files do. A service's status exits
1 unless the provider grants a token. Its `session` then says why: `rejected` for a secret the
provider refused, `unverified` for a provider it could not reach.

## `api/` — the one part that talks to YouTube

The server in `api/` keeps the channel's playlists mirrored through the Data API, signed in as the
channel with an OAuth refresh token. It reads each mix's tracklist with yt-dlp, signed in as nobody,
because the Data API reports neither a video's chapters nor its comments. It answers the HTTP API
the client speaks. The client holds no database and reads nothing from YouTube, so every answer on
every machine is the server's.

Every write to YouTube spends the channel's Data API quota. `ypl server status` shows what the day
has left. The server's reference is [`api/README.md`](api/README.md), beside the code it describes.
It covers the quota each request spends, the settings the server reads, how a sync merges an edit
made on YouTube with one made here, and every route.

## Where it writes

- `$XDG_CONFIG_HOME/ypl/config.toml`, or `~/.config/ypl/config.toml`, written by `ypl auth login`
  alone and only with the values it lacks.
- The OS keychain, under the service `ypl-cli`, one entry per client id. A host with no keychain
  gets a mode-600 file in the state directory instead.
- `$XDG_STATE_HOME/ypl/`, or `~/.local/state/ypl/`: the lock a token refresh holds, and `mpv.sock`
  while `ypl play` runs.
- A temporary file named `ypl-edit-*.ypl`, only when an edit is refused.

## License

[MIT](LICENSE)
