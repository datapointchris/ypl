"""One sync at a time, per machine.

Only a concern because the sync runs itself now. A timer firing while a run is
still going — a slow run, a laptop waking, a foreground run started by hand —
would put two processes through the same playlists at once: both adopting,
both writing the same M3U files and bases, both reading YouTube for the same
answers. SQLite survives that (the mirror is WAL); the files and the request
budget do not.

An advisory `flock` rather than a pid file, because the kernel releases it when
the process dies. A pid file left behind by a crash is a sync that never runs
again until someone notices, which for something unattended means never.
"""

import fcntl
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

from ypl import paths


def lock_file() -> Path:
    return paths.state_dir() / 'sync.lock'


@contextmanager
def held() -> Iterator[bool]:
    """Hold the sync lock for the block, yielding whether this run got it.

    False rather than an exception: another sync already running is the system
    working, not a failure, and the caller decides whether that is worth saying
    out loud.
    """
    path = lock_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = path.open('w')
    try:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            yield False
            return
        yield True
    finally:
        handle.close()
