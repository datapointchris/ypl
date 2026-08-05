"""A floor on how often this tool is allowed to call YouTube.

Shared by both directions, because the reason is the same in both. The write
path uses the YouTube Music web client's own endpoints and only justifies that
by staying at human scale. The read path is yt-dlp, which costs no quota — but
enriching a whole library is thousands of sequential extractions, carrying your
cookies, and a burst is the shape that gets an account looked at whatever it
costs in quota.

Separate from `remote` so that a read command can pace itself without importing
the write path.
"""

import time

DEFAULT_INTERVAL_SECONDS = 2.0


class Budget:
    """What one unattended run is allowed to spend before leaving the rest.

    The counterpart to `Throttle`: that one decides how fast, this one decides
    how long. Both exist because the work here is open-ended — six thousand
    videos to enrich, forty playlists to reconcile — and a sync that runs on a
    timer must never be the thing still running when the next one starts.

    Bounding a run is only safe because every step re-derives what is left from
    stored state: unenriched videos are a query, the work to push is the local
    file against the base. Stopping halfway is a shorter run, never a lost
    update, so a library converges across runs instead of within one.

    Counted in requests as well as seconds because a test cannot wait fifteen
    minutes to prove a ceiling holds.
    """

    def __init__(self, seconds: float | None = None, requests: int | None = None, clock=None):
        self.seconds = seconds
        self.requests = requests
        self.clock = clock or time.monotonic
        self.started = self.clock()
        self.spent = 0

    def charge(self, requests: int = 1) -> None:
        self.spent += requests

    @property
    def elapsed_seconds(self) -> float:
        return self.clock() - self.started

    @property
    def exhausted(self) -> bool:
        if self.requests is not None and self.spent >= self.requests:
            return True
        return self.seconds is not None and self.elapsed_seconds >= self.seconds


class Throttle:
    """A floor on the gap between calls.

    Deliberately a floor rather than a token bucket: a bucket permits a burst,
    and a burst is the shape that gets noticed. Sleeping is fine here because
    the queue drains in the background and nothing waits on it.
    """

    def __init__(self, interval_seconds: float = DEFAULT_INTERVAL_SECONDS, sleep=None, clock=None):
        self.interval_seconds = interval_seconds
        # Resolved here rather than as a default argument, which would bind
        # `time.sleep` once when this module is imported and leave nothing a
        # test could replace — a suite that really sleeps two seconds between
        # requests is a suite that gets its pacing deleted.
        self.sleep = sleep or time.sleep
        self.clock = clock or time.monotonic
        self.last_call: float | None = None

    def wait(self) -> float:
        """Block until the next call is allowed, returning how long that took."""
        if self.last_call is None:
            self.last_call = self.clock()
            return 0.0
        due = self.last_call + self.interval_seconds
        now = self.clock()
        waited = max(0.0, due - now)
        if waited:
            self.sleep(waited)
        self.last_call = self.clock()
        return waited
