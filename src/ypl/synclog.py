"""What each sync run did, one JSON line per run.

A sync that runs on a timer is a sync nobody watches, so the log is the whole
interface to it: what changed, what it refused, what broke, and when it last
managed to finish. `ypl status` reads the last line of this and nothing else.

State rather than data — it records what a rebuildable process did, and losing
it costs the answer to "what happened overnight" and nothing more. Append-only,
so a run in progress cannot corrupt the record of the last one.
"""

import datetime as dt
import json

from ypl import paths

# Enough to answer "has this been working" without reading a file that grows
# forever. A run a quarter of an hour is a few weeks of history.
KEEP_LINES = 500


def now_ts() -> str:
    return dt.datetime.now(dt.UTC).isoformat(timespec='seconds')


def record(entry: dict) -> dict:
    """Append one run, and trim the file if it has grown past what is useful."""
    line = {'ts': now_ts(), **entry}
    path = paths.sync_log_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('a') as log:
        log.write(json.dumps(line) + '\n')
    trim(path)
    return line


def trim(path) -> None:
    """Keep the tail, rewritten through a temp file so a truncation cannot land.

    Rewriting in place would leave the log empty for the moment between the
    truncate and the write, which is exactly when a second run reads it.
    """
    lines = path.read_text().splitlines()
    if len(lines) <= KEEP_LINES * 2:
        return
    temporary = path.with_suffix('.jsonl.tmp')
    temporary.write_text('\n'.join(lines[-KEEP_LINES:]) + '\n')
    temporary.replace(path)


def load(limit: int | None = None) -> list[dict]:
    """Runs, newest first. Unreadable lines are skipped rather than fatal."""
    path = paths.sync_log_file()
    if not path.exists():
        return []
    runs = []
    for line in reversed(path.read_text().splitlines()):
        if not line.strip():
            continue
        try:
            runs.append(json.loads(line))
        except json.JSONDecodeError:
            continue
        if limit and len(runs) >= limit:
            break
    return runs


def last() -> dict | None:
    runs = load(limit=1)
    return runs[0] if runs else None


# Enough runs to average out a slow one without reaching back to a week when the
# machine was asleep, which would report a rate the tool is not achieving now.
RATE_SAMPLE_RUNS = 8


def enrich_rate_per_hour(runs: list[dict] | None = None) -> float | None:
    """Videos enriched per hour of wall clock, or None with too little to say.

    Wall clock rather than time spent working, deliberately. What anyone wants
    from this is when the backlog will be gone, and a rate measured only inside
    the runs answers a question nobody asked: it was three videos a minute while
    the tool was awake and one a minute in reality, because two thirds of every
    hour was spent waiting for the next timer.

    Measured between run timestamps, which are written when a run ends, so the
    videos counted are those enriched in the spans the timestamps bracket — the
    oldest run's own work happened before the window and is left out of the
    numerator.
    """
    runs = runs if runs is not None else load(limit=RATE_SAMPLE_RUNS)
    if len(runs) < 2:
        return None
    try:
        newest = dt.datetime.fromisoformat(runs[0]['ts'])
        oldest = dt.datetime.fromisoformat(runs[-1]['ts'])
    except (KeyError, ValueError):
        return None
    hours = (newest - oldest).total_seconds() / 3600
    enriched = sum(run.get('enriched') or 0 for run in runs[:-1])
    if hours <= 0 or not enriched:
        return None
    return enriched / hours
