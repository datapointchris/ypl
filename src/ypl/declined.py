"""The playlists an automatic sync must not bring back.

`ypl sync` adopts every playlist the account owns, which is what makes syncing
transparent — and which would otherwise make deleting one impossible. Deleting a
local playlist that YouTube still holds is a statement about wanting it *here*,
so it is recorded, and the next sync leaves that playlist alone.

Data rather than state, by the same rule as the playlists themselves: the mirror
rebuilds from a free read and nothing rebuilds an intention. A declined list
thrown away with the mirror would silently re-adopt everything it named.
"""

import json
from pathlib import Path

from ypl import paths


class DeclinedError(RuntimeError):
    """The declined list exists and cannot be read.

    Raised rather than treated as empty. Reading a broken file as "nothing is
    declined" would re-adopt every playlist that was deliberately deleted, which
    is the exact failure this file exists to prevent.
    """


def load() -> set[str]:
    path = paths.declined_file()
    if not path.exists():
        return set()
    try:
        payload = json.loads(path.read_text())
        return {str(playlist_id) for playlist_id in payload['playlist_ids']}
    except (OSError, json.JSONDecodeError, KeyError, TypeError) as error:
        raise DeclinedError(f'{path} cannot be read: {error}') from error


def save(playlist_ids: set[str]) -> Path:
    path = paths.declined_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix('.json.tmp')
    temporary.write_text(json.dumps({'playlist_ids': sorted(playlist_ids)}, indent=2) + '\n')
    temporary.replace(path)
    return path


def add(playlist_id: str) -> None:
    if not playlist_id:
        return
    save(load() | {playlist_id})


def remove(playlist_id: str) -> None:
    """Un-decline, for when a playlist is deliberately adopted again."""
    declined = load()
    if playlist_id in declined:
        save(declined - {playlist_id})
