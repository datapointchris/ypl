"""Reads against YouTube, via the yt-dlp binary.

yt-dlp rather than the Data API because reads through it cost no quota at all,
and because chapters — the whole basis for a mix tracklist — are not exposed by
the Data API under any part/field combination. The 10,000 units a day are worth
spending only on writes.

Shelled out to rather than imported: yt-dlp ships breaking-fix releases
constantly and needs to track YouTube's changes independently of this tool's
lock file. Pinning it as a dependency would freeze the one component that must
never be frozen.
"""

import http.cookiejar
import json
import shutil
import subprocess
import tempfile
from pathlib import Path

from ypl.models import Chapter
from ypl.models import PlaylistRef
from ypl.models import RemotePlaylist
from ypl.models import RemoteVideo
from ypl.models import watch_url

BINARY = 'yt-dlp'

COOKIE_DOMAIN = 'youtube.com'

# A host that cannot resolve, so the cookie export happens and the run then
# stops without a request. Any real URL would download a page to no purpose.
UNRESOLVABLE_URL = 'https://cookies.invalid/'

# The signed-in account's own playlists. Not the Music library, which is a
# different list — see `fetch_account_playlists`.
ACCOUNT_PLAYLISTS_URL = 'https://www.youtube.com/feed/playlists'


class YtdlpUnavailableError(RuntimeError):
    pass


class YtdlpFailedError(RuntimeError):
    pass


def binary_path() -> str:
    found = shutil.which(BINARY)
    if not found:
        raise YtdlpUnavailableError(f'{BINARY} is not on PATH — install it to read playlists')
    return found


def run(arguments: list[str], timeout_seconds: int) -> str:
    command = [binary_path(), *arguments]
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout_seconds)  # noqa: S603
    if result.returncode != 0:
        raise YtdlpFailedError(result.stderr.strip() or f'{BINARY} exited {result.returncode}')
    return result.stdout


def cookie_arguments(cookies_from_browser: str | None) -> list[str]:
    """Private and unlisted playlists are invisible without a logged-in session."""
    return ['--cookies-from-browser', cookies_from_browser] if cookies_from_browser else []


def browser_cookies(browser: str, domain: str = COOKIE_DOMAIN, timeout_seconds: int = 120) -> dict[str, str]:
    """The YouTube cookies a browser is holding, read through yt-dlp.

    yt-dlp already decrypts every browser's cookie store — Safari's binary
    format, Chrome's keychain-encrypted database, Firefox's sqlite — and it is
    already a hard dependency here. Reimplementing any of that so the write
    path could sign itself in would be writing a worse copy of a binary that is
    already installed.

    yt-dlp has no "just dump the cookies" mode, so the jar is written as a side
    effect of a run that is made to fail: an unresolvable host means the cookies
    are exported before anything reaches the network, and nothing is requested
    from YouTube by a command whose only job is to read a local file. The exit
    code is therefore ignored and the jar is what gets checked.
    """
    with tempfile.TemporaryDirectory() as directory:
        jar_path = Path(directory) / 'cookies.txt'
        arguments = ['--cookies-from-browser', browser, '--cookies', str(jar_path), '--simulate', UNRESOLVABLE_URL]
        try:
            subprocess.run(  # noqa: S603
                [binary_path(), *arguments], capture_output=True, text=True, timeout=timeout_seconds
            )
        except subprocess.TimeoutExpired as error:
            raise YtdlpFailedError(f'{BINARY} did not finish reading cookies from {browser}') from error
        if not jar_path.exists():
            raise YtdlpFailedError(f'{BINARY} read no cookies from {browser} — is it a browser it supports?')

        jar = http.cookiejar.MozillaCookieJar(str(jar_path))
        # Expiry and session flags are ignored deliberately: a cookie the
        # browser is still holding is one the browser is still using, and
        # Google's session cookies are exactly the ones that matter here.
        jar.load(ignore_discard=True, ignore_expires=True)
        return {cookie.name: cookie.value or '' for cookie in jar if domain in (cookie.domain or '')}


def fetch_account_playlists(cookies_from_browser: str, timeout_seconds: int = 300) -> list[PlaylistRef]:
    """Every playlist the signed-in account has, in one flat request.

    Read from YouTube's own playlists feed rather than from the YouTube Music
    library, because they are not the same list: a library call on this account
    returns one podcast queue while the feed returns the forty-odd playlists
    actually saved. Music's library is a view of Music, and the playlists worth
    mirroring were made on YouTube.

    Needs a browser session — the feed is per-account and returns nothing
    without one, which is the same cookie already needed to read any private
    playlist.
    """
    arguments = ['--flat-playlist', '--dump-single-json', *cookie_arguments(cookies_from_browser), ACCOUNT_PLAYLISTS_URL]
    payload = json.loads(run(arguments, timeout_seconds))
    return [
        PlaylistRef(playlist_id=entry['id'], title=entry.get('title') or entry['id'])
        for entry in payload.get('entries') or []
        if entry.get('id')
    ]


def fetch_playlist(url: str, cookies_from_browser: str | None = None, timeout_seconds: int = 600) -> RemotePlaylist:
    """List a playlist's videos without fetching each one.

    Flat, so this is one request for the whole playlist regardless of length.
    The per-video description and chapters are deliberately not here — see
    `fetch_video`.
    """
    arguments = [
        '--flat-playlist',
        '--dump-single-json',
        *cookie_arguments(cookies_from_browser),
        url,
    ]
    payload = json.loads(run(arguments, timeout_seconds))
    return RemotePlaylist(
        playlist_id=payload['id'],
        title=payload.get('title') or payload['id'],
        description=payload.get('description') or '',
        channel=payload.get('channel') or payload.get('uploader') or '',
        videos=[flat_entry_to_video(entry) for entry in payload.get('entries') or []],
    )


def flat_entry_to_video(entry: dict) -> RemoteVideo:
    """A flat playlist entry.

    Deleted and private videos still occupy a position and still arrive here,
    with their title replaced by a placeholder and no channel. They are kept
    rather than dropped, because a gap in the mirror would silently shift every
    position after it.
    """
    title = entry.get('title') or ''
    return RemoteVideo(
        video_id=entry['id'],
        title=title,
        channel=entry.get('channel') or entry.get('uploader') or '',
        duration_seconds=entry.get('duration'),
        is_unavailable=title in {'[Deleted video]', '[Private video]', '[Unavailable video]'},
    )


def fetch_video(video_id: str, cookies_from_browser: str | None = None, timeout_seconds: int = 120) -> RemoteVideo:
    """Fully extract one video, for its description and chapters."""
    arguments = [
        '--dump-json',
        '--skip-download',
        *cookie_arguments(cookies_from_browser),
        watch_url(video_id),
    ]
    payload = json.loads(run(arguments, timeout_seconds))
    return RemoteVideo(
        video_id=payload['id'],
        title=payload.get('title') or '',
        channel=payload.get('channel') or payload.get('uploader') or '',
        duration_seconds=payload.get('duration'),
        description=payload.get('description') or '',
        upload_date=payload.get('upload_date'),
        chapters=[
            Chapter(
                start_seconds=int(chapter['start_time']),
                end_seconds=int(chapter['end_time']) if chapter.get('end_time') is not None else None,
                title=chapter.get('title') or '',
            )
            for chapter in payload.get('chapters') or []
        ],
    )
