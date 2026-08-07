"""The machine contract: exit codes, and which stream carries what.

`forge` and anything else shelling out sees only these two signals, so they are
the API rather than a nicety.
"""

import dataclasses
import datetime as dt
import json
import os
import shutil
import tempfile
from pathlib import Path

import pytest
from typer.testing import CliRunner

from ypl import basestore
from ypl import config
from ypl import db
from ypl import local
from ypl import main
from ypl import paths
from ypl import player
from ypl import remote
from ypl import runlock
from ypl import schedule
from ypl import service
from ypl import session
from ypl import synclog
from ypl import throttle
from ypl import youtubei
from ypl import ytdlp
from ypl.main import app
from ypl.models import Chapter
from ypl.models import PlaylistRef
from ypl.models import RemotePlaylist
from ypl.models import RemoteVideo
from ypl.remote import RemoteItem

runner = CliRunner()


@pytest.fixture(autouse=True)
def isolated_home(tmp_path, monkeypatch):
    """Keep the tests off the real mirror, config and playlist directory.

    HOME as well as the three XDG variables, because not everything this tool
    writes has an XDG home to redirect: a launch agent lives at a fixed path
    under `~/Library` on macOS, and a suite that overrode only the XDG three
    wrote one onto whichever machine ran it.
    """
    home = tmp_path / 'home'
    home.mkdir()
    monkeypatch.setenv('HOME', str(home))
    monkeypatch.setenv('XDG_STATE_HOME', str(tmp_path / 'state'))
    monkeypatch.setenv('XDG_CONFIG_HOME', str(tmp_path / 'config'))
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


@pytest.fixture(autouse=True)
def no_real_service_manager(monkeypatch):
    """launchctl and systemctl answer to the session running the suite.

    An isolated HOME keeps the written unit out of the way, but loading it does
    not go through the filesystem: `launchctl load` on a plist under a temporary
    directory still registers the job with the real launchd. Every test that
    invokes `sync` installs a timer, so this has to be autouse rather than
    something a timer test opts into — the tests that left one running on this
    machine, firing every thirty minutes against a checkout, were about
    adoption and had never heard of the scheduler.
    """
    monkeypatch.setattr(schedule, 'run_manager', lambda arguments: (True, ''))


@pytest.fixture(autouse=True)
def no_waiting(monkeypatch):
    """Pacing is real behaviour, tested directly in test_throttle.

    Left in here, every command that makes more than one request would sit out
    its own interval and the suite would spend minutes asleep.
    """
    monkeypatch.setattr(throttle.time, 'sleep', lambda seconds: None)


@pytest.fixture(autouse=True)
def no_update_check(monkeypatch):
    """The daily update notice would hit the network on every command."""
    monkeypatch.setattr('ypl.main.notify', lambda config: None)


@pytest.fixture
def synced(monkeypatch):
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda *args, **kwargs: RemotePlaylist(
            playlist_id='PL1',
            title='Get Insights',
            videos=[RemoteVideo(video_id='vid1', title='A Talk', channel='Someone', duration_seconds=600)],
        ),
    )
    runner.invoke(app, ['sync', 'https://example.invalid/PL1'])


def test_bare_invocation_answers_rather_than_printing_a_catalogue():
    """A deliberate departure from the fleet's no-args-shows-help rule.

    `~/dev/standards/cli-design.md` says bare always shows help, and argues it
    structurally: a tool that does work bare cannot gain a command later
    without silently changing what bare means. The standard allows an override
    for a tool whose identity is one read-only action, which this now is — the
    glance takes no options, writes nothing, and `--help` still answers the
    question it used to.

    What made it worth the departure: a bare `ypl` answered with thirty-nine
    commands in six panels, which is the answer to "what can this do" rather
    than to the question anyone types a bare command to ask.

    Exit 0, not 2 — it ran and answered.
    """
    result = runner.invoke(app, [])
    assert result.exit_code == 0
    assert 'Usage:' not in result.output
    assert 'ypl auth --browser safari' in result.output


def test_version_is_one_line_naming_the_tool_and_exits_clean():
    """The question every CLI here answers the same way, so a script can ask it."""
    result = runner.invoke(app, ['--version'])
    assert result.exit_code == 0
    assert result.stdout.startswith('ypl ')
    assert len(result.stdout.strip().splitlines()) == 1


@pytest.mark.parametrize('command', [['playlists'], ['videos'], ['config'], ['remote'], ['plays']])
def test_every_namespace_shows_help_when_given_no_verb(command):
    """The tree is walkable one token at a time — no hangs, no cryptic errors."""
    result = runner.invoke(app, command)
    assert result.exit_code == 2
    assert 'Usage:' in result.output


def test_a_bad_sort_is_a_usage_error_not_a_failure(synced):
    result = runner.invoke(app, ['playlists', 'urls', 'Get Insights', '--sort', 'sideways'])
    assert result.exit_code == 2


def test_an_unknown_playlist_is_a_failure_not_a_usage_error(synced):
    result = runner.invoke(app, ['playlists', 'show', 'nonexistent'])
    assert result.exit_code == 1


def test_json_output_is_parseable_with_nothing_else_on_stdout(synced):
    result = runner.invoke(app, ['playlists', 'list', '--json'])
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload[0]['title'] == 'Get Insights'


def test_an_empty_mirror_lists_nothing_and_still_succeeds():
    result = runner.invoke(app, ['playlists', 'list', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout) == []


def test_config_show_shows_every_setting_there_is():
    """It claimed "including defaults" and printed two of the seven, so
    `background_sync` — the one the README tells you to set — could not be
    confirmed from the command that exists to show what is in effect."""
    shown = json.loads(runner.invoke(app, ['config', 'show', '--json']).stdout)

    assert set(shown) == {field.name for field in dataclasses.fields(config.Config)}


def test_a_setting_is_shown_as_it_would_be_written():
    """So it can be copied back into the TOML rather than translated."""
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('background_sync = false\n')

    lines = dict(line.split(maxsplit=1) for line in runner.invoke(app, ['config', 'show']).output.splitlines())
    assert lines['background_sync'].strip() == 'false'
    assert lines['cookies_from_browser'].strip() == '(unset)'
    assert lines['mpv_arguments'].strip() == '(none)'


def test_unparseable_config_is_a_usage_error_naming_the_file():
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('this is not = = toml')

    result = runner.invoke(app, ['config', 'show'])
    assert result.exit_code == 2
    assert 'config.toml' in result.output


def test_a_narrow_terminal_does_not_break_a_path_across_lines(monkeypatch):
    """Rich's default hard wrap inserted a newline mid-filename.

    A path printed as `.../ypl/config\\n.toml` reads fine and pastes broken,
    which is the whole point of printing it. Pinned at a width narrower than any
    real path, because CI is where this surfaced and CI has no terminal.
    """
    monkeypatch.setenv('COLUMNS', '40')
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('nope = = nope')

    result = runner.invoke(app, ['config', 'show'])
    assert result.exit_code == 2
    assert str(paths.config_file()) in result.output


def test_a_nonsense_batch_size_is_rejected_rather_than_used():
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('enrich_batch_size = 0')

    result = runner.invoke(app, ['enrich'])
    assert result.exit_code == 2


def test_showing_an_unmirrored_video_fails_rather_than_printing_an_empty_table():
    result = runner.invoke(app, ['videos', 'show', 'nope'])
    assert result.exit_code == 1


def test_resolve_is_case_insensitive_from_the_cli(synced):
    result = runner.invoke(app, ['playlists', 'show', 'get insights', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)[0]['video_id'] == 'vid1'


def test_every_sort_the_cli_accepts_is_implemented_for_both_kinds_of_playlist():
    """The SQL and the Python sorts have to stay in step.

    A name in the CLI's vocabulary with no local implementation is a KeyError
    the moment someone points it at a local playlist.
    """
    assert set(service.SORT_CLAUSES) == set(service.LOCAL_SORT_KEYS) | {'position', 'random'}


def test_the_library_invents_no_sort_names_of_its_own():
    """One vocabulary, minus the name that means nothing across playlists.

    A video's position is a fact about a slot in one playlist, and the library
    holds videos sitting in several.
    """
    assert set(service.LIBRARY_SORT_CLAUSES) == set(service.SORT_CLAUSES) - {'position'}


@pytest.fixture
def built(synced):
    """A local playlist copied out of the mirrored one."""
    result = runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Get Insights'])
    assert result.exit_code == 0
    return result


def test_creating_a_local_playlist_writes_a_file_that_a_player_can_read(built):
    contents = (paths.playlists_dir() / 'sunday.m3u').read_text()
    assert contents.startswith('#EXTM3U')
    assert 'https://www.youtube.com/watch?v=vid1' in contents


def test_a_created_playlist_records_where_it_came_from(built):
    assert '#YPL-SOURCE:remote PL1' in (paths.playlists_dir() / 'sunday.m3u').read_text()


def test_a_playlist_made_here_is_named_in_kebab_case(synced):
    """What this tool assembles is identifiable as such, on the phone as well as here."""
    payload = json.loads(runner.invoke(app, ['playlists', 'create', 'Six Hour Work', '--from', 'Get Insights', '--json']).stdout)

    assert (payload['name'], payload['slug']) == ('six-hour-work', 'six-hour-work')
    assert '#PLAYLIST:six-hour-work' in (paths.playlists_dir() / 'six-hour-work.m3u').read_text()


def test_a_name_that_came_from_youtube_is_left_exactly_as_it_is(synced):
    """The other half of the rule. Re-casing a playlist made in the web player
    would rewrite someone's own name for it on their own account."""
    listed = json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)

    assert [row['title'] for row in listed] == ['Get Insights']


@pytest.mark.parametrize('typed', ['six hour work', 'Six Hour Work', 'six-hour-work', 'hour work'])
def test_a_kebab_case_name_is_still_reachable_by_typing_it_any_way(synced, typed):
    runner.invoke(app, ['playlists', 'create', 'Six Hour Work', '--from', 'Get Insights'])

    result = runner.invoke(app, ['playlists', 'show', typed, '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)[0]['video_id'] == 'vid1'


def test_both_kinds_of_playlist_are_listed_together(built):
    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)
    assert {row['title']: row['kind'] for row in payload} == {'Get Insights': 'remote', 'sunday': 'local'}


def test_a_new_playlist_is_synced_by_default_and_waiting_to_go_up(built):
    """Made here, meant for the phone — pending until the queue drains."""
    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json', '--source', 'local']).stdout)
    assert payload[0]['sync_state'] == 'pending'
    assert payload[0]['synced'] is True
    assert '#YPL-SYNCED:yes' in (paths.playlists_dir() / 'sunday.m3u').read_text()


def test_a_playlist_can_be_kept_off_youtube_at_creation(synced):
    runner.invoke(app, ['playlists', 'create', 'Scratch', '--from', 'Get Insights', '--local'])
    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json', '--source', 'local']).stdout)
    assert payload[0]['sync_state'] == 'local'
    assert '#YPL-SYNCED:no' in (paths.playlists_dir() / 'scratch.m3u').read_text()


def test_the_sync_state_survives_a_reorder_and_a_rename_of_its_contents(built):
    """Every write goes through the same save, so the directives must round trip."""
    runner.invoke(app, ['playlists', 'add', 'Sunday', '-4FPIL6e4SQ'])
    runner.invoke(app, ['playlists', 'order', 'Sunday', '--sort', 'title'])

    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json', '--source', 'local']).stdout)
    assert payload[0]['sync_state'] == 'pending'


@pytest.mark.parametrize(('source', 'expected'), [('local', ['sunday']), ('remote', ['Get Insights'])])
def test_the_listing_can_be_narrowed_to_one_kind(built, source, expected):
    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json', '--source', source]).stdout)
    assert [row['title'] for row in payload] == expected


def test_an_unknown_source_is_a_usage_error(built):
    assert runner.invoke(app, ['playlists', 'list', '--source', 'sideways']).exit_code == 2


def test_a_local_playlist_shows_the_mirror_metadata_for_its_videos(built):
    payload = json.loads(runner.invoke(app, ['playlists', 'show', 'Sunday', '--json']).stdout)
    assert payload[0]['title'] == 'A Talk'
    assert payload[0]['in_mirror'] is True


def test_creating_refuses_to_clobber_without_force(built):
    assert runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Get Insights']).exit_code == 1
    assert runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Get Insights', '--force']).exit_code == 0


def test_a_playlist_can_be_built_from_piped_urls(synced):
    """The other half of `playlists urls` — a selection becomes a playlist."""
    result = runner.invoke(app, ['playlists', 'create', 'Piped', '--json'], input='https://youtu.be/vid1\n')
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_count'] == 1


def test_a_piped_line_that_is_not_a_video_is_a_usage_error(synced):
    result = runner.invoke(app, ['playlists', 'create', 'Piped'], input='https://example.invalid/nope\n')
    assert result.exit_code == 2


def test_videos_can_be_added_and_removed_by_url_or_bare_id(built):
    added = runner.invoke(app, ['playlists', 'add', 'Sunday', 'https://youtu.be/-4FPIL6e4SQ', '--json'])
    assert json.loads(added.stdout)['video_count'] == 2

    removed = runner.invoke(app, ['playlists', 'remove', 'Sunday', '-4FPIL6e4SQ', '--json'])
    assert json.loads(removed.stdout)['removed'] == 1
    assert json.loads(removed.stdout)['video_count'] == 1


def test_removing_something_that_is_not_there_is_not_a_failure(built):
    result = runner.invoke(app, ['playlists', 'remove', 'Sunday', '-4FPIL6e4SQ', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['removed'] == 0


def test_editing_a_mirrored_playlist_says_so_rather_than_claiming_it_is_missing(synced):
    """`not found` would be a lie about a playlist that is plainly listed."""
    result = runner.invoke(app, ['playlists', 'add', 'Get Insights', 'vid1'])
    assert result.exit_code == 1
    assert 'mirrored' in result.output


def test_a_local_playlist_named_after_its_source_is_still_editable(synced):
    """Copying `Deep Night` to a local `Deep Night` must not make the name unusable."""
    assert runner.invoke(app, ['playlists', 'create', 'Get Insights', '--from', 'PL1']).exit_code == 0

    result = runner.invoke(app, ['playlists', 'add', 'Get Insights', '-4FPIL6e4SQ', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_count'] == 2


def test_a_name_matching_both_stores_is_ambiguous_for_reads(synced):
    """Reads span both stores, so the same title in each has to be an error."""
    assert runner.invoke(app, ['playlists', 'create', 'Get Insights', '--from', 'PL1']).exit_code == 0

    result = runner.invoke(app, ['playlists', 'show', 'Get Insights'])
    assert result.exit_code == 2
    assert 'remote PL1' in result.output


def test_deleting_a_local_playlist_asks_first(built):
    assert runner.invoke(app, ['playlists', 'delete', 'Sunday'], input='n\n').exit_code == 1
    assert (paths.playlists_dir() / 'sunday.m3u').exists()

    assert runner.invoke(app, ['playlists', 'delete', 'Sunday', '--yes']).exit_code == 0
    assert not (paths.playlists_dir() / 'sunday.m3u').exists()


def test_an_unreadable_playlist_file_is_named_without_breaking_the_listing(synced):
    paths.playlists_dir().mkdir(parents=True)
    (paths.playlists_dir() / 'broken.m3u').write_text('#EXTM3U\n/not/a/video.mp3\n')

    result = runner.invoke(app, ['playlists', 'list'])
    assert result.exit_code == 0
    assert 'broken.m3u' in result.output
    assert 'Get Insights' in result.output


@pytest.fixture
def many(monkeypatch):
    """A mirrored playlist of ten videos, newest first, alternating lengths."""
    videos = [
        RemoteVideo(video_id=f'vid{index:02d}', title=f'Mix {index:02d}', channel='Cercle', duration_seconds=1000 + index)
        for index in range(1, 11)
    ]
    monkeypatch.setattr(ytdlp, 'fetch_playlist', lambda *args, **kwargs: RemotePlaylist(playlist_id='PL9', title='Long One', videos=videos))
    runner.invoke(app, ['sync', 'https://example.invalid/PL9'])


def test_a_split_writes_one_playlist_per_part(many):
    payload = json.loads(runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '3', '--json']).stdout)
    assert [part['video_count'] for part in payload] == [4, 3, 3]
    assert [part['slug'] for part in payload] == ['long-one-1', 'long-one-2', 'long-one-3']


def test_a_split_by_size_names_its_parts_after_the_source(many):
    payload = json.loads(runner.invoke(app, ['playlists', 'split', 'Long One', '--size', '5', '--json']).stdout)
    assert [part['name'] for part in payload] == ['long-one-1', 'long-one-2']


def test_a_split_can_be_named_something_other_than_its_source(many):
    payload = json.loads(runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '2', '--name', 'Sunday', '--json']).stdout)
    assert [part['slug'] for part in payload] == ['sunday-1', 'sunday-2']


def test_a_split_keeps_every_video_exactly_once(many):
    runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '3'])
    split = [
        row['video_id']
        for slug in ['Long One 1', 'Long One 2', 'Long One 3']
        for row in json.loads(runner.invoke(app, ['playlists', 'show', slug, '--json']).stdout)
    ]
    assert sorted(split) == [f'vid{index:02d}' for index in range(1, 11)]


def test_a_split_needs_exactly_one_of_size_and_parts(many):
    assert runner.invoke(app, ['playlists', 'split', 'Long One']).exit_code == 2
    assert runner.invoke(app, ['playlists', 'split', 'Long One', '--size', '5', '--parts', '2']).exit_code == 2
    assert runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '0']).exit_code == 2


def test_a_split_that_would_overwrite_an_earlier_one_changes_nothing(many):
    """Checked up front, so a half-overwritten split cannot happen."""
    runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '2'])
    before = (paths.playlists_dir() / 'long-one-1.m3u').read_text()

    result = runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '3'])
    assert result.exit_code == 1
    assert (paths.playlists_dir() / 'long-one-1.m3u').read_text() == before
    assert not (paths.playlists_dir() / 'long-one-3.m3u').exists()


def test_a_forced_split_replaces_the_earlier_one(many):
    runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '2'])
    payload = json.loads(runner.invoke(app, ['playlists', 'split', 'Long One', '--parts', '2', '--force', '--json']).stdout)
    assert [part['video_count'] for part in payload] == [5, 5]


def test_ordering_rewrites_the_playlist_in_place(many):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Long One'])
    assert runner.invoke(app, ['playlists', 'order', 'Sunday', '--sort', 'longest']).exit_code == 0

    ordered = json.loads(runner.invoke(app, ['playlists', 'show', 'Sunday', '--json']).stdout)
    assert [row['video_id'] for row in ordered] == [f'vid{index:02d}' for index in range(10, 0, -1)]


def test_ordering_into_a_new_name_leaves_the_original_alone(many):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Long One'])
    assert runner.invoke(app, ['playlists', 'order', 'Sunday', '--sort', 'longest', '--into', 'Sunday Long']).exit_code == 0

    original = json.loads(runner.invoke(app, ['playlists', 'show', 'Sunday', '--json']).stdout)
    assert original[0]['video_id'] == 'vid01'
    assert (paths.playlists_dir() / 'sunday-long.m3u').exists()


def test_ordering_into_an_existing_name_refuses_without_force(many):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Long One'])
    runner.invoke(app, ['playlists', 'create', 'Taken', '--from', 'Long One'])

    assert runner.invoke(app, ['playlists', 'order', 'Sunday', '--sort', 'title', '--into', 'Taken']).exit_code == 1
    assert runner.invoke(app, ['playlists', 'order', 'Sunday', '--sort', 'title', '--into', 'Taken', '--force']).exit_code == 0


def test_ordering_a_mirrored_playlist_is_refused(synced):
    assert runner.invoke(app, ['playlists', 'order', 'Get Insights', '--sort', 'random']).exit_code == 1


def test_an_unknown_sort_is_a_usage_error_for_every_command_that_takes_one(built):
    for command in [['playlists', 'urls', 'Sunday'], ['playlists', 'split', 'Sunday', '--parts', '2'], ['playlists', 'order', 'Sunday']]:
        assert runner.invoke(app, [*command, '--sort', 'sideways']).exit_code == 2


@pytest.fixture
def two_videos(monkeypatch):
    """A mirror with two distinguishable videos, so order and identity are observable."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda *args, **kwargs: RemotePlaylist(
            playlist_id='PL2',
            title='More',
            videos=[RemoteVideo(video_id='vid1', title='A Talk'), RemoteVideo(video_id='vid2', title='Another')],
        ),
    )
    runner.invoke(app, ['sync', 'https://example.invalid/PL2'])


@pytest.fixture
def short_state_home(monkeypatch):
    """A state directory a unix socket address can actually fit in.

    pytest's tmp_path sits deep under macOS's private temp directory and is
    already past the ~104-byte limit before `ypl/mpv.sock` is appended.
    """
    directory = tempfile.mkdtemp(dir='/tmp')  # noqa: S108
    monkeypatch.setenv('XDG_STATE_HOME', directory)
    yield Path(directory)
    shutil.rmtree(directory, ignore_errors=True)


@pytest.fixture
def long_state_home(tmp_path, monkeypatch):
    """A state directory whose socket path cannot fit a unix socket address.

    Built deliberately rather than inherited: pytest's own tmp_path is past the
    104-byte limit under macOS's private temp directory and comfortably under it
    on Linux, so a test that leans on it passes locally and fails in CI.
    """
    directory = tmp_path.joinpath(*['nested-enough-to-overflow'] * 4)
    directory.mkdir(parents=True)
    monkeypatch.setenv('XDG_STATE_HOME', str(directory))
    return directory


@pytest.fixture
def played(monkeypatch):
    """Capture what would have been handed to mpv."""
    calls = []

    def record(urls, socket_path, extra_arguments=None):
        calls.append({'urls': urls, 'socket': socket_path, 'arguments': extra_arguments or []})
        return 0

    monkeypatch.setattr(player, 'play', record)
    return calls


def test_playing_hands_the_playlist_urls_to_mpv(synced, played):
    assert runner.invoke(app, ['play', 'Get Insights']).exit_code == 0
    assert played[0]['urls'] == ['https://www.youtube.com/watch?v=vid1']


def test_playing_opens_the_ipc_socket_so_now_can_read_it(short_state_home, synced, played):
    """short_state_home comes first: it redirects the mirror the sync then writes."""
    runner.invoke(app, ['play', 'Get Insights'])
    assert played[0]['socket'] == paths.mpv_socket()


def test_a_socket_path_too_long_for_the_kernel_plays_on_and_says_so(long_state_home, synced, played):
    """mpv would log `Could not create IPC socket` and play on regardless.

    That leaves `ypl now` reporting nothing with no way to tell why, so ypl
    checks the length itself and says what it gave up.
    """
    result = runner.invoke(app, ['play', 'Get Insights'])
    assert result.exit_code == 0
    assert played[0]['socket'] is None
    assert 'too long' in result.output


def test_audio_only_drops_the_video_window(synced, played):
    runner.invoke(app, ['play', 'Get Insights', '--audio'])
    assert '--no-video' in played[0]['arguments']


def test_configured_mpv_arguments_are_passed_through(synced, played):
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('mpv_arguments = ["--volume=70"]\n')

    runner.invoke(app, ['play', 'Get Insights'])
    assert played[0]['arguments'] == ['--volume=70']


def test_mpv_arguments_that_are_not_a_list_of_strings_are_rejected():
    """A bare string would be spread one character per mpv argument."""
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('mpv_arguments = "--volume=70"\n')

    assert runner.invoke(app, ['config', 'show']).exit_code == 2


def test_playing_a_local_playlist_applies_the_limit(built, played):
    runner.invoke(app, ['playlists', 'add', 'Sunday', '-4FPIL6e4SQ'])
    runner.invoke(app, ['play', 'Sunday', '--limit', '1'])
    assert played[0]['urls'] == ['https://www.youtube.com/watch?v=vid1']


def test_playing_an_empty_playlist_fails_rather_than_starting_mpv(played):
    runner.invoke(app, ['playlists', 'create', 'Empty'], input='')
    assert runner.invoke(app, ['play', 'Empty']).exit_code == 1
    assert played == []


def test_a_failing_mpv_makes_ypl_fail_too(synced, monkeypatch):
    """Otherwise a playlist that would not play looks like it played."""
    monkeypatch.setattr(player, 'play', lambda *args, **kwargs: 3)
    assert runner.invoke(app, ['play', 'Get Insights']).exit_code == 1


def test_a_missing_mpv_names_the_fix_and_fails(synced, monkeypatch):
    def unavailable(*args, **kwargs):
        raise player.MpvUnavailableError('mpv is not on PATH — install it to play playlists')

    monkeypatch.setattr(player, 'play', unavailable)
    result = runner.invoke(app, ['play', 'Get Insights'])
    assert result.exit_code == 1
    assert 'mpv' in result.output


def stub_now(monkeypatch, **state):
    monkeypatch.setattr(player, 'properties', lambda socket_path, names: {name: state.get(name) for name in names})


def test_now_prefers_the_mirrors_title_over_the_one_mpv_guessed(synced, monkeypatch):
    """Both are available here, so this pins which one wins rather than that one exists."""
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 10.0, 'media-title': 'Whatever mpv Saw'})

    payload = json.loads(runner.invoke(app, ['now', '--json']).stdout)
    assert payload['track'] is None
    assert payload['title'] == 'A Talk'


def test_now_says_why_there_is_no_track_when_the_video_is_unenriched(synced, monkeypatch):
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 10.0})

    result = runner.invoke(app, ['now'])
    assert 'ypl sync' in result.output


def test_now_falls_back_to_mpvs_own_title_for_a_video_not_in_the_mirror(monkeypatch):
    stub_now(monkeypatch, path='https://youtu.be/unknown123', **{'time-pos': 5.0, 'media-title': 'Something Else'})

    payload = json.loads(runner.invoke(app, ['now', '--json']).stdout)
    assert payload['title'] == 'Something Else'


def test_now_exits_1_when_nothing_is_playing(monkeypatch):
    """A status bar runs this unguarded, so it must not print a payload."""

    def not_playing(*args, **kwargs):
        raise player.NotPlayingError('nothing is listening')

    monkeypatch.setattr(player, 'properties', not_playing)
    result = runner.invoke(app, ['now', '--json'])
    assert result.exit_code == 1
    assert result.stdout == ''


def test_next_emits_the_fields_the_menu_register_names(synced):
    """`menu next` reads `label` and `id` out of the JSON by name.

    Renaming either field here silently breaks the pursuit rather than failing,
    so the contract is pinned from this side.
    """
    payload = json.loads(runner.invoke(app, ['next', '--json']).stdout)
    assert payload[0]['title'] == 'A Talk'
    assert payload[0]['video_id'] == 'vid1'
    assert payload[0]['url'] == 'https://www.youtube.com/watch?v=vid1'


def test_next_prints_one_line_per_suggestion_for_the_plain_resolver(synced):
    """The register's other mode takes the first line as the label."""
    result = runner.invoke(app, ['next'])
    assert result.exit_code == 0
    assert result.stdout.splitlines() == ['A Talk  https://www.youtube.com/watch?v=vid1']


def test_logging_a_listen_moves_it_down_the_next_order(two_videos):
    assert runner.invoke(app, ['plays', 'add', 'vid1']).exit_code == 0

    payload = json.loads(runner.invoke(app, ['next', '--json', '--limit', '2']).stdout)
    assert [row['video_id'] for row in payload] == ['vid2', 'vid1']


def test_the_round_trip_the_register_wires_up(synced):
    """resolve -> id -> on_log, exactly as `menu next` would run it."""
    suggestion = json.loads(runner.invoke(app, ['next', '--json']).stdout)[0]

    logged = runner.invoke(app, ['plays', 'add', suggestion['video_id'], '--json'])
    assert logged.exit_code == 0
    assert json.loads(logged.stdout)['play_count'] == 1


def test_logging_a_video_that_is_not_mirrored_fails_rather_than_writing_a_dangling_row(synced):
    result = runner.invoke(app, ['plays', 'add', 'notmirrored'])
    assert result.exit_code == 1
    assert 'not in the mirror' in result.output


def test_plays_are_listed_most_recent_first(two_videos):
    """Two plays, so the ordering is observable rather than assumed."""
    runner.invoke(app, ['plays', 'add', 'vid1'])
    runner.invoke(app, ['plays', 'add', 'vid2'])

    payload = json.loads(runner.invoke(app, ['plays', 'list', '--json']).stdout)
    assert [row['video_id'] for row in payload] == ['vid2', 'vid1']
    assert payload[0]['title'] == 'Another'


def test_next_on_an_empty_mirror_fails_rather_than_printing_nothing():
    """A resolver that exits 0 with no output would show as a blank pursuit."""
    assert runner.invoke(app, ['next']).exit_code == 1


def test_a_url_can_be_logged_as_well_as_an_id(two_videos):
    """Logged against the second video, so returning the first would not pass."""
    result = runner.invoke(app, ['plays', 'add', 'https://www.youtube.com/watch?v=vid2&t=90s', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_id'] == 'vid2'

    listed = json.loads(runner.invoke(app, ['plays', 'list', '--json']).stdout)
    assert [row['video_id'] for row in listed] == ['vid2']


# A signed-in jar: a SID cookie to hash, and the LOGIN_INFO that distinguishes
# a live session from a browser that merely still holds one.
BROWSER_COOKIES = {'__Secure-3PAPISID': 'aaa', 'SID': 'bbb', 'LOGIN_INFO': 'ccc'}

# Ownership is an id match now, so the fixtures need the account to *be* a
# channel rather than to have a name that happens to agree with one.
ACCOUNT_CHANNEL_ID = 'UCownchannel'
OTHER_CHANNEL_ID = 'UCsomebodyelse'


class FakeBackend:
    """A YouTube that actually holds a playlist, so writes can be observed.

    Class attributes rather than constructor arguments because the command
    builds its own backend — a test sets what YouTube holds, then invokes.
    Writes mutate that state, so an assertion after `apply` is about what
    YouTube ended up with rather than about which methods were called.
    """

    error: Exception | None = None
    add_error: Exception | None = None
    items: list[RemoteItem] = []
    created: list[str] = []
    handles: int = 0

    cookies: dict[str, str] = {}
    page_ids: list[str] = []

    def __init__(self, cookies, page_id='', **kwargs):
        FakeBackend.cookies = cookies
        FakeBackend.page_ids.append(page_id)
        self.page_id = page_id

    @classmethod
    def reset(cls):
        cls.error = None
        cls.add_error = None
        cls.items = []
        cls.created = []
        cls.handles = 0
        cls.cookies = {}
        cls.page_ids = []

    @classmethod
    def handle(cls, video_id):
        cls.handles += 1
        return f'h{cls.handles}-{video_id}'

    def account(self):
        if self.error:
            raise self.error
        return remote.RemoteAccount(name='iChrisBirch', handle='115127243664537694481')

    def playlist_items(self, playlist_id):
        if self.error:
            raise self.error
        return list(self.items)

    def create_playlist(self, title, description=''):
        if self.error:
            raise self.error
        FakeBackend.created.append(title)
        FakeBackend.items = []
        return 'PLNEW'

    def add_items(self, playlist_id, video_ids):
        if self.add_error:
            raise self.add_error
        FakeBackend.items = [*self.items, *(RemoteItem(video_id=v, set_video_id=self.handle(v)) for v in video_ids)]

    def remove_items(self, playlist_id, items):
        dropped = {item.set_video_id for item in items}
        FakeBackend.items = [item for item in self.items if item.set_video_id not in dropped]

    def move_item(self, playlist_id, item, before):
        remaining = [held for held in self.items if held.set_video_id != item.set_video_id]
        position = len(remaining)
        if before:
            position = next(index for index, held in enumerate(remaining) if held.set_video_id == before.set_video_id)
        remaining.insert(position, item)
        FakeBackend.items = remaining


@pytest.fixture
def signing_in(monkeypatch):
    """Stand in for YouTube, reset between tests because the state is class-level."""
    FakeBackend.reset()
    monkeypatch.setattr(youtubei, 'YouTubeiBackend', FakeBackend)
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: dict(BROWSER_COOKIES))
    yield FakeBackend
    FakeBackend.reset()


def test_signing_in_stores_the_browser_and_the_channel_it_acts_as(signing_in):
    result = runner.invoke(app, ['auth', '--browser', 'safari', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout) == {
        'browser': 'safari',
        'account': 'iChrisBirch',
        'page_id': '115127243664537694481',
        'path': str(paths.auth_file()),
    }


def test_the_page_id_is_what_gets_stored_not_the_account_that_was_selected(signing_in):
    """The whole bug, in one assertion.

    A jar reaches a personal Google account and the brand account beneath it,
    and the browser was left selected on the personal one — which owns no
    channel and no playlists. What has to be recorded is the identity that owns
    the channel, so every later request can name it.
    """
    runner.invoke(app, ['auth', '--browser', 'safari'])
    assert session.page_id() == '115127243664537694481'


def test_signing_in_stores_no_credential(signing_in):
    """There is nothing here worth protecting at 0600, because nothing is kept.

    The session file this replaces held a Google account cookie, which is the
    entire credential. Reading the browser's jar per run means the only durable
    facts are a browser name and a page id, neither of which signs anyone in.
    """
    runner.invoke(app, ['auth', '--browser', 'safari'])
    stored = json.loads(paths.auth_file().read_text())
    assert set(stored) == {'browser', 'page_id'}
    assert 'aaa' not in paths.auth_file().read_text()


def test_signing_in_twice_just_signs_in_again(signing_in):
    """No --replace, because there is no stored credential to refuse to clobber.

    The old flow guarded the overwrite because re-copying headers was a trip
    through DevTools. Naming a browser costs nothing, so the guard was asking
    permission to redo something free.
    """
    assert runner.invoke(app, ['auth', '--browser', 'safari']).exit_code == 0
    assert runner.invoke(app, ['auth', '--browser', 'firefox']).exit_code == 0
    assert session.browser() == 'firefox'


def test_naming_no_browser_at_all_is_a_usage_error(signing_in):
    """Exit 2: nothing was wrong, the one required fact was not given."""
    result = runner.invoke(app, ['auth'])
    assert result.exit_code == 2
    assert not paths.auth_file().exists()


def test_a_session_youtube_rejects_stores_nothing(signing_in):
    """Recording a browser that cannot sign in would only fail later, further from here."""
    signing_in.error = remote.RemoteAuthError('cookie expired')
    result = runner.invoke(app, ['auth', '--browser', 'safari'])
    assert result.exit_code == 1
    assert not paths.auth_file().exists()


def test_a_session_that_could_not_be_checked_leaves_an_existing_sign_in_alone(signing_in):
    """A throttle or a dead network says nothing about the browser.

    Nothing new is stored, and — the part that matters — the sign-in already
    there is not replaced by a worse one on the strength of a failure that was
    never about it.
    """
    session.remember_browser('safari', 'PAGEID')
    signing_in.error = remote.RemoteRateLimitedError('slow down')

    result = runner.invoke(app, ['auth', '--browser', 'firefox'])
    assert result.exit_code == 1
    assert session.browser() == 'safari'
    assert session.page_id() == 'PAGEID'


def test_deleting_a_playlist_takes_its_merge_base_with_it(synced):
    """A base that outlives its playlist reads as a pile of local deletions.

    The next playlist to slug the same way would inherit it, and the queue would
    carry those deletions out on YouTube.
    """
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'Get Insights'])
    basestore.save(basestore.Base(slug='sunday', playlist_id='PL9', items=[RemoteItem(video_id='vid1', set_video_id='h1')]))

    assert runner.invoke(app, ['playlists', 'delete', 'Sunday', '--yes']).exit_code == 0
    assert basestore.load('sunday') is None


def bind(name: str, remote_id: str = 'PLR') -> local.LocalPlaylist:
    """Put a local playlist in the state a push would leave it in."""
    playlist = local.load(local.path_for(name))
    playlist.remote_id = remote_id
    local.save(playlist, overwrite=True)
    return playlist


def set_order(name: str, video_ids: list[str]) -> None:
    """Rewrite a local playlist's order without going through a command."""
    playlist = local.load(local.path_for(name))
    by_id = {entry.video_id: entry for entry in playlist.entries}
    playlist.entries = [by_id[video_id] for video_id in video_ids]
    local.save(playlist, overwrite=True)


def test_signing_in_from_a_browser_needs_no_paste(signing_in):
    """The whole point: no DevTools, no clipboard, no EOF on stdin."""
    result = runner.invoke(app, ['auth', '--browser', 'safari', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['account'] == 'iChrisBirch'


def test_the_configured_browser_is_used_when_no_flag_is_given(signing_in, monkeypatch):
    """cookies_from_browser already says which browser is signed in."""
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('cookies_from_browser = "firefox"\n')
    asked = []
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: asked.append(browser) or dict(BROWSER_COOKIES))

    assert runner.invoke(app, ['auth']).exit_code == 0
    assert asked == ['firefox']


def test_a_browser_that_is_not_signed_in_fails_without_writing_anything(monkeypatch):
    """A jar can hold a SID cookie long after the session behind it ended.

    Deliberately against the real backend rather than the fake: this is the one
    refusal that happens before any request, and faking the thing under test
    would assert nothing.
    """
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'aaa'})

    result = runner.invoke(app, ['auth', '--browser', 'safari'])
    assert result.exit_code == 1
    assert not paths.auth_file().exists()


def test_a_browser_yt_dlp_cannot_read_names_the_browser(signing_in, monkeypatch):
    def unreadable(browser, **kwargs):
        raise ytdlp.YtdlpFailedError('could not find safari cookies database')

    monkeypatch.setattr(ytdlp, 'browser_cookies', unreadable)

    result = runner.invoke(app, ['auth', '--browser', 'safari'])
    assert result.exit_code == 1
    assert 'safari' in result.output


@pytest.fixture
def account(monkeypatch):
    """Two playlists on YouTube, listed the way the account feed lists them.

    `fetch_video` is stubbed as well, because a bare sync now enriches with
    whatever budget the rest of the run leaves — an unstubbed one would send the
    suite to the network.
    """
    monkeypatch.setattr(ytdlp, 'fetch_account_channel_id', lambda browser, **kwargs: ACCOUNT_CHANNEL_ID)
    monkeypatch.setattr(
        ytdlp,
        'fetch_account_playlists',
        lambda browser, **kwargs: [PlaylistRef(playlist_id='PLa', title='Deep Night'), PlaylistRef(playlist_id='PLb', title='Art')],
    )
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda url, **kwargs: RemotePlaylist(
            playlist_id=url,
            title={'PLa': 'Deep Night', 'PLb': 'Art'}[url],
            videos=[RemoteVideo(video_id=f'{url}vid', title='A Mix')],
            # The brand account, which is the fact the old backend could not
            # see: it answered with the *Google account* name, so every one of
            # this account's own playlists was judged to belong to somebody
            # else. Naming it `Chris Birch` here made the suite agree with the
            # bug. The sweep decides syncedness by ownership, and the feed
            # lists saved playlists too, so this is what the filter turns on.
            channel='iChrisBirch',
            channel_id=ACCOUNT_CHANNEL_ID,
        ),
    )
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(video_id=video_id, title='A Mix', channel='Someone', duration_seconds=3600),
    )


def test_syncing_one_playlist_by_url_still_works(synced):
    """The account sweep is the new default, not a replacement."""
    assert runner.invoke(app, ['playlists', 'show', 'Get Insights']).exit_code == 0


def test_a_macos_agent_runs_at_load_as_well_as_on_the_interval(tmp_path, monkeypatch):
    """The run that matters most is the first one after the machine was off."""
    monkeypatch.setattr(schedule, 'is_macos', lambda: True)
    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')

    assert schedule.ensure(30) is not None
    # Read back through agent_path rather than a path this test chose, so that
    # where a launch agent lands is covered rather than stubbed out.
    assert schedule.agent_path().is_relative_to(tmp_path)
    written = schedule.agent_path().read_text()
    assert '<key>RunAtLoad</key>' in written
    assert '<integer>1800</integer>' in written


@pytest.fixture
def homebrew_yt_dlp(monkeypatch):
    """yt-dlp where Homebrew puts it — which is not on the PATH launchd gives."""
    monkeypatch.setattr(schedule, 'tool_directories', lambda: ['/usr/local/bin'])


def test_a_macos_agent_carries_it_too(tmp_path, monkeypatch, homebrew_yt_dlp):
    monkeypatch.setattr(schedule, 'is_macos', lambda: True)
    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')

    schedule.ensure(30)

    written = schedule.agent_path().read_text()
    assert '<key>EnvironmentVariables</key>' in written
    assert f'<string>/usr/local/bin{os.pathsep}/usr/bin' in written


def test_the_timer_prefers_an_installed_ypl_to_the_one_in_a_virtualenv(tmp_path, monkeypatch):
    """Developing means running `uv run ypl sync`, which puts the checkout first
    on PATH. A timer bound to it dies with the next rebuild of that directory."""
    checkout = tmp_path / 'checkout' / '.venv' / 'bin'
    installed = tmp_path / 'installed'
    for directory in (checkout, installed):
        directory.mkdir(parents=True)
        (directory / 'ypl').touch(mode=0o755)
    monkeypatch.setenv('VIRTUAL_ENV', str(tmp_path / 'checkout' / '.venv'))
    monkeypatch.setenv('PATH', f'{checkout}{os.pathsep}{installed}')

    assert schedule.executable() == str(installed / 'ypl')


def test_the_timer_schedules_the_symlink_rather_than_what_it_points_at(tmp_path, monkeypatch):
    """`~/.local/bin/ypl` is a link into uv's tool directory, and that layout is
    uv's to change. The link is the path uv promises to keep."""
    real = tmp_path / 'share' / 'uv' / 'tools' / 'ypl' / 'bin'
    real.mkdir(parents=True)
    (real / 'ypl').touch(mode=0o755)
    binaries = tmp_path / 'bin'
    binaries.mkdir()
    (binaries / 'ypl').symlink_to(real / 'ypl')
    monkeypatch.setenv('PATH', str(binaries))

    assert schedule.executable() == str(binaries / 'ypl')


def test_the_checkouts_ypl_is_still_used_when_it_is_the_only_one(tmp_path, monkeypatch):
    """The fallback the docstring promises: a machine with nothing installed."""
    checkout = tmp_path / 'checkout' / '.venv' / 'bin'
    checkout.mkdir(parents=True)
    (checkout / 'ypl').touch(mode=0o755)
    monkeypatch.setenv('VIRTUAL_ENV', str(tmp_path / 'checkout' / '.venv'))
    monkeypatch.setenv('PATH', str(checkout))

    assert schedule.executable() == str(checkout / 'ypl')


def test_editing_a_playlist_applies_the_order_the_buffer_asked_for(two_videos):
    """Piped rather than typed: the same path an editor takes, minus the editor."""
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    buffer = 'vid2  Another\nvid1  A Talk\n'

    result = runner.invoke(app, ['playlists', 'edit', 'Sunday', '--json'], input=buffer)
    assert result.exit_code == 0
    assert json.loads(result.stdout)['reordered'] is True
    assert local.load(local.path_for('Sunday')).video_ids == ['vid2', 'vid1']


def test_a_deleted_line_removes_that_video(two_videos):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])

    result = runner.invoke(app, ['playlists', 'edit', 'Sunday', '--json'], input='vid1  A Talk\n')
    assert json.loads(result.stdout)['removed'] == ['vid2']
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1']


def test_an_empty_buffer_aborts_rather_than_emptying_the_playlist(two_videos):
    """Deleting every line by accident must not be how a playlist is lost."""
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])

    result = runner.invoke(app, ['playlists', 'edit', 'Sunday'], input='# everything deleted\n')
    assert result.exit_code == 0
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2']


def test_a_line_that_is_not_a_video_changes_nothing(two_videos):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])

    result = runner.invoke(app, ['playlists', 'edit', 'Sunday'], input='vid1 A Talk\nnot a video at all\n')
    assert result.exit_code == 2
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2']


def test_a_url_pasted_into_the_buffer_adds_that_video(two_videos):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    buffer = 'vid1 A Talk\nvid2 Another\nhttps://youtu.be/vid9\n'

    result = runner.invoke(app, ['playlists', 'edit', 'Sunday', '--json'], input=buffer)
    assert json.loads(result.stdout)['added'] == ['vid9']
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2', 'vid9']


def test_a_mirrored_playlist_cannot_be_edited(synced):
    """It is a mirror; the fix is to make a local copy, and it says so."""
    result = runner.invoke(app, ['playlists', 'edit', 'Get Insights'], input='vid1 A Talk\n')
    assert result.exit_code == 1
    assert 'read-only' in result.output


@pytest.fixture
def current(two_videos):
    """A local playlist, and ypl told it is the one being listened to."""
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    runner.invoke(app, ['use', 'Sunday'])


def playing(monkeypatch, video_id: str | None) -> None:
    """Stand in for mpv having a video open, or for mpv not being there."""
    if video_id is None:
        monkeypatch.setattr(player, 'properties', lambda *args, **kwargs: (_ for _ in ()).throw(player.NotPlayingError('no socket')))
        return
    monkeypatch.setattr(player, 'properties', lambda *args, **kwargs: {'path': f'https://youtu.be/{video_id}'})
    monkeypatch.setattr(player, 'command', lambda *args, **kwargs: None)


def test_keep_playing_leaves_mpv_alone(current, monkeypatch):
    playing(monkeypatch, 'vid1')
    sent = []
    monkeypatch.setattr(player, 'command', lambda socket_path, arguments: sent.append(arguments))

    runner.invoke(app, ['drop', '--keep-playing'])
    assert sent == []


@pytest.fixture
def enriched_library(two_videos, monkeypatch):
    """Two mixes with real tracklists, which is what curation reads."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(
            video_id=video_id,
            title='A Talk' if video_id == 'vid1' else 'Another',
            channel='Cercle',
            duration_seconds=7200 if video_id == 'vid1' else 600,
            chapters=[Chapter(start_seconds=0, end_seconds=600, title='Black Coffee - Wish You Were Here')]
            if video_id == 'vid1'
            else [Chapter(start_seconds=0, end_seconds=300, title='Bonobo - Kerala')],
        ),
    )
    runner.invoke(app, ['enrich', '--all'])


def test_the_library_says_which_of_your_playlists_hold_each_mix(enriched_library):
    """Your own playlist names are a label you already applied."""
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json']).stdout)
    assert payload[0]['playlists'] == ['More']


def test_the_library_sorts_longest_first_so_a_six_hour_set_is_choosable(enriched_library):
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json']).stdout)
    assert [row['video_id'] for row in payload] == ['vid1', 'vid2']


def test_position_is_not_a_sort_the_library_offers(enriched_library):
    result = runner.invoke(app, ['videos', 'list', '--sort', 'position'])
    assert result.exit_code == 2


def test_playlist_names_complete_from_both_stores(synced):
    """The whole point of completion here: nobody should retype a playlist name."""
    runner.invoke(app, ['playlists', 'create', 'Sunday Morning', '--from', 'Get Insights'])

    assert 'Get Insights' in main.complete_playlist('get')
    assert 'sunday-morning' in main.complete_playlist('sun')


def test_a_command_that_writes_only_completes_playlists_it_could_write_to(synced):
    """Offering a mirrored playlist to `ypl playlists edit` would be a lie."""
    runner.invoke(app, ['playlists', 'create', 'Sunday Morning', '--from', 'Get Insights'])

    assert main.complete_local_playlist('') == ['sunday-morning']


def test_completion_matches_anywhere_in_the_title_not_just_the_start(synced):
    """Titles here start with the artist or the event, and you remember the middle."""
    assert main.complete_playlist('insights') == ['Get Insights']


def test_signing_in_from_a_browser_teaches_the_reads_which_browser_it_was(signing_in, monkeypatch):
    """Two subsystems, one login. Signing in once should not have to be told twice."""
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'x'})
    runner.invoke(app, ['auth', '--browser', 'safari'])

    assert session.browser() == 'safari'
    assert main.reading_browser(config.Config()) == 'safari'


def test_the_config_still_wins_over_what_was_remembered(monkeypatch):
    """It is the setting a person wrote down; nothing inferred should override it."""
    monkeypatch.setattr(session, 'browser', lambda: 'safari')
    assert main.reading_browser(config.Config(cookies_from_browser='firefox')) == 'firefox'


def test_status_counts_what_a_run_did_rather_than_listing_it():
    """Seventeen playlist names in a status line is a Python list on your screen."""
    synclog.record({'bound': ['BEST', 'Art', 'Trip'], 'reconciled': [], 'pushed': ['Deep Night']})

    output = runner.invoke(app, ['status']).output
    assert '3 bound, 1 pushed' in output


def test_status_stops_short_of_reprinting_a_whole_failed_run():
    """A run where every playlist failed pushed the answer off the screen."""
    synclog.record({'failures': [f'playlist {index}: unreadable' for index in range(9)]})

    output = runner.invoke(app, ['status']).output
    assert 'playlist 0: unreadable' in output
    assert 'and 4 more — ypl status --json' in output
    assert 'playlist 8' not in output
    # The lines the whole command exists to answer still have to be below it.
    assert 'Tracklists' in output


def test_status_says_how_far_through_the_tracklists_the_library_is(synced):
    """A backlog with no total behind it is a number nobody can act on.

    Four thousand to enrich is either most of the library or a rounding error,
    and `Unenriched 4327 videos` did not say which.
    """
    output = runner.invoke(app, ['status']).output
    assert 'Tracklists      0 of 1 videos (0%)' in output
    assert '1 to go' in output


def test_status_projects_when_the_backlog_will_be_gone(synced, monkeypatch):
    """The line this command did not have, and the only one anyone reads it for."""
    monkeypatch.setattr(synclog, 'enrich_rate_per_hour', lambda: 0.5)

    assert 'done in 2h 00m' in runner.invoke(app, ['status']).output


def test_status_says_when_the_next_sync_is_due():
    """A timer you cannot see the next firing of is one you cannot tell apart
    from a dead one."""
    synclog.record({'seconds': 60.0})
    schedule.install(30)

    assert 'Next sync' in runner.invoke(app, ['status']).output


def test_status_reports_a_run_in_progress_as_one(monkeypatch):
    """Mid-run is exactly when the counts are worth reading, and `Syncing now
    yes` alone did not say the numbers below it were moving."""
    monkeypatch.setattr(runlock, 'started', lambda: dt.datetime.now(dt.UTC) - dt.timedelta(minutes=20))

    output = runner.invoke(app, ['status']).output
    assert 'yes — 20 min so far' in output
    assert 'Next sync       30 min after this run finishes' not in output


def test_status_explains_what_unreadable_means(synced):
    """Fifty-five failures reads as fifty-five things to fix, and it is none."""
    connection = db.connect()
    service.mark_unreadable(connection, 'vid1', 'Private video')

    assert 'deleted, private or region-locked' in runner.invoke(app, ['status']).output


def test_the_timer_runs_the_mode_that_does_not_stop_at_a_clock(homebrew_yt_dlp, monkeypatch):
    """The whole point of the background run: no time bound, so it drains."""
    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')

    timer = schedule.ensure(30)

    assert timer is not None
    assert timer.command == ['/usr/local/bin/ypl', 'sync', '--background']


def test_a_unit_written_before_the_background_flag_is_rewritten(homebrew_yt_dlp, monkeypatch):
    """Nothing has to be reinstalled by hand: `ensure` already compares the
    command, so the next sync on every machine picks the new one up."""
    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')
    schedule.install(30)
    stale = schedule.installed().path
    stale.write_text(stale.read_text().replace('<string>--background</string>\n', '').replace('--background', ''))

    assert schedule.ensure(30).command == ['/usr/local/bin/ypl', 'sync', '--background']


def test_a_bare_ypl_says_where_things_stand_and_what_to_run():
    """Not a catalogue.

    Thirty-nine commands in six panels answered "what can this do", which is
    not the question a bare command is typed to ask. On a machine that has
    never been set up there is exactly one thing to do, so that is what it says.
    """
    output = runner.invoke(app, []).output
    assert 'Signed in       no' in output
    assert 'Last sync       never' in output
    assert 'ypl auth --browser safari' in output


def test_a_bare_ypl_stops_naming_a_next_command_once_there_is_none():
    """Signed in, synced and on a timer is the steady state, and it reads as one."""
    session.remember_browser('safari', 'PAGEID')
    synclog.record({'bound': [], 'reconciled': [], 'pushed': []})

    output = runner.invoke(app, []).output
    assert 'Signed in       yes' in output
    assert 'ypl auth' not in output


def test_the_rate_is_measured_across_the_gaps_not_only_inside_the_runs():
    """The number anyone wants is when the backlog will be gone.

    A rate taken from time spent working said three videos a minute while the
    old fifteen-in-thirty timer was actually managing one — it spent two thirds
    of every hour asleep, and the projection built on it was out by three times.
    """
    runs = [
        {'ts': '2026-08-07T02:00:00+00:00', 'enriched': 45},
        {'ts': '2026-08-07T01:00:00+00:00', 'enriched': 40},
        {'ts': '2026-08-07T00:00:00+00:00', 'enriched': 50},
    ]
    assert synclog.enrich_rate_per_hour(runs) == pytest.approx(42.5)


def test_one_run_is_not_enough_to_claim_a_rate():
    """A projection from a single point is a guess wearing an estimate's clothes."""
    assert synclog.enrich_rate_per_hour([{'ts': '2026-08-07T02:00:00+00:00', 'enriched': 45}]) is None


def test_the_next_run_is_counted_from_when_the_last_one_ended():
    """What launchd does with a job still going when the interval elapses: the
    countdown restarts on exit. Counting from the start was wrong by the length
    of the run, every time."""
    ended = dt.datetime(2026, 8, 7, 1, 34, tzinfo=dt.UTC)
    assert schedule.next_fire(ended, 30) == dt.datetime(2026, 8, 7, 2, 4, tzinfo=dt.UTC)


def test_asking_whether_a_sync_is_running_does_not_reset_when_it_started():
    """`ypl status` opened the lock file for writing, which truncates — and
    truncating stamps it. Every check reset the one timestamp saying how long
    the running sync had been going."""
    with runlock.held() as mine:
        assert mine
        began = runlock.started()
        assert runlock.running()
        assert runlock.started() == began


def test_nothing_is_running_when_nothing_holds_the_lock():
    with runlock.held():
        pass
    assert runlock.started() is None


def test_an_exported_jar_is_what_a_run_hands_yt_dlp(tmp_path):
    """Naming the browser instead made yt-dlp decrypt the whole cookie store
    once per video, which was a third of what enriching a library cost."""
    jar = tmp_path / 'youtube.txt'
    assert ytdlp.Cookies(jar=jar).arguments() == ['--cookies', str(jar)]
    assert ytdlp.Cookies(browser='safari').arguments() == ['--cookies-from-browser', 'safari']
    assert ytdlp.Cookies().arguments() == []


def test_a_browser_that_cannot_be_exported_falls_back_to_naming_it(monkeypatch):
    """A slower sync is a better answer than no sync."""
    monkeypatch.setattr(ytdlp, 'export_cookie_jar', lambda *args, **kwargs: (_ for _ in ()).throw(ytdlp.YtdlpFailedError('no')))

    with ytdlp.exported_cookies('safari') as cookies:
        assert cookies == ytdlp.Cookies(browser='safari')
