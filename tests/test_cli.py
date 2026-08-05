"""The machine contract: exit codes, and which stream carries what.

`forge` and anything else shelling out sees only these two signals, so they are
the API rather than a nicety.
"""

import json
import os
import shutil
import stat
import tempfile
import tomllib
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
from ypl import ytdlp
from ypl import ytmusic
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


def test_bare_invocation_shows_help_rather_than_doing_anything():
    """Help on stdout, exit 2.

    Both halves matter and they answer different readers. A person gets the
    help text instead of an error, which is the no-args-shows-help rule; a
    caller gets 2, which says "incomplete command, retrying with different
    arguments could work" rather than 1's "it ran and failed". safekeep settled
    this for the fleet in `fix(safekeep): exit 2 on bare invocation`.
    """
    result = runner.invoke(app, [])
    assert result.exit_code == 2
    assert 'Usage:' in result.output


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


def test_an_ambiguous_playlist_name_exits_2_and_lists_the_candidates(monkeypatch):
    for playlist_id, title in [('PL1', 'Deep Night One'), ('PL2', 'Deep Night Two')]:
        monkeypatch.setattr(
            ytdlp,
            'fetch_playlist',
            lambda *args, pid=playlist_id, name=title, **kwargs: RemotePlaylist(playlist_id=pid, title=name),
        )
        runner.invoke(app, ['sync', f'https://example.invalid/{playlist_id}'])

    result = runner.invoke(app, ['playlists', 'show', 'Deep Night'])
    assert result.exit_code == 2
    assert 'Deep Night One' in result.output
    assert 'Deep Night Two' in result.output


def test_json_output_is_parseable_with_nothing_else_on_stdout(synced):
    result = runner.invoke(app, ['playlists', 'list', '--json'])
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload[0]['title'] == 'Get Insights'


def test_urls_emit_one_bare_url_per_line_for_piping(synced):
    result = runner.invoke(app, ['playlists', 'urls', 'Get Insights'])
    assert result.exit_code == 0
    assert result.stdout.strip() == 'https://www.youtube.com/watch?v=vid1'


def test_urls_json_carries_the_url_alongside_the_metadata(synced):
    result = runner.invoke(app, ['playlists', 'urls', 'Get Insights', '--json'])
    payload = json.loads(result.stdout)
    assert payload[0]['url'] == 'https://www.youtube.com/watch?v=vid1'
    assert payload[0]['title'] == 'A Talk'


def test_an_empty_mirror_lists_nothing_and_still_succeeds():
    result = runner.invoke(app, ['playlists', 'list', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout) == []


def test_a_missing_yt_dlp_names_the_fix_and_fails(monkeypatch):
    def unavailable(*args, **kwargs):
        raise ytdlp.YtdlpUnavailableError('yt-dlp is not on PATH — install it to read playlists')

    monkeypatch.setattr(ytdlp, 'fetch_playlist', unavailable)
    result = runner.invoke(app, ['sync', 'https://example.invalid/PL1'])
    assert result.exit_code == 1
    assert 'yt-dlp' in result.output


def test_config_path_prints_all_three_locations():
    result = runner.invoke(app, ['config', 'path'])
    assert result.exit_code == 0
    assert 'config' in result.stdout
    assert 'mirror' in result.stdout
    assert 'playlists' in result.stdout


def test_config_init_refuses_to_clobber_without_force():
    assert runner.invoke(app, ['config', 'init']).exit_code == 0
    assert runner.invoke(app, ['config', 'init']).exit_code == 1
    assert runner.invoke(app, ['config', 'init', '--force']).exit_code == 0


def test_the_example_config_is_valid_toml():
    result = runner.invoke(app, ['config', 'example'])
    assert result.exit_code == 0
    tomllib.loads(result.stdout)


def test_the_starter_config_written_by_init_loads_back():
    """The example and the loader have to agree, or `config init` ships a broken file."""
    assert runner.invoke(app, ['config', 'init']).exit_code == 0
    result = runner.invoke(app, ['config', 'show', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['enrich_batch_size'] == 50


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


def test_a_video_id_starting_with_a_hyphen_is_an_id_not_an_option(monkeypatch):
    """Base64url ids mean roughly one in thirty begins with `-`.

    Without `ignore_unknown_options` Click rejects `-4FPIL6e4SQ` as an unknown
    option, which is a failure that depends on which video you happened to pick.
    """
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda *args, **kwargs: RemotePlaylist(
            playlist_id='PL1',
            title='Mixes',
            videos=[RemoteVideo(video_id='-4FPIL6e4SQ', title='A Set')],
        ),
    )
    runner.invoke(app, ['sync', 'https://example.invalid/PL1'])

    result = runner.invoke(app, ['videos', 'show', '-4FPIL6e4SQ', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video']['video_id'] == '-4FPIL6e4SQ'


def test_enrich_reports_nothing_to_do_on_an_empty_mirror():
    result = runner.invoke(app, ['enrich', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['enriched'] == 0


def test_a_playlist_scoped_enrich_rejects_an_unknown_playlist(synced):
    result = runner.invoke(app, ['enrich', '--playlist', 'nonexistent'])
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


def test_a_playlist_can_be_promoted_and_demoted_after_the_fact(synced):
    runner.invoke(app, ['playlists', 'create', 'Scratch', '--from', 'Get Insights', '--local'])

    promoted = runner.invoke(app, ['playlists', 'promote', 'Scratch', '--json'])
    assert json.loads(promoted.stdout)['sync_state'] == 'pending'

    demoted = runner.invoke(app, ['playlists', 'demote', 'Scratch', '--json'])
    assert json.loads(demoted.stdout)['sync_state'] == 'local'


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


def test_urls_read_a_local_playlist_the_same_way_they_read_a_mirrored_one(built):
    result = runner.invoke(app, ['playlists', 'urls', 'Sunday'])
    assert result.exit_code == 0
    assert result.stdout.strip() == 'https://www.youtube.com/watch?v=vid1'


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


def test_ordering_keeps_unavailable_videos_rather_than_selecting_them_out(monkeypatch):
    """Reordering must not silently drop entries — that is what `create` is for."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda *args, **kwargs: RemotePlaylist(
            playlist_id='PL1',
            title='Mixed',
            videos=[RemoteVideo(video_id='ok', title='Fine'), RemoteVideo(video_id='gone', title='[Deleted video]', is_unavailable=True)],
        ),
    )
    runner.invoke(app, ['sync', 'https://example.invalid/PL1'])
    runner.invoke(app, ['playlists', 'create', 'Kept'], input='https://youtu.be/ok\nhttps://youtu.be/gone\n')

    result = runner.invoke(app, ['playlists', 'order', 'Kept', '--sort', 'title', '--json'])
    assert json.loads(result.stdout)['video_count'] == 2


def test_a_split_says_what_it_left_out_rather_than_coming_up_short_silently(monkeypatch):
    monkeypatch.setattr(
        ytdlp,
        'fetch_playlist',
        lambda *args, **kwargs: RemotePlaylist(
            playlist_id='PL1',
            title='Patchy',
            videos=[
                RemoteVideo(video_id='a', title='One'),
                RemoteVideo(video_id='gone', title='[Deleted video]', is_unavailable=True),
                RemoteVideo(video_id='b', title='Two'),
            ],
        ),
    )
    runner.invoke(app, ['sync', 'https://example.invalid/PL1'])

    result = runner.invoke(app, ['playlists', 'split', 'Patchy', '--parts', '2'])
    assert result.exit_code == 0
    assert '1 unavailable' in result.output


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


def test_now_reports_the_track_playing_inside_the_mix(synced, monkeypatch):
    """The whole reason chapter timestamps are stored: the track, not the mix."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(
            video_id=video_id,
            title='A Talk',
            duration_seconds=600,
            chapters=[Chapter(0, 120, 'Bonobo - Kerala'), Chapter(120, 300, 'Four Tet - Baby')],
        ),
    )
    runner.invoke(app, ['enrich'])
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 150.9})

    payload = json.loads(runner.invoke(app, ['now', '--json']).stdout)
    assert payload['track']['artist'] == 'Four Tet'
    assert payload['track']['title'] == 'Baby'
    assert payload['position_seconds'] == 150


def test_now_prefers_the_mirrors_title_over_the_one_mpv_guessed(synced, monkeypatch):
    """Both are available here, so this pins which one wins rather than that one exists."""
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 10.0, 'media-title': 'Whatever mpv Saw'})

    payload = json.loads(runner.invoke(app, ['now', '--json']).stdout)
    assert payload['track'] is None
    assert payload['title'] == 'A Talk'


def test_now_prints_the_track_and_the_position_without_json(synced, monkeypatch):
    """The human path builds a nested f-string that --json never exercises."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(
            video_id=video_id, title='A Talk', duration_seconds=600, chapters=[Chapter(0, 300, 'Bonobo - Kerala')]
        ),
    )
    runner.invoke(app, ['enrich'])
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 65.0, 'duration': 600})

    result = runner.invoke(app, ['now'])
    assert result.exit_code == 0
    assert 'Bonobo - Kerala' in result.output
    assert '1:05 / 10:00' in result.output


def test_now_says_why_there_is_no_track_when_the_video_is_unenriched(synced, monkeypatch):
    stub_now(monkeypatch, path='https://www.youtube.com/watch?v=vid1', **{'time-pos': 10.0})

    result = runner.invoke(app, ['now'])
    assert 'ypl enrich' in result.output


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


def test_listening_history_survives_the_mirror_being_thrown_away(two_videos):
    """The mirror is disposable — it re-syncs for free — and the history is not.

    Deleting it and starting over must not make `ypl next` forget.
    """
    runner.invoke(app, ['plays', 'add', 'vid1'])
    paths.database_file().unlink()
    runner.invoke(app, ['sync', 'https://example.invalid/PL2'])

    payload = json.loads(runner.invoke(app, ['next', '--json', '--limit', '2']).stdout)
    assert [row['video_id'] for row in payload] == ['vid2', 'vid1']
    assert payload[1]['play_count'] == 1


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


def test_a_video_reachable_only_through_a_local_playlist_can_still_be_enriched(monkeypatch):
    """Enrichment is a fact about a video, not about its membership of a mirror playlist."""
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(video_id=video_id, title='A Set', channel='Cercle', duration_seconds=3600),
    )
    assert runner.invoke(app, ['playlists', 'create', 'Hand Picked'], input='https://youtu.be/vid9\n').exit_code == 0

    result = runner.invoke(app, ['enrich', '--playlist', 'Hand Picked', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['enriched'] == 1

    shown = json.loads(runner.invoke(app, ['videos', 'show', 'vid9', '--json']).stdout)
    assert shown['video']['title'] == 'A Set'


HEADERS = 'cookie: __Secure-3PAPISID=aaa; SID=bbb\nx-goog-authuser: 0\n'


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

    def __init__(self, auth_file):
        self.auth_file = auth_file

    @classmethod
    def reset(cls):
        cls.error = None
        cls.add_error = None
        cls.items = []
        cls.created = []
        cls.handles = 0

    @classmethod
    def handle(cls, video_id):
        cls.handles += 1
        return f'h{cls.handles}-{video_id}'

    def account(self):
        if self.error:
            raise self.error
        return remote.RemoteAccount(name='Chris Birch', handle='@chrisbirch')

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
    monkeypatch.setattr(ytmusic, 'YtMusicBackend', FakeBackend)
    yield FakeBackend
    FakeBackend.reset()


def test_signing_in_stores_the_session_and_names_the_account(signing_in):
    result = runner.invoke(app, ['remote', 'auth', '--json'], input=HEADERS)
    assert result.exit_code == 0
    assert json.loads(result.stdout) == {
        'path': str(paths.ytmusic_auth_file()),
        'account': 'Chris Birch',
        'handle': '@chrisbirch',
        'browser': '',
    }


def test_the_stored_session_is_not_readable_by_anyone_else(signing_in):
    """The end-to-end half of the mode test — the command must not widen it."""
    runner.invoke(app, ['remote', 'auth'], input=HEADERS)
    assert stat.S_IMODE(paths.ytmusic_auth_file().stat().st_mode) == 0o600


def test_signing_in_again_refuses_rather_than_replacing_a_working_session(signing_in):
    """Re-copying headers is a browser round trip, so the overwrite is asked for."""
    runner.invoke(app, ['remote', 'auth'], input=HEADERS)
    stored = paths.ytmusic_auth_file().read_text()

    result = runner.invoke(app, ['remote', 'auth'], input='cookie: other\nx-goog-authuser: 0\n')
    assert result.exit_code == 1
    assert '--replace' in result.output
    assert paths.ytmusic_auth_file().read_text() == stored


def test_replace_signs_in_over_the_old_session(signing_in):
    runner.invoke(app, ['remote', 'auth'], input=HEADERS)
    result = runner.invoke(app, ['remote', 'auth', '--replace'], input='cookie: newer\nx-goog-authuser: 0\n')
    assert result.exit_code == 0
    assert json.loads(paths.ytmusic_auth_file().read_text())['cookie'] == 'newer'


def test_headers_that_do_not_parse_are_a_usage_error(signing_in):
    """Exit 2: the paste was wrong, which is the caller's input rather than a failure."""
    result = runner.invoke(app, ['remote', 'auth'], input='accept: */*\n')
    assert result.exit_code == 2
    assert not paths.ytmusic_auth_file().exists()


def test_a_session_youtube_rejects_is_not_left_behind(signing_in):
    """A stored credential that cannot work would only fail later, further from here."""
    signing_in.error = remote.RemoteAuthError('cookie expired')
    result = runner.invoke(app, ['remote', 'auth'], input=HEADERS)
    assert result.exit_code == 1
    assert not paths.ytmusic_auth_file().exists()


def test_a_session_that_could_not_be_checked_is_kept(signing_in):
    """A throttle or a dead network says nothing about the headers."""
    signing_in.error = remote.RemoteRateLimitedError('slow down')
    result = runner.invoke(app, ['remote', 'auth'], input=HEADERS)
    assert result.exit_code == 1
    assert paths.ytmusic_auth_file().exists()


def test_deleting_a_playlist_takes_its_merge_base_with_it(synced):
    """A base that outlives its playlist reads as a pile of local deletions.

    The next playlist to slug the same way would adopt it, and the queue would
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


@pytest.fixture
def bound(two_videos, signing_in):
    """A synced playlist on YouTube, with a base recorded for it."""
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    bind('Sunday')
    basestore.save(
        basestore.Base(
            slug='sunday',
            playlist_id='PLR',
            items=[RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')],
        )
    )
    return signing_in


def test_a_pull_applies_what_changed_on_youtube_and_keeps_what_changed_here(bound, monkeypatch):
    """The whole reconcile: a video deleted there, one added there, one deleted here."""
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid9', set_video_id='h9', title='On A Phone')]
    monkeypatch.setattr(
        ytdlp, 'fetch_video', lambda video_id, **kwargs: RemoteVideo(video_id=video_id, title='Whatever', channel='Someone')
    )
    assert runner.invoke(app, ['playlists', 'remove', 'Sunday', 'https://youtu.be/vid1']).exit_code == 0

    result = runner.invoke(app, ['remote', 'pull', '--json'])
    assert result.exit_code == 0
    payload = json.loads(result.stdout)[0]
    assert payload['pulled_in'] == ['vid9']
    assert payload['pulled_out'] == ['vid2']
    assert payload['pending_remove'] == ['vid1']
    assert local.load(local.path_for('Sunday')).video_ids == ['vid9']


def test_a_video_added_on_a_phone_keeps_the_title_youtube_gave_it(bound):
    """The mirror has never seen it, and an unlabelled entry plays as a bare URL."""
    bound.items = [
        RemoteItem(video_id='vid1', set_video_id='h1'),
        RemoteItem(video_id='vid2', set_video_id='h2'),
        RemoteItem(video_id='vid9', set_video_id='h9', title='On A Phone'),
    ]
    assert runner.invoke(app, ['remote', 'pull']).exit_code == 0
    entries = {entry.video_id: entry.title for entry in local.load(local.path_for('Sunday')).entries}
    assert entries['vid9'] == 'On A Phone'


def test_a_pull_records_the_read_as_the_new_base(bound):
    """Including the handles, which are the only copy of them anywhere."""
    bound.items = [RemoteItem(video_id='vid1', set_video_id='fresh1')]
    assert runner.invoke(app, ['remote', 'pull']).exit_code == 0

    recorded = basestore.load('sunday')
    assert recorded.video_ids == ['vid1']
    assert recorded.items[0].set_video_id == 'fresh1'
    assert recorded.playlist_id == 'PLR'


def test_pulling_twice_changes_nothing_the_second_time(bound):
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1')]
    runner.invoke(app, ['remote', 'pull'])
    first = local.load(local.path_for('Sunday')).video_ids

    result = runner.invoke(app, ['remote', 'pull', '--json'])
    assert json.loads(result.stdout)[0]['pulled_out'] == []
    assert local.load(local.path_for('Sunday')).video_ids == first


def test_a_pull_leaves_the_file_alone_when_the_base_cannot_be_read(bound):
    """Refusing is the point: read as absent, every video looks newly added here."""
    basestore.path_for('sunday').write_text('{not json')
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1')]

    result = runner.invoke(app, ['remote', 'pull'])
    assert result.exit_code == 1
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2']


def test_a_rate_limit_stops_the_run_rather_than_retrying(bound):
    bound.error = remote.RemoteRateLimitedError('slow down')
    result = runner.invoke(app, ['remote', 'pull'])
    assert result.exit_code == 1
    assert 'slow down' in result.output


def test_pulling_without_a_session_names_the_command_that_fixes_it(two_videos):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    bind('Sunday')

    result = runner.invoke(app, ['remote', 'pull'])
    assert result.exit_code == 1
    assert 'ypl remote auth' in result.output


def test_pulling_with_nothing_on_youtube_yet_succeeds_and_says_so(two_videos, signing_in):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    result = runner.invoke(app, ['remote', 'pull'])
    assert result.exit_code == 0
    assert 'Nothing to pull' in result.output


def test_naming_a_playlist_that_is_not_on_youtube_yet_is_an_error(two_videos, signing_in):
    """A sweep passes over it silently; asking for it by name has to answer."""
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])
    result = runner.invoke(app, ['remote', 'pull', 'Sunday'])
    assert result.exit_code == 1
    assert 'not on YouTube yet' in result.output


def test_a_demoted_playlist_is_not_pulled_into(bound):
    """Pulling would undo the demotion one video at a time."""
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1')]
    runner.invoke(app, ['playlists', 'demote', 'Sunday'])

    assert runner.invoke(app, ['remote', 'pull']).exit_code == 0
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2']

    named = runner.invoke(app, ['remote', 'pull', 'Sunday'])
    assert named.exit_code == 1
    assert 'local only' in named.output


@pytest.fixture
def library(monkeypatch, signing_in):
    """A mirrored account: one playlist of mine, one of someone else's, one of YouTube's.

    Channels are set because ownership is the thing the sweep filters on, and
    the fake account signs in as `Chris Birch` / `@chrisbirch`.
    """
    playlists = {
        'PLMINE': RemotePlaylist(
            playlist_id='PLMINE',
            title='DRIVE TIME',
            channel='Chris Birch',
            videos=[RemoteVideo(video_id='vid1', title='A Talk'), RemoteVideo(video_id='vid2', title='Another')],
        ),
        'PLTHEIRS': RemotePlaylist(
            playlist_id='PLTHEIRS', title='Their Mix', channel='Robert Greene', videos=[RemoteVideo(video_id='vid3', title='Theirs')]
        ),
        'LL': RemotePlaylist(
            playlist_id='LL', title='Liked videos', channel='Chris Birch', videos=[RemoteVideo(video_id='vid4', title='Liked')]
        ),
    }
    monkeypatch.setattr(ytdlp, 'fetch_playlist', lambda url, **kwargs: playlists[url.rsplit('/', 1)[-1]])
    for playlist_id in playlists:
        runner.invoke(app, ['sync', f'https://example.invalid/{playlist_id}'])
    signing_in.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')]
    return signing_in


def test_adopting_binds_a_local_file_to_the_playlist_youtube_holds(library):
    result = runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME', '--json'])
    assert result.exit_code == 0

    adopted = local.load(local.path_for('DRIVE TIME'))
    assert json.loads(result.stdout) == [{'name': 'DRIVE TIME', 'slug': 'drive-time', 'remote_id': 'PLMINE', 'video_count': 2}]
    assert adopted.remote_id == 'PLMINE'
    assert adopted.video_ids == ['vid1', 'vid2']
    assert adopted.synced


def test_an_adopted_playlist_keeps_the_name_youtube_gave_it(library):
    """The other half of the naming rule — what ypl makes is kebab, what it takes over is not."""
    runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    assert local.load(local.path_for('DRIVE TIME')).name == 'DRIVE TIME'


def test_adopting_records_what_youtube_held_so_the_first_push_has_nothing_to_do(library):
    """The base is the point of adopting rather than copying.

    Without it the first plan reads the whole playlist as added here — or, with
    an unreadable base, refuses — and neither is what taking over a playlist
    that is already correct should mean.
    """
    runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])

    assert [item.set_video_id for item in basestore.load('drive-time').items] == ['h1', 'h2']
    plan = json.loads(runner.invoke(app, ['remote', 'plan', '--json']).stdout)
    assert [(push['stale'], push['add'], push['remove'], push['moves']) for push in plan] == [(False, [], 0, 0)]


def test_an_adopted_playlist_is_one_playlist_rather_than_two(library):
    """Both stores hold it now, and a name that matched both would be ambiguous."""
    runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])

    listed = json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)
    assert [(row['title'], row['kind']) for row in listed if row['title'] == 'DRIVE TIME'] == [('DRIVE TIME', 'local')]
    assert runner.invoke(app, ['playlists', 'show', 'DRIVE TIME']).exit_code == 0


def test_a_bare_adopt_takes_over_what_the_account_owns_and_nothing_else(library):
    """Someone else's playlist would queue writes against a playlist we cannot write to,
    and YouTube's own lists are not playlists it hands over."""
    result = runner.invoke(app, ['remote', 'adopt', '--json'])
    assert result.exit_code == 0

    assert [row['remote_id'] for row in json.loads(result.stdout)] == ['PLMINE']
    assert not local.path_for('Their Mix').exists()
    assert not local.path_for('Liked videos').exists()


def test_a_playlist_someone_else_owns_is_still_adopted_when_it_is_named(library):
    """A collaborative playlist is a real case, and the channel cannot tell it apart."""
    library.items = [RemoteItem(video_id='vid3', set_video_id='h3')]
    assert runner.invoke(app, ['remote', 'adopt', 'Their Mix']).exit_code == 0
    assert local.load(local.path_for('Their Mix')).remote_id == 'PLTHEIRS'


def test_youtubes_own_lists_are_refused_even_by_name(library):
    result = runner.invoke(app, ['remote', 'adopt', 'Liked videos'])
    assert result.exit_code == 1
    assert not local.path_for('Liked videos').exists()


def test_adopting_the_same_playlist_twice_changes_nothing_and_says_why(library):
    """Resolution answers an adopted playlist with its file, so the second run
    has to say it is already here rather than that no such playlist is mirrored."""
    runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    before = local.path_for('DRIVE TIME').read_text()

    result = runner.invoke(app, ['remote', 'adopt', 'PLMINE'])
    assert result.exit_code == 1
    assert 'already a playlist here' in result.output
    assert local.path_for('DRIVE TIME').read_text() == before


def test_a_bare_adopt_with_everything_already_taken_over_succeeds_and_says_so(library):
    runner.invoke(app, ['remote', 'adopt'])
    result = runner.invoke(app, ['remote', 'adopt'])
    assert result.exit_code == 0
    assert 'Nothing to adopt' in result.output


def test_adopting_refuses_to_write_over_a_playlist_made_here(library):
    """A local playlist of the same name is authored data, not a copy to replace."""
    runner.invoke(app, ['playlists', 'create', 'drive time', '--from', 'DRIVE TIME'])

    result = runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    assert result.exit_code == 1
    assert local.load(local.path_for('DRIVE TIME')).remote_id == ''


def test_nothing_is_written_when_youtube_cannot_be_read(library):
    library.error = remote.RemoteError('network went away')

    result = runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    assert result.exit_code == 1
    assert not local.path_for('DRIVE TIME').exists()
    assert basestore.load('drive-time') is None


def set_order(name: str, video_ids: list[str]) -> None:
    """Rewrite a local playlist's order without going through a command."""
    playlist = local.load(local.path_for(name))
    by_id = {entry.video_id: entry for entry in playlist.entries}
    playlist.entries = [by_id[video_id] for video_id in video_ids]
    local.save(playlist, overwrite=True)


def test_planning_reads_and_writes_nothing(bound):
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')]
    runner.invoke(app, ['playlists', 'remove', 'Sunday', 'https://youtu.be/vid1'])

    result = runner.invoke(app, ['remote', 'plan', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)[0]['remove'] == 1
    assert [item.video_id for item in bound.items] == ['vid1', 'vid2']


def test_a_new_playlist_is_created_on_youtube_and_filled(two_videos, signing_in):
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])

    result = runner.invoke(app, ['remote', 'apply'])
    assert result.exit_code == 0
    assert signing_in.created == ['sunday']
    assert [item.video_id for item in signing_in.items] == ['vid1', 'vid2']
    assert local.load(local.path_for('Sunday')).remote_id == 'PLNEW'


def test_a_created_playlist_is_bound_before_its_videos_go_up(two_videos, signing_in):
    """Otherwise a failed fill orphans it, and the next run creates a second one."""
    signing_in.add_error = remote.RemoteError('network went away')
    runner.invoke(app, ['playlists', 'create', 'Sunday', '--from', 'More'])

    assert runner.invoke(app, ['remote', 'apply']).exit_code == 1
    assert local.load(local.path_for('Sunday')).remote_id == 'PLNEW'

    signing_in.add_error = None
    assert runner.invoke(app, ['remote', 'apply']).exit_code == 0
    assert signing_in.created == ['sunday']


def test_a_video_deleted_here_is_removed_on_youtube(bound):
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')]
    runner.invoke(app, ['playlists', 'remove', 'Sunday', 'https://youtu.be/vid1'])

    assert runner.invoke(app, ['remote', 'apply']).exit_code == 0
    assert [item.video_id for item in bound.items] == ['vid2']
    assert basestore.load('sunday').video_ids == ['vid2']


def test_a_reorder_here_becomes_moves_on_youtube(bound):
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')]
    set_order('Sunday', ['vid2', 'vid1'])

    result = runner.invoke(app, ['remote', 'apply', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)[0]['moves'] == 1
    assert [item.video_id for item in bound.items] == ['vid2', 'vid1']


def test_applying_twice_pushes_nothing_the_second_time(bound):
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid2', set_video_id='h2')]
    runner.invoke(app, ['playlists', 'remove', 'Sunday', 'https://youtu.be/vid1'])
    runner.invoke(app, ['remote', 'apply'])

    result = runner.invoke(app, ['remote', 'apply'])
    assert result.exit_code == 0
    assert 'already up to date' in result.output
    assert [item.video_id for item in bound.items] == ['vid2']


def test_a_playlist_youtube_has_changed_is_refused_until_it_is_pulled(bound):
    """Pushing on a stale base would decide a conflict without having seen it."""
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1'), RemoteItem(video_id='vid9', set_video_id='h9')]
    runner.invoke(app, ['playlists', 'remove', 'Sunday', 'https://youtu.be/vid1'])

    result = runner.invoke(app, ['remote', 'apply'])
    assert result.exit_code == 1
    assert 'ypl remote pull' in result.output
    assert [item.video_id for item in bound.items] == ['vid1', 'vid9']


def test_a_playlist_bound_to_youtube_with_no_base_is_refused_rather_than_pushed(bound):
    """No base means no way to tell a local addition from a remote deletion."""
    basestore.delete('sunday')
    bound.items = [RemoteItem(video_id='vid1', set_video_id='h1')]

    result = runner.invoke(app, ['remote', 'plan', '--json'])
    assert json.loads(result.stdout)[0]['stale'] is True


def test_a_limited_run_says_how_much_it_left(bound):
    runner.invoke(app, ['playlists', 'create', 'Monday', '--from', 'More'])
    result = runner.invoke(app, ['remote', 'plan', '--limit', '1'])
    assert result.exit_code == 0
    assert '1 of 2' in result.output


def test_signing_in_from_a_browser_needs_no_paste(signing_in, monkeypatch):
    """The whole point: no DevTools, no clipboard, no EOF on stdin."""
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'x', 'SID': 'y'})

    result = runner.invoke(app, ['remote', 'auth', '--browser', 'safari', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['account'] == 'Chris Birch'
    assert stat.S_IMODE(paths.ytmusic_auth_file().stat().st_mode) == 0o600


def test_the_configured_browser_is_used_when_no_flag_is_given(signing_in, monkeypatch):
    """cookies_from_browser already says which browser is signed in."""
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('cookies_from_browser = "firefox"\n')
    asked = []
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: asked.append(browser) or {'__Secure-3PAPISID': 'x'})

    assert runner.invoke(app, ['remote', 'auth']).exit_code == 0
    assert asked == ['firefox']


def test_a_browser_that_is_not_signed_in_fails_without_writing_anything(signing_in, monkeypatch):
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'SID': 'y'})

    result = runner.invoke(app, ['remote', 'auth', '--browser', 'safari'])
    assert result.exit_code == 1
    assert not paths.ytmusic_auth_file().exists()


def test_a_browser_yt_dlp_cannot_read_names_the_browser(signing_in, monkeypatch):
    def unreadable(browser, **kwargs):
        raise ytdlp.YtdlpFailedError('could not find safari cookies database')

    monkeypatch.setattr(ytdlp, 'browser_cookies', unreadable)

    result = runner.invoke(app, ['remote', 'auth', '--browser', 'safari'])
    assert result.exit_code == 1
    assert 'safari' in result.output


@pytest.fixture
def account(monkeypatch):
    """Two playlists on YouTube, listed the way the account feed lists them.

    `fetch_video` is stubbed as well, because a bare sync now enriches with
    whatever budget the rest of the run leaves — an unstubbed one would send the
    suite to the network.
    """
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
        ),
    )
    monkeypatch.setattr(
        ytdlp,
        'fetch_video',
        lambda video_id, **kwargs: RemoteVideo(video_id=video_id, title='A Mix', channel='Someone', duration_seconds=3600),
    )


def test_a_bare_sync_mirrors_the_whole_account(account):
    """No hunting for URLs: signing in is enough to say what you have."""
    result = runner.invoke(app, ['sync', '--browser', 'safari', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['mirrored'] == 2

    listed = json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)
    assert {row['title'] for row in listed} == {'Deep Night', 'Art'}


def test_the_configured_browser_is_the_default_for_a_bare_sync(account):
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('cookies_from_browser = "firefox"\n')
    assert runner.invoke(app, ['sync', '--json']).exit_code == 0


def test_a_bare_sync_with_no_browser_anywhere_says_what_to_do(account):
    result = runner.invoke(app, ['sync'])
    assert result.exit_code == 2
    assert '--browser' in result.output


def test_one_unreadable_playlist_does_not_cost_the_others_their_sync(account, monkeypatch):
    """A collaborative list gone private must not end the run."""

    def fetch(url, **kwargs):
        if url == 'PLa':
            raise ytdlp.YtdlpFailedError('ERROR: [youtube:tab] PLa: Playlist does not exist')
        return RemotePlaylist(playlist_id=url, title='Art', videos=[RemoteVideo(video_id='vid1', title='A Mix')])

    monkeypatch.setattr(ytdlp, 'fetch_playlist', fetch)

    result = runner.invoke(app, ['sync', '--browser', 'safari'])
    assert result.exit_code == 1
    assert 'Deep Night' in result.output
    assert [row['title'] for row in json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)] == ['Art']


def test_a_limited_sync_stops_where_it_was_told(account):
    result = runner.invoke(app, ['sync', '--browser', 'safari', '--limit', '1', '--json'])
    assert json.loads(result.stdout)['mirrored'] == 1


def test_syncing_one_playlist_by_url_still_works(synced):
    """The account sweep is the new default, not a replacement."""
    assert runner.invoke(app, ['playlists', 'show', 'Get Insights']).exit_code == 0


@pytest.fixture
def signed_in(signing_in):
    """A machine that has already signed in, without going through the paste.

    The backend is faked either way; what this adds is the session file, which
    is what `ypl sync` looks at to decide whether this machine can write.
    """
    paths.ytmusic_auth_file().parent.mkdir(parents=True, exist_ok=True)
    paths.ytmusic_auth_file().write_text('{}')
    return signing_in


def test_one_sync_mirrors_adopts_and_enriches_with_nothing_else_typed(account, signed_in):
    """The whole point: after signing in, one command leaves nothing owed."""
    run = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)

    assert (run['mirrored'], sorted(run['adopted']), run['enriched'], run['unenriched']) == (2, ['Art', 'Deep Night'], 2, 0)
    assert local.load(local.path_for('Deep Night')).remote_id == 'PLa'
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['unenriched'] == 0


def test_a_sync_without_a_session_mirrors_and_says_so_rather_than_failing(account):
    """Signing in is per-machine setup, and a machine that has not must still sync."""
    result = runner.invoke(app, ['sync', '--browser', 'safari', '--json'])
    assert result.exit_code == 0

    run = json.loads(result.stdout)
    assert (run['signed_in'], run['mirrored'], run['adopted']) == (False, 2, [])


def test_a_run_out_of_budget_stops_and_the_next_one_carries_on(account, signed_in):
    """Bounded rather than open-ended, which is only safe because it resumes.

    The ceiling is what lets this run unattended: a library converges over
    several runs and no single one has to be the one that finishes. Counted in
    requests rather than seconds so the ceiling lands in the same place every
    time — the mirror alone spends more than one.
    """
    connection = db.connect()
    stopped = service.sync_everything(connection, 'safari', backend=signed_in(None), budget=throttle.Budget(requests=1))
    assert (stopped.adopted, stopped.enriched, stopped.stopped) == ([], 0, 'budget')

    finished = service.sync_everything(connection, 'safari', backend=signed_in(None), budget=throttle.Budget())
    assert (sorted(finished.adopted), finished.unenriched) == (['Art', 'Deep Night'], 0)


def test_a_video_that_will_never_read_is_marked_rather_than_retried_forever(account, signed_in, monkeypatch):
    """The failure mode of an unattended enrich: spending every run on the dead."""
    monkeypatch.setattr(
        ytdlp, 'fetch_video', lambda video_id, **kwargs: (_ for _ in ()).throw(ytdlp.YtdlpFailedError('ERROR: Video unavailable'))
    )
    first = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)
    assert (first['enriched'], first['skipped'], first['unenriched']) == (0, 2, 0)

    second = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)
    assert second['skipped'] == 0


def test_a_video_that_might_read_next_time_is_left_in_the_queue(account, signed_in, monkeypatch):
    """A timeout is not a dead video, and treating it as one loses the tracklist."""
    monkeypatch.setattr(ytdlp, 'fetch_video', lambda video_id, **kwargs: (_ for _ in ()).throw(ytdlp.YtdlpFailedError('ERROR: timed out')))
    run = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)

    assert (run['skipped'], run['unenriched']) == (2, 2)


def test_a_deleted_playlist_is_not_dragged_back_by_the_next_sync(account, signed_in):
    """Deleting one is a statement about wanting it here, and sync has to hear it."""
    runner.invoke(app, ['sync', '--browser', 'safari'])
    assert runner.invoke(app, ['playlists', 'delete', 'Deep Night', '--yes']).exit_code == 0

    run = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)
    assert run['adopted'] == []
    assert not local.path_for('Deep Night').exists()


def test_adopting_a_declined_playlist_by_name_undoes_the_refusal(account, signed_in):
    runner.invoke(app, ['sync', '--browser', 'safari'])
    runner.invoke(app, ['playlists', 'delete', 'Deep Night', '--yes'])

    assert runner.invoke(app, ['remote', 'adopt', 'Deep Night']).exit_code == 0
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['declined'] == 0


def test_an_edit_here_reaches_youtube_on_the_next_sync_with_nothing_else_typed(account, signed_in):
    """The other direction, and the reason the loop is worth automating at all."""
    runner.invoke(app, ['sync', '--browser', 'safari'])
    runner.invoke(app, ['playlists', 'remove', 'Deep Night', 'PLavid'])

    runner.invoke(app, ['sync', '--browser', 'safari'])
    assert [item.video_id for item in signed_in.items] == []


def test_the_run_a_timer_leaves_behind_is_readable_afterwards(account, signed_in):
    """A sync nobody watches is only as good as what it wrote down."""
    runner.invoke(app, ['sync', '--browser', 'safari'])

    status = json.loads(runner.invoke(app, ['status', '--json']).stdout)
    assert status['last_sync']
    assert sorted(status['last_run']['adopted']) == ['Art', 'Deep Night']


@pytest.fixture
def linux_timer(monkeypatch):
    """A machine whose timer is systemd's, whichever one is running the suite."""
    monkeypatch.setattr(schedule, 'is_macos', lambda: False)
    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')


def test_syncing_once_leaves_the_machine_syncing_itself(account, signed_in, linux_timer):
    """No command installs this. Running the sync at all is what sets it up."""
    result = runner.invoke(app, ['sync', '--browser', 'safari'])
    assert result.exit_code == 0

    assert 'OnUnitActiveSec=30min' in schedule.timer_path().read_text()
    assert 'ExecStart=/usr/local/bin/ypl sync' in schedule.service_path().read_text()
    assert 'every 30 minutes' in result.output
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['scheduled'] is True


def test_a_second_sync_neither_reinstalls_the_timer_nor_mentions_it(account, signed_in, linux_timer):
    runner.invoke(app, ['sync', '--browser', 'safari'])
    result = runner.invoke(app, ['sync', '--browser', 'safari'])

    assert 'every 30 minutes' not in result.output


def test_turning_background_syncing_off_takes_the_timer_away_on_the_next_run(account, signed_in, linux_timer):
    """Off is a setting, not a verb: nothing installed it, so nothing uninstalls it."""
    runner.invoke(app, ['sync', '--browser', 'safari'])
    paths.config_file().write_text('background_sync = false\n')

    runner.invoke(app, ['sync', '--browser', 'safari'])
    assert not schedule.timer_path().exists()
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['scheduled'] is False


def test_a_machine_that_will_not_take_a_timer_still_syncs(account, signed_in, monkeypatch):
    """The run happening now matters more than the ones that would follow it."""
    monkeypatch.setattr(schedule, 'executable', lambda: (_ for _ in ()).throw(schedule.ScheduleError('no ypl on PATH')))

    result = runner.invoke(app, ['sync', '--browser', 'safari'])
    assert result.exit_code == 0
    assert sorted(json.loads(runner.invoke(app, ['status', '--json']).stdout)['last_run']['adopted']) == ['Art', 'Deep Night']


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


def test_a_timer_naming_a_ypl_that_moved_is_replaced_on_the_next_run(account, signed_in, monkeypatch):
    """A unit that fires and fails still reports as scheduled, which is the worst
    of the three states — this is how a timer pointed into a deleted checkout
    went on being reported as healthy."""
    monkeypatch.setattr(schedule, 'is_macos', lambda: False)
    monkeypatch.setattr(schedule, 'executable', lambda: '/somewhere/else/ypl')
    runner.invoke(app, ['sync', '--browser', 'safari'])
    assert 'ExecStart=/somewhere/else/ypl sync' in schedule.service_path().read_text()

    monkeypatch.setattr(schedule, 'executable', lambda: '/usr/local/bin/ypl')
    assert runner.invoke(app, ['sync', '--browser', 'safari']).exit_code == 0

    assert 'ExecStart=/usr/local/bin/ypl sync' in schedule.service_path().read_text()


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


def test_the_checkouts_ypl_is_still_used_when_it_is_the_only_one(tmp_path, monkeypatch):
    """The fallback the docstring promises: a machine with nothing installed."""
    checkout = tmp_path / 'checkout' / '.venv' / 'bin'
    checkout.mkdir(parents=True)
    (checkout / 'ypl').touch(mode=0o755)
    monkeypatch.setenv('VIRTUAL_ENV', str(tmp_path / 'checkout' / '.venv'))
    monkeypatch.setenv('PATH', str(checkout))

    assert schedule.executable() == str(checkout / 'ypl')


def test_a_sync_on_a_mac_writes_its_agent_inside_the_isolated_home(account, signed_in, tmp_path, monkeypatch):
    """The guard on `isolated_home`, and the bug it was written for.

    This suite installed a real launch agent on the machine running it — loaded,
    firing every thirty minutes, and pointed at a checkout — because the agent
    path comes from HOME and only the XDG variables were redirected.
    """
    monkeypatch.setattr(schedule, 'is_macos', lambda: True)

    runner.invoke(app, ['sync', '--browser', 'safari'])

    assert schedule.agent_path().is_relative_to(tmp_path)
    assert schedule.agent_path().exists()


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


def test_the_current_playlist_is_remembered_between_commands(current):
    result = runner.invoke(app, ['use', '--json'])
    assert json.loads(result.stdout)['playlist'] == 'sunday'


def test_dropping_takes_out_what_is_playing_with_no_id_typed(current, monkeypatch):
    playing(monkeypatch, 'vid1')

    result = runner.invoke(app, ['drop', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_id'] == 'vid1'
    assert local.load(local.path_for('Sunday')).video_ids == ['vid2']


def test_dropping_skips_it_in_mpv_too(current, monkeypatch):
    """Continuing to play what you just deleted is not what dropping it meant."""
    playing(monkeypatch, 'vid1')
    sent = []
    monkeypatch.setattr(player, 'command', lambda socket_path, arguments: sent.append(arguments))

    runner.invoke(app, ['drop'])
    assert sent == [['playlist-next']]


def test_keep_playing_leaves_mpv_alone(current, monkeypatch):
    playing(monkeypatch, 'vid1')
    sent = []
    monkeypatch.setattr(player, 'command', lambda socket_path, arguments: sent.append(arguments))

    runner.invoke(app, ['drop', '--keep-playing'])
    assert sent == []


def test_a_fragment_of_a_title_is_enough_when_nothing_is_playing_here(current, monkeypatch):
    """The answer for playback in a browser, where no socket can say what is on."""
    playing(monkeypatch, None)

    result = runner.invoke(app, ['drop', 'another', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_id'] == 'vid2'


def test_a_fragment_matching_two_videos_lists_them_rather_than_guessing(current, monkeypatch):
    playing(monkeypatch, None)
    runner.invoke(app, ['playlists', 'add', 'Sunday', 'https://youtu.be/vid9'])

    result = runner.invoke(app, ['drop', 'a'])
    assert result.exit_code == 2
    assert 'A Talk' in result.output
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2', 'vid9']


def test_dropping_with_nothing_playing_and_no_fragment_says_what_to_type(current, monkeypatch):
    playing(monkeypatch, None)

    result = runner.invoke(app, ['drop'])
    assert result.exit_code == 2
    assert 'name part of a title' in result.output


def test_later_moves_what_is_playing_down_the_playlist(current, monkeypatch):
    playing(monkeypatch, 'vid1')

    result = runner.invoke(app, ['later', '--json'])
    assert json.loads(result.stdout)['position'] == 2
    assert local.load(local.path_for('Sunday')).video_ids == ['vid2', 'vid1']


def test_sooner_is_later_in_reverse(current, monkeypatch):
    playing(monkeypatch, 'vid2')

    runner.invoke(app, ['sooner'])
    assert local.load(local.path_for('Sunday')).video_ids == ['vid2', 'vid1']


def test_moving_past_the_end_stops_at_the_end(current, monkeypatch):
    """`ypl later` on the last video is a reasonable thing to type without checking."""
    playing(monkeypatch, 'vid2')

    result = runner.invoke(app, ['later', '-n', '99', '--json'])
    assert json.loads(result.stdout)['position'] == 2
    assert local.load(local.path_for('Sunday')).video_ids == ['vid1', 'vid2']


def test_the_verbs_refuse_when_no_playlist_has_been_chosen(two_videos, monkeypatch):
    playing(monkeypatch, 'vid1')
    result = runner.invoke(app, ['drop'])
    assert result.exit_code == 2
    assert 'ypl use' in result.output


def test_a_video_playing_from_a_different_playlist_says_so(current, monkeypatch):
    playing(monkeypatch, 'vid9')

    result = runner.invoke(app, ['drop'])
    assert result.exit_code == 1
    assert 'sunday' in result.output


def test_editing_with_no_name_edits_the_current_playlist(current):
    result = runner.invoke(app, ['playlists', 'edit', '--json'], input='https://youtu.be/vid2\n')
    assert result.exit_code == 0
    assert json.loads(result.stdout)['name'] == 'sunday'


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


def test_the_library_hands_over_the_artists_inside_each_mix(enriched_library):
    """The whole point: a mix cannot be chosen from its title."""
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json']).stdout)
    by_id = {row['video_id']: row for row in payload}
    assert by_id['vid1']['artists'] == ['Black Coffee']
    assert by_id['vid2']['artists'] == ['Bonobo']


def test_the_library_says_which_of_your_playlists_hold_each_mix(enriched_library):
    """Your own playlist names are a label you already applied."""
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json']).stdout)
    assert payload[0]['playlists'] == ['More']


def test_the_library_can_be_filtered_to_mixes_long_enough_for_a_work_set(enriched_library):
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json', '--min-minutes', '60']).stdout)
    assert [row['video_id'] for row in payload] == ['vid1']


def test_the_library_can_be_filtered_by_an_artist_inside_the_mix(enriched_library):
    """Not the channel — someone who appears in a tracklist an hour in."""
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json', '--artist', 'bonobo']).stdout)
    assert [row['video_id'] for row in payload] == ['vid2']


def test_the_library_sorts_longest_first_so_a_six_hour_set_is_choosable(enriched_library):
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json']).stdout)
    assert [row['video_id'] for row in payload] == ['vid1', 'vid2']


def test_position_is_not_a_sort_the_library_offers(enriched_library):
    result = runner.invoke(app, ['videos', 'list', '--sort', 'position'])
    assert result.exit_code == 2


def test_the_library_carries_urls_so_a_selection_pipes_straight_into_a_playlist(enriched_library):
    """This is the whole curation loop: list, choose, create."""
    payload = json.loads(runner.invoke(app, ['videos', 'list', '--json', '--min-minutes', '60']).stdout)
    chosen = '\n'.join(row['url'] for row in payload)

    result = runner.invoke(app, ['playlists', 'create', 'Uptempo Work', '--json'], input=chosen)
    assert result.exit_code == 0
    assert json.loads(result.stdout)['video_count'] == 1


def test_enrich_all_does_not_stop_at_the_batch_size(two_videos, monkeypatch):
    monkeypatch.setattr(ytdlp, 'fetch_video', lambda video_id, **kwargs: RemoteVideo(video_id=video_id, title='A Mix', channel='Cercle'))
    paths.config_file().parent.mkdir(parents=True, exist_ok=True)
    paths.config_file().write_text('enrich_batch_size = 1\n')

    result = runner.invoke(app, ['enrich', '--all', '--json'])
    assert json.loads(result.stdout)['enriched'] == 2


def test_enrich_stops_when_youtube_pushes_back(two_videos, monkeypatch):
    """Answering "slow down" with more requests is the worst available move."""

    def rate_limited(video_id, **kwargs):
        raise ytdlp.YtdlpRateLimitedError('ERROR: Sign in to confirm you are not a bot')

    monkeypatch.setattr(ytdlp, 'fetch_video', rate_limited)

    result = runner.invoke(app, ['enrich', '--all'])
    assert result.exit_code == 1
    assert 'pushing back' in result.output
    assert 'request_interval_seconds' in result.output


def test_enrich_keeps_what_it_managed_before_being_stopped(two_videos, monkeypatch):
    """Stopping costs time and nothing else — the run is resumable by design."""
    calls = []

    def one_then_limited(video_id, **kwargs):
        calls.append(video_id)
        if len(calls) > 1:
            raise ytdlp.YtdlpRateLimitedError('HTTP Error 429: Too Many Requests')
        return RemoteVideo(video_id=video_id, title='A Mix', channel='Cercle')

    monkeypatch.setattr(ytdlp, 'fetch_video', one_then_limited)

    result = runner.invoke(app, ['enrich', '--all', '--json'])
    assert result.exit_code == 1
    payload = json.loads(result.stdout)
    assert payload['enriched'] == 1
    assert payload['rate_limited'] is True


def test_one_unreadable_video_is_skipped_rather_than_stopping_the_run(two_videos, monkeypatch):
    """A video failing on its own merits is not YouTube pushing back."""

    def one_bad(video_id, **kwargs):
        if video_id == 'vid1':
            raise ytdlp.YtdlpFailedError('ERROR: Video unavailable')
        return RemoteVideo(video_id=video_id, title='A Mix', channel='Cercle')

    monkeypatch.setattr(ytdlp, 'fetch_video', one_bad)

    result = runner.invoke(app, ['enrich', '--all', '--json'])
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload['enriched'] == 1
    assert payload['failed'] == 1


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


def test_titles_inside_the_current_playlist_complete_for_the_id_free_verbs(current):
    assert main.complete_entry('anot') == ['Another']


def test_completion_answers_with_nothing_rather_than_raising(monkeypatch):
    """It runs on a keystroke: a traceback would land in the middle of the line."""
    monkeypatch.setattr(db, 'connect', lambda: (_ for _ in ()).throw(RuntimeError('database is locked')))

    assert main.complete_playlist('any') == []
    assert main.complete_entry('any') == []


def test_completion_offers_nothing_when_no_playlist_is_current(two_videos):
    assert main.complete_entry('') == []


def test_signing_in_from_a_browser_teaches_the_reads_which_browser_it_was(signing_in, monkeypatch):
    """Two subsystems, one login. Signing in once should not have to be told twice."""
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'x'})
    runner.invoke(app, ['remote', 'auth', '--browser', 'safari'])

    assert session.browser() == 'safari'
    assert main.reading_browser(config.Config()) == 'safari'


def test_a_bare_sync_uses_the_browser_signed_in_with(account, signing_in, monkeypatch):
    """The bug this fixes: logged in, and `ypl sync` said it had nothing to read."""
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'x'})
    runner.invoke(app, ['remote', 'auth', '--browser', 'firefox'])

    result = runner.invoke(app, ['sync', '--json'])
    assert result.exit_code == 0
    assert json.loads(result.stdout)['mirrored'] == 2


def test_a_browser_that_worked_for_a_sync_is_remembered_too(account):
    """Naming it once is enough, whichever command was told."""
    assert runner.invoke(app, ['sync', '--browser', 'safari']).exit_code == 0
    assert session.browser() == 'safari'
    assert runner.invoke(app, ['sync', '--json']).exit_code == 0


def test_the_config_still_wins_over_what_was_remembered(monkeypatch):
    """It is the setting a person wrote down; nothing inferred should override it."""
    monkeypatch.setattr(session, 'browser', lambda: 'safari')
    assert main.reading_browser(config.Config(cookies_from_browser='firefox')) == 'firefox'


def test_a_second_sync_leaves_the_first_one_alone(account, signed_in, linux_timer):
    """A timer firing onto a run already going must not double it.

    Two syncs at once would adopt the same playlists twice and write the same
    files from both, and the timer exists precisely to start runs nobody is
    watching for.
    """
    with runlock.held() as mine:
        assert mine
        result = runner.invoke(app, ['sync', '--browser', 'safari'])

    assert result.exit_code == 0
    assert 'already running' in result.output
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['last_sync'] is None


def test_the_lock_is_released_when_the_run_ends(account, signed_in, linux_timer):
    """Held by the kernel rather than by a pid file, so a crash cannot wedge it."""
    assert runner.invoke(app, ['sync', '--browser', 'safari']).exit_code == 0

    with runlock.held() as mine:
        assert mine


def test_status_says_when_a_sync_is_going_on_right_now(account, signed_in):
    """A background run you cannot see mid-flight looks the same as a broken one."""
    assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['running'] is False

    with runlock.held():
        assert json.loads(runner.invoke(app, ['status', '--json']).stdout)['running'] is True


def test_status_counts_what_a_run_did_rather_than_listing_it():
    """Seventeen playlist names in a status line is a Python list on your screen."""
    synclog.record({'adopted': ['BEST', 'Art', 'Trip'], 'reconciled': [], 'pushed': ['Deep Night']})

    output = runner.invoke(app, ['status']).output
    assert '3 adopted, 1 pushed' in output


def test_status_stops_short_of_reprinting_a_whole_failed_run():
    """A run where every playlist failed pushed the answer off the screen."""
    synclog.record({'failures': [f'playlist {index}: unreadable' for index in range(9)]})

    output = runner.invoke(app, ['status']).output
    assert 'playlist 0: unreadable' in output
    assert 'and 4 more — ypl status --json' in output
    assert 'playlist 8' not in output
    # The lines the whole command exists to answer still have to be below it.
    assert 'Unenriched' in output


def test_a_signed_out_session_stops_the_remote_half_with_one_message(account, signed_in):
    """The shape this took in the wild: forty failures, each four kilobytes of
    YouTube JSON, and nowhere among them the fact that explains all of it."""
    signed_in.error = remote.RemoteAuthError('answered as a signed-out visitor')

    run = json.loads(runner.invoke(app, ['sync', '--browser', 'safari', '--json']).stdout)
    assert (run['signed_in'], run['adopted']) == (False, [])
    assert len(run['failures']) == 1
    assert 'ypl remote auth --replace' in run['failures'][0]


def test_a_read_with_no_slot_handles_is_not_adopted(library):
    """A signed-out read returns the playlist and no handles for any of it.

    Binding to that records a playlist that looks adopted and can never be
    pushed, so it has to be refused before anything is written.
    """
    library.items = [RemoteItem(video_id='vid1', set_video_id=''), RemoteItem(video_id='vid2', set_video_id='')]

    result = runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    assert result.exit_code == 1
    assert 'not signed in' in result.output
    assert not local.path_for('DRIVE TIME').exists()


def test_a_base_with_no_handles_is_read_again_rather_than_trusted(library):
    """Self-repair for the bases a signed-out run already wrote: they match the
    mirror exactly, so nothing else would ever ask for them again."""
    runner.invoke(app, ['remote', 'adopt', 'DRIVE TIME'])
    stored = basestore.load('drive-time')
    basestore.save(basestore.Base(slug='drive-time', playlist_id='PLMINE', items=[RemoteItem(v.video_id, '') for v in stored.items]))

    assert service.needs_reconcile(db.connect(), local.load(local.path_for('DRIVE TIME'))) is True


def test_the_session_is_rebuilt_from_the_browser_before_a_sync(account, signed_in, linux_timer, monkeypatch):
    """A stored session is a photograph of something that moves.

    Google rotates the session cookies while you stay signed in, so signing in
    once only means once if the file is rebuilt from the browser each run —
    which is why the first real run mirrored all afternoon while the write path
    had quietly become a signed-out visitor.
    """
    session.remember_browser('safari')
    monkeypatch.setattr(ytdlp, 'browser_cookies', lambda browser, **kwargs: {'__Secure-3PAPISID': 'fresh', 'SID': 'x'})
    paths.ytmusic_auth_file().write_text('{"cookie": "stale"}')

    assert runner.invoke(app, ['sync', '--browser', 'safari']).exit_code == 0
    assert 'fresh' in paths.ytmusic_auth_file().read_text()


def test_a_browser_that_cannot_be_read_leaves_the_stored_session_alone(monkeypatch):
    """Stale beats absent: it may still work, and nothing else can sign in."""
    paths.ytmusic_auth_file().parent.mkdir(parents=True, exist_ok=True)
    paths.ytmusic_auth_file().write_text('{"cookie": "stored"}')
    monkeypatch.setattr(
        ytdlp, 'browser_cookies', lambda browser, **kwargs: (_ for _ in ()).throw(ytdlp.YtdlpFailedError('safari is locked'))
    )

    assert ytmusic.refresh_session('safari', paths.ytmusic_auth_file()) is False
    assert 'stored' in paths.ytmusic_auth_file().read_text()
