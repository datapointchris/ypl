"""The ypl command tree."""

import json
import sqlite3
import sys
from typing import Annotated

import typer
from pyselfupdate import Config as UpdateConfig
from pyselfupdate import notify
from pyselfupdate.typercmd import run_update
from rich.console import Console
from rich.table import Table

from ypl import config
from ypl import db
from ypl import local
from ypl import m3u
from ypl import paths
from ypl import service
from ypl import ytdlp
from ypl.models import watch_url

ROOT_HELP = """\
Organize YouTube playlists.

The noun comes first and the verb last, so a list -> show -> urls loop on one
playlist changes only the final word.

One rule: nothing here writes to your YouTube account. `sync` and `enrich` pull
down through yt-dlp, which costs no API quota; playlists you build are written
as M3U files on this machine, and every other command reads what is already
here.

A playlist is named by its title, so `ypl playlists show 'Get Insights'` works
without an id; a partial title matches when it is unambiguous. Run any partial
command with no arguments or --help to see what comes next.
"""

PLAYLISTS_HELP = """\
Playlists — mirrored from YouTube, and the ones you build here.

Both kinds answer to the same commands. Mirrored playlists arrive through `ypl
sync` and are read-only; local playlists are M3U files under the data directory
that mpv, VLC and Kodi can play directly.

Examples:

  ypl playlists list                              both kinds, and how much is enriched
  ypl playlists show 'Get Insights'               the videos in one playlist
  ypl playlists urls 'Get Insights' --sort oldest --limit 1
                                                  the next URL to feed to relate
  ypl playlists create 'Sunday' --from 'Deep Night' --sort random --limit 20
                                                  a new local playlist
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
BUILDING = 'Building (writes M3U files here, never to YouTube)'
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

# soft_wrap because messages carry paths, playlist ids and video ids, and Rich's
# default hard wrap inserts a real newline at the terminal width — mid-token.
# A path broken as `.../ypl/config\n.toml` cannot be copied, which defeats the
# only reason it was printed. Letting the terminal wrap instead keeps the token
# intact on the clipboard. Tables keep the wrapping console; their columns
# handle width themselves.
messages = Console(stderr=True, highlight=False, soft_wrap=True)

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


def load_config_or_exit() -> config.Config:
    """A broken config is a usage error, not a crash — it got that way by hand."""
    try:
        return config.load()
    except config.ConfigError as error:
        messages.print(f'[red]{error.path} cannot be read:[/red] {error.reason}')
        messages.print('Run [bold]ypl config example[/bold] to see a valid file.')
        raise typer.Exit(2) from error


def print_candidates(name: str, error: service.AmbiguousPlaylistError) -> None:
    messages.print(f'[red]{name!r} matches {len(error.candidates)} playlists:[/red]')
    for candidate in error.candidates:
        messages.print(f'  {candidate.title}  ({candidate.kind} {candidate.identifier})')
    messages.print('Use the full title or the identifier in brackets.')


def resolve_or_exit(connection: sqlite3.Connection, name: str) -> service.ResolvedPlaylist:
    """Turn a playlist name into one playlist, or exit with a message naming the fix."""
    try:
        return service.resolve_playlist(connection, name)
    except service.AmbiguousPlaylistError as error:
        print_candidates(name, error)
        raise typer.Exit(2) from error
    except service.PlaylistNotFoundError as error:
        messages.print(f'[red]No playlist matching {name!r}.[/red] Run [bold]ypl playlists list[/bold] to see what there is.')
        raise typer.Exit(1) from error


def local_or_exit(connection: sqlite3.Connection, name: str) -> local.LocalPlaylist:
    """Resolve a name that has to be a local playlist, because it is about to change.

    Scoped to local playlists rather than resolved across both, so that a local
    playlist named after the mirrored one it was built from stays editable
    instead of being reported as ambiguous. A name that turns out to be the
    mirrored one is told so directly — `not found` would be a lie.
    """
    try:
        resolved = service.resolve_playlist(connection, name, service.LOCAL)
    except service.PlaylistNotFoundError as error:
        if any(candidate.title.lower() == name.lower() for candidate in service.known_playlists(connection, service.REMOTE)):
            messages.print(f'[red]{name!r} is mirrored from YouTube, and nothing here writes to your account.[/red]')
            messages.print(f'Make a local copy first: [bold]ypl playlists create {name!r} --from {name!r}[/bold]')
            raise typer.Exit(1) from error
        messages.print(f'[red]No local playlist matching {name!r}.[/red] Run [bold]ypl playlists list --source local[/bold] to see them.')
        raise typer.Exit(1) from error
    except service.AmbiguousPlaylistError as error:
        print_candidates(name, error)
        raise typer.Exit(2) from error
    if resolved.local is None:
        messages.print(f'[red]{name!r} is not a local playlist.[/red]')
        raise typer.Exit(1)
    return resolved.local


def video_ids_or_exit(values: list[str]) -> list[str]:
    """Accept URLs or bare ids, and say which one was not either."""
    video_ids = []
    for value in values:
        video_id = m3u.video_id_from(value)
        if not video_id:
            messages.print(f'[red]{value!r} is not a YouTube video URL or id.[/red]')
            raise typer.Exit(2)
        video_ids.append(video_id)
    return video_ids


def video_ids_from_stdin_or_exit() -> list[str]:
    """Read piped URLs, one per line.

    This is the other half of `playlists urls`, so a selection made by one
    command becomes a playlist without anything in between parsing YouTube
    links a second time.
    """
    if sys.stdin.isatty():
        messages.print(
            '[red]Nothing piped in.[/red] Pass --from, or pipe URLs: [bold]ypl playlists urls ... | ypl playlists create NAME[/bold]'
        )
        raise typer.Exit(2)
    return video_ids_or_exit([line for line in sys.stdin.read().splitlines() if line.strip()])


@app.command('sync', rich_help_panel=SYNCING)
def sync(
    url: str = typer.Argument(..., help='Playlist URL or id. A playlist enters the mirror this way; after that its title works.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Mirror a playlist's contents locally.

    One request for the whole playlist however long it is, and no API quota.
    Re-running picks up additions, removals and reordering.
    """
    settings = load_config_or_exit()
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
    settings = load_config_or_exit()
    connection = db.connect()
    resolved = resolve_or_exit(connection, playlist) if playlist else None
    video_ids = service.unenriched_for(connection, resolved, limit or settings.enrich_batch_size)
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
    remaining = len(service.unenriched_for(connection, resolved))
    if remaining:
        messages.print(f'{remaining} still unenriched — run again to continue')


@playlists_app.command('list', rich_help_panel=READING)
def playlists_list(
    source: str = typer.Option(None, '--source', help='Limit to one kind: local, or remote.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Show at most this many.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Every playlist, mirrored and local, with how much of it has tracklists."""
    if source not in (None, service.LOCAL, service.REMOTE):
        raise typer.BadParameter(f'{source!r} is not a kind — choose {service.LOCAL} or {service.REMOTE}', param_hint='--source')
    connection = db.connect()
    rows = service.playlist_summaries(connection, source, limit)
    if as_json:
        print_json(rows)
        return
    report_unreadable_playlists(source)
    if not rows:
        messages.print('Nothing here yet. Run [bold]ypl sync <playlist-url>[/bold] to mirror one.')
        return
    table = Table(title=f'{len(rows)} playlists')
    table.add_column('Title')
    table.add_column('Kind')
    table.add_column('Videos', justify='right')
    table.add_column('Enriched', justify='right')
    # Folded, never truncated: an id with an ellipsis in it cannot be pasted
    # into the next command, which is the only reason it is in the table.
    table.add_column('Id', overflow='fold')
    for row in rows:
        table.add_row(row['title'], row['kind'], str(row['item_count']), str(row['enriched_count']), row['identifier'])
    console.print(table)


def report_unreadable_playlists(source: str | None) -> None:
    """Name any local file that could not be parsed, without failing the listing."""
    if source == service.REMOTE:
        return
    for path, reason in local.list_playlists().problems:
        messages.print(f'[red]{path} could not be read:[/red] {reason}')


@playlists_app.command('show', rich_help_panel=READING)
def playlists_show(
    name: str = typer.Argument(..., help='Playlist title or id.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Show at most this many videos.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """The videos in one playlist, in playlist order."""
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    if playlist.local is not None:
        rows = service.local_playlist_videos(connection, playlist.local, limit)
        count = len(playlist.local.entries)
    else:
        rows = rows_to_dicts(service.playlist_videos(connection, playlist.identifier, limit))
        count = playlist.remote['item_count'] if playlist.remote else len(rows)
    if as_json:
        print_json(rows)
        return
    table = Table(title=f'{playlist.title} — {count} videos')
    table.add_column('#', justify='right')
    table.add_column('Title')
    table.add_column('Channel')
    table.add_column('Length', justify='right')
    table.add_column('Tracks', justify='right')
    table.add_column('Id', overflow='fold')
    for row in rows:
        title = row['title'] or '(not synced)'
        display = title if not row['is_unavailable'] else f'[red]{title}[/red]'
        tracks = str(row['track_count']) if row['enriched_ts'] else '-'
        table.add_row(str(row['position']), display, row['channel'], duration_words(row['duration_seconds']), tracks, row['video_id'])
    console.print(table)


@playlists_app.command('urls', rich_help_panel=READING)
def playlists_urls(
    name: str = typer.Argument(..., help='Playlist title or id.'),
    sort: str = typer.Option('position', '--sort', help='One of: position, oldest, newest, random.'),
    limit: int = typer.Option(None, '--limit', '-n', help='How many URLs to emit.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Emit video URLs, one per line, for piping into another tool.

    This is the command that replaces browsing the playlist and copying a link:

      ypl playlists urls 'Get Insights' --sort oldest --limit 1 | xargs relate analyze

    It is also how a selection becomes a playlist:

      ypl playlists urls 'Deep Night' --sort random --limit 20 | ypl playlists create 'Sunday'
    """
    if sort not in service.SORT_CLAUSES:
        raise typer.BadParameter(f'{sort!r} is not a sort — choose one of: {", ".join(service.SORT_CLAUSES)}', param_hint='--sort')
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    rows = selected_videos(connection, playlist, sort, limit)
    if as_json:
        print_json([row | {'url': watch_url(row['video_id'])} for row in rows])
        return
    for row in rows:
        print(watch_url(row['video_id']))


def selected_videos(connection: sqlite3.Connection, playlist: service.ResolvedPlaylist, sort: str, limit: int | None) -> list[dict]:
    """The sorted, playable videos of either kind of playlist."""
    if playlist.local is not None:
        return service.local_playlist_video_urls(connection, playlist.local, sort, limit)
    return rows_to_dicts(service.playlist_video_urls(connection, playlist.identifier, sort, limit))


@playlists_app.command('create', rich_help_panel=BUILDING)
def playlists_create(
    name: str = typer.Argument(..., help='What to call the new playlist.'),
    from_playlist: str = typer.Option(None, '--from', '-f', help='Take the videos from this playlist, mirrored or local.'),
    sort: str = typer.Option('position', '--sort', help='One of: position, oldest, newest, random.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Take at most this many videos.'),
    force: bool = typer.Option(False, '--force', help='Overwrite an existing playlist of the same name.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Write a new local playlist, from another playlist or from piped URLs.

    Nothing reaches YouTube: this writes one M3U file, which mpv, VLC and Kodi
    play directly and which costs no quota to build, reorder or throw away.
    """
    if sort not in service.SORT_CLAUSES:
        raise typer.BadParameter(f'{sort!r} is not a sort — choose one of: {", ".join(service.SORT_CLAUSES)}', param_hint='--sort')
    connection = db.connect()
    if from_playlist:
        source = resolve_or_exit(connection, from_playlist)
        video_ids = [row['video_id'] for row in selected_videos(connection, source, sort, limit)]
        provenance = f'{source.kind} {source.identifier}'
    else:
        # --sort is not applied here: whatever produced the pipe already chose
        # an order, and re-sorting it would silently discard that choice.
        piped = video_ids_from_stdin_or_exit()
        video_ids = piped[:limit] if limit else piped
        provenance = 'stdin'

    try:
        playlist = service.create_local_playlist(connection, name, video_ids, source=provenance, overwrite=force)
    except local.LocalPlaylistExistsError as error:
        messages.print(f'[red]{error.path} already exists.[/red] Pass --force to overwrite it.')
        raise typer.Exit(1) from error

    if as_json:
        print_json({'name': playlist.name, 'slug': playlist.slug, 'path': str(playlist.path), 'video_count': len(playlist.entries)})
        return
    messages.print(f'Wrote [bold]{playlist.name}[/bold] — {len(playlist.entries)} videos')
    messages.print(str(playlist.path))


# Bare video ids arrive here as arguments and one in thirty begins with a
# hyphen, which Click would read as an unknown option.
@playlists_app.command('add', rich_help_panel=BUILDING, context_settings={'ignore_unknown_options': True})
def playlists_add(
    name: str = typer.Argument(..., help='Local playlist to append to.'),
    # Annotated form because a list default built by a call is the shared-mutable
    # -default bug the linter is right about, whatever Typer does with it.
    videos: Annotated[list[str] | None, typer.Argument(help='Video URLs or ids. Read from stdin when none are given.')] = None,
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Append videos to a local playlist."""
    connection = db.connect()
    playlist = local_or_exit(connection, name)
    video_ids = video_ids_or_exit(videos) if videos else video_ids_from_stdin_or_exit()
    added = service.add_to_local_playlist(connection, playlist, video_ids)
    if as_json:
        print_json({'name': playlist.name, 'added': added, 'video_count': len(playlist.entries)})
        return
    messages.print(f'Added [bold]{added}[/bold] to {playlist.name} — {len(playlist.entries)} videos')


@playlists_app.command('remove', rich_help_panel=BUILDING, context_settings={'ignore_unknown_options': True})
def playlists_remove(
    name: str = typer.Argument(..., help='Local playlist to remove from.'),
    videos: Annotated[list[str] | None, typer.Argument(help='Video URLs or ids. Read from stdin when none are given.')] = None,
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Drop videos from a local playlist, wherever they appear in it."""
    connection = db.connect()
    playlist = local_or_exit(connection, name)
    video_ids = video_ids_or_exit(videos) if videos else video_ids_from_stdin_or_exit()
    removed = service.remove_from_local_playlist(playlist, video_ids)
    if as_json:
        print_json({'name': playlist.name, 'removed': removed, 'video_count': len(playlist.entries)})
        return
    if not removed:
        messages.print(f'None of those are in {playlist.name}.')
        return
    messages.print(f'Removed [bold]{removed}[/bold] from {playlist.name} — {len(playlist.entries)} videos')


@playlists_app.command('delete', rich_help_panel=BUILDING)
def playlists_delete(
    name: str = typer.Argument(..., help='Local playlist to delete.'),
    yes: bool = typer.Option(False, '--yes', '-y', help='Skip the confirmation.'),
) -> None:
    """Delete a local playlist file.

    Confirmed rather than forced, because unlike the mirror there is nothing to
    rebuild it from.
    """
    connection = db.connect()
    playlist = local_or_exit(connection, name)
    if not yes:
        typer.confirm(f'Delete {playlist.name} ({len(playlist.entries)} videos) at {playlist.path}?', abort=True)
    local.delete(playlist)
    messages.print(f'Deleted {playlist.path}')


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
    settings = load_config_or_exit()
    if as_json:
        print_json({'cookies_from_browser': settings.cookies_from_browser, 'enrich_batch_size': settings.enrich_batch_size})
        return
    messages.print(f'cookies_from_browser  {settings.cookies_from_browser or "(unset)"}')
    messages.print(f'enrich_batch_size     {settings.enrich_batch_size}')


@app.command('update', rich_help_panel=ADMIN)
def update() -> None:
    """Update ypl to the latest release."""
    run_update(UPDATE_CONFIG)
