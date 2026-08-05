"""Local playlists: the authored files under `$XDG_DATA_HOME/ypl/playlists`.

The directory is the store. There is no table for these and no index — a
playlist is one M3U file, the slug of its name is its filename, and listing the
directory is the query. That is deliberate rather than lazy: the mirror is
state that rebuilds from a free `ypl sync`, while a mood set or an event arc is
a decision with no remote to restore it from. Keeping the two in the same
database would tie the half that must survive to the half that is disposable.

This module owns reading and writing the files. `m3u` owns the format, and
`service` owns resolving a name to one of these — because resolution has to
consider the mirror too, and doing it here would need an import cycle.
"""

from dataclasses import dataclass
from dataclasses import field
from pathlib import Path

from ypl import m3u
from ypl import paths

SUFFIX = '.m3u'


class LocalPlaylistExistsError(FileExistsError):
    def __init__(self, path: Path):
        self.path = path
        super().__init__(f'{path} already exists')


@dataclass
class LocalPlaylist:
    name: str
    path: Path
    entries: list[m3u.Entry] = field(default_factory=list)
    created_ts: str = ''
    source: str = ''
    synced: bool = True
    remote_id: str = ''

    @property
    def slug(self) -> str:
        return self.path.stem

    @property
    def sync_state(self) -> str:
        """Where this playlist sits between "made here" and "on YouTube".

        Three states rather than a flag, because the middle one is real: a
        playlist is synced the moment it is made, but does not exist on YouTube
        until the queue next drains.
        """
        if not self.synced:
            return 'local'
        return 'synced' if self.remote_id else 'pending'

    @property
    def video_ids(self) -> list[str]:
        return [entry.video_id for entry in self.entries]


def slugify(name: str) -> str:
    """Turn a playlist name into a filename stem.

    Non-alphanumerics collapse to single hyphens so `Deep / Sunday Morning!`
    cannot produce a nested path or a leading dot. Letters outside ASCII are
    kept — every filesystem in use here handles them, and mangling them would
    make a playlist unfindable by its own name.
    """
    replaced = ''.join(character.lower() if character.isalnum() else '-' for character in name)
    return '-'.join(part for part in replaced.split('-') if part)


def path_for(name: str) -> Path:
    return paths.playlists_dir() / f'{slugify(name)}{SUFFIX}'


def load(path: Path) -> LocalPlaylist:
    parsed = m3u.parse(path.read_text())
    return LocalPlaylist(
        name=parsed.name or path.stem,
        path=path,
        entries=parsed.entries,
        created_ts=parsed.created_ts,
        source=parsed.source,
        synced=parsed.synced,
        remote_id=parsed.remote_id,
    )


def save(playlist: LocalPlaylist, overwrite: bool = False) -> Path:
    """Write the playlist out, refusing to clobber unless told to.

    Overwriting is the one way authored data is lost here, so it takes an
    explicit argument rather than being the default a caller forgets about.
    """
    if playlist.path.exists() and not overwrite:
        raise LocalPlaylistExistsError(playlist.path)
    playlist.path.parent.mkdir(parents=True, exist_ok=True)
    document = m3u.Playlist(
        name=playlist.name,
        entries=playlist.entries,
        created_ts=playlist.created_ts,
        source=playlist.source,
        synced=playlist.synced,
        remote_id=playlist.remote_id,
    )
    playlist.path.write_text(m3u.render(document))
    return playlist.path


def delete(playlist: LocalPlaylist) -> None:
    playlist.path.unlink()


def paths_on_disk() -> list[Path]:
    directory = paths.playlists_dir()
    if not directory.is_dir():
        return []
    return sorted(path for path in directory.iterdir() if path.suffix == SUFFIX and path.is_file())


@dataclass
class Listing:
    """What reading the whole directory found, including what it could not read.

    Unreadable files are carried alongside rather than raised, because one bad
    hand edit must not make every other playlist unreachable — and must not
    vanish silently either, which is what dropping them would do.
    """

    playlists: list[LocalPlaylist] = field(default_factory=list)
    problems: list[tuple[Path, str]] = field(default_factory=list)


def list_playlists() -> Listing:
    """Every local playlist, ordered by name."""
    listing = Listing()
    for path in paths_on_disk():
        try:
            listing.playlists.append(load(path))
        except (OSError, m3u.M3uError) as error:
            listing.problems.append((path, str(error)))
    listing.playlists.sort(key=lambda playlist: playlist.name.lower())
    return listing
