"""The mirror: what sync writes, what a re-sync preserves, how names resolve."""

import sqlite3

import pytest

from ypl import config
from ypl import db
from ypl import history
from ypl import local
from ypl import m3u
from ypl import service
from ypl import ytdlp
from ypl.local import LocalPlaylist
from ypl.models import Chapter
from ypl.models import RemotePlaylist
from ypl.models import RemoteVideo
from ypl.models import Track


@pytest.fixture(autouse=True)
def isolated_playlists(tmp_path, monkeypatch):
    """Resolution reads the local playlist directory, so it must not be the real one."""
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


@pytest.fixture
def connection(tmp_path):
    return db.connect(tmp_path / 'ypl.db')


def video(video_id, title='A Mix', **overrides):
    return RemoteVideo(video_id=video_id, title=title, channel='Cercle', duration_seconds=3600, **overrides)


def playlist(*videos, playlist_id='PL1', title='Deep Night'):
    return RemotePlaylist(playlist_id=playlist_id, title=title, channel='me', videos=list(videos))


@pytest.fixture
def stub_remote(monkeypatch):
    """Replace the network boundary, leaving every layer below it real."""

    def install(fetched_playlist=None, fetched_video=None):
        if fetched_playlist is not None:
            monkeypatch.setattr(ytdlp, 'fetch_playlist', lambda *args, **kwargs: fetched_playlist)
        if fetched_video is not None:
            monkeypatch.setattr(ytdlp, 'fetch_video', lambda *args, **kwargs: fetched_video)

    return install


def test_reopening_a_populated_mirror_neither_wipes_it_nor_duplicates_lookups(tmp_path, stub_remote):
    """The schema runs on every open, which is what lets a new table reach an old mirror.

    The risk that buys is re-running it over real data, so this pins that
    re-running is harmless: rows survive and the seeded lookup does not double.
    """
    database = tmp_path / 'ypl.db'
    first = db.connect(database)
    stub_remote(playlist(video('a')))
    service.sync_playlist(first, 'https://example.invalid/PL1')
    first.close()

    second = db.connect(database)
    assert [row['video_id'] for row in service.playlist_videos(second, 'PL1')] == ['a']
    assert second.execute('SELECT COUNT(*) AS found FROM track_sources').fetchone()['found'] == 4


def test_a_table_added_to_the_schema_reaches_a_mirror_that_already_exists(tmp_path):
    """A new table is the one migration this design supports, so it has to work.

    Simulated by dropping an existing one, which is what a mirror created before
    that table was added looks like.
    """
    database = tmp_path / 'ypl.db'
    db.connect(database).close()
    with sqlite3.connect(database) as raw:
        raw.execute('DROP TABLE tracks')

    reopened = db.connect(database)
    assert reopened.execute("SELECT name FROM sqlite_master WHERE name = 'tracks'").fetchone() is not None


def test_a_fresh_database_gets_the_schema_and_the_source_lookup(connection):
    sources = {row['source'] for row in connection.execute('SELECT source FROM track_sources')}
    assert sources == {'chapter', 'description', 'llm', 'manual'}


def test_sync_stores_the_playlist_its_videos_and_their_order(connection, stub_remote):
    stub_remote(playlist(video('a'), video('b'), video('c')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    rows = service.playlist_videos(connection, 'PL1')
    assert [row['video_id'] for row in rows] == ['a', 'b', 'c']
    assert [row['position'] for row in rows] == [1, 2, 3]


def test_a_playlist_may_hold_the_same_video_twice(connection, stub_remote):
    stub_remote(playlist(video('a'), video('b'), video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    rows = service.playlist_videos(connection, 'PL1')
    assert [row['video_id'] for row in rows] == ['a', 'b', 'a']


def test_resync_reorders_without_discarding_enrichment(connection, stub_remote):
    stub_remote(playlist(video('a'), video('b')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(fetched_video=RemoteVideo(video_id='a', title='A Mix', description='notes', chapters=[]))
    service.enrich_video(connection, 'a')

    stub_remote(playlist(video('b'), video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    rows = service.playlist_videos(connection, 'PL1')
    assert [row['video_id'] for row in rows] == ['b', 'a']
    assert service.get_video(connection, 'a')['enriched_ts'] is not None
    assert service.get_video(connection, 'a')['description'] == 'notes'


def test_a_video_removed_from_the_playlist_leaves_the_membership_row_behind(connection, stub_remote):
    stub_remote(playlist(video('a'), video('b')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    assert [row['video_id'] for row in service.playlist_videos(connection, 'PL1')] == ['a']


def test_unavailable_videos_are_kept_so_positions_do_not_shift(connection, stub_remote):
    stub_remote(playlist(video('a'), video('gone', title='[Deleted video]', is_unavailable=True), video('c')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    rows = service.playlist_videos(connection, 'PL1')
    assert [row['position'] for row in rows] == [1, 2, 3]
    assert rows[1]['is_unavailable'] == 1


def test_enrichment_skips_unavailable_and_already_enriched_videos(connection, stub_remote):
    stub_remote(playlist(video('a'), video('gone', is_unavailable=True), video('c')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(fetched_video=RemoteVideo(video_id='a', title='A Mix'))
    service.enrich_video(connection, 'a')

    assert service.unenriched_video_ids(connection) == ['c']


def test_enrichment_stores_the_tracklist_from_chapters(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(
        fetched_video=RemoteVideo(
            video_id='a',
            title='A Mix',
            chapters=[
                Chapter(start_seconds=0, end_seconds=120, title='Bonobo - Kerala'),
                Chapter(start_seconds=120, end_seconds=300, title='Four Tet - Baby'),
            ],
        )
    )
    tracks = service.enrich_video(connection, 'a')

    assert len(tracks) == 2
    stored = service.video_tracks(connection, 'a')
    assert [row['artist'] for row in stored] == ['Bonobo', 'Four Tet']
    assert stored[0]['end_seconds'] == 120


def test_re_enriching_replaces_the_tracklist_rather_than_appending(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(fetched_video=RemoteVideo(video_id='a', title='A', chapters=[Chapter(0, 60, 'X - One')]))
    service.enrich_video(connection, 'a')
    stub_remote(fetched_video=RemoteVideo(video_id='a', title='A', chapters=[Chapter(0, 60, 'Y - Two')]))
    service.enrich_video(connection, 'a')

    stored = service.video_tracks(connection, 'a')
    assert len(stored) == 1
    assert stored[0]['artist'] == 'Y'


def test_a_playlist_resolves_by_id_exact_title_and_partial_title(connection, stub_remote):
    stub_remote(playlist(video('a'), title='Get Insights'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    assert service.resolve_playlist(connection, 'PL1').title == 'Get Insights'
    assert service.resolve_playlist(connection, 'Get Insights').identifier == 'PL1'
    assert service.resolve_playlist(connection, 'get insights').identifier == 'PL1'
    assert service.resolve_playlist(connection, 'Insights').identifier == 'PL1'


def test_an_ambiguous_partial_name_is_an_error_listing_the_candidates(connection, stub_remote):
    stub_remote(playlist(video('a'), playlist_id='PL1', title='Deep Night One'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('b'), playlist_id='PL2', title='Deep Night Two'))
    service.sync_playlist(connection, 'https://example.invalid/PL2')

    with pytest.raises(service.AmbiguousPlaylistError) as caught:
        service.resolve_playlist(connection, 'Deep Night')
    assert {candidate.identifier for candidate in caught.value.candidates} == {'PL1', 'PL2'}


def test_an_exact_title_wins_over_a_partial_match_of_the_same_string(connection, stub_remote):
    stub_remote(playlist(video('a'), playlist_id='PL1', title='Deep'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('b'), playlist_id='PL2', title='Deep Night'))
    service.sync_playlist(connection, 'https://example.invalid/PL2')

    assert service.resolve_playlist(connection, 'Deep').identifier == 'PL1'


def test_an_unknown_name_raises_rather_than_returning_nothing(connection):
    with pytest.raises(service.PlaylistNotFoundError):
        service.resolve_playlist(connection, 'nope')


def test_urls_exclude_unavailable_videos(connection, stub_remote):
    stub_remote(playlist(video('a'), video('gone', is_unavailable=True)))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    rows = service.playlist_video_urls(connection, 'PL1', 'position')
    assert [row['video_id'] for row in rows] == ['a']


def test_urls_sort_oldest_first_and_put_undated_videos_last(connection, stub_remote):
    stub_remote(playlist(video('new'), video('old'), video('undated')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    for video_id, upload_date in [('new', '20240101'), ('old', '20200101'), ('undated', None)]:
        stub_remote(fetched_video=RemoteVideo(video_id=video_id, title='x', upload_date=upload_date))
        service.enrich_video(connection, video_id)

    oldest = service.playlist_video_urls(connection, 'PL1', 'oldest')
    assert [row['video_id'] for row in oldest] == ['old', 'new', 'undated']
    newest = service.playlist_video_urls(connection, 'PL1', 'newest')
    assert [row['video_id'] for row in newest] == ['new', 'old', 'undated']


def test_the_limit_applies_after_the_sort_not_before(connection, stub_remote):
    stub_remote(playlist(video('new'), video('old')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    for video_id, upload_date in [('new', '20240101'), ('old', '20200101')]:
        stub_remote(fetched_video=RemoteVideo(video_id=video_id, title='x', upload_date=upload_date))
        service.enrich_video(connection, video_id)

    rows = service.playlist_video_urls(connection, 'PL1', 'oldest', limit=1)
    assert [row['video_id'] for row in rows] == ['old']


def test_deleting_a_playlist_takes_its_membership_rows_but_not_the_videos(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    with connection:
        connection.execute('DELETE FROM playlists WHERE playlist_id = ?', ('PL1',))

    assert service.playlist_videos(connection, 'PL1') == []
    assert service.get_video(connection, 'a') is not None


def test_listing_playlists_reports_how_many_videos_are_enriched(connection, stub_remote):
    stub_remote(playlist(video('a'), video('b')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(fetched_video=RemoteVideo(video_id='a', title='A'))
    service.enrich_video(connection, 'a')

    row = service.playlist_summaries(connection)[0]
    assert row['item_count'] == 2
    assert row['enriched_count'] == 1


def test_next_prefers_a_video_that_has_never_been_played(connection, stub_remote):
    stub_remote(playlist(video('heard'), video('unheard')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    history.record('heard')

    assert [row['video_id'] for row in service.next_videos(connection, limit=2)] == ['unheard', 'heard']


def test_next_returns_the_least_recently_played_once_everything_has_been_heard(connection, stub_remote):
    """`second` is the oldest first listen and the newest last one.

    Ordering on its earliest play would put it first, which is why the history
    reads MAX rather than MIN — "when did I last hear this" is the question.
    """
    stub_remote(playlist(video('first'), video('second'), video('third')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    history.record('second', '2026-08-01')
    history.record('first', '2026-08-02')
    history.record('third', '2026-08-03')
    history.record('second', '2026-08-04')

    assert [row['video_id'] for row in service.next_videos(connection, limit=3)] == ['first', 'third', 'second']


def test_next_carries_the_play_count_so_a_caller_can_see_why(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    history.record('a')
    history.record('a')

    suggestion = service.next_videos(connection, limit=1)[0]
    assert suggestion['play_count'] == 2
    assert suggestion['last_played_ts'] is not None


def test_next_does_not_hand_back_the_same_order_every_time(connection, stub_remote):
    """Nothing is played on the first run, so every candidate ties.

    Sorted without shuffling first, that tie resolves to mirror order and the
    same mix is suggested forever.
    """
    stub_remote(playlist(*[video(f'v{index:02d}') for index in range(20)]))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    firsts = {service.next_videos(connection, limit=1)[0]['video_id'] for _ in range(15)}
    assert len(firsts) > 1


def test_next_never_suggests_an_unavailable_video(connection, stub_remote):
    stub_remote(playlist(video('gone', is_unavailable=True), video('fine')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')

    assert [row['video_id'] for row in service.next_videos(connection, limit=5)] == ['fine']


def test_next_can_be_scoped_to_one_playlist(connection, stub_remote):
    stub_remote(playlist(video('inside'), playlist_id='PL1', title='Wanted'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('outside'), playlist_id='PL2', title='Other'))
    service.sync_playlist(connection, 'https://example.invalid/PL2')

    wanted = service.resolve_playlist(connection, 'Wanted')
    assert [row['video_id'] for row in service.next_videos(connection, wanted, limit=5)] == ['inside']


def test_the_track_at_an_offset_is_the_latest_one_to_have_started(connection, stub_remote):
    """Several tracks can match at once when their ends are unknown.

    A tracklist parsed out of a description leaves `end_seconds` NULL, and every
    such track from the start of the video onwards satisfies "started before
    now and has not been said to finish". The latest start is the one playing.
    """
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    service.store_tracks(
        connection,
        'a',
        [
            Track(position=1, title='First', raw_text='', source='description', start_seconds=0, end_seconds=None),
            Track(position=2, title='Second', raw_text='', source='description', start_seconds=100, end_seconds=None),
            Track(position=3, title='Third', raw_text='', source='description', start_seconds=200, end_seconds=None),
        ],
    )

    assert service.track_at(connection, 'a', 150)['title'] == 'Second'
    assert service.track_at(connection, 'a', 0)['title'] == 'First'
    assert service.track_at(connection, 'a', 9999)['title'] == 'Third'


def test_a_track_that_has_finished_does_not_keep_playing(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    service.store_tracks(
        connection,
        'a',
        [Track(position=1, title='Only', raw_text='', source='chapter', start_seconds=0, end_seconds=60)],
    )

    assert service.track_at(connection, 'a', 30)['title'] == 'Only'
    assert service.track_at(connection, 'a', 60) is None


def test_a_track_with_no_start_time_cannot_be_placed_and_is_skipped(connection, stub_remote):
    stub_remote(playlist(video('a')))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    service.store_tracks(connection, 'a', [Track(position=1, title='Unplaceable', raw_text='', source='llm')])

    assert service.track_at(connection, 'a', 10) is None


@pytest.mark.parametrize(
    ('count', 'size', 'expected'),
    [
        (100, 50, [50, 50]),
        (101, 50, [51, 50]),
        (140, 90, [70, 70]),
        (100, 90, [100]),
        (10, 90, [10]),
        (0, 90, []),
        (5, 1, [1, 1, 1, 1, 1]),
    ],
)
def test_a_split_by_size_spreads_the_remainder_rather_than_leaving_a_stub(count, size, expected):
    """140 at a size of 90 is two of 70, not a 90 and a 50."""
    runs = service.split_evenly(list(range(count)), size=size)
    assert [len(run) for run in runs] == expected


@pytest.mark.parametrize(
    ('count', 'parts', 'expected'),
    [
        (10, 3, [4, 3, 3]),
        (9, 3, [3, 3, 3]),
        (2, 5, [1, 1]),
        (1, 1, [1]),
    ],
)
def test_a_split_by_parts_never_produces_more_parts_than_there_are_videos(count, parts, expected):
    runs = service.split_evenly(list(range(count)), parts=parts)
    assert [len(run) for run in runs] == expected


def test_a_split_keeps_every_video_exactly_once_and_in_order():
    runs = service.split_evenly(list(range(37)), size=10)
    assert [item for run in runs for item in run] == list(range(37))


@pytest.mark.parametrize('arguments', [{}, {'size': 10, 'parts': 3}])
def test_a_split_needs_exactly_one_of_size_and_parts(arguments):
    with pytest.raises(ValueError, match='exactly one'):
        service.split_evenly([1, 2, 3], **arguments)


@pytest.mark.parametrize(
    ('count', 'expected_first', 'expected_last'),
    [(3, 'Deep Night 1', 'Deep Night 3'), (12, 'Deep Night 01', 'Deep Night 12')],
)
def test_part_names_are_padded_so_ten_does_not_sort_before_two(count, expected_first, expected_last):
    names = service.part_names('Deep Night', count)
    assert (names[0], names[-1]) == (expected_first, expected_last)


def test_the_mirror_lets_a_reader_in_while_a_writer_holds_it(tmp_path):
    """Two writers now: a timer that syncs, and whoever is at the prompt.

    Under the default journal a read blocks behind an open write, so a
    background process nobody asked for would fail the command in front of you.
    """
    database = tmp_path / 'ypl.db'
    writer = db.connect(database)
    reader = db.connect(database)

    assert writer.execute('PRAGMA journal_mode').fetchone()[0] == 'wal'
    writer.execute('BEGIN')
    writer.execute("INSERT INTO videos (video_id, title) VALUES ('a', 'A Mix')")
    assert reader.execute('SELECT COUNT(*) FROM videos').fetchone()[0] == 0


def test_the_request_pace_has_a_floor_no_config_can_go_under():
    """`request_interval_seconds = 0` parsed fine and removed the pacing entirely.

    It is the one setting that can turn a personal tool into something shaped
    like a scraper, so the value is a floor on how slow to go rather than a free
    choice of how fast.
    """
    assert config.Config(request_interval_seconds=0).request_pace_seconds == config.MINIMUM_INTERVAL_SECONDS
    assert config.Config(request_interval_seconds=0.01).request_pace_seconds == config.MINIMUM_INTERVAL_SECONDS
    # Slower is always allowed.
    assert config.Config(request_interval_seconds=30).request_pace_seconds == 30


def bound_file(name, remote_id, synced=True):
    saved = LocalPlaylist(
        name=name,
        path=local.path_for(name),
        entries=[m3u.Entry(video_id='v1', title='A Mix')],
        synced=synced,
        remote_id=remote_id,
    )
    local.save(saved, overwrite=True)
    return saved


def mirrored(playlist_id, channel_id):
    return RemotePlaylist(playlist_id=playlist_id, title=playlist_id, channel='whoever', channel_id=channel_id)


def test_a_playlist_another_channel_owns_stops_being_synced():
    """The failure this fixes ran every half hour and never cleared.

    Two playlists saved from other channels were bound before ownership was
    consulted, so every run queued a reconcile the write client cannot serve and
    logged the same two failures forever.
    """
    bound_file('Understanding Trauma', 'PLtheirs')
    account = service.AccountSync(channel_id='UCmine', synced=[mirrored('PLtheirs', 'UCtheirs')])

    assert service.demote_unowned_playlists(account) == ['Understanding Trauma']
    assert local.load(local.path_for('Understanding Trauma')).synced is False


def test_demoting_keeps_the_remote_id_and_everything_in_the_file():
    """A demotion says where the file can be pushed, not what belongs in it."""
    bound_file('Understanding Trauma', 'PLtheirs')
    service.demote_unowned_playlists(service.AccountSync(channel_id='UCmine', synced=[mirrored('PLtheirs', 'UCtheirs')]))

    reloaded = local.load(local.path_for('Understanding Trauma'))
    assert reloaded.remote_id == 'PLtheirs'
    assert reloaded.video_ids == ['v1']


def test_a_playlist_this_account_owns_is_left_synced():
    bound_file('Chill Out', 'PLmine')
    account = service.AccountSync(channel_id='UCmine', synced=[mirrored('PLmine', 'UCmine')])

    assert service.demote_unowned_playlists(account) == []
    assert local.load(local.path_for('Chill Out')).synced is True


def test_an_unknown_account_channel_demotes_nothing():
    """The failure mode worth guarding: one bad account read is not proof.

    `owned_by` is false for an unknown account channel exactly as it is for a
    genuinely foreign one, so trusting it here would unsync the whole library on
    a run that could not work out who we are.
    """
    bound_file('Chill Out', 'PLmine')
    account = service.AccountSync(channel_id='', synced=[mirrored('PLmine', 'UCmine')])

    assert service.demote_unowned_playlists(account) == []
    assert local.load(local.path_for('Chill Out')).synced is True


def test_a_playlist_this_run_did_not_mirror_is_left_alone():
    """A limited run mirrors some of the library, and silence is not ownership."""
    bound_file('Chill Out', 'PLmine')
    account = service.AccountSync(channel_id='UCmine', synced=[mirrored('PLother', 'UCmine')])

    assert service.demote_unowned_playlists(account) == []
    assert local.load(local.path_for('Chill Out')).synced is True


def test_a_mirrored_playlist_with_no_known_owner_is_left_alone():
    bound_file('Chill Out', 'PLmine')
    account = service.AccountSync(channel_id='UCmine', synced=[mirrored('PLmine', '')])

    assert service.demote_unowned_playlists(account) == []
    assert local.load(local.path_for('Chill Out')).synced is True


class EmptyReadBackend:
    """A write client that answers with the page and no videos in it."""

    def playlist_items(self, playlist_id):
        return []


def test_a_write_read_that_comes_back_empty_is_refused_when_the_mirror_holds_videos(connection, stub_remote):
    """What left two playlists as header-only files that could not be played.

    An empty read passed the handle check vacuously — there were no handles for
    it to fault — and wrote a file that looked bound and held nothing.
    """
    stub_remote(fetched_playlist=playlist(video('v1'), video('v2')))
    service.sync_playlist(connection, 'PL1')
    candidate = service.ResolvedPlaylist(kind=service.REMOTE, title='Deep Night', identifier='PL1')

    with pytest.raises(service.BindError):
        service.bind_remote_playlist(connection, candidate, EmptyReadBackend())
    assert not local.path_for('Deep Night').exists()


def test_a_playlist_that_really_is_empty_still_binds(connection, stub_remote):
    """The mirror is what tells the two apart, and it agrees here."""
    stub_remote(fetched_playlist=playlist(playlist_id='PL1'))
    service.sync_playlist(connection, 'PL1')
    candidate = service.ResolvedPlaylist(kind=service.REMOTE, title='Deep Night', identifier='PL1')

    bound = service.bind_remote_playlist(connection, candidate, EmptyReadBackend())
    assert bound.video_ids == []
