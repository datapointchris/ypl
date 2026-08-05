"""The shapes that cross between yt-dlp, the parsers, and the mirror."""

from dataclasses import dataclass
from dataclasses import field


def watch_url(video_id: str) -> str:
    """The canonical URL for a video.

    One definition because it is what local playlist files contain, what gets
    piped to other tools, and what yt-dlp is handed — three places that have to
    agree on the same string.
    """
    return f'https://www.youtube.com/watch?v={video_id}'


@dataclass
class RemoteVideo:
    """One video as YouTube describes it.

    `description` and `chapters` are only populated by a full extraction —
    a flat playlist listing leaves them empty, which is exactly the difference
    between `ypl sync` and `ypl enrich`.
    """

    video_id: str
    title: str
    channel: str = ''
    duration_seconds: int | None = None
    description: str = ''
    upload_date: str | None = None
    is_unavailable: bool = False
    chapters: list['Chapter'] = field(default_factory=list)

    @property
    def url(self) -> str:
        return watch_url(self.video_id)


@dataclass
class Chapter:
    start_seconds: int
    end_seconds: int | None
    title: str


@dataclass
class RemotePlaylist:
    playlist_id: str
    title: str
    description: str = ''
    channel: str = ''
    videos: list[RemoteVideo] = field(default_factory=list)

    @property
    def url(self) -> str:
        return f'https://www.youtube.com/playlist?list={self.playlist_id}'


@dataclass
class Track:
    """One track inside a video.

    `artist` is None when the source text carried no separable artist, which is
    a different fact from an empty artist name and is stored as such.
    """

    position: int
    title: str
    raw_text: str
    source: str
    artist: str | None = None
    start_seconds: int | None = None
    end_seconds: int | None = None
