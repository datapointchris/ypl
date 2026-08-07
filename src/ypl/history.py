"""Listening history, as an append-only log under `$XDG_DATA_HOME`.

Not a table in the mirror, and the distinction is the same one that put local
playlists in files: the mirror is **state**, rebuildable from a free `ypl sync`,
so losing it costs only time. A record of what you have listened to is
rebuildable from nothing. Keeping it here means `ypl next` still knows after the
mirror is thrown away and re-synced, and means the history follows the machine
sync that carries the playlists.

Append-only rather than rewritten, because every write is one more line at the
end — the shape least likely to lose earlier entries when two machines both
append.
"""

import datetime as dt
import json
from dataclasses import dataclass

from ypl import paths


@dataclass
class Play:
    video_id: str
    played_ts: str


def now_ts() -> str:
    return dt.datetime.now(dt.UTC).isoformat(timespec='seconds')


def record(video_id: str, played_ts: str | None = None) -> Play:
    """Append one listen."""
    play = Play(video_id=video_id, played_ts=played_ts or now_ts())
    path = paths.plays_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('a') as log:
        log.write(json.dumps({'video_id': play.video_id, 'played_ts': play.played_ts}) + '\n')
    return play


def load() -> list[Play]:
    """Every listen, oldest first.

    A line that does not parse is skipped rather than fatal: the realistic way
    this file breaks is a half-written final line from an interrupted append,
    and losing one listen is not worth refusing to answer at all.
    """
    path = paths.plays_file()
    if not path.exists():
        return []
    plays = []
    for line in path.read_text().splitlines():
        if not line.strip():
            continue
        try:
            payload = json.loads(line)
            plays.append(Play(video_id=payload['video_id'], played_ts=payload['played_ts']))
        except (json.JSONDecodeError, KeyError, TypeError):
            continue
    return plays


def summary() -> dict[str, dict]:
    """When each video was last played and how many times, keyed by video id.

    The file is in append order, so the last mention of a video is its most
    recent listen — no timestamp comparison, which also settles two listens
    logged inside the same second.
    """
    found: dict[str, dict] = {}
    for play in load():
        entry = found.setdefault(play.video_id, {'last_played_ts': play.played_ts, 'play_count': 0})
        entry['last_played_ts'] = play.played_ts
        entry['play_count'] += 1
    return found
