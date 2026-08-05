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
