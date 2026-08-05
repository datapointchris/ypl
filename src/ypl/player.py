"""Playback, via the mpv binary.

Shelled out to rather than embedded, for the same reason as yt-dlp: mpv is the
thing that already knows how to stream YouTube, and it tracks YouTube's changes
on its own release schedule.

Every playback opens mpv's JSON IPC socket, which is what makes `ypl now` able
to say which track of a two-hour mix is playing. It costs one argument and
nothing when unused.
"""

import json
import shutil
import socket
import subprocess
from pathlib import Path

BINARY = 'mpv'
CONNECT_TIMEOUT_SECONDS = 2.0

# A unix socket address is a fixed-size field in the kernel: 104 bytes on macOS
# and the BSDs, 108 on Linux. Over it, mpv logs `Could not create IPC socket`
# and carries on playing, so playback works and `ypl now` silently reports that
# nothing is on. Checked against the smaller of the two, since the same
# $XDG_STATE_HOME can be shared between machines.
SOCKET_PATH_LIMIT = 104


class MpvUnavailableError(RuntimeError):
    pass


class NotPlayingError(RuntimeError):
    """No mpv is listening on the socket.

    Distinct from mpv being missing: the tool is installed and nothing is
    playing, which is an ordinary answer rather than a broken setup.
    """


def socket_is_addressable(socket_path: Path) -> bool:
    """Whether mpv can actually open a socket at this path."""
    return len(str(socket_path).encode()) < SOCKET_PATH_LIMIT


def binary_path() -> str:
    found = shutil.which(BINARY)
    if not found:
        raise MpvUnavailableError(f'{BINARY} is not on PATH — install it to play playlists')
    return found


def is_listening(socket_path: Path) -> bool:
    if not socket_path.exists():
        return False
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(CONNECT_TIMEOUT_SECONDS)
    try:
        connection.connect(str(socket_path))
    except OSError:
        return False
    else:
        return True
    finally:
        connection.close()


def clear_stale_socket(socket_path: Path) -> None:
    """Remove a socket left behind by an mpv that did not exit cleanly.

    mpv refuses to start when the path is taken, so a crash would otherwise
    make every later `ypl play` fail. Only removed once nothing answers on it,
    so a running player is never cut off.
    """
    if socket_path.exists() and not is_listening(socket_path):
        socket_path.unlink()


def play(urls: list[str], socket_path: Path | None, extra_arguments: list[str] | None = None) -> int:
    """Hand the URLs to mpv and wait for it, returning mpv's exit code.

    Passed as arguments rather than as a playlist file so that `--sort` and
    `--limit` mean the same thing here as everywhere else, and so a mirrored
    playlist plays without writing a file first.

    A socket_path of None plays without the IPC socket, which costs only
    `ypl now`.
    """
    socket_arguments = []
    if socket_path is not None:
        socket_path.parent.mkdir(parents=True, exist_ok=True)
        clear_stale_socket(socket_path)
        socket_arguments = [f'--input-ipc-server={socket_path}']
    command = [binary_path(), *socket_arguments, *(extra_arguments or []), *urls]
    return subprocess.run(command, check=False).returncode


def properties(socket_path: Path, names: list[str]) -> dict[str, object]:
    """Read several of mpv's properties over one connection.

    Batched because `ypl now` is the kind of command a status bar runs on a
    timer, and one connection per property would be three or four for every
    tick. A property mpv will not answer for — no chapter in this video, no
    position yet — comes back as None rather than failing the whole read, so
    the optional ones stay optional.

    Replies are matched by `request_id`: mpv pushes unsolicited event lines
    down the same socket, so the first line back is often not the answer.
    """
    connection = connect(socket_path)
    try:
        pending = {index: name for index, name in enumerate(names, start=1)}
        requests = [json.dumps({'command': ['get_property', name], 'request_id': request_id}) for request_id, name in pending.items()]
        connection.sendall(('\n'.join(requests) + '\n').encode())
        found: dict[str, object] = dict.fromkeys(names)
        for line in read_lines(connection):
            payload = json.loads(line)
            name = pending.pop(payload.get('request_id'), '')
            if not name:
                continue
            if payload.get('error') == 'success':
                found[name] = payload.get('data')
            if not pending:
                return found
    except (OSError, json.JSONDecodeError) as error:
        raise NotPlayingError(f'lost the connection to {socket_path}') from error
    finally:
        connection.close()
    raise NotPlayingError(f'{socket_path} closed without answering')


def command(socket_path: Path, arguments: list[str]) -> None:
    """Tell mpv to do something, and wait long enough to know it did.

    The reply is read rather than fired and forgotten, because the caller acts
    on whether it worked: dropping the playing track sends `playlist-next`, and
    a drop that silently failed to skip leaves you listening to a video you just
    deleted.
    """
    connection = connect(socket_path)
    try:
        connection.sendall((json.dumps({'command': arguments, 'request_id': 1}) + '\n').encode())
        for line in read_lines(connection):
            payload = json.loads(line)
            if payload.get('request_id') != 1:
                continue
            if payload.get('error') != 'success':
                raise NotPlayingError(f'mpv refused {arguments[0]}: {payload.get("error")}')
            return
    except (OSError, json.JSONDecodeError) as error:
        raise NotPlayingError(f'lost the connection to {socket_path}') from error
    finally:
        connection.close()
    raise NotPlayingError(f'{socket_path} closed without answering')


def connect(socket_path: Path) -> socket.socket:
    if not socket_path.exists():
        raise NotPlayingError(f'nothing is listening on {socket_path}')
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(CONNECT_TIMEOUT_SECONDS)
    try:
        connection.connect(str(socket_path))
    except OSError as error:
        connection.close()
        raise NotPlayingError(f'nothing is listening on {socket_path}') from error
    return connection


def read_lines(connection: socket.socket):
    """Yield complete lines from the socket until it goes quiet."""
    buffer = b''
    while True:
        received = connection.recv(4096)
        if not received:
            return
        buffer += received
        while b'\n' in buffer:
            line, buffer = buffer.split(b'\n', 1)
            if line.strip():
                yield line
