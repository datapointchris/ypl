"""The mpv IPC boundary, against a socket that answers like mpv does.

A fake server rather than a mocked socket, because the things that break here
are protocol-shaped — unsolicited event lines arriving before the reply, replies
coming back out of order, a property mpv declines to answer — and a mock would
be written to the same assumptions as the code it is checking.
"""

import contextlib
import json
import shutil
import socket
import tempfile
import threading
from pathlib import Path

import pytest

from ypl import player

EVENT_LINE = b'{"event":"playback-restart"}\n'


@pytest.fixture
def socket_dir():
    """A short path: a unix socket address is capped near 104 bytes.

    pytest's own tmp_path is nested deep enough under macOS's private temp
    directory to come close to that, and the failure is an opaque OSError.
    """
    directory = tempfile.mkdtemp(dir='/tmp')  # noqa: S108
    yield Path(directory)
    shutil.rmtree(directory, ignore_errors=True)


@pytest.fixture
def fake_mpv(socket_dir):
    """Serve one connection the way mpv's JSON IPC does."""
    servers = []

    def start(answers: dict, reply_in_reverse: bool = False) -> Path:
        socket_path = socket_dir / 'mpv.sock'
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(socket_path))
        server.listen(1)
        servers.append(server)
        threading.Thread(target=serve, args=(server, answers, reply_in_reverse), daemon=True).start()
        return socket_path

    yield start
    for server in servers:
        server.close()


def serve(server, answers, reply_in_reverse):
    """Answer whatever arrives, until the client hangs up.

    Driven by what is received rather than by an expected count, so a request
    for a property the fake has no answer for still gets its reply.
    """
    connection, _ = server.accept()
    buffer = b''
    # A client is allowed to connect and hang up without saying anything —
    # `clear_stale_socket` does exactly that to find out whether anyone is home.
    with connection, contextlib.suppress(OSError):
        connection.sendall(EVENT_LINE)
        while True:
            received = connection.recv(4096)
            if not received:
                return
            buffer += received
            requests = []
            while b'\n' in buffer:
                line, buffer = buffer.split(b'\n', 1)
                if line.strip():
                    requests.append(json.loads(line))
            replies = [reply_for(request, answers) for request in requests]
            for reply in reversed(replies) if reply_in_reverse else replies:
                connection.sendall(EVENT_LINE + json.dumps(reply).encode() + b'\n')


def reply_for(request, answers):
    name = request['command'][1]
    if name in answers:
        return {'data': answers[name], 'error': 'success', 'request_id': request['request_id']}
    return {'error': 'property unavailable', 'request_id': request['request_id']}


def test_properties_come_back_keyed_by_name(fake_mpv):
    socket_path = fake_mpv({'path': 'https://youtu.be/dQw4w9WgXcQ', 'time-pos': 91.4})
    assert player.properties(socket_path, ['path', 'time-pos']) == {'path': 'https://youtu.be/dQw4w9WgXcQ', 'time-pos': 91.4}


def test_event_lines_are_skipped_rather_than_read_as_the_answer(fake_mpv):
    """mpv pushes events down the same socket, so the first line back is rarely the reply."""
    socket_path = fake_mpv({'path': 'x'})
    assert player.properties(socket_path, ['path']) == {'path': 'x'}


def test_replies_are_matched_by_id_not_by_arrival_order(fake_mpv):
    socket_path = fake_mpv({'path': 'first', 'media-title': 'second'}, reply_in_reverse=True)
    assert player.properties(socket_path, ['path', 'media-title']) == {'path': 'first', 'media-title': 'second'}


def test_a_property_mpv_will_not_answer_is_none_rather_than_a_failure(fake_mpv):
    """No duration yet must not take the rest of the read down with it."""
    socket_path = fake_mpv({'path': 'x'})
    assert player.properties(socket_path, ['path', 'duration']) == {'path': 'x', 'duration': None}


def test_nothing_listening_is_a_distinct_error_from_mpv_being_missing(socket_dir):
    with pytest.raises(player.NotPlayingError):
        player.properties(socket_dir / 'absent.sock', ['path'])


def test_a_socket_file_with_no_mpv_behind_it_is_not_playing(socket_dir):
    """A crash leaves the file behind; connecting to it fails rather than hanging."""
    stale = socket_dir / 'stale.sock'
    stale.touch()
    with pytest.raises(player.NotPlayingError):
        player.properties(stale, ['path'])


def test_a_stale_socket_is_cleared_so_the_next_play_can_start(socket_dir):
    stale = socket_dir / 'stale.sock'
    stale.touch()
    player.clear_stale_socket(stale)
    assert not stale.exists()


def test_a_live_socket_is_never_cleared_out_from_under_a_running_player(fake_mpv):
    socket_path = fake_mpv({'path': 'x'})
    player.clear_stale_socket(socket_path)
    assert socket_path.exists()


def test_a_socket_path_too_long_for_the_kernel_is_caught_before_mpv_swallows_it(socket_dir):
    """mpv logs `Could not create IPC socket` and plays on, so nothing surfaces."""
    assert player.socket_is_addressable(socket_dir / 'mpv.sock')
    assert not player.socket_is_addressable(socket_dir / ('deep/' * 30) / 'mpv.sock')


def test_playing_without_a_socket_leaves_the_ipc_argument_off(monkeypatch, socket_dir):
    commands = []
    monkeypatch.setattr(player, 'binary_path', lambda: 'mpv')
    monkeypatch.setattr(player.subprocess, 'run', lambda command, check: commands.append(command) or FakeCompleted())

    player.play(['https://youtu.be/x'], None)
    assert not any(argument.startswith('--input-ipc-server') for argument in commands[0])


class FakeCompleted:
    returncode = 0


def test_a_missing_mpv_names_the_fix(monkeypatch, socket_dir):
    monkeypatch.setattr(shutil, 'which', lambda name: None)
    with pytest.raises(player.MpvUnavailableError, match='not on PATH'):
        player.play(['https://youtu.be/x'], socket_dir / 'mpv.sock')
