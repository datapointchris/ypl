"""Storing the YouTube session.

The credential is a Google account cookie, so the mode the file is written at
is the behaviour worth pinning — the parsing itself belongs to ytmusicapi.
Nothing here reaches YouTube.
"""

import json
import stat

import pytest

from ypl import remote
from ypl import ytmusic

HEADERS = """\
accept: */*
accept-language: en-US,en;q=0.9
cookie: __Secure-3PAPISID=aaa; SID=bbb
x-goog-authuser: 0
user-agent: Mozilla/5.0
"""


def mode_of(path):
    return stat.S_IMODE(path.stat().st_mode)


def test_the_session_lands_where_it_was_asked_to_and_parses_back(tmp_path):
    auth_file = tmp_path / 'config' / 'ypl' / 'ytmusic.json'
    ytmusic.write_session(HEADERS, auth_file)
    assert json.loads(auth_file.read_text())['cookie'] == '__Secure-3PAPISID=aaa; SID=bbb'


def test_the_session_file_is_never_readable_by_anyone_else(tmp_path):
    """The file is the whole credential, so 0600 is a property of the write."""
    auth_file = tmp_path / 'ytmusic.json'
    ytmusic.write_session(HEADERS, auth_file)
    assert mode_of(auth_file) == 0o600


def test_replacing_a_session_tightens_a_file_that_was_already_loose(tmp_path):
    """O_CREAT's mode applies to a new file only, and re-auth is the common case."""
    auth_file = tmp_path / 'ytmusic.json'
    auth_file.write_text('{}')
    auth_file.chmod(0o644)
    ytmusic.write_session(HEADERS, auth_file)
    assert mode_of(auth_file) == 0o600


def test_headers_without_a_cookie_are_refused_before_anything_is_written(tmp_path):
    auth_file = tmp_path / 'ytmusic.json'
    with pytest.raises(remote.RemoteAuthError):
        ytmusic.write_session('accept: */*\nx-goog-authuser: 0\n', auth_file)
    assert not auth_file.exists()


@pytest.mark.parametrize('headers_raw', ['', '   \n\n'])
def test_an_empty_paste_is_an_auth_error_not_a_traceback(tmp_path, headers_raw):
    with pytest.raises(remote.RemoteAuthError):
        ytmusic.write_session(headers_raw, tmp_path / 'ytmusic.json')
