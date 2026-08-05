"""The playlist file format: what it accepts, and what survives a round trip."""

import pytest

from ypl import m3u


@pytest.mark.parametrize(
    'text',
    [
        'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
        'http://youtube.com/watch?v=dQw4w9WgXcQ',
        'https://m.youtube.com/watch?v=dQw4w9WgXcQ',
        'https://music.youtube.com/watch?v=dQw4w9WgXcQ',
        'https://youtu.be/dQw4w9WgXcQ',
        'https://www.youtube.com/shorts/dQw4w9WgXcQ',
        'https://www.youtube.com/embed/dQw4w9WgXcQ',
        'https://www.youtube.com/live/dQw4w9WgXcQ',
        'dQw4w9WgXcQ',
    ],
)
def test_every_shape_youtube_serves_a_video_under_yields_the_same_id(text):
    assert m3u.video_id_from(text) == 'dQw4w9WgXcQ'


def test_the_extra_parameters_a_share_link_carries_are_ignored():
    """`&list=` and `&t=` ride along on anything copied out of a playlist page."""
    shared = 'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL1&index=4&t=93s&pp=xyz'
    assert m3u.video_id_from(shared) == 'dQw4w9WgXcQ'


@pytest.mark.parametrize('text', ['', '   ', 'not a url', 'https://vimeo.com/12345', 'https://www.youtube.com/playlist?list=PL1', 'short'])
def test_things_that_are_not_a_video_yield_nothing(text):
    assert m3u.video_id_from(text) is None


def test_a_playlist_survives_being_written_and_read_back():
    original = m3u.Playlist(
        name='Deep Sunday',
        created_ts='2026-08-05T10:00:00+00:00',
        source='remote PL1',
        entries=[
            m3u.Entry(video_id='dQw4w9WgXcQ', title='Cercle - Ben Böhmer', duration_seconds=3600),
            m3u.Entry(video_id='-4FPIL6e4SQ', title='', duration_seconds=None),
        ],
    )

    reread = m3u.parse(m3u.render(original))
    assert reread.name == original.name
    assert reread.created_ts == original.created_ts
    assert reread.source == original.source
    assert reread.entries == original.entries


def test_the_sync_binding_round_trips():
    original = m3u.Playlist(name='Sunday', synced=True, remote_id='PL123', entries=[m3u.Entry(video_id='dQw4w9WgXcQ')])
    reread = m3u.parse(m3u.render(original))
    assert (reread.synced, reread.remote_id) == (True, 'PL123')


def test_a_playlist_kept_off_youtube_says_so_rather_than_falling_to_the_default():
    """The default is synced, so `no` has to be written to survive a re-read."""
    reread = m3u.parse(m3u.render(m3u.Playlist(name='Scratch', synced=False)))
    assert reread.synced is False


@pytest.mark.parametrize('value', ['no', 'No', 'false', 'off', ''])
def test_the_ways_a_hand_edit_might_say_no(value):
    assert m3u.parse(f'#EXTM3U\n#YPL-SYNCED:{value}\n').synced is False


@pytest.mark.parametrize('value', ['yes', 'true', 'YES'])
def test_the_ways_a_hand_edit_might_say_yes(value):
    assert m3u.parse(f'#EXTM3U\n#YPL-SYNCED:{value}\n').synced is True


def test_a_file_written_before_syncing_existed_is_treated_as_synced():
    """The default has to be the one that matches how these are made now."""
    assert m3u.parse('#EXTM3U\n#PLAYLIST:Old\nhttps://youtu.be/dQw4w9WgXcQ\n').synced is True


def test_the_rendered_file_starts_with_the_header_every_player_looks_for():
    rendered = m3u.render(m3u.Playlist(name='X', entries=[m3u.Entry(video_id='dQw4w9WgXcQ')]))
    assert rendered.splitlines()[0] == '#EXTM3U'


def test_an_unknown_directive_is_skipped_rather_than_rejected():
    """Other tools write into these files, and one stray line must not lock us out."""
    parsed = m3u.parse('#EXTM3U\n#EXTGRP:whatever\n#EXTVLCOPT:no-video\nhttps://youtu.be/dQw4w9WgXcQ\n')
    assert [entry.video_id for entry in parsed.entries] == ['dQw4w9WgXcQ']


def test_a_line_that_is_not_a_video_is_an_error_naming_the_line():
    with pytest.raises(m3u.M3uError) as caught:
        m3u.parse('#EXTM3U\nhttps://youtu.be/dQw4w9WgXcQ\n/home/chris/song.mp3\n')
    assert caught.value.line_number == 3


def test_an_unknown_duration_round_trips_as_unknown_rather_than_as_minus_one():
    parsed = m3u.parse('#EXTINF:-1,Something\nhttps://youtu.be/dQw4w9WgXcQ\n')
    assert parsed.entries[0].duration_seconds is None


def test_an_entry_with_no_info_line_still_parses():
    parsed = m3u.parse('https://youtu.be/dQw4w9WgXcQ\n')
    assert parsed.entries[0] == m3u.Entry(video_id='dQw4w9WgXcQ', title='', duration_seconds=None)


def test_info_belongs_to_the_entry_that_follows_it_and_is_not_reused():
    parsed = m3u.parse('#EXTINF:60,First\nhttps://youtu.be/dQw4w9WgXcQ\nhttps://youtu.be/-4FPIL6e4SQ\n')
    assert [entry.title for entry in parsed.entries] == ['First', '']


def test_the_same_video_may_appear_twice():
    """A playlist is an ordered list of slots, and a mix does get played twice."""
    parsed = m3u.parse('https://youtu.be/dQw4w9WgXcQ\nhttps://youtu.be/dQw4w9WgXcQ\n')
    assert len(parsed.entries) == 2
