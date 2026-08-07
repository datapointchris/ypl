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

import datetime as dt
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

# What tells the run it is the unwatched one, and so has no reason to stop while
# there is work left. A unit written before this existed names a bare `ypl sync`,
# which `ensure` already compares and rewrites — so the change reaches every
# machine on its next sync without anything having to be reinstalled.
BACKGROUND_ARGUMENTS = ('--background',)

# The binaries a run shells out to. yt-dlp is the one that matters on a timer,
# since every read goes through it; mpv is here so that a unit written today
# does not need rewriting the day playback grows a scheduled use.
TOOLS = ('yt-dlp', 'mpv')

# What launchd hands an agent, and roughly what a systemd user unit gets. Note
# what is missing: `/usr/local/bin`, which is where Homebrew puts yt-dlp.
SYSTEM_PATH = ('/usr/bin', '/bin', '/usr/sbin', '/sbin')


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
    # The PATH the unit hands the run, empty when it sets none — which is what
    # every unit written before this was read back as.
    search_path: str = ''


def is_macos() -> bool:
    return platform.system() == 'Darwin'


def outside_virtualenv() -> str | None:
    """A `ypl` on PATH that is not the active virtualenv's own, if there is one."""
    environment = os.environ.get('VIRTUAL_ENV')
    if not environment:
        return None
    outside = [entry for entry in os.environ.get('PATH', '').split(os.pathsep) if entry and not Path(entry).is_relative_to(environment)]
    return shutil.which('ypl', path=os.pathsep.join(outside))


def executable() -> str:
    """The `ypl` a timer should call.

    Resolved to an absolute path because launchd and systemd run with almost no
    environment: `ypl` alone works in a shell and is not found by either. The
    fallback matters on a machine where the tool is run from a checkout rather
    than installed.

    A checkout's `ypl` is passed over when an installed one exists. `uv run ypl
    sync` while developing puts the virtualenv first on PATH, and a timer
    pointed into a checkout stops working the moment that directory is rebuilt
    or moved — silently, and for as long as nobody reads the log, because
    `ypl status` goes on reporting a timer that is installed and dead.

    Absolute but not resolved through symlinks. `~/.local/bin/ypl` is a link
    into uv's tool directory, and that directory's layout is uv's business:
    naming what the link points at today swaps a path uv promises to keep for
    one it does not.
    """
    found = outside_virtualenv() or shutil.which('ypl')
    if found:
        return os.path.abspath(found)
    if sys.argv and sys.argv[0] and Path(sys.argv[0]).exists():
        return os.path.abspath(sys.argv[0])
    raise ScheduleError('cannot find the ypl executable to schedule — is it installed on this machine?')


def tool_directories() -> list[str]:
    """Where the binaries a run shells out to actually are, in PATH order."""
    found: list[str] = []
    for tool in TOOLS:
        location = shutil.which(tool)
        if location:
            directory = str(Path(location).parent)
            if directory not in found:
                found.append(directory)
    return found


def unit_path() -> str:
    """The PATH a scheduled run needs.

    `ypl` is scheduled by absolute path because launchd and systemd run with
    almost no environment — and then the run shells out to `yt-dlp` by name and
    dies on exactly the same impoverishment. Every timer-driven sync on this
    machine failed with `yt-dlp is not on PATH` while every run at the prompt
    worked, which is the shape of a bug nobody sees: the only place it was
    written down was the timer's own log, and reading that log is the thing an
    unattended sync is supposed to make unnecessary.

    Built from where those binaries are rather than by copying the installing
    shell's PATH. What a run needs is its tools; an inherited PATH also bakes in
    everything else that happened to be set the day the timer went in.
    """
    entries = [*tool_directories(), *SYSTEM_PATH]
    return os.pathsep.join(dict.fromkeys(entries))


def can_find_tools(search_path: str) -> bool:
    """Whether a written unit's PATH reaches the binaries a run needs.

    Asked instead of comparing the whole PATH string so that an unrelated change
    to the shell's PATH does not rewrite and reload the unit on the next sync.
    The only question is whether the run will find its tools.
    """
    entries = set(search_path.split(os.pathsep))
    return all(directory in entries for directory in tool_directories())


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
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>{unit_path()}</string>
  </dict>
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
Environment=PATH={unit_path()}
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
    try:
        command = [executable(), 'sync', *BACKGROUND_ARGUMENTS]
    except ScheduleError:
        # Nothing can be installed and nothing can be compared, so whatever is
        # already there is the answer. Saying "no timer" would be a lie about a
        # unit that is still running.
        return existing
    # The command and the PATH as well as the interval, because a unit that
    # fires and fails is worse than no unit: it still reports as scheduled. Both
    # of those actually happened here — a unit naming a `ypl` that had moved, and
    # one that could not find yt-dlp.
    if existing and existing.interval_minutes == interval_minutes and existing.command == command and can_find_tools(existing.search_path):
        return existing
    try:
        return install(interval_minutes)
    except (ScheduleError, OSError):
        return None


def install(interval_minutes: int = DEFAULT_INTERVAL_MINUTES) -> Installed:
    command = [executable(), 'sync', *BACKGROUND_ARGUMENTS]
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

    Deliberately does not resolve this machine's `ypl`: this is the question
    asked before every sync, and a machine that cannot find one on its PATH must
    still be able to answer "no timer" rather than raise into the run. The
    command reported is the one the unit names, which is what makes a unit
    written by an older install — or by a checkout — recognisable as stale.

    A launch agent holds all of that in one file and the systemd pair splits it:
    the interval is the timer's and the command and the PATH are the service's.
    Reading both from the timer answered `[]` and `''` every time, which made
    every comparison fail and rewrote the unit on every single sync.
    """
    if is_macos():
        path = agent_path()
        if not path.exists():
            return None
        text = path.read_text()
        return Installed(
            path=path,
            manager='launchd',
            command=command_from(text),
            interval_minutes=interval_from(text),
            loaded=True,
            search_path=path_from(text),
        )

    if not timer_path().exists():
        return None
    service = service_path().read_text() if service_path().exists() else ''
    return Installed(
        path=timer_path(),
        manager='systemd',
        command=command_from(service),
        interval_minutes=interval_from(timer_path().read_text()),
        loaded=True,
        search_path=path_from(service),
    )


def path_from(text: str) -> str:
    """The PATH a written unit sets, or '' when it sets none."""
    lines = [line.strip() for line in text.splitlines()]
    for line in lines:
        if line.startswith('Environment=PATH='):
            return line.removeprefix('Environment=PATH=')
    for index, line in enumerate(lines):
        if line == '<key>PATH</key>' and index + 1 < len(lines):
            following = lines[index + 1]
            if following.startswith('<string>'):
                return following.removeprefix('<string>').removesuffix('</string>')
    return ''


def command_from(text: str) -> list[str]:
    """The command a written unit runs, whichever manager's file it is."""
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith('ExecStart='):
            return stripped.removeprefix('ExecStart=').split()
    arguments = []
    inside = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped == '<key>ProgramArguments</key>':
            inside = True
        elif inside and stripped.startswith('<string>'):
            arguments.append(stripped.removeprefix('<string>').removesuffix('</string>'))
        elif inside and stripped == '</array>':
            break
    return arguments


def next_fire(last_finished: dt.datetime | None, interval_minutes: int) -> dt.datetime | None:
    """When the timer is expected to start the next run.

    Derived rather than asked. `launchctl` reports no next-fire time for a
    `StartInterval` job at all, and `systemctl list-timers` reports an exact
    one — a figure that is precise on Linux and simply absent on macOS is worse
    than one estimate meaning the same thing on both.

    Measured from the end of the last run rather than its start, because that is
    what launchd does with a job still going when the interval elapses: the
    countdown restarts on exit. A fifteen-minute run on a thirty-minute timer
    therefore came round every forty-five minutes, and reporting the next sync
    as half an hour after the last one began would have been wrong by exactly
    the length of the run every single time.
    """
    if last_finished is None:
        return None
    return last_finished + dt.timedelta(minutes=interval_minutes)


def interval_from(text: str) -> int:
    """The interval a written unit is running on, whatever wrote it."""
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith('<integer>') and stripped.endswith('</integer>'):
            return int(stripped[len('<integer>') : -len('</integer>')]) // 60
        if stripped.startswith('OnUnitActiveSec='):
            return int(stripped.split('=', 1)[1].removesuffix('min'))
    return DEFAULT_INTERVAL_MINUTES
