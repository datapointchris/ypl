"""What ypl remembers between commands, so you do not repeat yourself.

Two facts, in two places, because they are two kinds of thing. Which playlist
is on is state — a pointer, rebuilt by playing something. The browser holding
your YouTube session is account setup, so it lives in the config directory
beside the session file, and losing it silently would turn a working `ypl sync`
back into one that claims to have nothing to read.

Which playlist is on, so the verbs that act on it need no arguments.

The reason this exists: identifying a video by pasting eleven characters is
intolerable while music is playing, and so is naming the playlist every time.
`ypl drop` should mean "this one, in what I am listening to" — which needs the
tool to hold two facts, what is on and where you are in it.

Only the first is stored. The second is read live from mpv when playback goes
through `ypl play`, and is simply unavailable when playback is in the YouTube
web player or on a phone, because YouTube exposes nothing that would answer it.
That asymmetry is why the verbs also take a fragment of a title: it is the
answer for the case a socket cannot cover, and it is still not an id.
"""

import json

from ypl import paths


def remember(name: str) -> None:
    path = paths.current_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({'playlist': name}) + '\n')


def current() -> str:
    """The playlist name last played or chosen, or '' when there is none.

    A file that cannot be read counts as none rather than raising: this is a
    convenience pointer, and refusing to run `ypl drop` because a one-line
    state file got mangled would be worse than asking which playlist again.
    """
    path = paths.current_file()
    if not path.exists():
        return ''
    try:
        payload = json.loads(path.read_text())
    except json.JSONDecodeError:
        return ''
    return payload.get('playlist') or '' if isinstance(payload, dict) else ''


def forget() -> None:
    paths.current_file().unlink(missing_ok=True)


def remember_browser(browser: str) -> None:
    """Record which browser a command successfully read a YouTube session from.

    Written by anything that proves it works — `remote auth` signing in, or a
    `sync --browser` that came back with playlists. Signing in once should not
    have to be told twice, and it was: `remote auth` stored the YouTube Music
    session while every read went through yt-dlp's own cookie handling, so the
    tool held two unrelated ideas of being logged in and only one of them was
    ever set.
    """
    path = paths.auth_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({'browser': browser}) + '\n')


def browser() -> str:
    """The browser signed in with, or '' when nothing has recorded one."""
    path = paths.auth_file()
    if not path.exists():
        return ''
    try:
        payload = json.loads(path.read_text())
    except json.JSONDecodeError:
        return ''
    return payload.get('browser') or '' if isinstance(payload, dict) else ''
