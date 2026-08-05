"""The machine contract: exit codes, and which stream carries what.

`forge` and anything else shelling out sees only these two signals, so they are
the API rather than a nicety.
"""

import json

import pytest
from typer.testing import CliRunner

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
    import tomllib

    result = runner.invoke(app, ['config', 'example'])
    assert result.exit_code == 0
    tomllib.loads(result.stdout)


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
