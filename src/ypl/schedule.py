"""Running `ypl sync` without anyone running it.

There is no command for this and there must not be one. The first `ypl sync` on
a machine installs the timer as a side effect and every run after that finds it
already there — a launch agent on macOS, a systemd user timer on Linux, both
firing at startup and then on an interval. A verb here would be one more thing
to remember, which is the exact problem the timer exists to remove.

The run that matters most is the first one after the machine has been off: that
is when the phone has been the only thing touching the playlists.

The unit is written rather than shipped in the repo, because it has to name this
machine's `ypl` and this machine's paths, and a file checked in with someone
else's home directory in it is a file that silently does nothing.
"""

import os
import platform
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

LABEL = 'com.ichrisbirch.ypl'
UNIT = 'ypl-sync'
DEFAULT_INTERVAL_MINUTES = 30


class ScheduleError(RuntimeError):
    """The timer cannot be installed, and the reason is worth reading."""


@dataclass
class Installed:
    """Where a timer landed, and what runs it."""

    path: Path
    manager: str
    command: list[str]
    interval_minutes: int
    loaded: bool = False


def is_macos() -> bool:
    return platform.system() == 'Darwin'


def executable() -> str:
    """The `ypl` a timer should call.

    Resolved to an absolute path because launchd and systemd run with almost no
    environment: `ypl` alone works in a shell and is not found by either. The
    fallback matters on a machine where the tool is run from a checkout rather
    than installed.
    """
    found = shutil.which('ypl')
    if found:
        return str(Path(found).resolve())
    if sys.argv and sys.argv[0] and Path(sys.argv[0]).exists():
        return str(Path(sys.argv[0]).resolve())
    raise ScheduleError('cannot find the ypl executable to schedule — is it installed on this machine?')


def agent_path() -> Path:
    return Path.home() / 'Library' / 'LaunchAgents' / f'{LABEL}.plist'


def unit_directory() -> Path:
    base = os.environ.get('XDG_CONFIG_HOME')
    return (Path(base) if base else Path.home() / '.config') / 'systemd' / 'user'


def timer_path() -> Path:
    return unit_directory() / f'{UNIT}.timer'


def service_path() -> Path:
    return unit_directory() / f'{UNIT}.service'


def log_path() -> Path:
    base = os.environ.get('XDG_STATE_HOME')
    return (Path(base) if base else Path.home() / '.local' / 'state') / 'ypl' / 'timer.log'


def plist(command: list[str], interval_minutes: int) -> str:
    """A launch agent that runs at load and then on an interval.

    `StartInterval` rather than `StartCalendarInterval`: the question is "has it
    run recently", not "has it run at ten past". launchd also fires a missed
    interval once the machine wakes, which is exactly the catch-up wanted here.
    """
    arguments = '\n'.join(f'    <string>{part}</string>' for part in command)
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{LABEL}</string>
  <key>ProgramArguments</key>
  <array>
{arguments}
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>{interval_minutes * 60}</integer>
  <key>StandardOutPath</key>
  <string>{log_path()}</string>
  <key>StandardErrorPath</key>
  <string>{log_path()}</string>
</dict>
</plist>
"""


def service_unit(command: list[str]) -> str:
    return f"""[Unit]
Description=Sync YouTube playlists with ypl

[Service]
Type=oneshot
ExecStart={' '.join(command)}
"""


def timer_unit(interval_minutes: int) -> str:
    """Fire shortly after boot, then on the interval.

    `Persistent=true` is the half that matters on a laptop: a run missed while
    the machine was asleep happens once it wakes, rather than waiting for the
    next interval to come round.
    """
    return f"""[Unit]
Description=Sync YouTube playlists with ypl

[Timer]
OnBootSec=2min
OnUnitActiveSec={interval_minutes}min
Persistent=true

[Install]
WantedBy=timers.target
"""


def run_manager(arguments: list[str]) -> tuple[bool, str]:
    """Ask launchd or systemd to do something, without making it fatal.

    A unit that is written but not loaded is a recoverable state a message can
    describe — `launchctl` refusing inside a sandbox, or a systemd user bus that
    is not running over SSH — and losing the written file over it would be
    worse than saying so.
    """
    try:
        finished = subprocess.run(arguments, capture_output=True, text=True, check=False)  # noqa: S603
    except (OSError, subprocess.SubprocessError) as error:
        return False, str(error)
    message = (finished.stderr or finished.stdout or '').strip()
    return finished.returncode == 0, message


def ensure(interval_minutes: int = DEFAULT_INTERVAL_MINUTES, wanted: bool = True) -> Installed | None:
    """Make this machine's timer match what the config asks for.

    Called by every `ypl sync`, so the first run on a machine sets it up and the
    rest cost one `stat`. Turning `background_sync` off removes it on the next
    run instead of needing a command to undo what no command created.

    A failure to install is returned as nothing rather than raised: the sync
    that is running right now is the one that matters, and a machine where
    launchd or systemd will not co-operate should still sync when it is asked.
    """
    if not wanted:
        uninstall()
        return None
    existing = installed()
    if existing and existing.interval_minutes == interval_minutes:
        return existing
    try:
        return install(interval_minutes)
    except (ScheduleError, OSError):
        return None


def install(interval_minutes: int = DEFAULT_INTERVAL_MINUTES) -> Installed:
    command = [executable(), 'sync']
    log_path().parent.mkdir(parents=True, exist_ok=True)
    if is_macos():
        path = agent_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(plist(command, interval_minutes))
        # Unloaded first so that re-installing with a new interval replaces the
        # running agent rather than being ignored until the next login.
        run_manager(['launchctl', 'unload', str(path)])
        loaded, _ = run_manager(['launchctl', 'load', str(path)])
        return Installed(path=path, manager='launchd', command=command, interval_minutes=interval_minutes, loaded=loaded)

    unit_directory().mkdir(parents=True, exist_ok=True)
    service_path().write_text(service_unit(command))
    timer_path().write_text(timer_unit(interval_minutes))
    run_manager(['systemctl', '--user', 'daemon-reload'])
    loaded, _ = run_manager(['systemctl', '--user', 'enable', '--now', f'{UNIT}.timer'])
    return Installed(path=timer_path(), manager='systemd', command=command, interval_minutes=interval_minutes, loaded=loaded)


def uninstall() -> list[Path]:
    """Remove the timer, returning what was actually there."""
    removed = []
    if is_macos():
        path = agent_path()
        if path.exists():
            run_manager(['launchctl', 'unload', str(path)])
            path.unlink()
            removed.append(path)
        return removed

    if timer_path().exists() or service_path().exists():
        run_manager(['systemctl', '--user', 'disable', '--now', f'{UNIT}.timer'])
    for path in (timer_path(), service_path()):
        if path.exists():
            path.unlink()
            removed.append(path)
    if removed:
        run_manager(['systemctl', '--user', 'daemon-reload'])
    return removed


def installed() -> Installed | None:
    """The timer this machine has, read back from the file rather than assumed.

    Deliberately does not resolve the executable: this is the question asked
    before every sync, and a machine that cannot find `ypl` on its PATH must
    still be able to answer "no timer" rather than raise into the run.
    """
    path = agent_path() if is_macos() else timer_path()
    if not path.exists():
        return None
    return Installed(
        path=path,
        manager='launchd' if is_macos() else 'systemd',
        command=[],
        interval_minutes=interval_from(path.read_text()),
        loaded=True,
    )


def interval_from(text: str) -> int:
    """The interval a written unit is running on, whatever wrote it."""
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith('<integer>') and stripped.endswith('</integer>'):
            return int(stripped[len('<integer>') : -len('</integer>')]) // 60
        if stripped.startswith('OnUnitActiveSec='):
            return int(stripped.split('=', 1)[1].removesuffix('min'))
    return DEFAULT_INTERVAL_MINUTES
