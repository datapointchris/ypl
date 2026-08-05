"""The local playlist directory: naming, clobbering, and unreadable files."""

import pytest

from ypl import local
from ypl import m3u
from ypl import paths


@pytest.fixture(autouse=True)
def isolated_data(tmp_path, monkeypatch):
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


def playlist(name='Deep Sunday', video_ids=('dQw4w9WgXcQ',)):
    return local.LocalPlaylist(
        name=name,
        path=local.path_for(name),
        entries=[m3u.Entry(video_id=video_id) for video_id in video_ids],
    )


@pytest.mark.parametrize(
    ('name', 'expected'),
    [
        ('Deep Sunday', 'deep-sunday'),
        ('Deep  Sunday!!', 'deep-sunday'),
        ('  Deep/Sunday  ', 'deep-sunday'),
        ('Deep_Sunday', 'deep-sunday'),
        ('90s Rave', '90s-rave'),
    ],
)
def test_a_name_becomes_a_flat_lowercase_filename(name, expected):
    assert local.slugify(name) == expected


def test_a_name_this_tool_authors_is_its_own_slug():
    """One string, not two: a playlist made here is named the way it is filed."""
    assert local.authored_name('Six Hour Work Uptempo House') == 'six-hour-work-uptempo-house'


def test_a_name_cannot_escape_the_playlist_directory():
    """A slash in a playlist name must not become a path separator."""
    assert local.path_for('../../etc/passwd').parent == paths.playlists_dir()


def test_saving_creates_the_directory_on_first_use():
    local.save(playlist())
    assert paths.playlists_dir().is_dir()


def test_saving_refuses_to_clobber_unless_told_to():
    local.save(playlist())
    with pytest.raises(local.LocalPlaylistExistsError):
        local.save(playlist(video_ids=('-4FPIL6e4SQ',)))

    local.save(playlist(video_ids=('-4FPIL6e4SQ',)), overwrite=True)
    assert local.load(local.path_for('Deep Sunday')).video_ids == ['-4FPIL6e4SQ']


def test_a_saved_playlist_keeps_its_display_name_not_just_its_slug():
    local.save(playlist(name='Deep Sunday'))
    assert local.load(local.path_for('Deep Sunday')).name == 'Deep Sunday'


def test_a_file_with_no_name_directive_falls_back_to_its_filename():
    paths.playlists_dir().mkdir(parents=True)
    (paths.playlists_dir() / 'hand-written.m3u').write_text('https://youtu.be/dQw4w9WgXcQ\n')

    assert local.list_playlists().playlists[0].name == 'hand-written'


def test_playlists_are_listed_by_name_regardless_of_filename_order():
    for name in ['Zebra', 'apple', 'Mango']:
        local.save(playlist(name=name))

    assert [found.name for found in local.list_playlists().playlists] == ['apple', 'Mango', 'Zebra']


def test_an_unreadable_file_is_reported_without_hiding_the_others():
    """One bad hand edit must not make every other playlist unreachable."""
    local.save(playlist(name='Good'))
    (paths.playlists_dir() / 'broken.m3u').write_text('#EXTM3U\n/not/a/video.mp3\n')

    listing = local.list_playlists()
    assert [found.name for found in listing.playlists] == ['Good']
    assert len(listing.problems) == 1
    assert listing.problems[0][0].name == 'broken.m3u'


def test_files_that_are_not_playlists_are_ignored():
    paths.playlists_dir().mkdir(parents=True)
    (paths.playlists_dir() / 'notes.txt').write_text('nothing to see')

    assert local.list_playlists() == local.Listing()


def test_deleting_removes_the_file_and_nothing_else():
    local.save(playlist(name='Keep'))
    local.save(playlist(name='Drop'))
    local.delete(local.load(local.path_for('Drop')))

    assert [found.name for found in local.list_playlists().playlists] == ['Keep']
