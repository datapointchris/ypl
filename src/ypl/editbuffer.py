"""The text a playlist becomes while it is being edited.

Modelled on `git rebase -i`, and for the same reason: rearranging a list is
something text editors are already extremely good at, and any bespoke interface
for it starts out worse than the one already open. One line per video, the id
first so it can be read back, the title after it so the line means something.
Move lines to reorder, delete lines to remove, save to apply.

The id is on the line but never typed. That is the whole point — identifying a
video by pasting eleven characters is what made editing a playlist while it was
playing intolerable, and no amount of extra commands fixes it. The editor
already knows how to move a line.

The format is pure — text in, ids out — so it can be tested by writing a
string. `open_in_editor` is the one effectful thing here, and it lives beside
the format rather than in the command layer because the two are one idea.
"""

import os
import shlex
import subprocess
import tempfile
from pathlib import Path

COMMENT = '#'

# $VISUAL first: the convention is that $EDITOR may be a line editor for dumb
# terminals and $VISUAL is the full-screen one, and a playlist is not something
# to rearrange in ed.
EDITOR_VARIABLES = ('VISUAL', 'EDITOR')
DEFAULT_EDITOR = 'vi'

SUFFIX = '.ypl'

INSTRUCTIONS = """\
# Reorder these lines to reorder the playlist.
# Delete a line to remove that video from it.
# Add a line with a URL or id to add one.
#
# Lines starting with # are ignored. Save an empty buffer to abort."""


class EditBufferError(ValueError):
    """A line in an edited buffer is not something that can be applied.

    Carries the line number, because the answer to "which one" is the whole
    difference between fixing it and reopening the editor to hunt.
    """

    def __init__(self, line_number: int, line: str, reason: str):
        self.line_number = line_number
        self.line = line
        self.reason = reason
        super().__init__(f'line {line_number}: {reason}: {line.strip()!r}')


class EditorFailedError(RuntimeError):
    """The editor could not be started, or exited badly."""


def editor_command() -> list[str]:
    """What to run, split the way a shell would.

    Split rather than executed as a string so `EDITOR="code --wait"` works,
    which is the form half the editors on a machine are configured with.
    """
    for variable in EDITOR_VARIABLES:
        configured = os.environ.get(variable)
        if configured and configured.strip():
            return shlex.split(configured)
    return [DEFAULT_EDITOR]


def open_in_editor(text: str) -> str | None:
    """Edit some text, returning it changed — or None when it was not.

    None rather than the identical text, because "you opened it and closed it
    again" is a different outcome from "you rearranged it back to how it was",
    and only the first should print nothing.
    """
    command = editor_command()
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / f'playlist{SUFFIX}'
        path.write_text(text)
        try:
            # Inherits the terminal deliberately: an editor with its stdout
            # captured draws its interface into a string.
            result = subprocess.run([*command, str(path)])  # noqa: S603
        except OSError as error:
            raise EditorFailedError(f'could not start {command[0]}: {error}') from error
        if result.returncode != 0:
            raise EditorFailedError(f'{command[0]} exited {result.returncode} — nothing was changed')
        edited = path.read_text()
    return None if edited == text else edited


def duration_words(seconds: int | None) -> str:
    if not seconds:
        return ''
    hours, remainder = divmod(int(seconds), 3600)
    minutes, secs = divmod(remainder, 60)
    return f'{hours}:{minutes:02d}:{secs:02d}' if hours else f'{minutes}:{secs:02d}'


def render(name: str, rows: list[dict]) -> str:
    """The buffer for one playlist.

    Columns are padded rather than tab-separated so the titles line up in any
    editor, and the id column is fixed-width because YouTube ids are.
    """
    total = duration_words(sum(row.get('duration_seconds') or 0 for row in rows))
    lines = [f'{COMMENT} {name} — {len(rows)} videos, {total}'.rstrip(), COMMENT, INSTRUCTIONS, COMMENT]
    for row in rows:
        title = row.get('title') or ''
        channel = row.get('channel') or ''
        label = f'{channel} - {title}' if channel and title else title or channel
        lines.append(f'{row["video_id"]:<12} {label:<60} {duration_words(row.get("duration_seconds"))}'.rstrip())
    return '\n'.join(lines) + '\n'


def parse(text: str, extract_id, known: frozenset[str] = frozenset()) -> list[str]:
    """The video ids an edited buffer asks for, in the order it puts them.

    `extract_id` is passed in rather than imported so this module stays free of
    everything else — it is the same URL-or-bare-id reader the rest of the CLI
    uses, so a line pasted from a browser works exactly like one that was
    already here.

    `known` is what the buffer was rendered from, and a token in it is taken as
    given however odd it looks. Without that, an id that does not match the
    eleven-character rule — a hand-edited M3U, a form YouTube has not used in
    fifteen years — would be written into the buffer and then rejected when the
    same buffer was read back, which is a tool refusing to accept its own
    output. Anything else has to look like an id, so that a line of prose is
    still caught rather than filed as a video.

    Duplicates are allowed through: a playlist may legitimately hold the same
    mix twice, and the buffer is the user saying what they want.
    """
    video_ids = []
    for number, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped or stripped.startswith(COMMENT):
            continue
        token = stripped.split()[0]
        video_id = token if token in known else extract_id(token)
        if not video_id:
            raise EditBufferError(number, line, 'does not start with a video id or URL')
        video_ids.append(video_id)
    return video_ids
