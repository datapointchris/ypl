"""The three-way merge behind a reconcile.

Pure: lists of video ids in, lists of video ids out. Nothing here reads a file
or touches YouTube, because this is the part that has to be reasoned about
exactly and a test that has to build a playlist file to state a case is a test
nobody writes.

Three inputs, and the third is the point. Local `[A, B, C]` against remote
`[A, C]` has two possible histories — B was deleted on the phone, or B was
added here and never pushed — and they are the same two lists with opposite
correct actions. The base, what YouTube held at the last reconcile, is what
tells them apart: anything in the base and missing from a side was deleted by
that side, and anything on a side and missing from the base was added by it.

Membership merges per video. Order does not: it is settled once for the whole
playlist, because per-item order merging turns a moved track into an argument
with no answer, and a playlist whose order is half one machine's and half
another's is worse than either.
"""

from dataclasses import dataclass
from dataclasses import field

LOCAL = 'local'
REMOTE = 'remote'

# One occurrence of one video: the id, and which copy of it this is. A playlist
# may legitimately hold the same mix twice — YouTube allows it and ypl adds
# with `duplicates=True` — so a bare set of ids would read the second copy as
# noise and quietly drop it. Keying by occurrence turns counting into set
# arithmetic and keeps every copy accounted for.
Key = tuple[str, int]


@dataclass
class Merge:
    """What the reconcile decided, and what is left to push.

    The pending lists are reporting rather than a queue. What has to go up is
    always re-derivable by comparing the local file against the base, which is
    why the queue is state rather than data and why nothing here is written
    down as an instruction to be replayed later.
    """

    order: list[str] = field(default_factory=list)
    pulled_in: list[str] = field(default_factory=list)
    pulled_out: list[str] = field(default_factory=list)
    pending_add: list[str] = field(default_factory=list)
    pending_remove: list[str] = field(default_factory=list)
    order_source: str = LOCAL

    @property
    def changed_here(self) -> bool:
        return bool(self.pulled_in or self.pulled_out)

    @property
    def to_push(self) -> bool:
        return bool(self.pending_add or self.pending_remove)


def keyed(video_ids: list[str]) -> list[Key]:
    """Number each video by which copy of itself it is, in order."""
    seen: dict[str, int] = {}
    keys = []
    for video_id in video_ids:
        keys.append((video_id, seen.get(video_id, 0)))
        seen[video_id] = seen.get(video_id, 0) + 1
    return keys


def reordered(base: list[Key], other: list[Key]) -> bool:
    """Whether `other` moved anything relative to the base.

    Only the videos both hold can answer this. Comparing the two lists whole
    would call every addition and every removal a reorder, which would hand
    remote the ordering of the entire playlist every time anyone added a track
    on their phone.
    """
    common = set(base) & set(other)
    return [key for key in base if key in common] != [key for key in other if key in common]


def weave(primary: list[Key], secondary: list[Key], wanted: set[Key]) -> list[Key]:
    """Order `wanted` by `primary`, slotting in what only `secondary` knows.

    An item the winning order has never seen goes where the other order put it
    — directly after whichever item precedes it there — rather than at the end.
    Appending would be simpler and would move a track added at the front of the
    playlist on a phone to the back of it here, which reads as ypl having
    reordered something nobody touched.

    The cursor only ever moves forward. Without that, two orders that disagree
    about the items they share drag it backwards: local `[c, b, a]` against
    remote `[a, b, c, g]` would anchor g to c, the last thing remote saw before
    it, and land it second in a playlist where remote meant it last. Held
    forward, an anchor that has stopped meaning anything degrades to the end of
    the list, which is where the side that reordered had it.
    """
    result = [key for key in primary if key in wanted]
    cursor = -1
    for key in secondary:
        if key not in wanted:
            continue
        if key in result:
            cursor = max(cursor, result.index(key))
            continue
        cursor += 1
        result.insert(cursor, key)
    return result


def merge(base: list[str], remote: list[str], local: list[str]) -> Merge:
    """Reconcile one playlist's three states.

    An empty base means this playlist has never been reconciled, and the result
    is the union of both sides: with nothing recorded, no absence can be read
    as a deletion, and guessing wrong would delete a playlist's worth of videos
    on the strength of never having looked.
    """
    base_keys, remote_keys, local_keys = keyed(base), keyed(remote), keyed(local)
    base_set, remote_set, local_set = set(base_keys), set(remote_keys), set(local_keys)

    removed_remotely = base_set - remote_set
    removed_locally = base_set - local_set
    wanted = (remote_set | local_set) - removed_remotely - removed_locally

    # Remote wins, so the only question is whether remote reordered at all. A
    # local reorder survives only while remote left its own order alone.
    order_source = REMOTE if reordered(base_keys, remote_keys) else LOCAL
    primary, secondary = (remote_keys, local_keys) if order_source == REMOTE else (local_keys, remote_keys)

    # A video both sides added independently is in neither report: it needs
    # nothing done to it, and pushing it again would make it a duplicate.
    arrived = wanted - base_set - local_set
    departed = removed_remotely & local_set
    to_add = wanted - base_set - remote_set
    to_remove = removed_locally & remote_set

    return Merge(
        order=[video_id for video_id, _ in weave(primary, secondary, wanted)],
        pulled_in=[video_id for video_id, _ in (key for key in remote_keys if key in arrived)],
        pulled_out=[video_id for video_id, _ in (key for key in base_keys if key in departed)],
        pending_add=[video_id for video_id, _ in (key for key in local_keys if key in to_add)],
        pending_remove=[video_id for video_id, _ in (key for key in base_keys if key in to_remove)],
        order_source=order_source,
    )
