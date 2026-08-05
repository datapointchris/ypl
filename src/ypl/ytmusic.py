"""The `remote.Backend` implementation, over `ytmusicapi`.

Isolated from `remote` so that the interface and the choice of provider stay
separable: swapping to the official Data API is replacing this module, not
rewriting the write path.

Imported lazily inside the constructor rather than at module scope, so the rest
of ypl — every read command, every local playlist operation — keeps working on a
machine where the write path has never been set up.
"""

from pathlib import Path

from ypl.remote import CREATE_INTERVAL_SECONDS
from ypl.remote import DEFAULT_PRIVACY
from ypl.remote import MAX_BATCH
from ypl.remote import RemoteAuthError
from ypl.remote import RemoteError
from ypl.remote import RemoteItem
from ypl.remote import RemoteRateLimitedError
from ypl.remote import Throttle
from ypl.remote import batched

# What YouTube says when it wants us to slow down. Matched on text because it
# arrives as a message inside a generic server error rather than as a status.
RATE_LIMIT_MARKERS = ('rate_limit_exceeded', 'resource_exhausted', 'too many', '429')

AUTH_MARKERS = ('unauthorized', 'not authorized', 'authentication', 'cookie', '401', '403')


def looks_like(message: str, markers: tuple[str, ...]) -> bool:
    lowered = message.lower()
    return any(marker in lowered for marker in markers)


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
