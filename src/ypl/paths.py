"""Every path ypl writes, resolved through the XDG base directories.

Three kinds, and the split is load-bearing rather than tidiness:

- The mirror is **state**. Nobody authored it and it rebuilds from `ypl sync`,
  which costs no API quota, so losing it costs time and nothing else.
- Local playlists are **data**. They are authored — a mood set or an event arc
  is a decision, and there is no remote to rebuild it from. Keeping them as M3U
  files rather than rows in the mirror is also what makes them playable by mpv,
  VLC and Kodi with no code, and syncable independently of a database that
  should not be synced.
- The config is **config**: hand-edited, read-only to the tool.
"""

import os
from pathlib import Path

TOOL = 'ypl'


def xdg_home(variable: str, fallback: Path) -> Path:
    override = os.environ.get(variable)
    return Path(override).expanduser() if override else fallback


def config_dir() -> Path:
    return xdg_home('XDG_CONFIG_HOME', Path.home() / '.config') / TOOL


def state_dir() -> Path:
    return xdg_home('XDG_STATE_HOME', Path.home() / '.local' / 'state') / TOOL


def data_dir() -> Path:
    return xdg_home('XDG_DATA_HOME', Path.home() / '.local' / 'share') / TOOL


def config_file() -> Path:
    return config_dir() / 'config.toml'


def database_file() -> Path:
    return state_dir() / 'ypl.db'


def playlists_dir() -> Path:
    return data_dir() / 'playlists'
