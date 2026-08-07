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
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
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

# The signed-in account's own Liked videos. Read for the channel it reports
# rather than for its contents — see `fetch_account_channel_id`.
ACCOUNT_CHANNEL_URL = 'https://www.youtube.com/playlist?list=LL'


class YtdlpUnavailableError(RuntimeError):
    pass


class YtdlpFailedError(RuntimeError):
    pass


# What yt-dlp says when the video itself is the problem rather than the request.
# Matched on text for the same reason the rate limit is: it arrives as a message
# inside a generic extraction failure rather than as anything a caller could
# tell apart by type.
GONE_MARKERS = (
    'video unavailable',
    'private video',
    'this video is private',
    'has been removed',
    'removed by the uploader',
    'members-only',
    'members only',
    'account associated with this video has been terminated',
    'not available in your country',
    'video is no longer available',
)


def is_gone(message: str) -> bool:
    """Whether a failed extraction says the video will never succeed.

    Worth telling apart from a failure that might pass next time, because an
    unattended enrich retries whatever it did not manage — and a handful of
    permanently dead videos, asked about every run forever, is a background
    process that spends its whole budget achieving nothing.
    """
    lowered = message.lower()
    return any(marker in lowered for marker in GONE_MARKERS)


class YtdlpRateLimitedError(YtdlpFailedError):
    """YouTube is refusing because of how much has been asked, not what.

    Its own class because the response has to be different: one video that
    cannot be read is skipped and reported, while this means stop. Reads cost
    no quota, but a bulk enrich is thousands of sequential extractions carrying
    your cookies, and continuing to send them after being told to slow down is
    what turns pacing into a pattern worth noticing.
    """


# What yt-dlp prints when YouTube is pushing back rather than saying no. The
# bot check is the one that matters most: it appears when a signed-in session
# has asked for too much too fast, and answering it with more requests is the
# worst available move.
RATE_LIMIT_MARKERS = (
    'sign in to confirm you',
    'http error 429',
    'too many requests',
    'rate limit',
    'temporarily blocked',
)


def binary_path() -> str:
    found = shutil.which(BINARY)
    if not found:
        raise YtdlpUnavailableError(f'{BINARY} is not on PATH — install it to read playlists')
    return found


def run(arguments: list[str], timeout_seconds: int) -> str:
    command = [binary_path(), *arguments]
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout_seconds)  # noqa: S603
    if result.returncode != 0:
        message = result.stderr.strip() or f'{BINARY} exited {result.returncode}'
        lowered = message.lower()
        if any(marker in lowered for marker in RATE_LIMIT_MARKERS):
            raise YtdlpRateLimitedError(message)
        raise YtdlpFailedError(message)
    return result.stdout


@dataclass(frozen=True)
class Cookies:
    """How a yt-dlp call proves who is asking, and what proving it costs.

    Naming a browser makes yt-dlp decrypt that browser's entire cookie store on
    every invocation, and enriching a library is one invocation per video.
    Measured against Safari on the same video: 11.6 seconds with the browser
    named, 7.9 with the same cookies exported once to a file, byte-identical
    metadata either way. A third of what a full library costs was being spent
    re-reading a file that had not changed since the run began.

    Both forms are kept because they answer different needs. A one-off read of a
    single playlist has nothing to amortise an export over, and `ypl auth` has
    no run to hang one on.
    """

    browser: str | None = None
    jar: Path | None = None

    def arguments(self) -> list[str]:
        """Private and unlisted playlists are invisible without a logged-in session."""
        if self.jar:
            return ['--cookies', str(self.jar)]
        if self.browser:
            return ['--cookies-from-browser', self.browser]
        return []


def cookie_arguments(cookies: Cookies | None) -> list[str]:
    return cookies.arguments() if cookies else []


def export_cookie_jar(
    browser: str, destination: Path, domain: str = COOKIE_DOMAIN, timeout_seconds: int = 120
) -> http.cookiejar.MozillaCookieJar:
    """Write one browser's cookies for one domain to a jar yt-dlp can be handed.

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

    Filtered to the one domain rather than handed on whole, because the whole
    jar is every site the browser has ever set a cookie for — 1.3 MB here — and
    yt-dlp parses all of it on every video. Filtering took the same read from
    11.1 seconds to 7.9. It is also simply the right thing to write down: a
    file of every session this browser holds is not what a YouTube read needs.
    """
    with tempfile.TemporaryDirectory() as directory:
        exported = Path(directory) / 'cookies.txt'
        arguments = ['--cookies-from-browser', browser, '--cookies', str(exported), '--simulate', UNRESOLVABLE_URL]
        try:
            subprocess.run(  # noqa: S603
                [binary_path(), *arguments], capture_output=True, text=True, timeout=timeout_seconds
            )
        except subprocess.TimeoutExpired as error:
            raise YtdlpFailedError(f'{BINARY} did not finish reading cookies from {browser}') from error
        if not exported.exists():
            raise YtdlpFailedError(f'{BINARY} read no cookies from {browser} — is it a browser it supports?')

        whole = http.cookiejar.MozillaCookieJar(str(exported))
        # Expiry and session flags are ignored deliberately: a cookie the
        # browser is still holding is one the browser is still using, and
        # Google's session cookies are exactly the ones that matter here.
        whole.load(ignore_discard=True, ignore_expires=True)

    jar = http.cookiejar.MozillaCookieJar(str(destination))
    for cookie in whole:
        if domain in (cookie.domain or ''):
            jar.set_cookie(cookie)
    # 0600, by `FileCookieJar.save` itself. These are a live Google session and
    # the browser is where they belong; the only reason a copy exists is that
    # yt-dlp has to be handed one per process.
    jar.save(ignore_discard=True, ignore_expires=True)
    return jar


def browser_cookies(browser: str, domain: str = COOKIE_DOMAIN, timeout_seconds: int = 120) -> dict[str, str]:
    """The YouTube cookies a browser is holding, as the write path wants them."""
    with tempfile.TemporaryDirectory() as directory:
        jar = export_cookie_jar(browser, Path(directory) / 'youtube.txt', domain=domain, timeout_seconds=timeout_seconds)
        return {cookie.name: cookie.value or '' for cookie in jar}


@contextmanager
def exported_cookies(browser: str | None) -> Iterator[Cookies]:
    """One cookie decrypt for a whole run, in a jar that does not outlive it.

    The lifetime is the point as much as the saving is. A run holds the jar for
    as long as it is making requests and the directory goes with it, so a
    crashed sync leaves no Google session sitting in the state directory waiting
    for somebody to notice it.

    A browser that cannot be read is not fatal: yt-dlp is handed the browser name
    instead, which is what every call did before this existed. A slower sync is
    a better answer than no sync, and the failure is one `ypl auth` reports
    properly.
    """
    if not browser:
        yield Cookies()
        return
    with tempfile.TemporaryDirectory() as directory:
        try:
            path = Path(directory) / 'youtube.txt'
            export_cookie_jar(browser, path)
            yield Cookies(jar=path)
        except (YtdlpFailedError, YtdlpUnavailableError, OSError):
            yield Cookies(browser=browser)


def fetch_account_playlists(cookies: Cookies, timeout_seconds: int = 300) -> list[PlaylistRef]:
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
    arguments = ['--flat-playlist', '--dump-single-json', *cookie_arguments(cookies), ACCOUNT_PLAYLISTS_URL]
    payload = json.loads(run(arguments, timeout_seconds))
    return [
        PlaylistRef(playlist_id=entry['id'], title=entry.get('title') or entry['id'])
        for entry in payload.get('entries') or []
        if entry.get('id')
    ]


def fetch_account_channel_id(cookies: Cookies, timeout_seconds: int = 120) -> str:
    """Which channel this browser is signed in as, by id.

    Read off `LL` — the account's own Liked videos — because it is the one list
    guaranteed to exist and to belong to whoever is signed in, and it reports
    the channel on the same flat request everything else here uses.
    `--playlist-items 0` asks for none of its several thousand videos, so this
    is one cheap request for one fact.

    An id rather than a name, and read here rather than from the account menu,
    because the name was the whole problem: the menu answers with the *Google
    account* — `Chris Birch` — for a channel called `iChrisBirch`, so comparing
    them judged every playlist this account owns to belong to somebody else.
    Ids are what the two reads agree on.
    """
    arguments = [
        '--flat-playlist',
        '--playlist-items',
        '0',
        '--dump-single-json',
        *cookie_arguments(cookies),
        ACCOUNT_CHANNEL_URL,
    ]
    payload = json.loads(run(arguments, timeout_seconds))
    return payload.get('channel_id') or ''


def fetch_playlist(url: str, cookies: Cookies | None = None, timeout_seconds: int = 600) -> RemotePlaylist:
    """List a playlist's videos without fetching each one.

    Flat, so this is one request for the whole playlist regardless of length.
    The per-video description and chapters are deliberately not here — see
    `fetch_video`.
    """
    arguments = [
        '--flat-playlist',
        '--dump-single-json',
        *cookie_arguments(cookies),
        url,
    ]
    payload = json.loads(run(arguments, timeout_seconds))
    return RemotePlaylist(
        playlist_id=payload['id'],
        title=payload.get('title') or payload['id'],
        description=payload.get('description') or '',
        channel=payload.get('channel') or payload.get('uploader') or '',
        channel_id=payload.get('channel_id') or '',
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


def fetch_video(video_id: str, cookies: Cookies | None = None, timeout_seconds: int = 120) -> RemoteVideo:
    """Fully extract one video, for its description and chapters."""
    arguments = [
        '--dump-json',
        '--skip-download',
        *cookie_arguments(cookies),
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
