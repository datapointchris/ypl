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


def load() -> Config:
    """Read the config file, or return defaults when there is none.

    A missing config is the normal case rather than an error: every setting here
    has a working default, and public playlists need none of them.
    """
    path = paths.config_file()
    if not path.exists():
        return Config()
    payload = tomllib.loads(path.read_text())
    return Config(
        cookies_from_browser=payload.get('cookies_from_browser'),
        enrich_batch_size=payload.get('enrich_batch_size', 50),
    )


def write_example() -> Path:
    path = paths.config_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(EXAMPLE)
    return path
