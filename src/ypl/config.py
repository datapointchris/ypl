"""User-authored settings, read from TOML at the XDG config path."""

import tomllib
from dataclasses import dataclass
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
"""


@dataclass
class Config:
    cookies_from_browser: str | None = None
    enrich_batch_size: int = 50


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
    return Config(
        cookies_from_browser=payload.get('cookies_from_browser'),
        enrich_batch_size=batch_size,
    )


def write_example() -> Path:
    path = paths.config_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(EXAMPLE)
    return path
