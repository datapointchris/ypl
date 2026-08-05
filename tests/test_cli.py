"""The machine contract: exit codes, and which stream carries what.

`forge` and anything else shelling out sees only these two signals, so they are
the API rather than a nicety.
"""

import json
import tomllib

import pytest
from typer.testing import CliRunner

from ypl import paths
from ypl import service
from ypl import ytdlp
from ypl.main import app
from ypl.models import RemotePlaylist
from ypl.models import RemoteVideo

runner = CliRunner()


@pytest.fixture(autouse=True)
def isolated_home(tmp_path, monkeypatch):
    """Keep the tests off the real mirror, config and playlist directory."""
    monkeypatch.setenv('XDG_STATE_HOME', str(tmp_path / 'state'))
    monkeypatch.setenv('XDG_CONFIG_HOME', str(tmp_path / 'config'))
    monkeypatch.setenv('XDG_DATA_HOME', str(tmp_path / 'data'))


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


@pytest.mark.parametrize('command', [['playlists'], ['videos'], ['config']])
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


def test_the_sort_names_the_cli_accepts_are_the_ones_the_service_implements():
    assert set(service.SORT_CLAUSES) == {'position', 'oldest', 'newest', 'random'}


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


def test_both_kinds_of_playlist_are_listed_together(built):
    payload = json.loads(runner.invoke(app, ['playlists', 'list', '--json']).stdout)
    assert {row['title']: row['kind'] for row in payload} == {'Get Insights': 'remote', 'Sunday': 'local'}


@pytest.mark.parametrize(('source', 'expected'), [('local', ['Sunday']), ('remote', ['Get Insights'])])
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
