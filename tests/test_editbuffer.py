"""The buffer a playlist becomes in $EDITOR, and what comes back out of it.

The format is the contract: whatever a person does to those lines has to mean
something exact when it is read back. Reordering, deleting and pasting are the
three things worth pinning, plus the two mistakes — a mangled line and an empty
buffer — where doing the wrong thing costs an authored playlist.
"""

import pytest

from ypl import editbuffer
from ypl import m3u

ROWS = [
    {'video_id': 'aaaaaaaaaaa', 'title': 'At Citadelle', 'channel': 'Cercle', 'duration_seconds': 7533},
    {'video_id': 'bbbbbbbbbbb', 'title': 'At Salle Wagram', 'channel': 'Cercle', 'duration_seconds': 6482},
]


def ids(text: str) -> list[str]:
    return editbuffer.parse(text, m3u.video_id_from)


def test_every_video_gets_a_line_that_says_what_it_is():
    rendered = editbuffer.render('Sunday', ROWS)
    assert 'aaaaaaaaaaa' in rendered
    assert 'Cercle - At Citadelle' in rendered
    assert '2:05:33' in rendered


def test_the_header_says_what_is_being_edited_and_how_long_it_runs():
    header = editbuffer.render('Sunday', ROWS).splitlines()[0]
    assert 'Sunday' in header
    assert '2 videos' in header
    assert '3:53:35' in header


def test_a_rendered_buffer_read_straight_back_is_the_same_playlist():
    """The round trip has to be exact, or opening and saving would rearrange things."""
    assert ids(editbuffer.render('Sunday', ROWS)) == ['aaaaaaaaaaa', 'bbbbbbbbbbb']


def test_moving_a_line_moves_the_video():
    lines = editbuffer.render('Sunday', ROWS).splitlines()
    reordered = '\n'.join([*lines[:-2], lines[-1], lines[-2]])
    assert ids(reordered) == ['bbbbbbbbbbb', 'aaaaaaaaaaa']


def test_deleting_a_line_removes_the_video():
    lines = [line for line in editbuffer.render('Sunday', ROWS).splitlines() if 'aaaaaaaaaaa' not in line]
    assert ids('\n'.join(lines)) == ['bbbbbbbbbbb']


def test_a_pasted_url_is_a_video_added():
    """Pasting a link from a browser is the obvious thing to try, so it works."""
    text = editbuffer.render('Sunday', ROWS) + 'https://www.youtube.com/watch?v=ccccccccccc\n'
    assert ids(text)[-1] == 'ccccccccccc'


def test_the_same_video_twice_is_allowed_through():
    """A playlist may legitimately hold one mix twice; the buffer is the answer."""
    assert ids('aaaaaaaaaaa one\naaaaaaaaaaa one again\n') == ['aaaaaaaaaaa', 'aaaaaaaaaaa']


def test_comments_and_blank_lines_are_ignored():
    assert ids('# a comment\n\n   \naaaaaaaaaaa At Citadelle\n') == ['aaaaaaaaaaa']


def test_a_line_that_is_not_a_video_names_its_line_number():
    """These files are hand-edited; "line 4" is the difference between a fix and a hunt."""
    with pytest.raises(editbuffer.EditBufferError) as error:
        ids('aaaaaaaaaaa fine\nwhat is this\n')
    assert error.value.line_number == 2


def test_an_empty_buffer_yields_nothing_rather_than_erroring():
    """The command reads this as an abort, the way `git rebase -i` does."""
    assert ids(editbuffer.render('Sunday', ROWS).splitlines()[0]) == []


def test_the_editor_is_visual_then_editor_then_vi(monkeypatch):
    monkeypatch.delenv('VISUAL', raising=False)
    monkeypatch.delenv('EDITOR', raising=False)
    assert editbuffer.editor_command() == ['vi']

    monkeypatch.setenv('EDITOR', 'nvim')
    assert editbuffer.editor_command() == ['nvim']

    monkeypatch.setenv('VISUAL', 'code --wait')
    assert editbuffer.editor_command() == ['code', '--wait']


def test_an_editor_that_changes_nothing_reports_nothing(monkeypatch):
    """Distinct from rearranging back to the original, which is a real save."""
    monkeypatch.setenv('EDITOR', 'true')
    assert editbuffer.open_in_editor('some text\n') is None


def test_what_the_editor_wrote_is_what_comes_back(monkeypatch):
    monkeypatch.setenv('EDITOR', 'sh -c \'echo edited > "$1"\' --')
    assert editbuffer.open_in_editor('some text\n') == 'edited\n'


def test_an_editor_that_exits_badly_is_an_error_rather_than_an_empty_playlist(monkeypatch):
    """`:cq` out of vim has to mean abort, not delete every video."""
    monkeypatch.setenv('EDITOR', 'false')
    with pytest.raises(editbuffer.EditorFailedError):
        editbuffer.open_in_editor('some text\n')


def test_an_editor_that_is_not_installed_says_so(monkeypatch):
    monkeypatch.setenv('EDITOR', 'definitely-not-an-editor-99')
    with pytest.raises(editbuffer.EditorFailedError):
        editbuffer.open_in_editor('some text\n')


def test_an_id_the_playlist_already_holds_is_taken_as_given():
    """A tool must accept its own output: whatever is in the file gets rendered,
    so whatever is in the file has to parse back, however unlike an id it looks.
    """
    assert editbuffer.parse('odd-one x\n', m3u.video_id_from, frozenset({'odd-one'})) == ['odd-one']


def test_a_line_of_prose_is_still_caught_when_other_ids_are_known():
    with pytest.raises(editbuffer.EditBufferError):
        editbuffer.parse('what is this\n', m3u.video_id_from, frozenset({'odd-one'}))
