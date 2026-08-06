"""Which browser holds the YouTube session, and which channel to act as.

Account setup rather than state, which is why it lives in the config directory:
losing it silently would turn a working `ypl sync` back into one that claims to
have nothing to read.

A current playlist used to be remembered here too, for `use`, `drop`, `later`
and `sooner` — verbs that acted on a playlist you could not see in the command.
They are gone, and so is the pointer: a command that says which playlist it
means needs nothing remembered between runs.
"""

import json

from ypl import paths


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
