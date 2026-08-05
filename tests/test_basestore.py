"""The recorded remote state that makes a three-way merge possible.

What is worth pinning here is not the JSON but the three things the merge
depends on: that the order survives a round trip, that an unreadable base is
never quietly read as an absent one, and that a `setVideoId` written down is
the one that comes back.
"""

import json

import pytest

from ypl import basestore
from ypl import paths
from ypl.remote import RemoteItem


@pytest.fixture(autouse=True)
def isolated_home(tmp_path, monkeypatch):
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


def base(*pairs) -> basestore.Base:
    return basestore.Base(
        slug='sunday',
        playlist_id='PL1',
        items=[RemoteItem(video_id=video_id, set_video_id=handle) for video_id, handle in pairs],
    )


def test_a_playlist_that_has_never_been_reconciled_has_no_base():
    """None rather than an empty base: they mean opposite things to the merge."""
    assert basestore.load('sunday') is None


def test_a_base_comes_back_in_the_order_it_was_written():
    """Order is the whole record of remote's ordering — nothing else carries it."""
    basestore.save(base(('vid1', 'h1'), ('vid2', 'h2'), ('vid3', 'h3')))
    assert basestore.load('sunday').video_ids == ['vid1', 'vid2', 'vid3']


def test_the_handles_survive_the_round_trip():
    """The point of the file: yt-dlp never returns a setVideoId, so this is the only copy."""
    basestore.save(base(('vid1', 'h1'), ('vid2', 'h2')))
    loaded = basestore.load('sunday')
    assert [item.set_video_id for item in loaded.items] == ['h1', 'h2']
    assert loaded.playlist_id == 'PL1'


def test_a_video_held_twice_yields_both_of_its_slots():
    """Removing "that video" is ambiguous until a caller picks a slot."""
    basestore.save(base(('vid1', 'first'), ('vid2', 'other'), ('vid1', 'second')))
    assert basestore.load('sunday').handles_for('vid1') == ['first', 'second']


def test_saving_stamps_the_reconcile():
    saved = basestore.save(base(('vid1', 'h1')))
    assert json.loads(saved.read_text())['reconciled_ts']


def test_a_second_reconcile_replaces_the_first():
    basestore.save(base(('vid1', 'h1'), ('vid2', 'h2')))
    basestore.save(base(('vid2', 'h2')))
    assert basestore.load('sunday').video_ids == ['vid2']


def test_a_base_that_cannot_be_read_is_refused_rather_than_treated_as_absent():
    """Reading it as absent would make every local video look newly added."""
    path = basestore.path_for('sunday')
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text('{not json')
    with pytest.raises(basestore.BaseStoreError):
        basestore.load('sunday')


@pytest.mark.parametrize('payload', ['[]', '{"items": "vid1"}', '{"items": [{"set_video_id": "h1"}]}'])
def test_a_base_of_the_wrong_shape_names_the_file_it_came_from(payload):
    path = basestore.path_for('sunday')
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(payload)
    with pytest.raises(basestore.BaseStoreError) as error:
        basestore.load('sunday')
    assert str(path) in str(error.value)


def test_an_interrupted_write_leaves_the_previous_base_intact(monkeypatch):
    """The rename is what makes this safe — a truncated base is unrecoverable."""
    basestore.save(base(('vid1', 'h1')))

    def fail(*args, **kwargs):
        raise OSError('disk full')

    monkeypatch.setattr(basestore.os, 'replace', fail)
    with pytest.raises(OSError):
        basestore.save(base(('vid2', 'h2')))
    assert basestore.load('sunday').video_ids == ['vid1']


def test_deleting_a_base_is_forgetting_it_not_an_error():
    basestore.save(base(('vid1', 'h1')))
    basestore.delete('sunday')
    basestore.delete('sunday')
    assert basestore.load('sunday') is None


def test_bases_live_beside_the_playlists_rather_than_in_the_mirror():
    """Data, not state: it must not sit anywhere a re-sync would throw away."""
    assert basestore.path_for('sunday').parent == paths.data_dir() / 'remote'
