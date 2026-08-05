"""The floor on how often this tool calls YouTube.

Shared by both directions and tested with an injected clock, because the one
thing worth asserting about a throttle is that it actually waits — and a test
that proves it by waiting is a test nobody keeps.
"""

from ypl import throttle


def test_the_first_call_goes_straight_through():
    """Pacing is about bursts. One request is not a burst."""
    slept = []
    pace = throttle.Throttle(interval_seconds=2.0, sleep=slept.append, clock=lambda: 100.0)
    assert pace.wait() == 0.0
    assert slept == []


def test_a_second_call_too_soon_waits_out_the_difference():
    clock = iter([100.0, 100.5, 102.0])
    pace = throttle.Throttle(interval_seconds=2.0, sleep=lambda seconds: None, clock=lambda: next(clock))
    pace.wait()
    assert pace.wait() == 1.5


def test_a_call_after_the_interval_does_not_wait_at_all():
    """Slow work paces itself; the throttle must not add to it."""
    clock = iter([100.0, 105.0, 105.0])
    pace = throttle.Throttle(interval_seconds=2.0, sleep=lambda seconds: None, clock=lambda: next(clock))
    pace.wait()
    assert pace.wait() == 0.0
