"""Writes against YouTube, through the YouTube Music web client's own endpoints.

Not the official Data API. `playlistItems.insert` costs 50 units of a per-project
10,000/day, which is 200 writes a day permanently — a cap no OAuth setup lifts
and that a Google audit personal tools do not get would be needed to raise.
Splitting an 1,800-video playlist through it is eighteen days of queue draining,
which forces exactly the shape that looks least like a person: a daemon making
requests around the clock for weeks.

`ytmusicapi` speaks the web client's protocol with the session cookie, and adds
batch: one request carries an `actions` array holding a hundred additions. The
same 1,800-video reorganisation is a couple of dozen requests. Doing it this way
generates *less* traffic than clicking through the web UI would.

That trade is deliberate and it is not free — the YouTube Terms prohibit
automated access to the Service, and the credential is a Google account cookie.
Everything here is therefore built to stay at human scale rather than to go as
fast as it can: every call goes through `Throttle`, batches are bounded, and a
429 stops the run instead of retrying into it.

The backend is a Protocol so the Data API remains a one-module swap if that
trade ever stops being worth it.
"""

import time
from collections.abc import Hashable
from dataclasses import dataclass
from typing import Protocol

DEFAULT_INTERVAL_SECONDS = 2.0

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

    def add_items(self, playlist_id: str, video_ids: list[str]) -> None: ...

    def remove_items(self, playlist_id: str, items: list[RemoteItem]) -> None: ...

    def move_item(self, playlist_id: str, item: RemoteItem, before: RemoteItem | None) -> None: ...


class Throttle:
    """A floor on the gap between calls.

    Deliberately a floor rather than a token bucket: a bucket permits a burst,
    and a burst is the shape that gets noticed. Sleeping is fine here because
    the queue drains in the background and nothing waits on it.
    """

    def __init__(self, interval_seconds: float = DEFAULT_INTERVAL_SECONDS, sleep=time.sleep, clock=time.monotonic):
        self.interval_seconds = interval_seconds
        self.sleep = sleep
        self.clock = clock
        self.last_call: float | None = None

    def wait(self) -> float:
        """Block until the next call is allowed, returning how long that took."""
        if self.last_call is None:
            self.last_call = self.clock()
            return 0.0
        due = self.last_call + self.interval_seconds
        now = self.clock()
        waited = max(0.0, due - now)
        if waited:
            self.sleep(waited)
        self.last_call = self.clock()
        return waited


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
