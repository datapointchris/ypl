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
| `$XDG_CONFIG_HOME/ypl/config.toml` | Settings. `ypl config init` writes a starter. |

`ypl config path` prints all three.

## Not done yet

`ypl play`, and the `remote` write queue.
`youtube_playlists/main.py` is the previous argparse splitter, kept until the write path replaces
it — it still runs as a script but is no longer part of the installed package.

## License

[MIT](https://tldrlegal.com/license/mit-license)
