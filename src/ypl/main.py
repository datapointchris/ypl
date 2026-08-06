"""The ypl command tree."""

import contextlib
import dataclasses
import importlib.metadata
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

from ypl import basestore
from ypl import config
from ypl import db
from ypl import declined
from ypl import editbuffer
from ypl import history
from ypl import local
from ypl import m3u
from ypl import merge
from ypl import paths
from ypl import player
from ypl import remote
from ypl import runlock
from ypl import schedule
from ypl import service
from ypl import session
from ypl import synclog
from ypl import throttle
from ypl import ytdlp
from ypl import ytmusic
from ypl.models import watch_url

ROOT_HELP = """\
Organize YouTube playlists.

The noun comes first and the verb last, so a list -> show -> urls loop on one
playlist changes only the final word.

`ypl sync` does all of it: mirrors your account, takes over playlists made in
the web player, brings down changes made on your phone, sends up changes made
here, and fills in tracklists with whatever time is left. Run it once and it
keeps running itself, at startup and on an interval. Sign in with `ypl remote
auth` and that is the setup; `ypl status` says whether it is working.

Organizing happens locally and instantly: playlists you build are M3U files on
this machine, playable straight away. The `ypl remote` verbs are the same work
done deliberately, for when a piece of it needs forcing.

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
  ypl playlists split 'Deep Night' --size 90      cut a long one into parts
  ypl playlists order 'Sunday' --sort longest     rearrange one you built
"""

VIDEOS_HELP = """\
Videos, and the tracklists parsed out of them.

Examples:

  ypl videos show dQw4w9WgXcQ                     the tracklist with chapter timestamps
"""

PLAYS_HELP = """\
The listening history behind `ypl next`.

Recorded when a listen is logged, not inferred from playback — `ypl play` hands
mpv the whole list at once and never learns which of it got played.

Examples:

  ypl plays add dQw4w9WgXcQ                       record a listen
  ypl plays list                                  what has been played lately
"""

CONFIG_HELP = """\
The settings file at $XDG_CONFIG_HOME/ypl/config.toml.

Examples:

  ypl config init                                 write a starter file
  ypl config example                              print it without writing anything
  ypl config path                                 where it would be read from
"""

REMOTE_HELP = """\
Going back up to YouTube.

Everything else in ypl reads or writes locally. These commands are the only
ones that change anything on YouTube, and they are deliberately slow: every
call is throttled, and a rate limit stops the run rather than being retried
into.

Examples:

  ypl remote auth                                 sign in, once per machine
  ypl remote pull                                 reconcile with YouTube
  ypl remote plan                                 what would go up
  ypl remote apply                                send it
  ypl remote apply --limit 5                      a drain on a timer
"""

# The fallback, and it stays a fallback: --browser needs none of this. Ctrl-D
# is called out twice over because it only signals EOF at the start of a line,
# so a paste with no trailing newline swallows the first one and looks broken.
AUTH_INSTRUCTIONS = """\
[bold]ypl remote auth --browser safari[/bold] does this without DevTools — the
session comes out of a browser you are already signed in to. Otherwise:

Sign in at [bold]music.youtube.com[/bold], open DevTools on the Network tab,
and click something — Library, or any playlist — to make the page talk; it
records only while it is open. Filter for [bold]/youtubei/[/bold], pick any
POST, and copy its whole Request Headers block.

Paste it below, then press Enter and Ctrl-D.
"""

READING = 'Reading (the local mirror)'
SYNCING = 'Syncing (pulls from YouTube, no API quota)'
BUILDING = 'Building (writes M3U files here; YouTube only via ypl remote)'
PLAYING = 'Listening (playing, and changing what is on while it plays)'
WRITING = 'Writing (the only commands that reach YouTube)'
ADMIN = 'Admin'

# `status` is a glance, and a run where every playlist failed printed one line
# each — which pushed the lines answering "is this working" off the screen. The
# whole list is still in the log, and `--json` still carries all of them.
STATUS_FAILURES_SHOWN = 5

app = typer.Typer(name='ypl', no_args_is_help=True, help=ROOT_HELP)
playlists_app = typer.Typer(name='playlists', no_args_is_help=True, help=PLAYLISTS_HELP)
videos_app = typer.Typer(name='videos', no_args_is_help=True, help=VIDEOS_HELP)
config_app = typer.Typer(name='config', no_args_is_help=True, help=CONFIG_HELP)
plays_app = typer.Typer(name='plays', no_args_is_help=True, help=PLAYS_HELP)
remote_app = typer.Typer(name='remote', no_args_is_help=True, help=REMOTE_HELP)

app.add_typer(playlists_app, name='playlists', rich_help_panel=READING)
app.add_typer(videos_app, name='videos', rich_help_panel=READING)
app.add_typer(plays_app, name='plays', rich_help_panel=PLAYING)
app.add_typer(remote_app, name='remote', rich_help_panel=WRITING)
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


def installed_version() -> str:
    try:
        return importlib.metadata.version('ypl')
    except importlib.metadata.PackageNotFoundError:
        return 'unknown'


def installed_commit() -> str:
    """The commit this build came from, when uv installed it from a git ref.

    An install from a release does not need one — the version identifies it
    exactly. One installed from a branch reports the same version for as long as
    the branch moves, and that is the case this answers.
    """
    with contextlib.suppress(importlib.metadata.PackageNotFoundError, OSError, json.JSONDecodeError):
        direct_url = importlib.metadata.distribution('ypl').read_text('direct_url.json')
        if direct_url:
            return json.loads(direct_url).get('vcs_info', {}).get('commit_id') or ''
    return ''


def show_version(asked: bool) -> None:
    """`ypl --version`, in the one line every CLI here answers it with."""
    if not asked:
        return
    commit = installed_commit()
    console.print(f'ypl {installed_version()}{f" @ {commit[:8]}" if commit else ""}')
    raise typer.Exit()


@app.callback()
def root(
    ctx: typer.Context,
    version: Annotated[
        bool | None,
        typer.Option('--version', callback=show_version, is_eager=True, help='Show the installed version and exit.'),
    ] = None,
) -> None:
    if ctx.invoked_subcommand != 'update':
        notify(UPDATE_CONFIG)


def matching_playlist_titles(incomplete: str, kind: str | None) -> list[str]:
    """Playlist titles for the shell to offer.

    Every failure is swallowed and answered with nothing. A completion runs on
    a keystroke, in a shell that will happily print a traceback into the middle
    of the line being typed, and no reason for one — an empty mirror, a
    database mid-write, a config that stopped parsing — is worth that.
    """
    try:
        connection = db.connect()
        titles = [playlist.title for playlist in service.known_playlists(connection, kind)]
    except Exception:  # noqa: BLE001 - see the docstring; a completion never raises
        return []
    # Slugified on both sides, so that what completes and what resolves agree:
    # typing `six hour` has to offer the `six-hour-work` this tool named itself.
    typed = local.slugify(incomplete)
    return [title for title in titles if typed in local.slugify(title)]


# Typer reads a completion callback's signature and refuses any parameter it
# does not recognise, so these take `incomplete` and nothing else. The shared
# work lives in a function that is not a callback.
def complete_playlist(incomplete: str) -> list[str]:
    return matching_playlist_titles(incomplete, None)


def complete_local_playlist(incomplete: str) -> list[str]:
    """Only the playlists a command could actually change."""
    return matching_playlist_titles(incomplete, service.LOCAL)


def complete_entry(incomplete: str) -> list[str]:
    """Titles inside the current playlist, for the verbs that take a fragment.

    The same reasoning as the ids: what you can say about the thing playing is
    its name, so the shell should finish it for you.
    """
    try:
        connection = db.connect()
        chosen = session.current()
        if not chosen:
            return []
        playlist = service.resolve_playlist(connection, chosen, service.LOCAL)
        rows = service.local_playlist_videos(connection, playlist.local) if playlist.local else []
    except Exception:  # noqa: BLE001 - see complete_playlist
        return []
    lowered = incomplete.lower()
    return [row['title'] for row in rows if row['title'] and lowered in row['title'].lower()]


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


def reading_browser(settings: config.Config, override: str | None = None) -> str | None:
    """Which browser's cookies a read should borrow.

    An explicit flag wins, then the config, then whatever browser signing in
    last worked with. That last step is the one that matters: `ypl remote auth`
    proves a browser holds a YouTube session, and a tool that then asks you to
    tell it the same thing again in a config file is asking you to log in
    twice.
    """
    return override or settings.cookies_from_browser or session.browser() or None


def load_config_or_exit() -> config.Config:
    """A broken config is a usage error, not a crash — it got that way by hand."""
    try:
        return config.load()
    except config.ConfigError as error:
        messages.print(f'[red]{error.path} cannot be read:[/red] {error.reason}')
        messages.print('Run [bold]ypl config example[/bold] to see a valid file.')
        raise typer.Exit(2) from error


def report_unavailable_skipped(total: int, kept: int) -> None:
    """Say what a selection left behind.

    Deleted and private videos are dropped from anything that produces something
    to play, and a playlist that comes out five short with no explanation reads
    as a bug in the split.
    """
    if total > kept:
        messages.print(f'{total - kept} unavailable videos left out')


def check_sort(sort: str) -> None:
    if sort not in service.SORT_CLAUSES:
        raise typer.BadParameter(f'{sort!r} is not a sort — choose one of: {", ".join(service.SORT_CLAUSES)}', param_hint='--sort')


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
            messages.print(f'[red]{name!r} is mirrored from YouTube, and a mirrored playlist is read-only here.[/red]')
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


def video_id_argument(value: str) -> str:
    """A URL or a bare id, without the eleven-character rule.

    That rule earns its place where nothing downstream checks the id — adding a
    video to a playlist file writes whatever it is told. Here the mirror is
    about to reject an id it does not know, so guessing first would only replace
    a precise error with a vague one.
    """
    return m3u.video_id_from(value) or value.strip()


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


def signed_in_backend(quiet: bool) -> ytmusic.YtMusicBackend | None:
    """The write backend when this machine has signed in, and None when it has not.

    None rather than an exit: mirroring needs no session, and a machine that has
    never run `remote auth` must still get a working `ypl sync`. Signing in is
    per-machine setup, and holding the whole sync hostage to it would make the
    automatic case fail on exactly the machine nobody is watching.
    """
    if not paths.ytmusic_auth_file().exists():
        if not quiet:
            messages.print('Not signed in — mirroring only. [bold]ypl remote auth --browser safari[/bold] syncs both ways.')
        return None
    # Rebuilt from the browser first, because the stored copy goes stale on its
    # own: Google rotates the session cookies while you stay signed in.
    ytmusic.refresh_session(session.browser(), paths.ytmusic_auth_file())
    try:
        return ytmusic.YtMusicBackend(paths.ytmusic_auth_file())
    except remote.RemoteError as error:
        if not quiet:
            messages.print(f'[yellow]Signed-in session unusable, mirroring only:[/yellow] {error}')
        return None


def sync_everything_or_exit(connection: sqlite3.Connection, browser: str, limit: int | None, quiet: bool) -> service.SyncRun:
    settings = load_config_or_exit()

    def announce(reference) -> None:
        if not quiet:
            messages.print(f'  {reference.title}')

    def announce_video(index: int, total: int, video_id: str) -> None:
        if not quiet:
            messages.print(f'  [{index}/{total}] enriching {video_id}')

    try:
        return service.sync_everything(
            connection,
            browser,
            backend=signed_in_backend(quiet),
            limit=limit,
            on_playlist=announce,
            on_video=announce_video,
            pace=throttle.Throttle(settings.request_interval_seconds),
            budget=throttle.Budget(seconds=settings.sync_seconds),
        )
    except ytdlp.YtdlpUnavailableError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    except ytdlp.YtdlpRateLimitedError as error:
        messages.print(f'[red]YouTube is pushing back:[/red] {error}')
        messages.print('Nothing is lost — the next run picks up where this stopped.')
        raise typer.Exit(1) from error
    except ytdlp.YtdlpFailedError as error:
        messages.print('[red]Could not list your playlists.[/red] Is that browser signed in to YouTube?')
        messages.print(str(error))
        raise typer.Exit(1) from error


def keep_running(settings: config.Config, quiet: bool) -> None:
    """Leave this machine set up to sync itself, from the first run onwards.

    Part of syncing rather than a command of its own: a timer you have to
    install is one more thing to remember, and remembering is the problem the
    timer exists to solve. The first run installs it, every later one finds it
    already there, and `background_sync = false` in the config takes it away
    again — no verb to undo what no verb created.
    """
    was = schedule.installed()
    timer = schedule.ensure(settings.background_sync_minutes, wanted=settings.background_sync)
    if quiet or (was and timer):
        return
    if timer:
        messages.print(f'Now syncing on its own — every {timer.interval_minutes} minutes, and at startup ({timer.path})')
    elif was:
        messages.print('Background syncing turned off — the timer is gone. Run this again whenever you want.')


def report_sync(run: service.SyncRun) -> None:
    """What a run did, in one line per thing that actually happened."""
    messages.print(f'Mirrored [bold]{run.mirrored}[/bold] playlists, {run.videos} videos')
    for pull in run.reconciled:
        report_pull(pull)
    for push in run.pushed:
        messages.print(f'[bold]{push.playlist.name}[/bold] — pushed: {describe_push(push)}')
    for name in run.adopted:
        messages.print(f'[bold]{name}[/bold] — adopted from YouTube')
    if run.enriched:
        messages.print(f'Enriched [bold]{run.enriched}[/bold] videos, found {run.tracks_found} tracks')
    if run.skipped:
        messages.print(f'{len(run.skipped)} videos could not be read — deleted, private or region-locked')
    if run.withheld:
        messages.print(
            f'{len(run.withheld)} not public, so YouTube Music will not serve them — '
            f'mirrored and readable here, but not adopted or pushed: {", ".join(run.withheld)}'
        )
    for name, reason in run.failures:
        messages.print(f'[yellow]{name}[/yellow]: {reason}')
    if run.stopped == 'budget':
        messages.print(
            f'Stopped at the {round(run.seconds / 60)} minute budget — {run.unenriched} videos still to enrich, next run continues.'
        )
    elif run.stopped == 'rate limit':
        messages.print('[red]Stopped — YouTube asked us to slow down.[/red] Everything done so far is stored.')
    elif run.unenriched:
        messages.print(f'{run.unenriched} videos still to enrich — next run continues.')
    elif not run.changed:
        messages.print('Everything already in sync.')


@app.command('sync', rich_help_panel=SYNCING)
def sync(
    url: str = typer.Argument(None, help='Playlist URL or id. Every playlist in your account when omitted.'),
    browser: str = typer.Option(None, '--browser', '-b', help='Browser whose YouTube session to borrow. Defaults to cookies_from_browser.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Mirror at most this many playlists.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Sync everything, in both directions.

    One command and no order to remember: your playlists are mirrored, any that
    are new here are taken over, changes made on a phone arrive, and changes
    made here go up. Signing in once with [bold]ypl remote auth[/bold] is the
    last thing it asks of you — before that, and on a machine that has never
    signed in, this mirrors and stops.

    Cheap enough to run on a timer, because the mirror read costs no API quota
    and is also what says which playlists changed: one whose mirror still
    matches its file is never read again through the write client. A library
    where nothing moved overnight spends nothing.

    A single playlist by URL only mirrors it, and needs nothing if it is public.
    """
    settings = load_config_or_exit()
    connection = db.connect()

    if url is None:
        source = reading_browser(settings, browser)
        if not source:
            messages.print('[red]Nothing to sync from.[/red] Give a playlist URL, or name a browser to read your account:')
            messages.print('  [bold]ypl sync --browser safari[/bold]  — remembered afterwards, so once is enough')
            messages.print('  [bold]ypl remote auth --browser safari[/bold]  — signs in for writing too')
            raise typer.Exit(2)

        with runlock.held() as mine:
            # Exit 0: a timer firing onto a run already going is the system
            # working. Failing here would fill the agent's log with errors
            # describing a sync that is, at that moment, happening.
            if not mine:
                if as_json:
                    print_json({'skipped': 'a sync is already running'})
                else:
                    messages.print('A sync is already running — leaving it to finish.')
                return

            if not as_json:
                messages.print('Reading your playlists...')
            run = sync_everything_or_exit(connection, source, limit, quiet=as_json)
            session.remember_browser(source)
            synclog.record(run.payload())
            keep_running(settings, quiet=as_json)
        if as_json:
            print_json(run.payload())
        else:
            report_sync(run)
        if run.failures:
            raise typer.Exit(1)
        return

    try:
        playlist = service.sync_playlist(connection, url, cookies_from_browser=reading_browser(settings, browser))
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
    playlist: str = typer.Option(None, '--playlist', '-p', help='Limit to one playlist, by title or id.', autocompletion=complete_playlist),
    limit: int = typer.Option(None, '--limit', '-n', help='How many videos to fetch this run.'),
    everything: bool = typer.Option(False, '--all', help='Keep going until nothing is left, however long that takes.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Fetch each video in full and parse its tracklist.

    `ypl sync` already does this with whatever time its budget leaves, so this
    is the forcing version: one playlist now, or a long run in the foreground
    rather than waiting for the timer to work through it.

    Chapters where a video has them, timestamped description lines where it does
    not. One request per video, so this is the slow half — it is resumable, and
    a video already enriched is skipped. Stopping it costs nothing: every video
    enriched is already stored.
    """
    settings = load_config_or_exit()
    connection = db.connect()
    resolved = resolve_or_exit(connection, playlist) if playlist else None
    batch = None if everything else limit or settings.enrich_batch_size
    video_ids = service.unenriched_for(connection, resolved, batch)
    if not video_ids:
        messages.print('Nothing left to enrich.')
        if as_json:
            print_json({'enriched': 0, 'tracks_found': 0, 'failed': 0})
        return

    enriched = 0
    tracks_found = 0
    failures: list[dict] = []
    stopped = ''
    # Paced, because this is the one command that makes thousands of requests
    # in a row. A rate limit ends the run rather than being retried into: every
    # video already enriched is stored, so stopping costs nothing but time.
    pace = throttle.Throttle(settings.request_interval_seconds)
    with messages.status(f'Enriching {len(video_ids)} videos...') as status:
        for index, video_id in enumerate(video_ids, start=1):
            status.update(f'[{index}/{len(video_ids)}] {video_id}')
            pace.wait()
            try:
                tracks = service.enrich_video(connection, video_id, cookies_from_browser=reading_browser(settings))
            except ytdlp.YtdlpRateLimitedError as error:
                stopped = str(error).splitlines()[-1] if str(error) else 'YouTube asked us to slow down'
                break
            except ytdlp.YtdlpFailedError as error:
                failures.append({'video_id': video_id, 'error': str(error).splitlines()[-1] if str(error) else 'unknown'})
                continue
            enriched += 1
            tracks_found += len(tracks)

    if as_json:
        print_json(
            {
                'enriched': enriched,
                'tracks_found': tracks_found,
                'failed': len(failures),
                'failures': failures,
                'rate_limited': bool(stopped),
            }
        )
        if stopped:
            raise typer.Exit(1)
        return
    messages.print(f'Enriched [bold]{enriched}[/bold] videos, found [bold]{tracks_found}[/bold] tracks')
    if failures:
        messages.print(f'{len(failures)} failed:')
        for failure in failures:
            messages.print(f'  {failure["video_id"]}  {failure["error"]}')
    remaining = len(service.unenriched_for(connection, resolved))
    if stopped:
        messages.print(f'[red]Stopped — YouTube is pushing back:[/red] {stopped}')
        messages.print(f'{remaining} left. Leave it a few hours; everything enriched so far is already stored.')
        messages.print('Raise [bold]request_interval_seconds[/bold] in the config to go gentler next time.')
        raise typer.Exit(1)
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
    # Sync state rather than store, because "is this on YouTube yet" is the
    # question a listing gets asked. Mirrored playlists read `remote`.
    table.add_column('State')
    table.add_column('Videos', justify='right')
    table.add_column('Enriched', justify='right')
    # Folded, never truncated: an id with an ellipsis in it cannot be pasted
    # into the next command, which is the only reason it is in the table.
    table.add_column('Id', overflow='fold')
    for row in rows:
        table.add_row(row['title'], row['sync_state'], str(row['item_count']), str(row['enriched_count']), row['identifier'])
    console.print(table)


def report_unreadable_playlists(source: str | None) -> None:
    """Name any local file that could not be parsed, without failing the listing."""
    if source == service.REMOTE:
        return
    for path, reason in local.list_playlists().problems:
        messages.print(f'[red]{path} could not be read:[/red] {reason}')


@playlists_app.command('show', rich_help_panel=READING)
def playlists_show(
    name: str = typer.Argument(..., help='Playlist title or id.', autocompletion=complete_playlist),
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
    name: str = typer.Argument(..., help='Playlist title or id.', autocompletion=complete_playlist),
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
    check_sort(sort)
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    rows = service.playlist_selection(connection, playlist, sort, limit)
    if as_json:
        print_json([row | {'url': watch_url(row['video_id'])} for row in rows])
        return
    for row in rows:
        print(watch_url(row['video_id']))


@playlists_app.command('create', rich_help_panel=BUILDING)
def playlists_create(
    name: str = typer.Argument(..., help='What to call the new playlist. Kebab-cased, whatever you type.'),
    from_playlist: str = typer.Option(None, '--from', '-f', help='Take the videos from this playlist, mirrored or local.'),
    sort: str = typer.Option('position', '--sort', help=f'One of: {", ".join(service.SORT_CLAUSES)}.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Take at most this many videos.'),
    force: bool = typer.Option(False, '--force', help='Overwrite an existing playlist of the same name.'),
    local_only: bool = typer.Option(False, '--local', help='Keep it here — do not sync it to YouTube.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Write a new local playlist, from another playlist or from piped URLs.

    The file is written immediately and nothing reaches YouTube in this
    command — mpv, VLC and Kodi play it straight away. It is marked for syncing
    by default, so it appears on your phone once the queue next drains. Pass
    --local for one that should stay on this machine.

    The name is kebab-cased: 'Six Hour Work' becomes six-hour-work, here and on
    YouTube. Playlists made in the web player keep the names they were given.
    """
    check_sort(sort)
    connection = db.connect()
    source = None
    if from_playlist:
        source = resolve_or_exit(connection, from_playlist)
        video_ids = [row['video_id'] for row in service.playlist_selection(connection, source, sort, limit)]
        provenance = f'{source.kind} {source.identifier}'
    else:
        # --sort is not applied here: whatever produced the pipe already chose
        # an order, and re-sorting it would silently discard that choice.
        piped = video_ids_from_stdin_or_exit()
        video_ids = piped[:limit] if limit else piped
        provenance = 'stdin'

    try:
        playlist = service.create_local_playlist(connection, name, video_ids, source=provenance, overwrite=force, synced=not local_only)
    except local.LocalPlaylistExistsError as error:
        messages.print(f'[red]{error.path} already exists.[/red] Pass --force to overwrite it.')
        raise typer.Exit(1) from error

    if as_json:
        print_json({'name': playlist.name, 'slug': playlist.slug, 'path': str(playlist.path), 'video_count': len(playlist.entries)})
        return
    messages.print(f'Wrote [bold]{playlist.name}[/bold] — {len(playlist.entries)} videos')
    if source is not None and not limit:
        report_unavailable_skipped(service.playlist_video_count(connection, source), len(playlist.entries))
    messages.print(str(playlist.path))


# Bare video ids arrive here as arguments and one in thirty begins with a
# hyphen, which Click would read as an unknown option.
@playlists_app.command('add', rich_help_panel=BUILDING, context_settings={'ignore_unknown_options': True})
def playlists_add(
    name: str = typer.Argument(..., help='Local playlist to append to.', autocompletion=complete_local_playlist),
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
    name: str = typer.Argument(..., help='Local playlist to remove from.', autocompletion=complete_local_playlist),
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


@playlists_app.command('split', rich_help_panel=BUILDING)
def playlists_split(
    name: str = typer.Argument(..., help='Playlist to cut up, mirrored or local.', autocompletion=complete_playlist),
    size: int = typer.Option(None, '--size', '-s', help='Roughly how many videos per part.'),
    parts: int = typer.Option(None, '--parts', '-p', help='How many parts to cut it into.'),
    prefix: str = typer.Option(None, '--name', help="What to call the parts. Defaults to the source playlist's title."),
    sort: str = typer.Option('position', '--sort', help=f'One of: {", ".join(service.SORT_CLAUSES)}.'),
    force: bool = typer.Option(False, '--force', help='Overwrite parts left by an earlier split.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Cut a long playlist into several local ones.

    Parts come out evenly rather than as full-size chunks and a stub: 140 videos
    at a size of 90 is two parts of 70, not a 90 and a 50.

      ypl playlists split 'Deep Night' --size 90 --sort random
    """
    if (size is None) == (parts is None):
        raise typer.BadParameter('pass either --size or --parts, not both', param_hint='--size / --parts')
    if (size is not None and size < 1) or (parts is not None and parts < 1):
        raise typer.BadParameter('must be at least 1', param_hint='--size / --parts')
    check_sort(sort)

    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    try:
        created = service.split_playlist(connection, playlist, prefix or playlist.title, size, parts, sort, overwrite=force)
    except local.LocalPlaylistExistsError as error:
        messages.print(f'[red]{error.path} already exists.[/red] Pass --force to overwrite the earlier split.')
        raise typer.Exit(1) from error

    if as_json:
        print_json([{'name': part.name, 'slug': part.slug, 'path': str(part.path), 'video_count': len(part.entries)} for part in created])
        return
    if not created:
        messages.print(f'[red]{playlist.title} has no playable videos to split.[/red]')
        raise typer.Exit(1)
    messages.print(f'Cut [bold]{playlist.title}[/bold] into {len(created)} playlists:')
    for part in created:
        messages.print(f'  {part.name}  {len(part.entries)} videos')
    report_unavailable_skipped(service.playlist_video_count(connection, playlist), sum(len(part.entries) for part in created))


@playlists_app.command('order', rich_help_panel=BUILDING)
def playlists_order(
    name: str = typer.Argument(..., help='Local playlist to reorder.', autocompletion=complete_local_playlist),
    sort: str = typer.Option(..., '--sort', help=f'One of: {", ".join(service.SORT_CLAUSES)}.'),
    into: str = typer.Option(None, '--into', help='Write the result as a new playlist instead of reordering this one.'),
    force: bool = typer.Option(False, '--force', help='Overwrite the playlist named by --into.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Reorder a local playlist.

    Unavailable videos are kept — this rearranges a playlist rather than
    selecting from one.

      ypl playlists order 'Sunday' --sort longest --into 'Sunday Long'
    """
    check_sort(sort)
    connection = db.connect()
    playlist = local_or_exit(connection, name)
    try:
        ordered = service.order_local_playlist(connection, playlist, sort, into, overwrite=force)
    except local.LocalPlaylistExistsError as error:
        messages.print(f'[red]{error.path} already exists.[/red] Pass --force to overwrite it.')
        raise typer.Exit(1) from error

    if as_json:
        print_json({'name': ordered.name, 'slug': ordered.slug, 'path': str(ordered.path), 'video_count': len(ordered.entries)})
        return
    messages.print(f'Ordered [bold]{ordered.name}[/bold] by {sort} — {len(ordered.entries)} videos')
    messages.print(str(ordered.path))


@playlists_app.command('edit', rich_help_panel=BUILDING)
def playlists_edit(
    name: str = typer.Argument(
        None, help='Local playlist to rearrange. The current one when omitted.', autocompletion=complete_local_playlist
    ),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Rearrange a playlist in your editor.

    Opens one line per video — the id first, then the title — in $EDITOR. Move
    lines to reorder, delete a line to remove that video, paste a URL on its own
    line to add one. Save to apply, or save an empty buffer to abort.

    Modelled on `git rebase -i`, because rearranging a list is something your
    editor is already better at than any command could be. Reads the buffer from
    stdin instead when something is piped in.
    """
    connection = db.connect()
    playlist = current_playlist_or_exit(connection, name)
    rows = service.local_playlist_videos(connection, playlist)

    if sys.stdin.isatty():
        try:
            edited = editbuffer.open_in_editor(editbuffer.render(playlist.name, rows))
        except editbuffer.EditorFailedError as error:
            messages.print(f'[red]{error}[/red]')
            raise typer.Exit(1) from error
        if edited is None:
            messages.print(f'[bold]{playlist.name}[/bold] unchanged')
            return
    else:
        edited = sys.stdin.read()

    try:
        video_ids = editbuffer.parse(edited, m3u.video_id_from, frozenset(playlist.video_ids))
    except editbuffer.EditBufferError as error:
        messages.print(f'[red]{error}[/red]')
        messages.print('Nothing was changed.')
        raise typer.Exit(2) from error

    if not video_ids:
        # An empty buffer aborts rather than emptying the playlist, which is
        # what `git rebase -i` does and what someone who deleted every line by
        # accident needs it to do.
        messages.print(f'[bold]{playlist.name}[/bold] unchanged — an empty buffer aborts')
        return

    unknown = [video_id for video_id in video_ids if not m3u.is_video_id(video_id)]
    result = service.apply_edit(connection, playlist, video_ids)

    if as_json:
        print_json(
            {
                'name': result.playlist.name,
                'slug': result.playlist.slug,
                'video_count': len(result.playlist.entries),
                'added': result.added,
                'removed': result.removed,
                'reordered': result.reordered,
            }
        )
        return
    if not result.changed:
        messages.print(f'[bold]{result.playlist.name}[/bold] unchanged')
        return
    changes = []
    if result.added:
        changes.append(f'{len(result.added)} added')
    if result.removed:
        changes.append(f'{len(result.removed)} removed')
    if result.reordered:
        changes.append('reordered')
    messages.print(f'[bold]{result.playlist.name}[/bold] — {", ".join(changes)}, {len(result.playlist.entries)} videos')
    if unknown:
        messages.print(f'{len(unknown)} of those are not eleven-character ids — check them if a player rejects the file')
    if result.playlist.synced:
        messages.print('Run [bold]ypl remote plan[/bold] to see what that means for YouTube')


@playlists_app.command('promote', rich_help_panel=BUILDING)
def playlists_promote(
    name: str = typer.Argument(..., help='Local playlist to start syncing.', autocompletion=complete_local_playlist),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Start syncing a local playlist to YouTube.

    It goes up on the next drain and appears in your library on every device
    you are signed in on. Playlists are made synced by default, so this is for
    one that was made with --local or demoted since.
    """
    connection = db.connect()
    playlist = service.set_synced(local_or_exit(connection, name), True)
    if as_json:
        print_json({'name': playlist.name, 'slug': playlist.slug, 'sync_state': playlist.sync_state})
        return
    messages.print(f'[bold]{playlist.name}[/bold] will sync — it goes up on the next [bold]ypl remote apply[/bold]')


@playlists_app.command('demote', rich_help_panel=BUILDING)
def playlists_demote(
    name: str = typer.Argument(..., help='Playlist to stop syncing.', autocompletion=complete_local_playlist),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Stop syncing a playlist, keeping the local file.

    Anything already on YouTube is left alone — this changes what ypl pushes
    from here, and does not reach across to delete it.
    """
    connection = db.connect()
    playlist = service.set_synced(local_or_exit(connection, name), False)
    if as_json:
        print_json({'name': playlist.name, 'slug': playlist.slug, 'sync_state': playlist.sync_state})
        return
    messages.print(f'[bold]{playlist.name}[/bold] is local only now')
    if playlist.remote_id:
        messages.print(f'{playlist.remote_id} is still on YouTube — delete it there if you want it gone')


@playlists_app.command('delete', rich_help_panel=BUILDING)
def playlists_delete(
    name: str = typer.Argument(..., help='Local playlist to delete.', autocompletion=complete_local_playlist),
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
    service.delete_local_playlist(playlist)
    messages.print(f'Deleted {playlist.path}')


@videos_app.command('list', rich_help_panel=READING)
def videos_list(
    playlist: str = typer.Option(None, '--playlist', '-p', help='Only videos in this playlist.', autocompletion=complete_playlist),
    artist: str = typer.Option(None, '--artist', '-a', help='Only videos with a track by an artist matching this.'),
    min_minutes: int = typer.Option(None, '--min-minutes', help='Only videos at least this long.'),
    max_minutes: int = typer.Option(None, '--max-minutes', help='Only videos at most this long.'),
    sort: str = typer.Option('longest', '--sort', help=f'One of: {", ".join(service.LIBRARY_SORT_CLAUSES)}.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Show at most this many.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """The whole library, one row per mix, with the artists inside it.

    The read curation runs on. A mix cannot be chosen from its title and is far
    too long to hand over whole, so each one is collapsed to what decides
    whether it belongs in a set: how long it is, whose channel it came from,
    which of your playlists hold it, and its artists, commonest first.

      ypl videos list --json --min-minutes 60 | claude -p 'pick six hours of uptempo house'

    Nothing here says "house" or "120bpm" — a chapter marker never does. What
    says it is knowing what those artists sound like, which is why this hands
    over the artists rather than trying to label the genre.
    """
    if sort not in service.LIBRARY_SORT_CLAUSES:
        raise typer.BadParameter(
            f'{sort!r} is not a sort here — choose one of: {", ".join(service.LIBRARY_SORT_CLAUSES)}', param_hint='--sort'
        )
    connection = db.connect()
    resolved = resolve_or_exit(connection, playlist) if playlist else None
    rows = service.library_videos(
        connection,
        playlist_id=resolved.identifier if resolved and resolved.kind == service.REMOTE else None,
        artist=artist,
        min_seconds=min_minutes * 60 if min_minutes else None,
        max_seconds=max_minutes * 60 if max_minutes else None,
        sort=sort,
        limit=limit,
    )
    if as_json:
        print_json(rows)
        return
    if not rows:
        messages.print('Nothing matches. [bold]ypl sync[/bold] first, or loosen the filters.')
        return

    total = sum(row['duration_seconds'] or 0 for row in rows)
    table = Table(title=f'{len(rows)} videos, {timestamp_words(total)}')
    table.add_column('Title')
    table.add_column('Channel')
    table.add_column('Length', justify='right')
    table.add_column('Tracks', justify='right')
    table.add_column('Artists')
    for row in rows:
        table.add_row(
            row['title'],
            row['channel'],
            duration_words(row['duration_seconds']),
            str(row['track_count']),
            ', '.join(row['artists'][:3]),
        )
    console.print(table)
    unenriched = sum(1 for row in rows if not row['enriched_ts'])
    if unenriched:
        messages.print(f'{unenriched} have no tracklist yet — [bold]ypl enrich --all[/bold] is what fills them in')


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
    print(f'plays     {paths.plays_file()}')


@config_app.command('show')
def config_show(
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Print the settings in effect, including defaults."""
    settings = load_config_or_exit()
    # Walked off the dataclass rather than listed by hand. The listed version
    # printed two of the seven, so `background_sync` — which the README tells you
    # to set to turn the timer off — could not be confirmed from the command
    # whose whole job is showing what is in effect, and neither could the
    # `sync_minutes` budget that decides how long a run lasts.
    values = {field.name: getattr(settings, field.name) for field in dataclasses.fields(settings)}
    if as_json:
        print_json(values)
        return
    width = max(len(name) for name in values)
    for name, value in values.items():
        messages.print(f'{name:<{width}}  {as_written(value)}')


def as_written(value: object) -> str:
    """A setting as it would be written in the config, so it can be copied back."""
    if value is None:
        return '(unset)'
    if isinstance(value, bool):
        return 'true' if value else 'false'
    if isinstance(value, list):
        return ', '.join(str(item) for item in value) or '(none)'
    return str(value)


@app.command('play', rich_help_panel=PLAYING)
def play(
    name: str = typer.Argument(..., help='Playlist to play, mirrored or local.', autocompletion=complete_playlist),
    sort: str = typer.Option('position', '--sort', help=f'One of: {", ".join(service.SORT_CLAUSES)}.'),
    limit: int = typer.Option(None, '--limit', '-n', help='Play at most this many videos.'),
    audio: bool = typer.Option(False, '--audio', '-a', help='Audio only, no video window.'),
) -> None:
    """Play a playlist through mpv.

    Runs in the foreground and exits when mpv does. The URLs are handed to mpv
    as arguments rather than as a playlist file, so --sort and --limit mean the
    same thing here as everywhere else and a mirrored playlist plays without
    writing a file first.

      ypl play 'Sunday' --audio --sort random
    """
    check_sort(sort)
    settings = load_config_or_exit()
    connection = db.connect()
    playlist = resolve_or_exit(connection, name)
    rows = service.playlist_selection(connection, playlist, sort, limit)
    if not rows:
        messages.print(f'[red]{playlist.title} has nothing playable in it.[/red]')
        raise typer.Exit(1)

    arguments = [*settings.mpv_arguments, *(['--no-video'] if audio else [])]
    socket_path: paths.Path | None = paths.mpv_socket()
    if socket_path is not None and not player.socket_is_addressable(socket_path):
        # mpv would log this and play on regardless, leaving `ypl now` quietly
        # reporting nothing with no way to tell why.
        messages.print(f'[red]{socket_path} is too long for a unix socket[/red] — playing without [bold]ypl now[/bold] support.')
        socket_path = None

    # Playing a local playlist is the clearest possible statement of which one
    # `drop` and `later` mean. A mirrored one is read-only, so it is not one
    # those verbs could act on and is deliberately not remembered.
    if playlist.local:
        session.remember(playlist.local.name)

    messages.print(f'Playing [bold]{playlist.title}[/bold] — {len(rows)} videos')
    try:
        exit_code = player.play([watch_url(row['video_id']) for row in rows], socket_path, arguments)
    except player.MpvUnavailableError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    if exit_code:
        raise typer.Exit(1)


@app.command('now', rich_help_panel=PLAYING)
def now(
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """What is playing right now, down to the track.

    Reads the socket `ypl play` opened. Because the mirror holds a tracklist
    with real timestamps, this reports the track inside a two-hour mix rather
    than the name of the mix.

    Exits 1 when nothing is playing, so a status bar can run it unguarded.
    """
    connection = db.connect()
    try:
        state = player.properties(paths.mpv_socket(), ['path', 'time-pos', 'media-title', 'duration'])
    except player.NotPlayingError as error:
        messages.print(f'[red]Nothing playing:[/red] {error}')
        raise typer.Exit(1) from error

    video_id = m3u.video_id_from(str(state['path'] or ''))
    position = int(state['time-pos']) if isinstance(state['time-pos'], int | float) else None
    video = service.get_video(connection, video_id) if video_id else None
    track = service.track_at(connection, video_id, position) if video_id and position is not None else None
    title = video['title'] if video else str(state['media-title'] or state['path'] or '')
    duration = video['duration_seconds'] if video else state['duration']

    if as_json:
        print_json(
            {
                'video_id': video_id,
                'title': title,
                'channel': video['channel'] if video else '',
                'position_seconds': position,
                'duration_seconds': int(duration) if isinstance(duration, int | float) else None,
                'track': dict(track) if track else None,
            }
        )
        return
    if track:
        messages.print(f'[bold]{f"{track['artist']} - " if track["artist"] else ""}{track["title"]}[/bold]')
    messages.print(
        f'{title}  {timestamp_words(position)} / {timestamp_words(int(duration) if isinstance(duration, int | float) else None)}'
    )
    if not track and video and not video['enriched_ts']:
        messages.print('Not enriched — run [bold]ypl enrich[/bold] to get the track instead of the video.')


@app.command('use', rich_help_panel=PLAYING)
def use(
    name: str = typer.Argument(
        None, help='Playlist to work on. Prints the current one when omitted.', autocompletion=complete_local_playlist
    ),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Set the playlist that `drop`, `later` and `sooner` act on.

    `ypl play` sets this on its own; this is for when playback is happening
    somewhere ypl cannot see — the YouTube app, or a browser tab.
    """
    connection = db.connect()
    if name is None:
        chosen = session.current()
        if not chosen:
            messages.print('No current playlist. Run [bold]ypl use <name>[/bold] or [bold]ypl play <name>[/bold].')
            raise typer.Exit(1)
        if as_json:
            print_json({'playlist': chosen})
            return
        messages.print(f'[bold]{chosen}[/bold]')
        return

    playlist = local_or_exit(connection, name)
    session.remember(playlist.name)
    if as_json:
        print_json({'playlist': playlist.name, 'video_count': len(playlist.entries)})
        return
    messages.print(f'Working on [bold]{playlist.name}[/bold] — {len(playlist.entries)} videos')


def current_playlist_or_exit(connection: sqlite3.Connection, name: str | None) -> local.LocalPlaylist:
    chosen = name or session.current()
    if not chosen:
        messages.print('[red]No current playlist.[/red] Run [bold]ypl use <name>[/bold], or pass --playlist.')
        raise typer.Exit(2)
    return local_or_exit(connection, chosen)


def playing_video_id() -> str:
    """What mpv has open, or '' when it is not the thing playing."""
    try:
        state = player.properties(paths.mpv_socket(), ['path'])
    except player.NotPlayingError:
        return ''
    return m3u.video_id_from(str(state['path'] or '')) or ''


def target_entry_or_exit(connection: sqlite3.Connection, playlist: local.LocalPlaylist, match: str | None) -> int:
    """Which entry a verb means: the one named, or the one playing."""
    rows = service.local_playlist_videos(connection, playlist)
    if match:
        try:
            return service.find_entry(rows, match)
        except service.AmbiguousEntryError as error:
            messages.print(f'[red]{error}:[/red]')
            for candidate in error.candidates:
                messages.print(f'  {candidate["title"]}')
            raise typer.Exit(2) from error
        except service.EntryNotFoundError as error:
            messages.print(f'[red]{error}[/red]')
            raise typer.Exit(1) from error

    video_id = playing_video_id()
    if not video_id:
        messages.print('[red]Nothing is playing through ypl,[/red] so name part of a title: [bold]ypl drop wagram[/bold]')
        raise typer.Exit(2)
    try:
        return service.entry_position(playlist, video_id)
    except service.EntryNotFoundError as error:
        messages.print(f'[red]{error}[/red] — is [bold]{playlist.name}[/bold] the playlist you are listening to?')
        raise typer.Exit(1) from error


def report_after_change(playlist: local.LocalPlaylist) -> None:
    if playlist.synced:
        messages.print('Run [bold]ypl remote plan[/bold] to see what that means for YouTube')


@app.command('drop', rich_help_panel=PLAYING)
def drop(
    match: str = typer.Argument(None, help='Part of a title. Defaults to what is playing.', autocompletion=complete_entry),
    playlist_name: str = typer.Option(
        None, '--playlist', '-p', help='Playlist to act on. Defaults to the current one.', autocompletion=complete_local_playlist
    ),
    keep_playing: bool = typer.Option(False, '--keep-playing', help='Do not skip it in mpv.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Take the playing video out of the playlist.

    Names no id: it is whatever mpv has open, or whatever fragment of a title
    you can type. mpv skips to the next video as well, since continuing to play
    what you just deleted is not what dropping it meant.
    """
    connection = db.connect()
    playlist = current_playlist_or_exit(connection, playlist_name)
    index = target_entry_or_exit(connection, playlist, match)
    was_playing = playlist.entries[index].video_id == playing_video_id()
    dropped = service.drop_entry(playlist, index)

    if was_playing and not keep_playing:
        # Best effort: the video is already out of the playlist file, and mpv
        # having gone away between the two is not a reason to report a failure.
        with contextlib.suppress(player.NotPlayingError):
            player.command(paths.mpv_socket(), ['playlist-next'])

    if as_json:
        print_json({'playlist': playlist.name, 'video_id': dropped.video_id, 'video_count': len(playlist.entries)})
        return
    messages.print(f'Dropped [bold]{dropped.title or dropped.video_id}[/bold] — {len(playlist.entries)} left')
    report_after_change(playlist)


def shift(connection: sqlite3.Connection, playlist_name: str | None, match: str | None, offset: int, as_json: bool) -> None:
    playlist = current_playlist_or_exit(connection, playlist_name)
    index = target_entry_or_exit(connection, playlist, match)
    entry = playlist.entries[index]
    destination = service.move_entry(playlist, index, offset)

    if as_json:
        print_json({'playlist': playlist.name, 'video_id': entry.video_id, 'position': destination + 1})
        return
    messages.print(f'[bold]{entry.title or entry.video_id}[/bold] is now #{destination + 1} of {len(playlist.entries)}')
    report_after_change(playlist)


@app.command('later', rich_help_panel=PLAYING)
def later(
    match: str = typer.Argument(None, help='Part of a title. Defaults to what is playing.', autocompletion=complete_entry),
    count: int = typer.Option(1, '--count', '-n', help='How many places to move it back.'),
    playlist_name: str = typer.Option(
        None, '--playlist', '-p', help='Playlist to act on. Defaults to the current one.', autocompletion=complete_local_playlist
    ),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Push a video further down the playlist."""
    shift(db.connect(), playlist_name, match, count, as_json)


@app.command('sooner', rich_help_panel=PLAYING)
def sooner(
    match: str = typer.Argument(None, help='Part of a title. Defaults to what is playing.', autocompletion=complete_entry),
    count: int = typer.Option(1, '--count', '-n', help='How many places to move it up.'),
    playlist_name: str = typer.Option(
        None, '--playlist', '-p', help='Playlist to act on. Defaults to the current one.', autocompletion=complete_local_playlist
    ),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Bring a video further up the playlist."""
    shift(db.connect(), playlist_name, match, -count, as_json)


@app.command('next', rich_help_panel=PLAYING)
def next_up(
    playlist: str = typer.Option(None, '--playlist', '-p', help='Choose from one playlist instead of the whole mirror.'),
    limit: int = typer.Option(1, '--limit', '-n', help='How many suggestions to emit.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """What to put on next — least recently listened to, never-played first.

    The resolver `menu next` delegates to, so a `listen` pursuit answers with a
    mix rather than with the word "listen". Register it as:

      resolve: ypl next --json
      label: title
      id: video_id
      on_log: ypl plays add {id}
    """
    connection = db.connect()
    resolved = resolve_or_exit(connection, playlist) if playlist else None
    suggestions = service.next_videos(connection, resolved, limit)
    if as_json:
        print_json([row | {'url': watch_url(row['video_id'])} for row in suggestions])
        return
    if not suggestions:
        messages.print('Nothing to play. Run [bold]ypl sync <playlist-url>[/bold] to mirror something first.')
        raise typer.Exit(1)
    for row in suggestions:
        print(f'{row["title"]}  {watch_url(row["video_id"])}')


@plays_app.command('add', context_settings={'ignore_unknown_options': True})
def plays_add(
    video_id: str = typer.Argument(..., help='Video URL or id that was listened to.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Record that a video was listened to.

    What `ypl next` reads to stop suggesting the same mix. Written when a listen
    is logged rather than inferred from playback, because `ypl play` hands mpv
    the whole list at once and never learns which of it got played.
    """
    resolved_id = video_id_argument(video_id)
    connection = db.connect()
    video = service.get_video(connection, resolved_id)
    # Checked rather than recorded blindly: the log is append-only data with no
    # remote to correct it from, so a typo would stay in it.
    if not video:
        messages.print(f'[red]{resolved_id} is not in the mirror.[/red] Sync a playlist containing it first.')
        raise typer.Exit(1)

    history.record(resolved_id)
    if as_json:
        print_json({'video_id': resolved_id, 'play_count': history.summary()[resolved_id]['play_count']})
        return
    messages.print(f'Logged [bold]{video["title"]}[/bold]')


@plays_app.command('list')
def plays_list(
    limit: int = typer.Option(20, '--limit', '-n', help='Show at most this many.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """What has been listened to, most recent first."""
    connection = db.connect()
    rows = service.recent_plays(connection, limit)
    if as_json:
        print_json(rows)
        return
    if not rows:
        messages.print('Nothing logged yet. [bold]ypl plays add <id>[/bold] records a listen.')
        return
    table = Table(title=f'{len(rows)} plays')
    table.add_column('When')
    table.add_column('Title')
    table.add_column('Channel')
    table.add_column('Id', overflow='fold')
    for row in rows:
        table.add_row(row['played_ts'], row['title'], row['channel'], row['video_id'])
    console.print(table)


def headers_from_browser_or_exit(browser: str) -> str:
    """Read the browser's YouTube cookies and build a session out of them."""
    try:
        cookies = ytdlp.browser_cookies(browser)
    except ytdlp.YtdlpUnavailableError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    except ytdlp.YtdlpFailedError as error:
        messages.print(f'[red]Could not read cookies from {browser}.[/red] {error}')
        raise typer.Exit(1) from error
    try:
        return ytmusic.session_headers(cookies)
    except remote.RemoteAuthError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error


@remote_app.command('auth')
def remote_auth(
    browser: str = typer.Option(
        None, '--browser', '-b', help='Read the session from this browser: safari, firefox, chrome, brave, edge...'
    ),
    replace: bool = typer.Option(False, '--replace', help='Overwrite a session that is already stored.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Store the YouTube session the write path signs in with.

    [bold]ypl remote auth --browser safari[/bold] takes it straight out of a
    browser you are already signed in to, through yt-dlp, and asks nothing of
    you. With no --browser it falls back to `cookies_from_browser` in the
    config, and failing that to pasted request headers on stdin.

    Whatever the source, the session is written at 0600 and then used once to
    ask YouTube whose account it reaches — headers that parse are not yet a
    session that works.
    """
    auth_file = paths.ytmusic_auth_file()
    if auth_file.exists() and not replace:
        messages.print(f'[red]{auth_file} already exists.[/red] Pass --replace to sign in again.')
        raise typer.Exit(1)

    source = browser or reading_browser(load_config_or_exit())
    if source:
        headers_raw = headers_from_browser_or_exit(source)
    else:
        if sys.stdin.isatty():
            messages.print(AUTH_INSTRUCTIONS)
        headers_raw = sys.stdin.read()

    try:
        ytmusic.write_session(headers_raw, auth_file)
    except remote.RemoteAuthError as error:
        messages.print(f'[red]Those headers cannot be used.[/red] {error}')
        raise typer.Exit(2) from error
    except remote.RemoteError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error

    try:
        account = ytmusic.YtMusicBackend(auth_file).account()
    except remote.RemoteAuthError as error:
        # The headers parsed and the cookie does not work, so what was just
        # written is a credential that can only fail later, further from here.
        auth_file.unlink(missing_ok=True)
        messages.print(f'[red]YouTube rejected that session.[/red] {error}')
        messages.print('Nothing was stored. Copy the headers again from a tab that is signed in.')
        raise typer.Exit(1) from error
    except remote.RemoteError as error:
        # Kept: this is a throttle or a network failure, which says nothing
        # about the session, and re-copying the headers would not help.
        messages.print(f'[yellow]Stored, but could not be checked:[/yellow] {error}')
        raise typer.Exit(1) from error

    if source:
        # Every read borrows cookies through yt-dlp rather than through this
        # session, so a browser that just proved it holds one is exactly what
        # `ypl sync` needs to know and had no way of learning.
        session.remember_browser(source)

    if as_json:
        print_json({'path': str(auth_file), 'account': account.name, 'handle': account.handle, 'browser': source or ''})
        return
    messages.print(f'Signed in as [bold]{account.name or "an account YouTube did not name"}[/bold] {account.handle}'.strip())
    messages.print(str(auth_file))


def backend_or_exit() -> ytmusic.YtMusicBackend:
    ytmusic.refresh_session(session.browser(), paths.ytmusic_auth_file())
    try:
        return ytmusic.YtMusicBackend(paths.ytmusic_auth_file())
    except remote.RemoteAuthError as error:
        messages.print(f'[red]{error}[/red]')
        messages.print('Run [bold]ypl remote auth[/bold] to sign in.')
        raise typer.Exit(1) from error
    except remote.RemoteError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error


def bound_playlist_named(name: str) -> local.LocalPlaylist | None:
    """The local playlist bound to a YouTube one this name could mean.

    Wanted only for the error message, and only because resolution now answers
    an adopted playlist with its file: the mirrored candidate is gone by the
    time the name fails to match one, and "no mirrored playlist matching
    DRIVE TIME" would be a lie about the single case where it worked already.
    """
    needle = local.slugify(name)
    for playlist in local.list_playlists().playlists:
        if playlist.remote_id and needle in {local.slugify(playlist.name), local.slugify(playlist.remote_id)}:
            return playlist
    return None


def mirrored_or_exit(connection: sqlite3.Connection, name: str) -> service.ResolvedPlaylist:
    """Resolve a name that has to be a mirrored playlist, because a file is about to bind to it.

    Scoped to the mirror for the mirror image of `local_or_exit`'s reason: a
    local playlist carrying the same name must not make the YouTube playlist
    unreachable.
    """
    try:
        return service.resolve_playlist(connection, name, service.REMOTE)
    except service.PlaylistNotFoundError as error:
        bound = bound_playlist_named(name)
        if bound:
            messages.print(f'[red]{bound.name} is already a playlist here.[/red] Reconcile it with [bold]ypl remote pull[/bold].')
            raise typer.Exit(1) from error
        messages.print(f'[red]No mirrored playlist matching {name!r}.[/red] Run [bold]ypl sync[/bold] to mirror the account first.')
        raise typer.Exit(1) from error
    except service.AmbiguousPlaylistError as error:
        print_candidates(name, error)
        raise typer.Exit(2) from error


def account_or_exit(backend: ytmusic.YtMusicBackend) -> remote.RemoteAccount:
    try:
        return backend.account()
    except remote.RemoteError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error


@remote_app.command('adopt')
def remote_adopt(
    name: str = typer.Argument(
        None, help='Mirrored playlist to take over. Every one this account owns when omitted.', autocompletion=complete_playlist
    ),
    limit: int = typer.Option(None, '--limit', '-n', help='Adopt at most this many playlists.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Take over a playlist YouTube already holds.

    Writes the local file, binds it to the YouTube playlist and records what it
    read as the base — so a playlist made in the web player years ago can now be
    edited here and pushed back. Its name is kept exactly as YouTube has it.

    Bare, this covers every mirrored playlist the signed-in account owns.
    Playlists mirrored from someone else's channel are left out, since nothing
    here could write to them, but naming one adopts it anyway.
    """
    connection = db.connect()
    backend = backend_or_exit()
    if name is None:
        targets = service.adoptable_playlists(connection, account_or_exit(backend))
    else:
        targets = [mirrored_or_exit(connection, name)]
    if not targets:
        messages.print('Nothing to adopt — every playlist this account owns is already bound to a file here.')
        messages.print('Run [bold]ypl sync[/bold] first if YouTube has playlists this mirror has not seen.')
        return
    if limit and len(targets) > limit:
        # Named rather than silent, as in `plan`: a run that covered half of
        # them and said nothing reads as the whole library being adopted.
        messages.print(f'Adopting {limit} of {len(targets)} playlists — run again for the rest.')
        targets = targets[:limit]

    adopted: list[local.LocalPlaylist] = []
    refused: list[tuple[service.ResolvedPlaylist, str]] = []
    failure: Exception | None = None
    for target in targets:
        try:
            adopted.append(service.adopt_playlist(connection, target, backend))
        except (service.AdoptionError, local.LocalPlaylistExistsError) as error:
            refused.append((target, str(error)))
        except remote.RemoteError as error:
            # Including the rate limit: it clears in hours, and everything
            # adopted so far is already written down.
            failure = error
            break

    if as_json:
        print_json(
            [
                {'name': playlist.name, 'slug': playlist.slug, 'remote_id': playlist.remote_id, 'video_count': len(playlist.entries)}
                for playlist in adopted
            ]
        )
    else:
        for playlist in adopted:
            messages.print(f'[bold]{playlist.name}[/bold] — adopted, {len(playlist.entries)} videos')
        for target, reason in refused:
            messages.print(f'[bold]{target.title}[/bold] — [yellow]skipped[/yellow]: {reason}')

    if failure:
        messages.print(f'[red]{failure}[/red]')
        messages.print(f'Stopped after {len(adopted)} of {len(targets)} — everything adopted is already recorded.')
        raise typer.Exit(1)
    if refused:
        raise typer.Exit(1)


def pull_targets_or_exit(connection: sqlite3.Connection, name: str | None) -> list[local.LocalPlaylist]:
    """Which playlists this run reconciles.

    A named one that cannot be pulled is an error rather than a silent skip —
    the caller asked for that playlist by name — while a sweep passes over the
    same playlists without comment, because "not on YouTube yet" is the normal
    state of one made a minute ago.
    """
    if name is None:
        return service.pullable_playlists()
    playlist = local_or_exit(connection, name)
    if not playlist.remote_id:
        messages.print(f'[red]{playlist.name} is not on YouTube yet.[/red] It goes up on the next [bold]ypl remote apply[/bold].')
        raise typer.Exit(1)
    if not playlist.synced:
        messages.print(f'[red]{playlist.name} is local only.[/red] Run [bold]ypl playlists promote {playlist.name!r}[/bold] first.')
        raise typer.Exit(1)
    return [playlist]


def report_pull(pull: service.Pull) -> None:
    result = pull.result
    if not result.changed_here and not result.to_push:
        messages.print(f'[bold]{pull.playlist.name}[/bold] — up to date')
        return
    changes = []
    if result.pulled_in:
        changes.append(f'{len(result.pulled_in)} added here')
    if result.pulled_out:
        changes.append(f'{len(result.pulled_out)} removed here')
    if result.order_source == merge.REMOTE:
        changes.append('order taken from YouTube')
    if result.to_push:
        changes.append(f'{len(result.pending_add) + len(result.pending_remove)} to push')
    messages.print(f'[bold]{pull.playlist.name}[/bold] — {", ".join(changes)}')


@remote_app.command('pull')
def remote_pull(
    name: str = typer.Argument(
        None, help='Playlist to reconcile. Every synced playlist when omitted.', autocompletion=complete_local_playlist
    ),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Reconcile playlists with YouTube, letting YouTube win.

    Reads what is on YouTube, merges it into the local file against what was
    there at the last reconcile, and records the read as the new base. Changes
    made on a phone arrive here; changes made here stay made and go up on the
    next [bold]ypl remote apply[/bold].
    """
    connection = db.connect()
    targets = pull_targets_or_exit(connection, name)
    if not targets:
        messages.print('Nothing to pull — no playlist here is on YouTube yet.')
        return

    backend = backend_or_exit()
    pulls = []
    try:
        for playlist in targets:
            pulls.append(service.pull_playlist(connection, playlist, backend))
    except remote.RemoteRateLimitedError as error:
        # Stopped rather than retried: the limit clears on its own in hours,
        # and everything reconciled so far is already saved.
        messages.print(f'[red]YouTube asked us to slow down.[/red] {error}')
        messages.print(f'Reconciled {len(pulls)} of {len(targets)} — run this again later.')
        raise typer.Exit(1) from error
    except remote.RemoteError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    except basestore.BaseStoreError as error:
        messages.print(f'[red]{error}[/red]')
        messages.print('That file is the record of what YouTube last held. Delete it only if you accept that')
        messages.print('everything in the playlist will then look newly added here, and go back up to YouTube.')
        raise typer.Exit(1) from error

    if as_json:
        print_json(
            [
                {
                    'name': pull.playlist.name,
                    'slug': pull.playlist.slug,
                    'remote_id': pull.playlist.remote_id,
                    'video_count': len(pull.playlist.entries),
                    'order_source': pull.result.order_source,
                    'pulled_in': pull.result.pulled_in,
                    'pulled_out': pull.result.pulled_out,
                    'pending_add': pull.result.pending_add,
                    'pending_remove': pull.result.pending_remove,
                }
                for pull in pulls
            ]
        )
        return
    for pull in pulls:
        report_pull(pull)


def push_targets_or_exit(connection: sqlite3.Connection, name: str | None) -> list[local.LocalPlaylist]:
    if name is None:
        return service.pushable_playlists()
    playlist = local_or_exit(connection, name)
    if not playlist.synced:
        messages.print(f'[red]{playlist.name} is local only.[/red] Run [bold]ypl playlists promote {playlist.name!r}[/bold] first.')
        raise typer.Exit(1)
    return [playlist]


def describe_push(push: service.Push) -> str:
    if push.stale:
        return 'YouTube has changed since the last reconcile — run [bold]ypl remote pull[/bold] first'
    parts = []
    if push.create:
        parts.append('create')
    if push.diff.add:
        parts.append(f'{len(push.diff.add)} to add')
    if push.diff.remove:
        parts.append(f'{len(push.diff.remove)} to remove')
    if push.moves:
        parts.append(f'{push.moves} moves')
    return ', '.join(parts) if parts else 'nothing to push'


def push_payload(push: service.Push) -> dict:
    return {
        'name': push.playlist.name,
        'slug': push.playlist.slug,
        'remote_id': push.playlist.remote_id,
        'create': push.create,
        'add': push.diff.add,
        'remove': len(push.diff.remove),
        'moves': push.moves,
        'stale': push.stale,
    }


def plan_pushes(
    connection: sqlite3.Connection, name: str | None, limit: int | None
) -> tuple[ytmusic.YtMusicBackend | None, list[service.Push]]:
    """Plan every target, and hand back the backend that read them.

    The same backend carries on into `apply`, so a run is one signed-in session
    and one throttle rather than a fresh one per playlist.
    """
    targets = push_targets_or_exit(connection, name)
    if limit:
        # Named rather than silent: a run that covered half the playlists and
        # said "done" reads as everything being up on YouTube.
        if len(targets) > limit:
            messages.print(f'Planning {limit} of {len(targets)} playlists — run again for the rest.')
        targets = targets[:limit]
    if not targets:
        return None, []

    backend = backend_or_exit()
    plans = []
    try:
        for playlist in targets:
            plans.append(service.plan_push(playlist, backend))
    except remote.RemoteRateLimitedError as error:
        messages.print(f'[red]YouTube asked us to slow down.[/red] {error}')
        raise typer.Exit(1) from error
    except remote.RemoteError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    except basestore.BaseStoreError as error:
        messages.print(f'[red]{error}[/red]')
        raise typer.Exit(1) from error
    return backend, plans


@remote_app.command('plan')
def remote_plan(
    name: str = typer.Argument(None, help='Playlist to plan. Every synced playlist when omitted.', autocompletion=complete_local_playlist),
    limit: int = typer.Option(None, '--limit', '-n', help='Plan at most this many playlists.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """What `ypl remote apply` would change on YouTube.

    A dry run by construction rather than by flag: this is the same reads and
    the same arithmetic apply does, stopping before the first write.
    """
    connection = db.connect()
    _, plans = plan_pushes(connection, name, limit)
    if as_json:
        print_json([push_payload(push) for push in plans])
        return
    if not plans:
        messages.print('Nothing to plan — no playlist here is set to sync.')
        return
    for push in plans:
        messages.print(f'[bold]{push.playlist.name}[/bold] — {describe_push(push)}')


@remote_app.command('apply')
def remote_apply(
    name: str = typer.Argument(None, help='Playlist to push. Every synced playlist when omitted.', autocompletion=complete_local_playlist),
    limit: int = typer.Option(None, '--limit', '-n', help='Push at most this many playlists, for a drain on a timer.'),
    as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.'),
) -> None:
    """Make YouTube match the local files.

    Slowly, and on purpose: every call is spaced, additions go up a hundred at
    a time, and a rate limit stops the run instead of being retried into. A
    playlist YouTube has changed since the last reconcile is skipped rather
    than guessed at — [bold]ypl remote pull[/bold] is what settles that.
    """
    connection = db.connect()
    backend, plans = plan_pushes(connection, name, limit)
    if not plans or backend is None:
        if not as_json:
            messages.print('Nothing to apply — no playlist here is set to sync.')
        return

    done: list[service.Push] = []
    failure: Exception | None = None
    for push in plans:
        if push.stale or push.empty:
            done.append(push)
            continue
        try:
            done.append(service.apply_push(push, backend))
        except remote.RemoteError as error:
            failure = error
            break

    if as_json:
        print_json([push_payload(push) for push in done])
    else:
        for push in done:
            if push.stale:
                messages.print(f'[bold]{push.playlist.name}[/bold] — [yellow]skipped[/yellow]: {describe_push(push)}')
            elif push.empty:
                messages.print(f'[bold]{push.playlist.name}[/bold] — already up to date')
            else:
                messages.print(f'[bold]{push.playlist.name}[/bold] — pushed: {describe_push(push)}')

    if failure:
        messages.print(f'[red]{failure}[/red]')
        messages.print(f'Stopped after {len(done)} of {len(plans)} — everything already pushed is recorded.')
        raise typer.Exit(1)
    if any(push.stale for push in done):
        raise typer.Exit(1)


@app.command('status', rich_help_panel=SYNCING)
def status(as_json: bool = typer.Option(False, '--json', help='Output as JSON to stdout.')) -> None:
    """Whether everything is in sync, and what the last run did.

    The window onto a sync nobody watches. What it answers is the question a
    background process makes hard: is this working, and if it is behind, by how
    much and since when.
    """
    connection = db.connect()
    playlists = local.list_playlists().playlists
    pending = [playlist.name for playlist in playlists if playlist.synced and (not playlist.remote_id or service.needs_push(playlist))]
    last = synclog.last()
    timer = schedule.installed()
    payload = {
        'running': runlock.running(),
        'signed_in': paths.ytmusic_auth_file().exists(),
        'scheduled': bool(timer),
        'interval_minutes': timer.interval_minutes if timer else None,
        'last_sync': last.get('ts') if last else None,
        'last_run': last,
        'playlists': len(playlists),
        'unenriched': len(service.unenriched_everywhere(connection)),
        'unreadable': len(service.unreadable_video_ids(connection)),
        'pending_push': pending,
        'declined': len(declined.load()),
    }
    if as_json:
        print_json(payload)
        return

    messages.print(f'Syncing now     {"yes" if payload["running"] else "no"}')
    messages.print(f'Signed in       {"yes" if payload["signed_in"] else "no — ypl remote auth"}')
    messages.print(f'Scheduled       {f"every {timer.interval_minutes} min ({timer.manager})" if timer else "no — run ypl sync once"}')
    messages.print(f'Last sync       {payload["last_sync"] or "never"}')
    if last:
        done = [f'{len(last[key])} {key}' for key in ('adopted', 'reconciled', 'pushed') if last.get(key)]
        messages.print(f'                {", ".join(done) if done else "nothing to do"}')
        failures = last.get('failures') or []
        for failure in failures[:STATUS_FAILURES_SHOWN]:
            messages.print(f'                [yellow]{failure}[/yellow]')
        if len(failures) > STATUS_FAILURES_SHOWN:
            # The command rather than the count, per the no-remainder-counts rule
            # in `~/dev/standards/cli-design.md`.
            messages.print(f'                [yellow]and {len(failures) - STATUS_FAILURES_SHOWN} more — ypl status --json[/yellow]')
    messages.print(f'Playlists       {payload["playlists"]} here, {payload["declined"]} declined')
    if last and last.get('withheld'):
        count = len(last['withheld'])
        messages.print(f'Not public      {count} playlist{"" if count == 1 else "s"} YouTube Music will not serve — read-only here')
    messages.print(f'Unenriched      {payload["unenriched"]} videos, {payload["unreadable"]} unreadable')
    if pending:
        messages.print(f'Waiting to push {", ".join(pending)}')


@app.command('update', rich_help_panel=ADMIN)
def update() -> None:
    """Update ypl to the latest release."""
    run_update(UPDATE_CONFIG)
