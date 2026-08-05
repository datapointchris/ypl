"""Everything that touches the mirror.

The effectful bookend to `tracklist`: this module reads the world and writes it
back, and holds no parsing logic of its own.
"""

import sqlite3
from datetime import UTC
from datetime import datetime

from ypl import tracklist
from ypl import ytdlp
from ypl.models import RemotePlaylist
from ypl.models import Track


class PlaylistNotFoundError(LookupError):
    pass


class AmbiguousPlaylistError(LookupError):
    def __init__(self, name: str, candidates: list[sqlite3.Row]):
        self.name = name
        self.candidates = candidates
        super().__init__(f'{name!r} matches {len(candidates)} playlists')


def now_ts() -> str:
    return datetime.now(UTC).isoformat(timespec='seconds')


def sync_playlist(connection: sqlite3.Connection, url: str, cookies_from_browser: str | None = None) -> RemotePlaylist:
    """Mirror one playlist's current contents.

    The membership rows are replaced wholesale rather than diffed, because a
    playlist is an ordered list and a reorder on YouTube changes every position
    after the moved item. `videos` rows are upserted instead, so an enrichment
    already paid for is not thrown away by a re-sync.
    """
    playlist = ytdlp.fetch_playlist(url, cookies_from_browser=cookies_from_browser)
    with connection:
        connection.execute(
            """
            INSERT INTO playlists (playlist_id, title, description, channel, item_count, synced_ts)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT (playlist_id) DO UPDATE SET
                title = excluded.title,
                description = excluded.description,
                channel = excluded.channel,
                item_count = excluded.item_count,
                synced_ts = excluded.synced_ts
            """,
            (
                playlist.playlist_id,
                playlist.title,
                playlist.description,
                playlist.channel,
                len(playlist.videos),
                now_ts(),
            ),
        )
        for video in playlist.videos:
            connection.execute(
                """
                INSERT INTO videos (video_id, title, channel, duration_seconds, is_unavailable)
                VALUES (?, ?, ?, ?, ?)
                ON CONFLICT (video_id) DO UPDATE SET
                    title = excluded.title,
                    channel = excluded.channel,
                    duration_seconds = excluded.duration_seconds,
                    is_unavailable = excluded.is_unavailable
                """,
                (video.video_id, video.title, video.channel, video.duration_seconds, int(video.is_unavailable)),
            )
        connection.execute('DELETE FROM playlist_videos WHERE playlist_id = ?', (playlist.playlist_id,))
        connection.executemany(
            'INSERT INTO playlist_videos (playlist_id, video_id, position) VALUES (?, ?, ?)',
            [(playlist.playlist_id, video.video_id, position) for position, video in enumerate(playlist.videos, start=1)],
        )
    return playlist


def store_tracks(connection: sqlite3.Connection, video_id: str, tracks: list[Track]) -> None:
    with connection:
        connection.execute('DELETE FROM tracks WHERE video_id = ?', (video_id,))
        connection.executemany(
            """
            INSERT INTO tracks (video_id, position, start_seconds, end_seconds, artist, title, raw_text, source)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            """,
            [
                (
                    video_id,
                    track.position,
                    track.start_seconds,
                    track.end_seconds,
                    track.artist,
                    track.title,
                    track.raw_text,
                    track.source,
                )
                for track in tracks
            ],
        )


def enrich_video(connection: sqlite3.Connection, video_id: str, cookies_from_browser: str | None = None) -> list[Track]:
    """Fetch one video in full and store whatever tracklist it yields."""
    video = ytdlp.fetch_video(video_id, cookies_from_browser=cookies_from_browser)
    tracks = tracklist.best_tracklist(video.chapters, video.description)
    with connection:
        connection.execute(
            'UPDATE videos SET description = ?, upload_date = ?, enriched_ts = ? WHERE video_id = ?',
            (video.description, video.upload_date, now_ts(), video_id),
        )
    store_tracks(connection, video_id, tracks)
    return tracks


def unenriched_video_ids(connection: sqlite3.Connection, playlist_id: str | None = None, limit: int | None = None) -> list[str]:
    query = """
        SELECT DISTINCT v.video_id
        FROM videos v
        JOIN playlist_videos pv ON pv.video_id = v.video_id
        WHERE v.enriched_ts IS NULL AND v.is_unavailable = 0
    """
    parameters: list[object] = []
    if playlist_id:
        query += ' AND pv.playlist_id = ?'
        parameters.append(playlist_id)
    query += ' ORDER BY v.video_id'
    if limit:
        query += ' LIMIT ?'
        parameters.append(limit)
    return [row['video_id'] for row in connection.execute(query, parameters)]


def list_playlists(connection: sqlite3.Connection, limit: int | None = None) -> list[sqlite3.Row]:
    query = """
        SELECT p.playlist_id, p.title, p.channel, p.item_count, p.synced_ts,
               (SELECT COUNT(*) FROM playlist_videos pv
                JOIN videos v ON v.video_id = pv.video_id
                WHERE pv.playlist_id = p.playlist_id AND v.enriched_ts IS NOT NULL) AS enriched_count
        FROM playlists p
        ORDER BY p.title
    """
    if limit:
        query += ' LIMIT ?'
        return list(connection.execute(query, (limit,)))
    return list(connection.execute(query))


def resolve_playlist(connection: sqlite3.Connection, name: str) -> sqlite3.Row:
    """Find a playlist by id, exact title, or unambiguous case-insensitive match.

    Ambiguity is an error listing the candidates rather than a guess — picking
    one silently is how the wrong playlist gets rewritten.
    """
    by_id = connection.execute('SELECT * FROM playlists WHERE playlist_id = ?', (name,)).fetchone()
    if by_id:
        return by_id
    matches = list(connection.execute('SELECT * FROM playlists WHERE title = ? COLLATE NOCASE', (name,)))
    if not matches:
        matches = list(connection.execute('SELECT * FROM playlists WHERE title LIKE ? COLLATE NOCASE', (f'%{name}%',)))
    if not matches:
        raise PlaylistNotFoundError(name)
    if len(matches) > 1:
        raise AmbiguousPlaylistError(name, matches)
    return matches[0]


def playlist_videos(connection: sqlite3.Connection, playlist_id: str, limit: int | None = None) -> list[sqlite3.Row]:
    query = """
        SELECT pv.position, v.video_id, v.title, v.channel, v.duration_seconds,
               v.is_unavailable, v.enriched_ts,
               (SELECT COUNT(*) FROM tracks t WHERE t.video_id = v.video_id) AS track_count
        FROM playlist_videos pv
        JOIN videos v ON v.video_id = pv.video_id
        WHERE pv.playlist_id = ?
        ORDER BY pv.position
    """
    parameters: list[object] = [playlist_id]
    if limit:
        query += ' LIMIT ?'
        parameters.append(limit)
    return list(connection.execute(query, parameters))


SORT_CLAUSES = {
    'position': 'pv.position ASC',
    'oldest': 'v.upload_date ASC NULLS LAST, pv.position ASC',
    'newest': 'v.upload_date DESC NULLS LAST, pv.position ASC',
    'random': 'RANDOM()',
}


def playlist_video_urls(connection: sqlite3.Connection, playlist_id: str, sort: str, limit: int | None = None) -> list[sqlite3.Row]:
    """The selector behind `| relate` and `menu next`.

    Unavailable videos are excluded: the point of this command is to hand a URL
    to something that will fetch it, and a deleted video wastes that call.
    """
    query = f"""
        SELECT v.video_id, v.title, v.channel, v.upload_date, pv.position
        FROM playlist_videos pv
        JOIN videos v ON v.video_id = pv.video_id
        WHERE pv.playlist_id = ? AND v.is_unavailable = 0
        ORDER BY {SORT_CLAUSES[sort]}
    """
    parameters: list[object] = [playlist_id]
    if limit:
        query += ' LIMIT ?'
        parameters.append(limit)
    return list(connection.execute(query, parameters))


def video_tracks(connection: sqlite3.Connection, video_id: str) -> list[sqlite3.Row]:
    return list(
        connection.execute(
            'SELECT position, start_seconds, end_seconds, artist, title, raw_text, source FROM tracks WHERE video_id = ? ORDER BY position',
            (video_id,),
        )
    )


def get_video(connection: sqlite3.Connection, video_id: str) -> sqlite3.Row | None:
    return connection.execute('SELECT * FROM videos WHERE video_id = ?', (video_id,)).fetchone()
