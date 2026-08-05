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
ypl playlists list                                     # what is mirrored
ypl playlists show 'Deep Night'                        # the videos in one
ypl videos show <id>                                   # its tracklist with timestamps
```

Once synced, a playlist is named by its title — a partial title works when it is unambiguous.

Every read takes `--json`, which goes to stdout with nothing else on it. `urls` emits bare URLs for
piping:

```bash
ypl playlists urls 'Get Insights' --sort oldest --limit 1 | xargs relate analyze
```

Private and unlisted playlists need a logged-in session — set `cookies_from_browser` in the config.

## Where things live

| Path | Holds |
| --- | --- |
| `$XDG_STATE_HOME/ypl/ypl.db` | The mirror. Rebuildable from `ypl sync`, so not worth syncing between machines. |
| `$XDG_DATA_HOME/ypl/playlists/` | Local playlists as M3U. Authored, so these are the ones worth keeping. |
| `$XDG_CONFIG_HOME/ypl/config.toml` | Settings. `ypl config init` writes a starter. |

`ypl config path` prints all three.

## Not done yet

Local M3U playlists, playback, and the `remote` write queue. `youtube_playlists/main.py` is the
previous argparse splitter, kept until the write path replaces it — it still runs as a script but
is no longer part of the installed package.

## License

[MIT](https://tldrlegal.com/license/mit-license)
