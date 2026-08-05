"""The mirror: what sync writes, what a re-sync preserves, how names resolve."""

import pytest

from ypl import db
from ypl import service
from ypl import ytdlp
from ypl.models import Chapter
from ypl.models import RemotePlaylist
from ypl.models import RemoteVideo


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

    assert service.resolve_playlist(connection, 'PL1')['title'] == 'Get Insights'
    assert service.resolve_playlist(connection, 'Get Insights')['playlist_id'] == 'PL1'
    assert service.resolve_playlist(connection, 'get insights')['playlist_id'] == 'PL1'
    assert service.resolve_playlist(connection, 'Insights')['playlist_id'] == 'PL1'


def test_an_ambiguous_partial_name_is_an_error_listing_the_candidates(connection, stub_remote):
    stub_remote(playlist(video('a'), playlist_id='PL1', title='Deep Night One'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('b'), playlist_id='PL2', title='Deep Night Two'))
    service.sync_playlist(connection, 'https://example.invalid/PL2')

    with pytest.raises(service.AmbiguousPlaylistError) as caught:
        service.resolve_playlist(connection, 'Deep Night')
    assert {row['playlist_id'] for row in caught.value.candidates} == {'PL1', 'PL2'}


def test_an_exact_title_wins_over_a_partial_match_of_the_same_string(connection, stub_remote):
    stub_remote(playlist(video('a'), playlist_id='PL1', title='Deep'))
    service.sync_playlist(connection, 'https://example.invalid/PL1')
    stub_remote(playlist(video('b'), playlist_id='PL2', title='Deep Night'))
    service.sync_playlist(connection, 'https://example.invalid/PL2')

    assert service.resolve_playlist(connection, 'Deep')['playlist_id'] == 'PL1'


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

    row = service.list_playlists(connection)[0]
    assert row['item_count'] == 2
    assert row['enriched_count'] == 1
