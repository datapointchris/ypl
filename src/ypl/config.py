"""User-authored settings, read from TOML at the XDG config path."""

import tomllib
from dataclasses import dataclass
from dataclasses import field
from pathlib import Path

from ypl import paths

EXAMPLE = """\
# Which browser's cookies to borrow when reading playlists.
# Needed only for private and unlisted playlists — public ones read without it.
# One of: firefox, chrome, chromium, brave, edge, safari, vivaldi, opera.
# cookies_from_browser = "firefox"

# How many videos `ypl enrich` fetches in one run when --limit is not given.
# Each video is one request, so a whole library is better done in sittings.
enrich_batch_size = 50

# Seconds to wait between requests when a command makes many of them —
# `enrich --all` over a library, or a bare `sync` over every playlist. Reads
# cost no API quota, but thousands of back-to-back extractions carrying your
# cookies is a burst, and a burst is the shape that gets an account looked at.
# Lower it if you are impatient and signed out; raise it if YouTube complains.
request_interval_seconds = 2.0

# Whether the first `ypl sync` on a machine sets itself up to keep running —
# at startup and every `background_sync_minutes` after that. There is no command
# for this on purpose: turning it off here removes the timer on the next run.
background_sync = true
background_sync_minutes = 30

# How long one `ypl sync` may spend before stopping and leaving the rest for
# the next run. Everything it does is resumable, so a ceiling costs nothing but
# patience — a library enriches itself over a day or two of background runs
# rather than in one sitting that holds the machine. 0 means no ceiling.
sync_minutes = 15

# Arguments added to every `ypl play`. The escape hatch for anything mpv can do
# that ypl does not have a flag for — profiles, output devices, cache sizes.
# `ypl play --audio` already covers the common one.
# mpv_arguments = ["--profile=low-latency", "--volume=70"]
"""


@dataclass
class Config:
    cookies_from_browser: str | None = None
    enrich_batch_size: int = 50
    request_interval_seconds: float = 2.0
    sync_minutes: float = 15.0
    background_sync: bool = True
    background_sync_minutes: int = 30
    mpv_arguments: list[str] = field(default_factory=list)

    @property
    def sync_seconds(self) -> float | None:
        """The ceiling as `Budget` wants it — None rather than zero for no limit."""
        return self.sync_minutes * 60 if self.sync_minutes else None


class ConfigError(ValueError):
    """A config file exists but cannot be used.

    Distinct from a missing file, which is normal. This one is always the result
    of a hand edit, so the message has to name the file and the parser's
    complaint rather than surfacing a traceback.
    """

    def __init__(self, path: Path, reason: str):
        self.path = path
        self.reason = reason
        super().__init__(f'{path}: {reason}')


def load() -> Config:
    """Read the config file, or return defaults when there is none.

    A missing config is the normal case rather than an error: every setting here
    has a working default, and public playlists need none of them.
    """
    path = paths.config_file()
    if not path.exists():
        return Config()
    try:
        payload = tomllib.loads(path.read_text())
    except tomllib.TOMLDecodeError as error:
        raise ConfigError(path, str(error)) from error
    batch_size = payload.get('enrich_batch_size', 50)
    if not isinstance(batch_size, int) or batch_size < 1:
        raise ConfigError(path, f'enrich_batch_size must be a positive integer, got {batch_size!r}')
    interval = payload.get('request_interval_seconds', 2.0)
    if not isinstance(interval, int | float) or isinstance(interval, bool) or interval < 0:
        raise ConfigError(path, f'request_interval_seconds must be a number of seconds, got {interval!r}')
    sync_minutes = payload.get('sync_minutes', 15.0)
    if not isinstance(sync_minutes, int | float) or isinstance(sync_minutes, bool) or sync_minutes < 0:
        raise ConfigError(path, f'sync_minutes must be a number of minutes, got {sync_minutes!r}')
    background_sync = payload.get('background_sync', True)
    if not isinstance(background_sync, bool):
        raise ConfigError(path, f'background_sync must be true or false, got {background_sync!r}')
    background_minutes = payload.get('background_sync_minutes', 30)
    if not isinstance(background_minutes, int) or isinstance(background_minutes, bool) or background_minutes < 1:
        raise ConfigError(path, f'background_sync_minutes must be a positive integer, got {background_minutes!r}')
    mpv_arguments = payload.get('mpv_arguments', [])
    # Checked rather than trusted: these go straight onto an mpv command line,
    # and a bare string would be spread one character per argument.
    if not isinstance(mpv_arguments, list) or not all(isinstance(argument, str) for argument in mpv_arguments):
        raise ConfigError(path, f'mpv_arguments must be a list of strings, got {mpv_arguments!r}')
    return Config(
        cookies_from_browser=payload.get('cookies_from_browser'),
        enrich_batch_size=batch_size,
        request_interval_seconds=float(interval),
        sync_minutes=float(sync_minutes),
        background_sync=background_sync,
        background_sync_minutes=background_minutes,
        mpv_arguments=mpv_arguments,
    )


def write_example() -> Path:
    path = paths.config_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(EXAMPLE)
    return path
