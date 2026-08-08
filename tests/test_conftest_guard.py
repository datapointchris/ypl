"""The subprocess guard in conftest is itself load-bearing, so it gets tests.

It is the only thing standing between the suite and the signed-in account, and
it is autouse — so when it misfires it fails a test that has nothing to do with
it, and when it under-fires nothing fails at all.
"""

import subprocess

import pytest


def test_a_forbidden_binary_is_refused():
    with pytest.raises(AssertionError, match='a test tried to run'):
        subprocess.run(['/opt/homebrew/bin/yt-dlp', '--version'], check=False)


def test_it_matches_the_executable_and_not_the_arguments():
    """A temp path containing a forbidden name is not an invocation of it.

    mkdtemp produced `/tmp/tmpv3_xagop/`, and "tmpv3" contains "mpv", so a
    substring test against the whole command line failed an editor test as if
    it had launched a video player. Roughly one run in sixty, which reads as a
    broken commit rather than a broken guard.
    """
    completed = subprocess.run(['echo', '/tmp/tmpv3_xagop/playlist.ypl'], capture_output=True, check=False)

    assert completed.returncode == 0


def test_a_forbidden_name_inside_a_directory_is_not_an_invocation():
    completed = subprocess.run(['echo', '/home/someone/mpv-notes/list.txt'], capture_output=True, check=False)

    assert completed.returncode == 0


def test_the_guard_covers_popen_too():
    with pytest.raises(AssertionError, match='a test tried to run'):
        subprocess.Popen(['mpv', '--version'])
