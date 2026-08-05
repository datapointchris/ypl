"""The ypl command tree."""

import json
import sqlite3

import typer
from pyselfupdate import Config as UpdateConfig
from pyselfupdate import notify
from pyselfupdate.typercmd import run_update
from rich.console import Console
from rich.table import Table

from ypl import config
from ypl import db
from ypl import paths
from ypl import service
from ypl import ytdlp

ROOT_HELP = """\
Organize YouTube playlists of long DJ mixes.

The noun comes first and the verb last, so a list -> show -> urls loop on one
playlist changes only the final word.

One rule: nothing here writes to your YouTube account. `sync` and `enrich` pull
down through yt-dlp, which costs no API quota, and every other command reads the
local mirror.

A playlist is named by its title once synced, so `ypl playlists show 'Get
Insights'` works without an id; a partial title matches when it is unambiguous.
Run any partial command with no arguments or --help to see what comes next.
"""

PLAYLISTS_HELP = """\
Playlists in the local mirror.

Examples:

  ypl playlists list                              what is mirrored, and how much is enriched
  ypl playlists show 'Get Insights'               the videos in one playlist
  ypl playlists urls 'Get Insights' --sort oldest --limit 1
                                                  the next URL to feed to relate
"""

VIDEOS_HELP = """\
Videos, and the tracklists parsed out of them.

Examples:

  ypl videos show dQw4w9WgXcQ                     the tracklist with chapter timestamps
"""

CONFIG_HELP = """\
The settings file at $XDG_CONFIG_HOME/ypl/config.toml.

Examples:

  ypl config init                                 write a starter file
  ypl config example                              print it without writing anything
  ypl config path                                 where it would be read from
"""

READING = 'Reading (the local mirror)'
SYNCING = 'Syncing (pulls from YouTube, no API quota)'
ADMIN = 'Admin'

app = typer.Typer(name='ypl', no_args_is_help=True, help=ROOT_HELP)
playlists_app = typer.Typer(name='playlists', no_args_is_help=True, help=PLAYLISTS_HELP)
videos_app = typer.Typer(name='videos', no_args_is_help=True, help=VIDEOS_HELP)
config_app = typer.Typer(name='config', no_args_is_help=True, help=CONFIG_HELP)

app.add_typer(playlists_app, name='playlists', rich_help_panel=READING)
app.add_typer(videos_app, name='videos', rich_help_panel=READING)
app.add_typer(config_app, name='config', rich_help_panel=ADMIN)

# Data goes to stdout and nothing else does, so a caller parsing --json never
# has to strip a warning line out of the payload first.
console = Console(highlight=False)
messages = Console(stderr=True, highlight=False)

UPDATE_CONFIG = UpdateConfig(tool='ypl', owner='datapointchris')


@app.callback()
def root(ctx: typer.Context) -> None:
    if ctx.invoked_subcommand != 'update':
        notify(UPDATE_CONFIG)


def print_json(data: object) -> None:
    """Emit JSON to stdout, bypassing Rich markup."""
    print(json.dumps(data, default=str))


def rows_to_dicts(rows: list[sqlite3.Row]) -> list[dict]:
    return [dict(row) for row in rows]


def duration_words(seconds: int | None) -> str:
    if not seconds:
        return '-'
    hours, remainder = divmod(seconds, 3600)
    minutes, _ = divmod(remainder, 60)
    return f'{hours}:{minutes:02d}' if hours else f'{minutes}m'


def timestamp_words(seconds: int | None) -> str:
    if seconds is None:
        return '-'
    hours, remainder = divmod(seconds, 3600)
    minutes, secs = divmod(remainder, 60)
    return f'{hours}:{minutes:02d}:{secs:02d}' if hours else f'{minutes}:{secs:02d}'


def resolve_or_exit(connection: sqlite3.Connection, name: str) -> sqlite3.Row:
    """Turn a playlist name into a row, or exit with a message naming the fix."""
    try:
        return service.resolve_playlist(connection, name)
    except service.AmbiguousPlaylistError as error:
        messages.print(f'[red]{name!r} matches {len(error.candidates)} playlists:[/red]')
        for candidate in error.candidates:
            messages.print(f'  {candidate["title"]}  ({candidate["playlist_id"]})')
        messages.print('Use the full title or the playlist id.')
        raise typer.Exit(2) from error
    except service.PlaylistNotFoundError as error:
        messages.print(f'[red]No playlist matching {name!r}.[/red] Run [bold]ypl playlists list[/bold] to see what is mirrored.')
        raise typer.Exit(1) from error


@app.command('sync', rich_help_panel=SYNCING)
def sync(
    url: str = typer.Argument(..., help='Playlist URL or id. A playlist enters the mirror this way; after that its title works.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Mirror a playlist's contents locally.

    One request for the whole playlist however long it is, and no API quota.
    Re-running picks up additions, removals and reordering.
    """
    settings = config.load()
    connection = db.connect()
    try:
        playlist = service.sync_playlist(connection, url, cookies_from_browser=settings.cookies_from_browser)
    except ytdlp.YtdlpUnavailableError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    except ytdlp.YtdlpFailedError as error:
        messages.print(f'[red]Could not read {url}[/red]')
        messages.print(str(error))
        raise typer.Exit(1) from error

    unavailable = sum(1 for video in playlist.videos if video.is_unavailable)
    if as_json:
        print_json(
            {
                'playlist_id': playlist.playlist_id,
                'title': playlist.title,
                'video_count': len(playlist.videos),
                'unavailable_count': unavailable,
            }
        )
        return
    messages.print(f'Synced [bold]{playlist.title}[/bold] ({playlist.playlist_id}): {len(playlist.videos)} videos')
    if unavailable:
        messages.print(f'{unavailable} are deleted or private — kept, so positions still line up')
    messages.print(f'Next: [bold]ypl enrich --playlist {playlist.title!r}[/bold] to pull tracklists')


@app.command('enrich', rich_help_panel=SYNCING)
def enrich(
    playlist: str = typer.Option(None, '--playlist', '-p', help='Limit to one playlist, by title or id.'),
    limit: int = typer.Option(None, '--limit', '-n', help='How many videos to fetch this run.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Fetch each video in full and parse its tracklist.

    Chapters where a video has them, timestamped description lines where it does
    not. One request per video, so this is the slow half — it is resumable, and
    a video already enriched is skipped.
    """
    settings = config.load()
    connection = db.connect()
    playlist_id = resolve_or_exit(connection, playlist)['playlist_id'] if playlist else None
    video_ids = service.unenriched_video_ids(connection, playlist_id, limit or settings.enrich_batch_size)
    if not video_ids:
        messages.print('Nothing left to enrich.')
        if as_json:
            print_json({'enriched': 0, 'tracks_found': 0, 'failed': 0})
        return

    enriched = 0
    tracks_found = 0
    failures: list[dict] = []
    with messages.status(f'Enriching {len(video_ids)} videos...') as status:
        for index, video_id in enumerate(video_ids, start=1):
            status.update(f'[{index}/{len(video_ids)}] {video_id}')
            try:
                tracks = service.enrich_video(connection, video_id, cookies_from_browser=settings.cookies_from_browser)
            except ytdlp.YtdlpFailedError as error:
                failures.append({'video_id': video_id, 'error': str(error).splitlines()[-1] if str(error) else 'unknown'})
                continue
            enriched += 1
            tracks_found += len(tracks)

    if as_json:
        print_json({'enriched': enriched, 'tracks_found': tracks_found, 'failed': len(failures), 'failures': failures})
        return
    messages.print(f'Enriched [bold]{enriched}[/bold] videos, found [bold]{tracks_found}[/bold] tracks')
    if failures:
        messages.print(f'{len(failures)} failed:')
        for failure in failures:
            messages.print(f'  {failure["video_id"]}  {failure["error"]}')
    remaining = len(service.unenriched_video_ids(connection, playlist_id))
    if remaining:
        messages.print(f'{remaining} still unenriched — run again to continue')


@playlists_app.command('list')
def playlists_list(
    limit: int = typer.Option(None, '--limit', '-n', help='Show at most this many.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Every playlist in the mirror, with how much of it has tracklists."""
    connection = db.connect()
    rows = service.list_playlists(connection, limit)
    if as_json:
        print_json(rows_to_dicts(rows))
        return
    if not rows:
        messages.print('Nothing mirrored yet. Run [bold]ypl sync <playlist-url>[/bold] to add one.')
        return
    table = Table(title=f'{len(rows)} playlists')
    table.add_column('Title')
    table.add_column('Videos', justify='right')
    table.add_column('Enriched', justify='right')
    # Folded, never truncated: an id with an ellipsis in it cannot be pasted
    # into the next command, which is the only reason it is in the table.
    table.add_column('Id', overflow='fold')
    for row in rows:
        table.add_row(row['title'], str(row['item_count']), str(row['enriched_count']), row['playlist_id'])
    console.print(table)


@playlists_app.command('show')
def playlists_show(
    name: str = typer.Argument(..., help='Playlist title or id.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Show at most this many videos.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """The videos in one playlist, in playlist order."""
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    rows = service.playlist_videos(connection, playlist['playlist_id'], limit)
    if as_json:
        print_json(rows_to_dicts(rows))
        return
    table = Table(title=f'{playlist["title"]} — {playlist["item_count"]} videos')
    table.add_column('#', justify='right')
    table.add_column('Title')
    table.add_column('Channel')
    table.add_column('Length', justify='right')
    table.add_column('Tracks', justify='right')
    table.add_column('Id', overflow='fold')
    for row in rows:
        title = row['title'] if not row['is_unavailable'] else f'[red]{row["title"]}[/red]'
        tracks = str(row['track_count']) if row['enriched_ts'] else '-'
        table.add_row(str(row['position']), title, row['channel'], duration_words(row['duration_seconds']), tracks, row['video_id'])
    console.print(table)


@playlists_app.command('urls')
def playlists_urls(
    name: str = typer.Argument(..., help='Playlist title or id.'),
    sort: str = typer.Option('position', '--sort', help='One of: position, oldest, newest, random.'),
    limit: int = typer.Option(None, '--limit', '-n', help='How many URLs to emit.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Emit video URLs, one per line, for piping into another tool.

    This is the command that replaces browsing the playlist and copying a link:

      ypl playlists urls 'Get Insights' --sort oldest --limit 1 | xargs relate analyze
    """
    if sort not in service.SORT_CLAUSES:
        raise typer.BadParameter(f'{sort!r} is not a sort — choose one of: {", ".join(service.SORT_CLAUSES)}', param_hint='--sort')
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    rows = service.playlist_video_urls(connection, playlist['playlist_id'], sort, limit)
    if as_json:
        print_json([dict(row) | {'url': f'https://www.youtube.com/watch?v={row["video_id"]}'} for row in rows])
        return
    for row in rows:
        print(f'https://www.youtube.com/watch?v={row["video_id"]}')


# YouTube video ids are base64url, so roughly one in thirty starts with a
# hyphen and Click would read it as an unknown option. Requiring `--` before
# every id is not a real answer when the caller is a pipeline or an agent
# pasting an id straight out of the previous command's output.
@videos_app.command('show', context_settings={'ignore_unknown_options': True})
def videos_show(
    video_id: str = typer.Argument(..., help='YouTube video id.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """One video and its tracklist."""
    connection = db.connect()
    video = service.get_video(connection, video_id)
    if not video:
        messages.print(f'[red]{video_id} is not in the mirror.[/red] Sync a playlist containing it first.')
        raise typer.Exit(1)
    tracks = service.video_tracks(connection, video_id)
    if as_json:
        print_json({'video': dict(video), 'tracks': rows_to_dicts(tracks)})
        return
    messages.print(f'[bold]{video["title"]}[/bold]')
    messages.print(f'{video["channel"]} — {duration_words(video["duration_seconds"])}')
    if not video['enriched_ts']:
        messages.print('Not enriched yet. Run [bold]ypl enrich[/bold] to pull its tracklist.')
        return
    if not tracks:
        messages.print('Enriched, but no tracklist found in its chapters or description.')
        return
    table = Table(title=f'{len(tracks)} tracks')
    table.add_column('#', justify='right')
    table.add_column('Start', justify='right')
    table.add_column('Artist')
    table.add_column('Title')
    table.add_column('Source')
    for track in tracks:
        table.add_row(
            str(track['position']),
            timestamp_words(track['start_seconds']),
            track['artist'] or '-',
            track['title'],
            track['source'],
        )
    console.print(table)


@config_app.command('init')
def config_init(
    force: bool = typer.Option(False, '--force', help='Overwrite an existing config file.'),
) -> None:
    """Write a starter config file."""
    path = paths.config_file()
    if path.exists() and not force:
        messages.print(f'[red]{path} already exists.[/red] Pass --force to overwrite it.')
        raise typer.Exit(1)
    config.write_example()
    messages.print(f'Wrote {path}')


@config_app.command('example')
def config_example() -> None:
    """Print the annotated starter config without writing anything."""
    print(config.EXAMPLE)


@config_app.command('path')
def config_path() -> None:
    """Print where the config, mirror and local playlists live."""
    print(f'config    {paths.config_file()}')
    print(f'mirror    {paths.database_file()}')
    print(f'playlists {paths.playlists_dir()}')


@config_app.command('show')
def config_show(
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Print the settings in effect, including defaults."""
    settings = config.load()
    if as_json:
        print_json({'cookies_from_browser': settings.cookies_from_browser, 'enrich_batch_size': settings.enrich_batch_size})
        return
    messages.print(f'cookies_from_browser  {settings.cookies_from_browser or "(unset)"}')
    messages.print(f'enrich_batch_size     {settings.enrich_batch_size}')


@app.command('update', rich_help_panel=ADMIN)
def update() -> None:
    """Update ypl to the latest release."""
    run_update(UPDATE_CONFIG)
