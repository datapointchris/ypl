"""Connection handling for the local mirror."""

import sqlite3
from importlib import resources
from pathlib import Path

from ypl import paths

SCHEMA_FILE = 'schema.sql'
INDEXES_FILE = 'indexes.sql'


def read_sql(filename: str) -> str:
    return resources.files('ypl').joinpath(filename).read_text()


def connect(database: Path | None = None) -> sqlite3.Connection:
    """Open the mirror, creating anything it is missing.

    Both files run every time rather than only on a fresh database, which is
    what lets a table added to the schema reach a mirror that already exists.
    Every statement in them is idempotent for exactly that reason.

    Foreign keys are off by default in SQLite and are per-connection, so the
    pragma has to be issued here rather than declared in the schema.
    """
    database = database or paths.database_file()
    database.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(database)
    connection.row_factory = sqlite3.Row
    connection.execute('PRAGMA foreign_keys = ON')
    connection.executescript(read_sql(SCHEMA_FILE))
    connection.executescript(read_sql(INDEXES_FILE))
    connection.commit()
    return connection
