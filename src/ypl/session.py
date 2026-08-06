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


def remember_browser(browser_name: str, page_id: str = '') -> None:
    """Record which browser holds the YouTube session, and which channel to act as.

    These two are the whole of what signing in stores. There is no session file
    any more: the cookies are read out of the browser on every run, so the only
    durable facts are where to read them from and which of the identities they
    carry to send as `x-goog-pageid`.

    The page id is the fix the rebuild is downstream of. A jar can reach several
    identities — a personal Google account and the brand account that actually
    owns the channel — and without naming one, every request authenticates as
    whichever the browser last selected. That was the personal account, which
    owns nothing, so every playlist read back `owned: false` and every write
    failed.
    """
    path = paths.auth_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({'browser': browser_name, 'page_id': page_id}) + '\n')


def stored_auth() -> dict[str, str]:
    """What signing in wrote, or an empty mapping when nothing has.

    A file that cannot be parsed counts as nothing rather than raising, by the
    same rule as `current`: the answer to a mangled one-line file is to sign in
    again, not to make every command that reads it fail.
    """
    path = paths.auth_file()
    if not path.exists():
        return {}
    try:
        payload = json.loads(path.read_text())
    except json.JSONDecodeError:
        return {}
    return payload if isinstance(payload, dict) else {}


def browser() -> str:
    """The browser signed in with, or '' when nothing has recorded one."""
    return stored_auth().get('browser') or ''


def page_id() -> str:
    """The channel to act as, or '' when nothing has recorded one."""
    return stored_auth().get('page_id') or ''
