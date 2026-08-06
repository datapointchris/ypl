"""One sync at a time, per machine.

Only a concern because the sync runs itself now. A timer firing while a run is
still going — a slow run, a laptop waking, a foreground run started by hand —
would put two processes through the same playlists at once: both binding,
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


def running() -> bool:
    """Whether a sync is going on right now.

    Asked by `ypl status`, which is otherwise only able to describe runs that
    have already finished — and a background process you cannot see mid-run is
    one you cannot tell apart from a broken one. Taking the lock and dropping it
    again is the check; nothing else can answer it without a pid file.
    """
    path = lock_file()
    if not path.exists():
        return False
    with path.open('w') as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return True
        fcntl.flock(handle, fcntl.LOCK_UN)
    return False


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
