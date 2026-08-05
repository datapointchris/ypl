"""The `remote.Backend` implementation, over `ytmusicapi`.

Isolated from `remote` so that the interface and the choice of provider stay
separable: swapping to the official Data API is replacing this module, not
rewriting the write path.

Imported lazily inside the constructor rather than at module scope, so the rest
of ypl — every read command, every local playlist operation — keeps working on a
machine where the write path has never been set up.
"""

import hashlib
import os
import time
from pathlib import Path

from ypl.remote import CREATE_INTERVAL_SECONDS
from ypl.remote import DEFAULT_PRIVACY
from ypl.remote import MAX_BATCH
from ypl.remote import RemoteAccount
from ypl.remote import RemoteAuthError
from ypl.remote import RemoteError
from ypl.remote import RemoteItem
from ypl.remote import RemoteRateLimitedError
from ypl.remote import batched
from ypl.throttle import Throttle

# Owner read/write only. The file holds a Google account session cookie, which
# is the whole credential — anything that can read it is signed in as you.
SESSION_MODE = 0o600

# What the SAPISIDHASH is computed against, and the origin YouTube Music's own
# client sends. A hash built against a different origin is rejected.
ORIGIN = 'https://music.youtube.com'

SAPISID_COOKIE = '__Secure-3PAPISID'

# What YouTube says when it wants us to slow down. Matched on text because it
# arrives as a message inside a generic server error rather than as a status.
RATE_LIMIT_MARKERS = ('rate_limit_exceeded', 'resource_exhausted', 'too many', '429')

AUTH_MARKERS = ('unauthorized', 'not authorized', 'authentication', 'cookie', '401', '403')


def looks_like(message: str, markers: tuple[str, ...]) -> bool:
    lowered = message.lower()
    return any(marker in lowered for marker in markers)


def session_headers(cookies: dict[str, str]) -> str:
    """Build the request headers ytmusicapi wants out of a browser's cookies.

    The headers pasted out of DevTools are only a delivery mechanism for three
    facts, and every one of them is derivable from the cookie jar. `cookie` is
    the credential. `x-goog-authuser` selects the signed-in account, and 0 is
    the one you are signed in as unless you have several. `authorization` looks
    like the important one and is the least: ytmusicapi recomputes the
    SAPISIDHASH from the cookie before every single request, because it is
    timestamped and would expire otherwise — the stored value exists only so
    the session is recognised as browser auth rather than OAuth.

    So this hashes one, correctly, and never relies on it again.
    """
    sapisid = cookies.get(SAPISID_COOKIE)
    if not sapisid:
        raise RemoteAuthError(f'that browser is not signed in to YouTube — no {SAPISID_COOKIE} cookie. Sign in and try again')
    stamp = str(int(time.time()))
    digest = hashlib.sha1(f'{stamp} {sapisid} {ORIGIN}'.encode()).hexdigest()  # noqa: S324 - Google's scheme, not a choice
    return '\n'.join(
        [
            'cookie: ' + '; '.join(f'{name}={value}' for name, value in cookies.items()),
            'x-goog-authuser: 0',
            f'authorization: SAPISIDHASH {stamp}_{digest}',
            f'origin: {ORIGIN}',
        ]
    )


def write_session(headers_raw: str, auth_file: Path) -> Path:
    """Turn request headers pasted out of a browser into a session file.

    ytmusicapi's own `setup` writes the file itself, and it writes it at
    whatever the umask allows — a session cookie sitting world-readable, even
    for the moment before a chmod, is not something to hand off. So it is
    called for its parsing only and the write happens here, with the mode
    supplied at creation.

    Browser headers rather than OAuth because ytmusicapi's OAuth flow now needs
    a TV-type Google client of your own, which is the Data API project setup
    this backend exists to avoid.
    """
    try:
        from ytmusicapi import setup
    except ImportError as error:  # pragma: no cover - dependency is declared
        raise RemoteError('ytmusicapi is not installed') from error

    if not headers_raw.strip():
        raise RemoteAuthError('no request headers were given')
    try:
        session = setup(headers_raw=headers_raw)
    except Exception as error:
        raise RemoteAuthError(str(error)) from error

    auth_file.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(auth_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, SESSION_MODE)
    with os.fdopen(descriptor, 'w') as handle:
        # The creation mode above does nothing when the file already exists, and
        # replacing a session is the common case.
        os.fchmod(handle.fileno(), SESSION_MODE)
        handle.write(session)
    return auth_file


class YtMusicBackend:
    """Writes through the YouTube Music web client's endpoints.

    Every call is throttled, because the whole argument for using this rather
    than the sanctioned API is that it can do the same work in far fewer
    requests — which only holds if it does not then spend that saving on speed.
    """

    def __init__(self, auth_file: Path, throttle: Throttle | None = None, create_throttle: Throttle | None = None):
        try:
            from ytmusicapi import YTMusic
        except ImportError as error:  # pragma: no cover - dependency is declared
            raise RemoteError('ytmusicapi is not installed') from error

        if not auth_file.exists():
            raise RemoteAuthError(f'{auth_file} does not exist — run `ypl remote auth` first')
        try:
            self.client = YTMusic(str(auth_file))
        except Exception as error:
            raise self.translate(error) from error
        self.throttle = throttle or Throttle()
        # Separate, because creation is the only endpoint with a measured limit
        # and sharing one floor would either crawl every add or outrun the one
        # limit we know about.
        self.create_throttle = create_throttle or Throttle(CREATE_INTERVAL_SECONDS)

    @staticmethod
    def translate(error: Exception) -> RemoteError:
        """Turn ytmusicapi's two error classes into something the queue can act on.

        The distinction that matters is not user-versus-server but whether
        waiting will help: a throttle clears on its own, a dead cookie never
        does.
        """
        message = str(error)
        if looks_like(message, RATE_LIMIT_MARKERS):
            return RemoteRateLimitedError(message)
        if looks_like(message, AUTH_MARKERS):
            return RemoteAuthError(message)
        return RemoteError(message)

    def call(self, method, *args, throttle: Throttle | None = None, **kwargs):
        (throttle or self.throttle).wait()
        try:
            return method(*args, **kwargs)
        except Exception as error:
            raise self.translate(error) from error

    def account(self) -> RemoteAccount:
        """Who the stored session signs in as.

        The one call that distinguishes a headers file that parsed from a
        session that works: the paste is validated locally, the cookie is only
        ever tested by YouTube answering.
        """
        payload = self.call(self.client.get_account_info)
        return RemoteAccount(name=payload.get('accountName') or '', handle=payload.get('channelHandle') or '')

    def playlist_items(self, playlist_id: str) -> list[RemoteItem]:
        """Read the playlist the way the write path needs it.

        A separate read from `ypl sync` despite the cost, because `setVideoId`
        is the handle for removing or moving a slot and yt-dlp does not return
        it. Reading immediately before writing is also what makes a push act on
        what is there now rather than on what was mirrored days ago.
        """
        payload = self.call(self.client.get_playlist, playlist_id, limit=None)
        return [
            RemoteItem(
                video_id=track['videoId'],
                set_video_id=track.get('setVideoId') or '',
                title=track.get('title') or '',
            )
            for track in payload.get('tracks') or []
            if track.get('videoId')
        ]

    def create_playlist(self, title: str, description: str = '') -> str:
        created = self.call(
            self.client.create_playlist,
            title,
            description,
            privacy_status=DEFAULT_PRIVACY,
            throttle=self.create_throttle,
        )
        if not isinstance(created, str):
            raise RemoteError(f'playlist was not created: {created}')
        return created

    def add_items(self, playlist_id: str, video_ids: list[str]) -> None:
        for batch in batched(video_ids, MAX_BATCH):
            self.call(self.client.add_playlist_items, playlist_id, batch, duplicates=True)

    def remove_items(self, playlist_id: str, items: list[RemoteItem]) -> None:
        for batch in batched(items, MAX_BATCH):
            payload = [{'videoId': item.video_id, 'setVideoId': item.set_video_id} for item in batch]
            self.call(self.client.remove_playlist_items, playlist_id, payload)

    def move_item(self, playlist_id: str, item: RemoteItem, before: RemoteItem | None) -> None:
        """Move one slot ahead of another, or to the end when `before` is None.

        One request per move — `edit_playlist` takes a single `moveItem` and
        there is no batch form. That asymmetry is why reordering is the
        expensive operation here while adding a hundred videos is one call, and
        why the queue computes the shortest move sequence rather than
        rewriting the order slot by slot.
        """
        move = (item.set_video_id, before.set_video_id) if before else item.set_video_id
        self.call(self.client.edit_playlist, playlist_id, moveItem=move)
