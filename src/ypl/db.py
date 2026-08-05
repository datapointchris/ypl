"""Connection handling for the local mirror."""

import sqlite3
from importlib import resources
from pathlib import Path

from ypl import paths

SCHEMA_FILE = 'schema.sql'
INDEXES_FILE = 'indexes.sql'

# Long enough to outlast a write, because the writer is now a background timer
# and the reader is someone at a prompt. Five seconds — Python's default — is
# the wrong trade once one side is unattended: the run that fails is invisible
# until `ypl status` is read, while the command that fails is in your face.
BUSY_TIMEOUT_SECONDS = 30


def read_sql(filename: str) -> str:
    return resources.files('ypl').joinpath(filename).read_text()


def connect(database: Path | None = None) -> sqlite3.Connection:
    """Open the mirror, creating anything it is missing.

    Both files run every time rather than only on a fresh database, which is
    what lets a table added to the schema reach a mirror that already exists.
    Every statement in them is idempotent for exactly that reason.

    Foreign keys are off by default in SQLite and are per-connection, so the
    pragma has to be issued here rather than declared in the schema.

    WAL because there are two writers now. The sync runs on a timer and spends
    most of a run writing enrichment rows, and under the default journal a read
    at the prompt blocks behind it — `ypl playlists show` would fail while a
    background process nobody asked for held the database. WAL lets the reader
    carry on against the last committed state, which is exactly right here: a
    tracklist that arrives a minute later costs nothing.
    """
    database = database or paths.database_file()
    database.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(database, timeout=BUSY_TIMEOUT_SECONDS)
    connection.row_factory = sqlite3.Row
    connection.execute('PRAGMA journal_mode = WAL')
    connection.execute('PRAGMA foreign_keys = ON')
    connection.executescript(read_sql(SCHEMA_FILE))
    connection.executescript(read_sql(INDEXES_FILE))
    connection.commit()
    return connection
