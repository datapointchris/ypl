"""Everything that touches stored state — the mirror, and the local playlists.

The effectful bookend to `tracklist`: this module reads the world and writes it
back, and holds no parsing logic of its own.

It reaches into `local` for one reason: a name typed on the command line can
name a mirrored playlist or an authored one, and only a layer that can see both
stores can say which — or refuse when the answer is both.
"""

import random
import sqlite3
from dataclasses import dataclass
from datetime import UTC
from datetime import datetime

from ypl import local
from ypl import m3u
from ypl import tracklist
from ypl import ytdlp
from ypl.local import LocalPlaylist
from ypl.models import RemotePlaylist
from ypl.models import Track

REMOTE = 'remote'
LOCAL = 'local'


class PlaylistNotFoundError(LookupError):
    pass


class AmbiguousPlaylistError(LookupError):
    def __init__(self, name: str, candidates: list['ResolvedPlaylist']):
        self.name = name
        self.candidates = candidates
        super().__init__(f'{name!r} matches {len(candidates)} playlists')


@dataclass
class ResolvedPlaylist:
    """A playlist named on the command line, from whichever store holds it.

    One type for both so that every command takes a name and then decides what
    to do, rather than each command owning a second lookup path for local files.
    """

    kind: str
    title: str
    identifier: str
    remote: sqlite3.Row | None = None
    # LocalPlaylist is imported by name rather than reached through the module:
    # a dataclass evaluates the annotation after binding the field, so
    # `local.LocalPlaylist` here would look up the field `local`, not the
    # module, and fail at import time.
    local: LocalPlaylist | None = None


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
    """Fetch one video in full and store whatever tracklist it yields.

    Inserts rather than only updating, because enrichment is a fact about a
    video and not about its membership of anything: a video reached through a
    local playlist may never have been in a mirrored playlist at all, and the
    full extraction returns every field `sync` would have written anyway.
    """
    video = ytdlp.fetch_video(video_id, cookies_from_browser=cookies_from_browser)
    tracks = tracklist.best_tracklist(video.chapters, video.description)
    with connection:
        connection.execute(
            """
            INSERT INTO videos (video_id, title, channel, duration_seconds, description, upload_date, enriched_ts)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT (video_id) DO UPDATE SET
                title = excluded.title,
                channel = excluded.channel,
                duration_seconds = excluded.duration_seconds,
                description = excluded.description,
                upload_date = excluded.upload_date,
                enriched_ts = excluded.enriched_ts
            """,
            (video_id, video.title, video.channel, video.duration_seconds, video.description, video.upload_date, now_ts()),
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


def unenriched_among(connection: sqlite3.Connection, video_ids: list[str]) -> list[str]:
    """Which of these videos still need enriching, in the order given.

    A video absent from the mirror counts as unenriched rather than unknown,
    which is what makes a local playlist of hand-pasted URLs enrichable — the
    full extraction creates the row.
    """
    if not video_ids:
        return []
    placeholders = ','.join('?' for _ in video_ids)
    enriched = {
        row['video_id']
        for row in connection.execute(
            f'SELECT video_id FROM videos WHERE enriched_ts IS NOT NULL AND video_id IN ({placeholders})',
            video_ids,
        )
    }
    pending = []
    seen: set[str] = set()
    for video_id in video_ids:
        if video_id in enriched or video_id in seen:
            continue
        seen.add(video_id)
        pending.append(video_id)
    return pending


def unenriched_for(connection: sqlite3.Connection, playlist: ResolvedPlaylist | None, limit: int | None = None) -> list[str]:
    """What `ypl enrich` should fetch next, whichever store the scope came from."""
    if playlist is not None and playlist.local is not None:
        pending = unenriched_among(connection, playlist.local.video_ids)
        return pending[:limit] if limit else pending
    return unenriched_video_ids(connection, playlist.identifier if playlist else None, limit)


def videos_by_id(connection: sqlite3.Connection, video_ids: list[str]) -> dict[str, sqlite3.Row]:
    """The mirror's rows for these videos, keyed by id, missing ones absent.

    Local playlists carry video ids that the mirror may or may not know, so
    every command reading one has to tolerate a gap rather than join over it.
    """
    if not video_ids:
        return {}
    placeholders = ','.join('?' for _ in video_ids)
    query = f"""
        SELECT v.*, (SELECT COUNT(*) FROM tracks t WHERE t.video_id = v.video_id) AS track_count
        FROM videos v WHERE v.video_id IN ({placeholders})
    """
    return {row['video_id']: row for row in connection.execute(query, video_ids)}


def local_playlist_videos(connection: sqlite3.Connection, playlist: LocalPlaylist, limit: int | None = None) -> list[dict]:
    """A local playlist's entries in file order, filled in from the mirror.

    The columns match `playlist_videos` so one renderer serves both. What the
    mirror does not know falls back to what the file itself recorded, which is
    the point of writing a title into the M3U at all.
    """
    known = videos_by_id(connection, playlist.video_ids)
    rows = []
    for position, entry in enumerate(playlist.entries, start=1):
        video = known.get(entry.video_id)
        rows.append(
            {
                'position': position,
                'video_id': entry.video_id,
                'title': video['title'] if video else entry.title,
                'channel': video['channel'] if video else '',
                'duration_seconds': video['duration_seconds'] if video else entry.duration_seconds,
                'is_unavailable': video['is_unavailable'] if video else 0,
                'enriched_ts': video['enriched_ts'] if video else None,
                'track_count': video['track_count'] if video else 0,
                'upload_date': video['upload_date'] if video else None,
                'in_mirror': video is not None,
            }
        )
    return rows[:limit] if limit else rows


def upload_date_number(upload_date: str | None) -> int:
    """yt-dlp's `YYYYMMDD` as a sortable integer, 0 when it is missing or odd."""
    return int(upload_date) if upload_date and upload_date.isdigit() else 0


# The direction lives in the key rather than in `reverse=True`, which would also
# flip the position tiebreaker and pull the unknowns to the front. The leading
# boolean is what puts them last in both directions, matching `NULLS LAST` in
# the SQL above; position breaks every remaining tie so a sort is repeatable.
LOCAL_SORT_KEYS = {
    'oldest': lambda row: (row['upload_date'] is None, upload_date_number(row['upload_date']), row['position']),
    'newest': lambda row: (row['upload_date'] is None, -upload_date_number(row['upload_date']), row['position']),
    'longest': lambda row: (row['duration_seconds'] is None, -(row['duration_seconds'] or 0), row['position']),
    'shortest': lambda row: (row['duration_seconds'] is None, row['duration_seconds'] or 0, row['position']),
    'title': lambda row: (row['title'].lower(), row['position']),
}


def sort_local_rows(rows: list[dict], sort: str) -> list[dict]:
    """Apply the same sort vocabulary the mirror queries use.

    Sorted here rather than in SQL because a local playlist's order is the file,
    and a video it names may have no row in the mirror to sort by.
    """
    if sort == 'position':
        return rows
    if sort == 'random':
        shuffled = list(rows)
        random.shuffle(shuffled)
        return shuffled
    return sorted(rows, key=LOCAL_SORT_KEYS[sort])


def local_playlist_video_urls(
    connection: sqlite3.Connection,
    playlist: LocalPlaylist,
    sort: str,
    limit: int | None = None,
) -> list[dict]:
    """The selector, against a local playlist.

    Unavailable videos are dropped for the same reason as the mirrored version:
    the output exists to be handed to something that will fetch it.
    """
    rows = [row for row in local_playlist_videos(connection, playlist) if not row['is_unavailable']]
    ordered = sort_local_rows(rows, sort)
    return ordered[:limit] if limit else ordered


def playlist_summaries(connection: sqlite3.Connection, kind: str | None = None, limit: int | None = None) -> list[dict]:
    """One row per playlist across both stores, for `playlists list`.

    Local playlists report an enriched count too — it is the answer to "can I
    order this by mood yet", which is the same question the mirrored count
    answers.
    """
    summaries = []
    for candidate in known_playlists(connection, kind):
        if candidate.remote is not None:
            row = candidate.remote
            summaries.append(
                {
                    'kind': REMOTE,
                    'title': row['title'],
                    'identifier': row['playlist_id'],
                    'item_count': row['item_count'],
                    'enriched_count': enriched_count(connection, row['playlist_id']),
                    'channel': row['channel'],
                    'synced_ts': row['synced_ts'],
                    'playlist_id': row['playlist_id'],
                }
            )
            continue
        if candidate.local is None:
            continue
        playlist = candidate.local
        video_ids = playlist.video_ids
        known = videos_by_id(connection, video_ids)
        summaries.append(
            {
                'kind': LOCAL,
                'title': playlist.name,
                'identifier': playlist.slug,
                'item_count': len(video_ids),
                'enriched_count': sum(1 for video_id in video_ids if video_id in known and known[video_id]['enriched_ts']),
                'created_ts': playlist.created_ts,
                'source': playlist.source,
                'path': str(playlist.path),
            }
        )
    return summaries[:limit] if limit else summaries


def enriched_count(connection: sqlite3.Connection, playlist_id: str) -> int:
    row = connection.execute(
        """
        SELECT COUNT(*) AS enriched FROM playlist_videos pv
        JOIN videos v ON v.video_id = pv.video_id
        WHERE pv.playlist_id = ? AND v.enriched_ts IS NOT NULL
        """,
        (playlist_id,),
    ).fetchone()
    return row['enriched']


def display_title(video: sqlite3.Row) -> str:
    """What a player shows for an entry.

    `channel - title` follows the artist-title convention every M3U reader
    expects, and is never parsed back apart — the mirror holds the two fields
    separately, so this is display text and nothing else.
    """
    return f'{video["channel"]} - {video["title"]}' if video['channel'] else video['title']


def entries_for(connection: sqlite3.Connection, video_ids: list[str]) -> list[m3u.Entry]:
    """Turn video ids into playlist entries, labelled from the mirror.

    An id the mirror has never seen still becomes an entry: the playlist is a
    list of videos, and refusing to write one because it has not been synced
    would make pasting a URL in harder than it needs to be. It writes an
    unlabelled entry, which `ypl enrich` then fills in.
    """
    known = videos_by_id(connection, video_ids)
    return [
        m3u.Entry(
            video_id=video_id,
            title=display_title(known[video_id]) if video_id in known else '',
            duration_seconds=known[video_id]['duration_seconds'] if video_id in known else None,
        )
        for video_id in video_ids
    ]


def create_local_playlist(
    connection: sqlite3.Connection,
    name: str,
    video_ids: list[str],
    source: str = '',
    overwrite: bool = False,
) -> LocalPlaylist:
    playlist = LocalPlaylist(
        name=name,
        path=local.path_for(name),
        entries=entries_for(connection, video_ids),
        created_ts=now_ts(),
        source=source,
    )
    local.save(playlist, overwrite=overwrite)
    return playlist


def add_to_local_playlist(connection: sqlite3.Connection, playlist: LocalPlaylist, video_ids: list[str]) -> int:
    """Append videos to a local playlist, returning how many were added.

    Duplicates are permitted for the same reason the mirror permits them: a
    playlist is an ordered list of slots, and the same mix legitimately appears
    twice in one built up over years.
    """
    playlist.entries.extend(entries_for(connection, video_ids))
    local.save(playlist, overwrite=True)
    return len(video_ids)


def remove_from_local_playlist(playlist: LocalPlaylist, video_ids: list[str]) -> int:
    """Drop every entry naming one of these videos, returning how many went."""
    targets = set(video_ids)
    kept = [entry for entry in playlist.entries if entry.video_id not in targets]
    removed = len(playlist.entries) - len(kept)
    playlist.entries = kept
    local.save(playlist, overwrite=True)
    return removed


def playlist_video_count(connection: sqlite3.Connection, playlist: ResolvedPlaylist) -> int:
    """How many videos a playlist holds, before anything is filtered out of it."""
    if playlist.local is not None:
        return len(playlist.local.entries)
    return playlist.remote['item_count'] if playlist.remote else len(playlist_videos(connection, playlist.identifier))


def playlist_selection(connection: sqlite3.Connection, playlist: ResolvedPlaylist, sort: str, limit: int | None = None) -> list[dict]:
    """The sorted, playable videos of either kind of playlist.

    The one selector behind `urls`, `create --from` and `split`, so a `--sort`
    means the same thing wherever it is typed.
    """
    if playlist.local is not None:
        return local_playlist_video_urls(connection, playlist.local, sort, limit)
    return [dict(row) for row in playlist_video_urls(connection, playlist.identifier, sort, limit)]


def split_evenly(items: list, size: int | None = None, parts: int | None = None) -> list[list]:
    """Divide a list into runs of roughly `size`, or into exactly `parts` runs.

    Given a size, the number of runs is chosen so that no run is more than one
    item off the others: 140 videos at a size of 90 becomes two runs of 70, not
    a 90 and a 50. A stub playlist is not what anyone splitting a playlist
    wants, so the remainder is spread one item at a time instead.
    """
    if (size is None) == (parts is None):
        raise ValueError('split needs exactly one of size or parts')
    if not items:
        return []
    if parts is None and size is not None:
        parts = max(1, round(len(items) / size))
    parts = min(parts or 1, len(items))

    run_size, remainder = divmod(len(items), parts)
    runs = []
    start = 0
    for index in range(parts):
        end = start + run_size + (1 if index < remainder else 0)
        runs.append(items[start:end])
        start = end
    return runs


def part_names(prefix: str, count: int) -> list[str]:
    """Name the parts of a split so they list in the order they were cut.

    Zero-padded to the width of the last one, because `X 10` sorts before `X 2`
    otherwise and the listing is the only place these are seen together.
    """
    width = len(str(count))
    return [f'{prefix} {index:0{width}d}' for index in range(1, count + 1)]


def split_playlist(
    connection: sqlite3.Connection,
    playlist: ResolvedPlaylist,
    prefix: str,
    size: int | None = None,
    parts: int | None = None,
    sort: str = 'position',
    overwrite: bool = False,
) -> list[LocalPlaylist]:
    """Cut a playlist into several local ones.

    Every part is checked for a collision before any is written, so a split that
    would half-overwrite an earlier one fails having changed nothing.
    """
    video_ids = [row['video_id'] for row in playlist_selection(connection, playlist, sort)]
    runs = split_evenly(video_ids, size=size, parts=parts)
    names = part_names(prefix, len(runs))
    if not overwrite:
        for name in names:
            path = local.path_for(name)
            if path.exists():
                raise local.LocalPlaylistExistsError(path)
    return [
        create_local_playlist(connection, name, run, source=f'{playlist.kind} {playlist.identifier}', overwrite=True)
        for name, run in zip(names, runs, strict=True)
    ]


def order_local_playlist(
    connection: sqlite3.Connection,
    playlist: LocalPlaylist,
    sort: str,
    into: str | None = None,
    overwrite: bool = False,
) -> LocalPlaylist:
    """Reorder a local playlist, in place or into a new one.

    Unavailable videos are kept rather than dropped: this rearranges a playlist
    rather than selecting from one, and silently losing entries to a reorder is
    not what the word means.
    """
    rows = sort_local_rows(local_playlist_videos(connection, playlist), sort)
    entries = [playlist.entries[row['position'] - 1] for row in rows]
    if into is None:
        playlist.entries = entries
        local.save(playlist, overwrite=True)
        return playlist
    ordered = LocalPlaylist(
        name=into,
        path=local.path_for(into),
        entries=entries,
        created_ts=now_ts(),
        source=f'{LOCAL} {playlist.slug}',
    )
    local.save(ordered, overwrite=overwrite)
    return ordered


def known_playlists(connection: sqlite3.Connection, kind: str | None = None) -> list[ResolvedPlaylist]:
    """Every playlist either store holds, mirrored ones first."""
    found = []
    if kind in (None, REMOTE):
        found += [
            ResolvedPlaylist(kind=REMOTE, title=row['title'], identifier=row['playlist_id'], remote=row)
            for row in connection.execute('SELECT * FROM playlists ORDER BY title')
        ]
    if kind in (None, LOCAL):
        found += [
            ResolvedPlaylist(kind=LOCAL, title=playlist.name, identifier=playlist.slug, local=playlist)
            for playlist in local.list_playlists().playlists
        ]
    return found


def resolve_playlist(connection: sqlite3.Connection, name: str, kind: str | None = None) -> ResolvedPlaylist:
    """Find a playlist by identifier, exact title, or unambiguous partial title.

    Both stores are searched, because a name typed at the prompt does not say
    which one it means and requiring a flag to disambiguate the common case
    would be the wrong tax. Ambiguity — including a local playlist sharing a
    mirrored one's title — is an error listing the candidates rather than a
    guess, since picking one silently is how the wrong playlist gets rewritten.

    An exact title beats a partial match of the same string, so a playlist
    called `Deep` stays reachable once `Deep Night` exists.
    """
    candidates = known_playlists(connection, kind)
    for candidate in candidates:
        if candidate.identifier == name:
            return candidate

    lowered = name.lower()
    matches = [candidate for candidate in candidates if candidate.title.lower() == lowered]
    if not matches:
        matches = [candidate for candidate in candidates if lowered in candidate.title.lower()]
    if not matches:
        raise PlaylistNotFoundError(name)
    if len(matches) > 1:
        raise AmbiguousPlaylistError(name, matches)
    return matches[0]


def playlist_videos(connection: sqlite3.Connection, playlist_id: str, limit: int | None = None) -> list[sqlite3.Row]:
    query = """
        SELECT pv.position, v.video_id, v.title, v.channel, v.duration_seconds,
               v.is_unavailable, v.enriched_ts, v.upload_date,
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
    'longest': 'v.duration_seconds DESC NULLS LAST, pv.position ASC',
    'shortest': 'v.duration_seconds ASC NULLS LAST, pv.position ASC',
    'title': 'v.title COLLATE NOCASE ASC, pv.position ASC',
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


def track_at(connection: sqlite3.Connection, video_id: str, position_seconds: int) -> sqlite3.Row | None:
    """The track playing at this offset into a video.

    This is the whole point of storing chapter timestamps: a two-hour mix can
    report the track rather than the video title. Tracks with no start time —
    a tracklist parsed out of prose — cannot be placed and are skipped.

    `end_seconds` is NULL on the last track of a description-derived tracklist,
    which runs to the end of the video, so a missing end matches rather than
    excludes.
    """
    return connection.execute(
        """
        SELECT position, start_seconds, end_seconds, artist, title, source
        FROM tracks
        WHERE video_id = ?
          AND start_seconds IS NOT NULL
          AND start_seconds <= ?
          AND (end_seconds IS NULL OR end_seconds > ?)
        ORDER BY start_seconds DESC
        LIMIT 1
        """,
        (video_id, position_seconds, position_seconds),
    ).fetchone()


def get_video(connection: sqlite3.Connection, video_id: str) -> sqlite3.Row | None:
    return connection.execute('SELECT * FROM videos WHERE video_id = ?', (video_id,)).fetchone()
