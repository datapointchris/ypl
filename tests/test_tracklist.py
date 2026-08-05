"""Tracklist parsing against the shapes real mixes actually use."""

import pytest

from ypl.models import Chapter
from ypl.tracklist import best_tracklist
from ypl.tracklist import split_artist_and_title
from ypl.tracklist import tracks_from_chapters
from ypl.tracklist import tracks_from_description


@pytest.mark.parametrize(
    ('text', 'artist', 'title'),
    [
        ('Totally Enormous Extinct Dinosaurs - Islas Canarias', 'Totally Enormous Extinct Dinosaurs', 'Islas Canarias'),
        ('Tennyson & Mr. Carmack - Tuesday', 'Tennyson & Mr. Carmack', 'Tuesday'),
        ('Disclosure ft. Fatoumata Diawara - Douha (Mali Mali)', 'Disclosure ft. Fatoumata Diawara', 'Douha (Mali Mali)'),
        ('Bonobo – Kerala', 'Bonobo', 'Kerala'),
        ('Four Tet — Baby', 'Four Tet', 'Baby'),
        ('Overmono ~ So U Kno', 'Overmono', 'So U Kno'),
    ],
)
def test_splits_on_every_dash_variant(text, artist, title):
    assert split_artist_and_title(text) == (artist, title)


def test_hyphenated_names_survive_because_the_separator_needs_spaces():
    assert split_artist_and_title('Jay-Z') == (None, 'Jay-Z')
    assert split_artist_and_title('Jay-Z - Dirt Off Your Shoulder') == ('Jay-Z', 'Dirt Off Your Shoulder')


def test_splits_once_so_a_remix_suffix_stays_on_the_title():
    assert split_artist_and_title('Bicep - Glue - Extended Mix') == ('Bicep', 'Glue - Extended Mix')


@pytest.mark.parametrize(
    ('text', 'title'),
    [
        ('1. Caribou - Odessa', 'Odessa'),
        ('01) Caribou - Odessa', 'Odessa'),
        ('#3 Caribou - Odessa', 'Odessa'),
    ],
)
def test_strips_leading_track_numbers(text, title):
    assert split_artist_and_title(text) == ('Caribou', title)


def test_a_title_with_no_separator_has_no_artist():
    assert split_artist_and_title('Intro') == (None, 'Intro')


@pytest.mark.parametrize('unknown', ['ID - ID', 'id - Unreleased', '? - Something', 'Unknown - Track'])
def test_dj_placeholder_artists_are_not_recorded_as_artists(unknown):
    artist, title = split_artist_and_title(unknown)
    assert artist is None
    assert title


def test_a_dangling_separator_is_not_a_split():
    assert split_artist_and_title('Bonobo - ') == (None, 'Bonobo -')


def test_chapters_carry_their_real_timestamps():
    chapters = [
        Chapter(start_seconds=0, end_seconds=120, title='Totally Enormous Extinct Dinosaurs - Islas Canarias'),
        Chapter(start_seconds=120, end_seconds=360, title='Tennyson & Mr. Carmack - Tuesday'),
    ]
    tracks = tracks_from_chapters(chapters)
    assert [track.position for track in tracks] == [1, 2]
    assert tracks[0].start_seconds == 0
    assert tracks[0].end_seconds == 120
    assert tracks[1].artist == 'Tennyson & Mr. Carmack'
    assert all(track.source == 'chapter' for track in tracks)


def test_raw_text_is_preserved_so_a_bad_split_is_recoverable():
    chapters = [Chapter(start_seconds=0, end_seconds=60, title='weird -- formatting')]
    assert tracks_from_chapters(chapters)[0].raw_text == 'weird -- formatting'


@pytest.mark.parametrize(
    ('line', 'seconds'),
    [
        ('0:00 Bonobo - Kerala', 0),
        ('12:34 Bonobo - Kerala', 754),
        ('1:02:03 Bonobo - Kerala', 3723),
        ('[4:20] Bonobo - Kerala', 260),
        ('(4:20) Bonobo - Kerala', 260),
        ('4:20 - Bonobo - Kerala', 260),
    ],
)
def test_description_timestamps_in_every_common_format(line, seconds):
    tracks = tracks_from_description(line)
    assert len(tracks) == 1
    assert tracks[0].start_seconds == seconds
    assert tracks[0].artist == 'Bonobo'
    assert tracks[0].title == 'Kerala'


def test_description_lines_without_a_timestamp_are_ignored():
    description = """\
Follow me on Instagram: https://instagram.com/example
Subscribe for more mixes!

0:00 Bonobo - Kerala
4:20 Four Tet - Baby

#deephouse #mix
Buy the record at https://example.bandcamp.com
"""
    tracks = tracks_from_description(description)
    assert [track.title for track in tracks] == ['Kerala', 'Baby']


def test_each_description_track_ends_where_the_next_begins():
    tracks = tracks_from_description('0:00 A - One\n2:00 B - Two\n5:00 C - Three')
    assert [track.end_seconds for track in tracks] == [120, 300, None]


def test_a_description_with_no_timestamps_yields_nothing_rather_than_guesses():
    description = 'Tracklist:\nBonobo - Kerala\nFour Tet - Baby\n'
    assert tracks_from_description(description) == []


def test_chapters_win_when_both_are_available():
    chapters = [Chapter(start_seconds=0, end_seconds=60, title='From - Chapters')]
    tracks = best_tracklist(chapters, '0:00 From - Description')
    assert tracks[0].source == 'chapter'
    assert tracks[0].title == 'Chapters'


def test_the_description_is_used_when_there_are_no_chapters():
    tracks = best_tracklist([], '0:00 From - Description')
    assert tracks[0].source == 'description'


def test_nothing_parseable_is_an_empty_tracklist_not_an_error():
    assert best_tracklist([], 'no tracklist here at all') == []
