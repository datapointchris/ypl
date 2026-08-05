"""Turning a video's chapters or description into a tracklist.

Pure: no I/O, no clock, no network. Everything here takes text and returns
`Track` objects, which is what makes the full spread of real-world tracklist
formatting affordable to test.

Regex is the right tool *here* specifically because the input is unstructured.
The chapter list itself is structured data and is read as such — yt-dlp hands
over start and end times as numbers and they are used as numbers. Only the
chapter *title*, which is free text a human typed, is parsed.
"""

import re

from ypl.models import Chapter
from ypl.models import Track

SOURCE_CHAPTER = 'chapter'
SOURCE_DESCRIPTION = 'description'

# Hyphen, en dash, em dash and tilde, each surrounded by whitespace. The spaces
# are required: "Jay-Z" and "Tyler, The Creator - IGOR" must split differently,
# and only the padding distinguishes them.
ARTIST_TITLE_SEPARATOR = re.compile(r'\s+[-–—~]\s+')

# A leading track number: "1.", "01)", "1 -", "#3".
LEADING_TRACK_NUMBER = re.compile(r'^\s*#?\d{1,3}\s*[.):]\s*|^\s*#\d{1,3}\s+')

# h:mm:ss or m:ss, optionally bracketed, at the start of a line.
LEADING_TIMESTAMP = re.compile(r'^\s*[\[(]?\s*(?:(\d{1,2}):)?(\d{1,2}):(\d{2})\s*[\])]?\s*[-–—]?\s*')

# The DJ convention for a track nobody has identified. Recording it as an artist
# name would make every unidentified track in the library look like the work of
# one prolific artist called ID, and mix-similarity is computed on shared
# artists, so those would be false matches.
UNKNOWN_ARTIST_NAMES = {'id', '?', 'unknown', 'n/a'}


def parse_timestamp(hours: str | None, minutes: str, seconds: str) -> int:
    return int(hours or 0) * 3600 + int(minutes) * 60 + int(seconds)


def split_artist_and_title(text: str) -> tuple[str | None, str]:
    """Split "Artist - Title" into its parts.

    Splits on the first separator only, so "Bicep - Glue - Extended Mix" keeps
    the remix suffix on the title where it belongs.
    """
    cleaned = LEADING_TRACK_NUMBER.sub('', text).strip()
    parts = ARTIST_TITLE_SEPARATOR.split(cleaned, maxsplit=1)
    if len(parts) != 2:
        return None, cleaned
    artist = parts[0].strip()
    title = parts[1].strip()
    if not artist or not title:
        return None, cleaned
    if artist.lower() in UNKNOWN_ARTIST_NAMES:
        return None, title
    return artist, title


def tracks_from_chapters(chapters: list[Chapter]) -> list[Track]:
    """The high-confidence path: real timestamps, one chapter per track."""
    tracks = []
    for position, chapter in enumerate(chapters, start=1):
        artist, title = split_artist_and_title(chapter.title)
        tracks.append(
            Track(
                position=position,
                artist=artist,
                title=title,
                raw_text=chapter.title,
                source=SOURCE_CHAPTER,
                start_seconds=chapter.start_seconds,
                end_seconds=chapter.end_seconds,
            )
        )
    return tracks


def tracks_from_description(description: str) -> list[Track]:
    """The fallback: timestamped lines in a video description.

    Only lines that open with a timestamp are considered. That is a deliberately
    narrow rule — a description also holds social links, label credits and
    hashtags, and any looser heuristic pulls those in as tracks. A mix whose
    tracklist is written without timestamps yields nothing here and is left for
    the LLM pass, which is the honest outcome rather than a guess.
    """
    tracks: list[Track] = []
    for line in description.splitlines():
        match = LEADING_TIMESTAMP.match(line)
        if not match:
            continue
        remainder = line[match.end() :].strip()
        if not remainder:
            continue
        artist, title = split_artist_and_title(remainder)
        tracks.append(
            Track(
                position=len(tracks) + 1,
                artist=artist,
                title=title,
                raw_text=line.strip(),
                source=SOURCE_DESCRIPTION,
                start_seconds=parse_timestamp(*match.groups()),
            )
        )
    return close_open_ends(tracks)


def close_open_ends(tracks: list[Track]) -> list[Track]:
    """Give each description-derived track the next one's start as its end.

    Chapters arrive with both ends already; a timestamped description line only
    says where a track begins. The last track's end stays None because the
    description cannot know the video's duration.
    """
    for current, following in zip(tracks, tracks[1:], strict=False):
        current.end_seconds = following.start_seconds
    return tracks


def best_tracklist(chapters: list[Chapter], description: str) -> list[Track]:
    """Prefer chapters, fall back to the description, return nothing rather than junk."""
    if chapters:
        return tracks_from_chapters(chapters)
    return tracks_from_description(description)
