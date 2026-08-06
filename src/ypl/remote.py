"""What the write path needs from YouTube, and the planning that is provider-free.

The interface and the provider stay separable: this module is the `Backend`
Protocol, the pacing constants and the reorder planner, and `src/ypl/youtubei.py`
is the only thing that knows what a request looks like. Swapping to the official
Data API would be replacing that module, not rewriting the write path — which is
the point of the split, and why the argument for the current provider lives
there rather than here.

The pacing lives here because the reason for it is not a provider detail. The
credential is a Google account cookie and the Terms prohibit automated access,
so everything is built to stay at human scale rather than to go as fast as it
can: every call goes through `Throttle`, batches are bounded, and a 429 stops
the run instead of retrying into it.
"""

from collections.abc import Hashable
from dataclasses import dataclass
from typing import Protocol

# Playlist creation gets its own, far slower floor. It is the one endpoint with
# a limit anyone has actually measured — roughly twenty in fifteen minutes
# before a 429 that clears in hours — and it is also the operation that now
# fires most often, because every playlist made here is synced. A single
# creation still goes straight through; only a burst gets spaced out.
CREATE_INTERVAL_SECONDS = 60.0

# Private, so a playlist reaches your own phone and nobody else's. A private
# playlist still appears in your library on every device you are signed in on,
# which is the whole point of pushing it.
DEFAULT_PRIVACY = 'PRIVATE'

# Adds and removes carry an `actions` array, so a hundred of them cost one
# request. Bounded anyway: the web client never sends an unbounded batch, and a
# request that stands out is the thing worth avoiding.
MAX_BATCH = 100


class RemoteError(RuntimeError):
    pass


class RemoteAuthError(RemoteError):
    """The stored session is missing, expired, or not for an account that owns this."""


class RemoteRateLimitedError(RemoteError):
    """YouTube asked us to slow down.

    Raised rather than retried. The documented limit is per-endpoint and clears
    on its own in hours, so the useful response is to stop and drain later —
    retrying into it is what turns a throttle into a pattern worth noticing.
    """


@dataclass
class RemoteItem:
    """One slot in a remote playlist.

    `set_video_id` is the playlist-item handle, distinct from the video id and
    the only way to remove or move a specific slot. It comes from the write
    backend's own read of the playlist; yt-dlp does not expose it, which is why
    a push reads the playlist again rather than trusting the mirror.
    """

    video_id: str
    set_video_id: str
    title: str = ''


@dataclass
class RemoteAccount:
    """Who a stored session belongs to."""

    name: str = ''
    handle: str = ''


class Backend(Protocol):
    """What the write path needs from YouTube, and nothing more."""

    def account(self) -> RemoteAccount:
        """Whose account this session reaches.

        Part of the interface rather than a setup detail: a session file that
        parses says nothing about whether the cookie still works, so every
        backend needs one call that answers that, and it may as well be one
        whose answer is worth printing.
        """
        ...

    def playlist_items(self, playlist_id: str) -> list[RemoteItem]: ...

    def create_playlist(self, title: str, description: str = '') -> str: ...

    def delete_playlist(self, playlist_id: str) -> None:
        """Remove the playlist from YouTube.

        Part of the interface because the file and the playlist are one thing:
        `ypl playlists delete` destroys both, and a backend that could create
        but not delete would leave the account accumulating every playlist this
        tool ever made.
        """
        ...

    def rename_playlist(self, playlist_id: str, title: str) -> None:
        """Change the playlist's title to match the file's.

        A rename is the one local edit with no representation in the items, so
        without this the sync would have to answer a renamed file by deleting
        the playlist and making a new one — losing its id, its handles and its
        place in anyone's library.
        """
        ...

    def add_items(self, playlist_id: str, video_ids: list[str]) -> None: ...

    def remove_items(self, playlist_id: str, items: list[RemoteItem]) -> None: ...

    def move_item(self, playlist_id: str, item: RemoteItem, before: RemoteItem | None) -> None: ...


def batched(items: list, size: int = MAX_BATCH) -> list[list]:
    return [items[start : start + size] for start in range(0, len(items), size)]


def longest_kept_run(order: list[int]) -> set[int]:
    """Positions forming the longest already-correctly-ordered subsequence.

    A strictly increasing subsequence of target positions is the set of slots
    that are already in the right order relative to each other, so every other
    slot has to move and these do not. Patience sorting, because a library
    playlist runs to thousands of items and the quadratic version starts to
    show.
    """
    if not order:
        return set()
    tails: list[int] = []  # tails[length - 1] = index of the smallest tail of that length
    previous: list[int | None] = [None] * len(order)
    for index, value in enumerate(order):
        low, high = 0, len(tails)
        while low < high:
            middle = (low + high) // 2
            if order[tails[middle]] < value:
                low = middle + 1
            else:
                high = middle
        previous[index] = tails[low - 1] if low else None
        if low == len(tails):
            tails.append(index)
        else:
            tails[low] = index

    kept = set()
    cursor: int | None = tails[-1]
    while cursor is not None:
        kept.add(cursor)
        cursor = previous[cursor]
    return kept


def move_plan[Slot: Hashable](current: list[Slot], desired: list[Slot]) -> list[tuple[Slot, Slot | None]]:
    """The shortest sequence of moves turning `current` into `desired`.

    Each pair is (what to move, what it should end up in front of), with None
    meaning the end. Keys must be unique and both lists must hold the same set —
    a playlist may contain the same video twice, so callers key on something
    that separates the copies: the playlist-item handle once a slot exists, and
    the occurrence of a video id while planning against slots that do not.

    Worth the trouble because a move is one request and cannot be batched.
    Rewriting a 200-item playlist slot by slot is 200 requests; moving only what
    is genuinely out of place is usually a handful. The moves are emitted in
    reverse target order so that each item's successor is already final when the
    item is placed in front of it.
    """
    if current == desired:
        return []
    target = {key: position for position, key in enumerate(desired)}
    if set(current) != set(target) or len(current) != len(desired):
        raise ValueError('move_plan needs both orders to hold the same items')

    kept_positions = longest_kept_run([target[key] for key in current])
    kept = {current[position] for position in kept_positions}

    successors = {key: desired[position + 1] if position + 1 < len(desired) else None for position, key in enumerate(desired)}
    return [(key, successors[key]) for key in reversed(desired) if key not in kept]
