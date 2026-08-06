"""User-authored settings, read from TOML at the XDG config path."""

import tomllib
from dataclasses import dataclass
from dataclasses import field
from pathlib import Path

from ypl import paths

# A floor on the gap between requests, below which no config may go. The
# credential is a Google account cookie and the Terms prohibit automated
# access; pacing is the whole of what keeps this at human scale, so it is not
# something a config file gets to switch off.
MINIMUM_INTERVAL_SECONDS = 1.0


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
    def request_pace_seconds(self) -> float:
        """The gap between requests, floored so it cannot be configured away.

        `request_interval_seconds = 0` parsed fine and removed the pacing
        entirely, which is the one setting in this file that can turn a personal
        tool into something that looks like a scraper. The value is a floor on
        how *slow* to go, so raising it is always allowed and lowering it past
        the floor is not.
        """
        return max(self.request_interval_seconds, MINIMUM_INTERVAL_SECONDS)

    @property
    def sync_seconds(self) -> float | None:
        """The ceiling as `Budget` wants it — None rather than zero for no limit.

        None here is not unbounded in practice: the write path carries its own
        request ceiling — `youtubei.MAX_REQUESTS_PER_RUN` — which no config
        reaches. This one only decides how long a run may spend.
        """
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
