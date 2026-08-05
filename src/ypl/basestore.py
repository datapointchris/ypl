"""What YouTube held at the last reconcile, one file per playlist.

This is the third state a remote-wins merge needs. Local `[A, B, C]` against
remote `[A, C]` has two possible histories — B was deleted on the phone, or B
was added here and not pushed yet — and they are the same two lists with
opposite correct actions. YouTube exposes no per-item modification time and a
removal leaves nothing behind at all, so the only way to tell them apart is to
have written down what was there last time. With a base, each side's changes
are computable separately: remote-only changes get applied locally, local-only
changes get queued, and anything both touched resolves remote's way.

It is **data**, not state, which is why it lives beside the playlists under
`$XDG_DATA_HOME` rather than in the mirror. The mirror is rebuildable from a
free `ypl sync`; this is not rebuildable at all, because re-reading YouTube
answers what is there now rather than what was there then. Deleting it does not
lose a playlist, but it does make the next merge ambiguous in exactly the way
above.

It doubles as the handle map. `setVideoId` identifies a *slot* rather than a
video, it is the only way to remove or move one, and only the write backend's
own read returns it — yt-dlp does not. Recording it here means the push path
knows the handles it needs before it starts, and re-reads to confirm them
rather than to discover them.
"""

import json
import os
from dataclasses import dataclass
from dataclasses import field
from datetime import UTC
from datetime import datetime
from pathlib import Path

from ypl import paths
from ypl.remote import RemoteItem

SUFFIX = '.json'


class BaseStoreError(ValueError):
    """A base file exists but cannot be read.

    Never softened into "no base": an unreadable base and an absent one mean
    opposite things to the merge. Absent says nothing has ever been reconciled,
    so everything local is new; unreadable says a snapshot exists and cannot be
    trusted, and pushing on that assumption would queue deletions for videos
    that were only ever added on the phone.
    """

    def __init__(self, path: Path, reason: str):
        self.path = path
        self.reason = reason
        super().__init__(f'{path}: {reason}')


@dataclass
class Base:
    """One playlist's remote state as of the last reconcile.

    Order is carried by the list itself, which is what lets the merge ask
    whether remote's order changed since the base — the one question that
    decides whose ordering wins for the whole playlist.
    """

    slug: str
    playlist_id: str = ''
    reconciled_ts: str = ''
    items: list[RemoteItem] = field(default_factory=list)

    @property
    def video_ids(self) -> list[str]:
        return [item.video_id for item in self.items]

    @property
    def pushable(self) -> bool:
        """Whether this base carries the handles a push would need.

        A read taken while signed out returns the playlist and no `setVideoId`
        for any of it — enough to look like a successful reconcile, and useless
        the moment something has to be removed or moved. Judged on the whole
        list rather than per item, because one missing handle is an oddity and
        none of them is a different kind of read entirely.
        """
        return not self.items or any(item.set_video_id for item in self.items)

    def handles_for(self, video_id: str) -> list[str]:
        """Every slot holding this video, in playlist order.

        A list rather than one handle because a playlist may hold the same
        video twice — YouTube allows it and `add_items` is called with
        `duplicates=True` — so removing "that video" is ambiguous until a
        caller picks which slot it means.
        """
        return [item.set_video_id for item in self.items if item.video_id == video_id]


def now_ts() -> str:
    return datetime.now(UTC).isoformat(timespec='seconds')


def path_for(slug: str) -> Path:
    return paths.remote_dir() / f'{slug}{SUFFIX}'


def load(slug: str) -> Base | None:
    """The recorded base, or None when this playlist has never been reconciled."""
    path = path_for(slug)
    if not path.exists():
        return None
    try:
        payload = json.loads(path.read_text())
    except json.JSONDecodeError as error:
        raise BaseStoreError(path, str(error)) from error
    if not isinstance(payload, dict):
        raise BaseStoreError(path, f'expected an object, got {type(payload).__name__}')

    raw_items = payload.get('items', [])
    if not isinstance(raw_items, list):
        raise BaseStoreError(path, f'items must be a list, got {type(raw_items).__name__}')
    items = []
    for position, raw in enumerate(raw_items):
        if not isinstance(raw, dict) or not raw.get('video_id'):
            raise BaseStoreError(path, f'item {position} has no video_id')
        items.append(
            RemoteItem(
                video_id=raw['video_id'],
                set_video_id=raw.get('set_video_id') or '',
                title=raw.get('title') or '',
            )
        )
    return Base(
        slug=payload.get('slug') or slug,
        playlist_id=payload.get('playlist_id') or '',
        reconciled_ts=payload.get('reconciled_ts') or '',
        items=items,
    )


def save(base: Base, reconciled_ts: str | None = None) -> Path:
    """Record a reconcile, replacing the previous base atomically.

    Written to a neighbouring temporary file and renamed over the old one,
    because a half-written base is worse than an absent one: it would raise on
    every subsequent merge until someone deleted it by hand, and the state it
    described is unrecoverable by then.
    """
    base.reconciled_ts = reconciled_ts or now_ts()
    payload = {
        'slug': base.slug,
        'playlist_id': base.playlist_id,
        'reconciled_ts': base.reconciled_ts,
        'items': [{'video_id': item.video_id, 'set_video_id': item.set_video_id, 'title': item.title} for item in base.items],
    }
    path = path_for(base.slug)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f'{path.name}.writing')
    try:
        temporary.write_text(json.dumps(payload, indent=2) + '\n')
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)
    return path


def delete(slug: str) -> None:
    """Forget a playlist's base.

    For a playlist that no longer exists here. A stale base is not inert: a new
    playlist that happens to slug the same way would inherit it and read as
    having had every one of those videos deleted locally, which the queue would
    faithfully carry out on YouTube.
    """
    path_for(slug).unlink(missing_ok=True)
