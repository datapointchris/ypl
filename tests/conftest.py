"""Test-wide guarantees, the first of which is that no test reaches YouTube.

The suite drives the real app — that is the point of it — and the real app
shells out to `yt-dlp` carrying whatever cookies the machine has. So every
process this code can start is refused here by default, and a test that needs
one says so by stubbing the specific function it calls.

This is not hypothetical tidiness. When `ypl sync` became the only way to drive
a reconcile, tests that had carefully stubbed the playlist reads still left
`fetch_video` real, and the enrich tail at the end of every run went to YouTube
on the signed-in account — unthrottled, because the suite also disables the
pacing. A suite that can quietly make requests is the one thing this project
must not have, so the default is loud refusal rather than a stub that happens
to be in place.
"""

import subprocess
from pathlib import PurePath

import pytest

# Anything that reaches YouTube, the account, or this machine's services. A
# local editor process is allowed — it touches a temp file and nothing else.
FORBIDDEN = ('yt-dlp', 'mpv', 'launchctl', 'systemctl')


@pytest.fixture(autouse=True)
def no_subprocesses(monkeypatch):
    """Refuse every child process, naming the command that wanted one.

    Patched at `subprocess.run` rather than at each caller because the callers
    are the thing that keeps changing: `ytdlp` alone has two entry points, and
    `player`, `schedule` and `editbuffer` each have their own. A guarantee that
    has to be re-applied per module is not one.
    """

    real_run, real_popen = subprocess.run, subprocess.Popen

    def guard(real):
        def checked(command, *args, **kwargs):
            parts = list(command) if isinstance(command, list | tuple) else [str(command)]
            wanted = ' '.join(str(part) for part in parts)
            # Match the executable, never the whole command line. Every call site
            # here puts the binary at argv[0] (`[binary_path(), *arguments]`),
            # while the rest of the line carries temp paths the suite does not
            # control — and a substring test against those is a coin flip:
            # mkdtemp produced /tmp/tmpv3_xagop/, in which "tmpv3" contains
            # "mpv", so an editor test failed as if it had launched a video
            # player. Roughly one run in sixty, which reads as a broken commit
            # rather than a broken guard.
            executable = PurePath(parts[0]).name if parts else ''
            if executable in FORBIDDEN:
                raise AssertionError(
                    f'a test tried to run {wanted}\n'
                    'Stub the function that runs it — nothing in this suite may reach YouTube or this machine.'
                )
            return real(command, *args, **kwargs)

        return checked

    monkeypatch.setattr(subprocess, 'run', guard(real_run))
    monkeypatch.setattr(subprocess, 'Popen', guard(real_popen))
