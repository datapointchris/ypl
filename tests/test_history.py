"""The listening log: append order, tolerance for damage, and what it survives."""

import pytest

from ypl import history
from ypl import paths


@pytest.fixture(autouse=True)
def isolated_data(tmp_path, monkeypatch):
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


def test_a_listen_is_appended_rather_than_replacing_the_last_one():
    history.record('a')
    history.record('b')
    history.record('a')

    assert [play.video_id for play in history.load()] == ['a', 'b', 'a']


def test_the_summary_counts_listens_and_keeps_the_most_recent():
    history.record('a', '2026-08-01')
    history.record('b', '2026-08-02')
    history.record('a', '2026-08-03')

    assert history.summary() == {
        'a': {'last_played_ts': '2026-08-03', 'play_count': 2},
        'b': {'last_played_ts': '2026-08-02', 'play_count': 1},
    }


def test_the_most_recent_listen_is_the_last_one_written_not_the_latest_clock_reading():
    """Two listens in the same second carry the same timestamp.

    The file records the order they happened in, so append order decides and a
    tie never has to be broken on the clock.
    """
    history.record('a', '2026-08-01T10:00:00+00:00')
    history.record('b', '2026-08-01T10:00:00+00:00')

    assert history.summary()['b']['last_played_ts'] == '2026-08-01T10:00:00+00:00'
    assert [play.video_id for play in history.load()][-1] == 'b'


def test_a_half_written_final_line_costs_one_listen_not_all_of_them():
    """The realistic damage is an append interrupted partway through."""
    history.record('a')
    history.record('b')
    with paths.plays_file().open('a') as log:
        log.write('{"video_id": "c", "played')

    assert [play.video_id for play in history.load()] == ['a', 'b']


def test_a_line_missing_the_fields_is_skipped_rather_than_crashing():
    history.record('a')
    with paths.plays_file().open('a') as log:
        log.write('{"something_else": 1}\n[]\n\n')
    history.record('b')

    assert [play.video_id for play in history.load()] == ['a', 'b']


def test_no_log_yet_is_an_empty_history_not_an_error():
    assert history.load() == []
    assert history.summary() == {}


def test_the_log_lives_beside_the_playlists_not_inside_the_mirror():
    """Both are authored and neither can be rebuilt by re-reading YouTube.

    A mirror is deleted and re-synced freely, so it must not be the only copy.
    """
    history.record('a')
    assert paths.plays_file().parent == paths.playlists_dir().parent
    assert paths.database_file().parent != paths.plays_file().parent
