# ypl

Organize YouTube playlists of long DJ mixes.

Most of the library this was built for is mixed sets — one video holding twenty to forty tracks by
different artists. YouTube already carries those tracklists as chapter markers or timestamped
description lines, so `ypl` pulls them down, parses them, and keeps them locally where they can be
searched, compared and rearranged.

## Why it reads with yt-dlp

The YouTube Data API gives 10,000 quota units a day per project. A list call costs 1 unit; an
insert or delete costs 50. That is 200 writes a day, it is a project-level cap rather than an
account setting, and raising it needs a Google audit that personal tools do not get.

So reads go through `yt-dlp`, which costs nothing — and which is the only way to get chapters at
all, since the Data API does not expose them under any part or field combination.

The consequence shapes the whole tool: organizing happens locally and instantly, and pushing
anything back to YouTube is a separate, deliberate, queued act.

## Install

```bash
uv tool install .
```

Needs [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) on `PATH`.

## Use

```bash
ypl sync 'https://www.youtube.com/playlist?list=...'   # mirror a playlist locally
ypl enrich --playlist 'Deep Night' --limit 50          # pull tracklists, resumably
ypl playlists list                                     # everything, mirrored and local
ypl playlists show 'Deep Night'                        # the videos in one
ypl videos show <id>                                   # its tracklist with timestamps
```

A playlist is named by its title — a partial title works when it is unambiguous.

Every read takes `--json`, which goes to stdout with nothing else on it. `urls` emits bare URLs for
piping:

```bash
ypl playlists urls 'Get Insights' --sort oldest --limit 1 | xargs relate analyze
```

## Playlists you build

A playlist you make is an M3U file under `$XDG_DATA_HOME/ypl/playlists/`, and nothing about it
touches YouTube:

```bash
ypl playlists create 'Sunday' --from 'Deep Night' --sort newest --limit 20
ypl playlists add 'Sunday' 'https://youtu.be/...'      # URLs or bare ids
ypl playlists remove 'Sunday' <id>
ypl playlists delete 'Sunday'
ypl playlists split 'Deep Night' --size 90             # into 'Deep Night 1', 'Deep Night 2'...
ypl playlists order 'Sunday' --sort longest            # in place, or --into 'Sunday Long'
```

`split` takes `--size` (roughly how many per part) or `--parts` (how many parts). Parts come out
even rather than as full chunks and a stub — 140 videos at a size of 90 is two parts of 70, not a
90 and a 50.

`--sort` means the same thing everywhere it appears: `position`, `oldest`, `newest`, `longest`,
`shortest`, `title`, `random`.

A playlist you make is synced to YouTube by default, so it turns up in the YouTube app on your
phone once the queue next drains. It is written as `PRIVATE`, which still appears in your own
library on every device you are signed in on. `--local` keeps one here instead, and `promote` /
`demote` change their mind later:

```bash
ypl playlists create 'Scratch' --from 'Deep Night' --local   # stays on this machine
ypl playlists promote 'Scratch'                              # goes up on the next drain
ypl playlists demote 'Scratch'                               # stop pushing; leaves YouTube alone
```

`ypl playlists list` shows where each one sits: `local`, `pending` (synced but not on YouTube yet),
`synced`, or `remote` for a mirrored one.

`create` also reads URLs from a pipe, so a selection made by one command becomes a playlist:

```bash
ypl playlists urls 'Deep Night' --sort random --limit 20 | ypl playlists create 'Sunday'
```

An entry is a video. A DJ mix holding forty tracks is one entry, because a video is the smallest
thing that can be played; the tracklist is metadata about it and lives in the mirror, where it is
what ordering and similarity are computed from.

The files are plain extended M3U, so `mpv --playlist ~/.local/share/ypl/playlists/sunday.m3u`
plays one with no help from `ypl`, and VLC and Kodi open them too. What ypl needs beyond the
format rides on `#YPL-` comment lines, which every player ignores.

Both kinds of playlist answer to the same read commands. Only local ones can be changed:
`ypl playlists list --source local` shows which are which.

Private and unlisted playlists need a logged-in session — set `cookies_from_browser` in the config.

## Where things live

| Path | Holds |
| --- | --- |
| `$XDG_STATE_HOME/ypl/ypl.db` | The mirror. Rebuildable from `ypl sync`, so not worth syncing between machines. |
| `$XDG_DATA_HOME/ypl/playlists/` | Local playlists as M3U. Authored, so these are the ones worth keeping. |
| `$XDG_DATA_HOME/ypl/plays.jsonl` | Listening history, appended one line per listen. Not rebuildable from anything, so it sits with the playlists rather than in the mirror. |
| `$XDG_DATA_HOME/ypl/remote/` | What YouTube held for each playlist at the last reconcile, one JSON file per playlist. The base of the three-way merge, and the only copy of each slot's `setVideoId`. Not rebuildable — re-reading YouTube answers what is there now, not what was there then. |
| `$XDG_CONFIG_HOME/ypl/config.toml` | Settings. `ypl config init` writes a starter. |
| `$XDG_CONFIG_HOME/ypl/ytmusic.json` | The YouTube session, mode 0600. Written by `ypl remote auth`; it is a Google account cookie, so treat it as the password it is. |
| `$XDG_STATE_HOME/ypl/mpv.sock` | mpv's IPC socket while `ypl play` is running. Read by `ypl now`. |

`ypl config path` prints the first three.

## Playing

```bash
ypl play 'Sunday' --audio --sort random    # runs mpv in the foreground
ypl now                                    # what is playing, down to the track
```

`play` hands the URLs to mpv as arguments rather than as a playlist file, so `--sort` and
`--limit` mean the same thing here as everywhere else and a mirrored playlist plays without
writing a file first. `mpv_arguments` in the config is the escape hatch for anything else mpv can
do.

Every playback opens mpv's JSON IPC socket, which is what `ypl now` reads. Because the mirror
holds a tracklist with real timestamps, it reports the track inside a two-hour mix rather than the
name of the mix:

```console
$ ypl now
Four Tet - Baby
Shimza for Cercle at Citadelle de Sisteron  0:42:13 / 2:05:33
```

It exits 1 when nothing is playing, with nothing on stdout, so a status bar can run it unguarded.
On Arch, waybar's built-in `mpris` module already shows mpv without any of this — install
`mpv-mpris` and `ypl play` appears there on its own.

## What to put on next

```bash
ypl next                        # least recently listened to, never-played first
ypl next --playlist 'Sunday'    # choose from one playlist
ypl plays add <id>              # record a listen
ypl plays list                  # what has been played lately
```

History is recorded when a listen is logged, not inferred from playback: `ypl play` hands mpv the
whole list at once and blocks, so it never learns which of it actually got played.

It is a file rather than a table because the mirror is disposable — it re-syncs for free — and a
record of what you have listened to cannot be rebuilt from anything. Deleting the mirror does not
make `ypl next` forget.

`ypl next` is the resolver [`menu next`](https://github.com/datapointchris/dotfiles) delegates to,
so a pursuit answers with a mix rather than with the word "listen". In `~/.config/menu/pursuits.yml`:

```yaml
listen:
  description: Put a mix on
  weight: 15
  resolve: ypl next --json
  label: title
  id: video_id
  on_log: ypl plays add {id}
```

Each call draws afresh among everything tied at the same rank — on a library nothing has been
played from, that is all of it — so repeated calls do not keep naming the same mix. `menu next`
caches its own draw, which is where stability within a session comes from.

## Writing back to YouTube

Writes go through the YouTube Music web client's own endpoints (`ytmusicapi`), not the official
Data API. `playlistItems.insert` costs 50 units of a per-project 10,000/day, which is 200 writes a
day permanently — splitting an 1,800-video playlist through it is eighteen days of queue draining,
which forces the shape that looks least like a person: a daemon making requests around the clock
for weeks.

The web client protocol batches. One request carries an `actions` array of a hundred additions, so
the same reorganisation is a couple of dozen requests — *less* traffic than doing it by hand in the
browser. That is the argument for this route, and it only holds if the saving is not spent on
speed, so every call goes through a throttle, batches are bounded, and a rate-limit response stops
the run rather than retrying into it.

Reordering is the exception: a move is one request and cannot be batched, so ypl computes the
shortest move sequence rather than rewriting the playlist slot by slot. Moving one video to the
front of a 200-track playlist is one request, not 200.

The backend sits behind an interface, so the Data API remains a one-module swap.

Signing in is a paste, once per machine. There is no OAuth: ytmusicapi's OAuth flow now needs a
TV-type Google client of your own, which is the Data API project setup this route exists to avoid.

```bash
ypl remote auth                    # paste the headers, Ctrl-D
pbpaste | ypl remote auth          # or pipe them in
```

Sign in at [music.youtube.com](https://music.youtube.com), open DevTools → Network, filter for
`browse`, click a POST request and copy the whole Request Headers block. They are stored at
`$XDG_CONFIG_HOME/ypl/ytmusic.json`, mode 0600 — the cookie in there is the entire credential, so
treat the file as the password it is. `ypl remote auth` then asks YouTube whose account it reaches
and prints the answer, because a paste that parses is not yet a session that works; one YouTube
rejects is deleted rather than stored to fail later. `--replace` signs in over a stored session.

## Not done yet

The reconcile and the background queue that drains it — `ypl remote pull`, `plan`, `push`,
`apply`. Nothing has written to YouTube yet.

## License

[MIT](https://tldrlegal.com/license/mit-license)
