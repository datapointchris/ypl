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


def auth_file() -> Path:
    """What ypl learned while signing in, beside the session it learned it from.

    Only the browser, so far. It sits in the config directory rather than in
    state because it is part of account setup and because deleting it should be
    as deliberate as deleting the session next to it — losing it silently would
    turn a working `ypl sync` back into one that says it has nothing to read.
    """
    return config_dir() / 'auth.json'


def ytmusic_auth_file() -> Path:
    """The YouTube session `ypl remote` writes through.

    Config rather than state, despite being written by the tool: it is account
    setup, it survives every re-sync, and deleting it costs a trip to a browser
    rather than a command. It holds a Google session cookie, so it is written
    at 0600 and never at the umask's mode.
    """
    return config_dir() / 'ytmusic.json'


def playlists_dir() -> Path:
    return data_dir() / 'playlists'


def remote_dir() -> Path:
    """The base of every three-way merge: remote state as of the last reconcile.

    Data rather than state, by the same rule as the playlists beside it. The
    mirror rebuilds from a free `ypl sync`, but nothing rebuilds a snapshot of
    what YouTube held at a past moment — once that moment passes it is gone,
    and losing it makes the next merge unable to tell a remote deletion from a
    local addition.
    """
    return data_dir() / 'remote'


def plays_file() -> Path:
    """Listening history — data, not state.

    Beside the playlists rather than inside the mirror: both are authored, and
    neither can be rebuilt by re-reading YouTube.
    """
    return data_dir() / 'plays.jsonl'


def current_file() -> Path:
    """Which playlist the id-free verbs act on.

    State, not data: it is a pointer to something else, it is rebuilt by
    playing or naming a playlist, and losing it costs one command.
    """
    return state_dir() / 'current.json'


def mpv_socket() -> Path:
    """Where `ypl play` opens mpv's IPC socket, and `ypl now` looks for it.

    State rather than runtime: `$XDG_RUNTIME_DIR` is the more correct home for
    a socket, but macOS does not define one and this tool has to work on both.
    """
    return state_dir() / 'mpv.sock'
