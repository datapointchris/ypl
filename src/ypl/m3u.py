"""The extended M3U format, parsed and rendered as text.

Format only — this module never touches the filesystem, which is `local`'s job.

M3U rather than a table or a JSON file because the local playlists are the
authored half of ypl, and a plain list of URLs is playable by mpv, VLC and Kodi
with no code at all. That stays true only while the file remains valid M3U,
which is why ypl's own metadata rides on `#YPL-` comment lines: every player
skips a `#` directive it does not recognise, so the extra facts cost nothing.
The alternative — a sidecar JSON next to each playlist — was rejected because
two files describing one playlist can disagree, and hand-editing the M3U is
expected rather than exceptional.

An entry is a video and only a video. A DJ mix holding forty tracks is still one
entry, because a YouTube video is the smallest thing that can actually be
played; the tracklist is metadata about the entry and lives in the mirror.
"""

import urllib.parse
from dataclasses import dataclass
from dataclasses import field

from ypl.models import watch_url

HEADER = '#EXTM3U'
NAME_DIRECTIVE = '#PLAYLIST:'
INFO_DIRECTIVE = '#EXTINF:'
CREATED_DIRECTIVE = '#YPL-CREATED:'
SOURCE_DIRECTIVE = '#YPL-SOURCE:'

WATCH_HOSTS = {'youtube.com', 'www.youtube.com', 'm.youtube.com', 'music.youtube.com'}
SHORT_HOSTS = {'youtu.be'}
# Every path shape YouTube serves a single video under. `watch?v=` carries the
# id in the query instead and is handled separately.
PATH_PREFIXES = ('/shorts/', '/embed/', '/v/', '/live/')

# Ids have been eleven base64url characters for the whole life of the site. The
# length is only used to tell a bare id apart from a typo on the command line —
# an id arriving inside a URL is taken as given, so a future change to the
# format costs nothing there.
ID_LENGTH = 11
ID_ALPHABET = frozenset('abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_')


class M3uError(ValueError):
    """A line in a playlist file cannot be read.

    Carries the line number because these files are hand-edited, and "line 12"
    is the difference between a fix and a hunt.
    """

    def __init__(self, line_number: int, line: str, reason: str):
        self.line_number = line_number
        self.line = line
        self.reason = reason
        super().__init__(f'line {line_number}: {reason}')


@dataclass
class Entry:
    """One video in a local playlist.

    `title` is display text for whatever ends up playing the file, not a field
    anything parses back apart: the mirror is authoritative for a video's real
    title and channel. It exists so the file still says something useful when
    read by a player, or by a person, with no database around.
    """

    video_id: str
    title: str = ''
    duration_seconds: int | None = None

    @property
    def url(self) -> str:
        return watch_url(self.video_id)


@dataclass
class Playlist:
    name: str = ''
    entries: list[Entry] = field(default_factory=list)
    created_ts: str = ''
    source: str = ''


def is_video_id(text: str) -> bool:
    return len(text) == ID_LENGTH and set(text) <= ID_ALPHABET


def first_path_segment(path: str) -> str:
    return path.lstrip('/').split('/')[0]


def video_id_from(text: str) -> str | None:
    """Pull a video id out of a URL or accept a bare one, else None.

    Parsed structurally rather than by pattern match, so a URL carrying extra
    query parameters — `&list=`, `&t=`, the tracking ones YouTube's share button
    appends — yields the same id as the bare link.
    """
    candidate = text.strip()
    if not candidate:
        return None
    if '/' not in candidate:
        return candidate if is_video_id(candidate) else None

    parsed = urllib.parse.urlparse(candidate if '//' in candidate else f'//{candidate}')
    host = parsed.netloc.lower()
    if host in SHORT_HOSTS:
        return first_path_segment(parsed.path) or None
    if host not in WATCH_HOSTS:
        return None
    from_query = urllib.parse.parse_qs(parsed.query).get('v')
    if from_query:
        return from_query[0]
    for prefix in PATH_PREFIXES:
        if parsed.path.startswith(prefix):
            return first_path_segment(parsed.path[len(prefix) :]) or None
    return None


def parse_info(value: str) -> tuple[int | None, str]:
    """Split `#EXTINF:` into its duration and its display text.

    A duration of -1 is the format's way of saying unknown, and a missing comma
    is tolerated because a hand-written file often has one.
    """
    duration_text, _, title = value.partition(',')
    try:
        seconds = int(float(duration_text))
    except ValueError:
        return None, title.strip()
    return (seconds if seconds >= 0 else None), title.strip()


def parse(text: str) -> Playlist:
    """Read a playlist file's contents.

    Unknown `#` directives are skipped rather than rejected: other tools write
    into M3U files too, and refusing to open a playlist because VLC left a
    directive behind would be the wrong trade for a format chosen precisely so
    other tools could read it.
    """
    playlist = Playlist()
    pending_duration: int | None = None
    pending_title = ''
    for line_number, raw_line in enumerate(text.splitlines(), start=1):
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith('#'):
            if line.startswith(NAME_DIRECTIVE):
                playlist.name = line[len(NAME_DIRECTIVE) :].strip()
            elif line.startswith(CREATED_DIRECTIVE):
                playlist.created_ts = line[len(CREATED_DIRECTIVE) :].strip()
            elif line.startswith(SOURCE_DIRECTIVE):
                playlist.source = line[len(SOURCE_DIRECTIVE) :].strip()
            elif line.startswith(INFO_DIRECTIVE):
                pending_duration, pending_title = parse_info(line[len(INFO_DIRECTIVE) :])
            continue
        video_id = video_id_from(line)
        if not video_id:
            raise M3uError(line_number, line, f'{line!r} is not a YouTube video URL or id')
        playlist.entries.append(Entry(video_id=video_id, title=pending_title, duration_seconds=pending_duration))
        pending_duration = None
        pending_title = ''
    return playlist


def render(playlist: Playlist) -> str:
    """Write the playlist back out, header directives first."""
    lines = [HEADER]
    if playlist.name:
        lines.append(f'{NAME_DIRECTIVE}{playlist.name}')
    if playlist.created_ts:
        lines.append(f'{CREATED_DIRECTIVE}{playlist.created_ts}')
    if playlist.source:
        lines.append(f'{SOURCE_DIRECTIVE}{playlist.source}')
    for entry in playlist.entries:
        lines.append('')
        duration = entry.duration_seconds if entry.duration_seconds is not None else -1
        lines.append(f'{INFO_DIRECTIVE}{duration},{entry.title}')
        lines.append(entry.url)
    return '\n'.join(lines) + '\n'
