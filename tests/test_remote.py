"""The write path's pure parts: throttling, batching, and the reorder plan.

Nothing here touches YouTube. The reorder plan is the piece worth the most
scrutiny — a move is one request and cannot be batched, so the difference
between a good plan and a naive one is the difference between five requests and
two hundred.
"""

import pytest

from ypl import remote
from ypl import throttle as throttle_module


def apply_moves(current: list[str], moves: list[tuple[str, str | None]]) -> list[str]:
    """Do to a list what the backend would do to a playlist."""
    order = list(current)
    for key, before in moves:
        order.remove(key)
        order.insert(order.index(before) if before else len(order), key)
    return order


@pytest.mark.parametrize(
    ('current', 'desired'),
    [
        (['a', 'b', 'c'], ['a', 'b', 'c']),
        (['a', 'b', 'c'], ['c', 'b', 'a']),
        (['a', 'b', 'c', 'd'], ['b', 'a', 'd', 'c']),
        (['a'], ['a']),
        ([], []),
        (['a', 'b', 'c', 'd', 'e'], ['e', 'a', 'b', 'c', 'd']),
        (['a', 'b', 'c', 'd', 'e'], ['b', 'c', 'd', 'e', 'a']),
        (list('abcdefghij'), list('jihgfedcba')),
        (list('abcdefghij'), list('acbdfeghji')),
    ],
)
def test_the_plan_actually_produces_the_order_it_promised(current, desired):
    assert apply_moves(current, remote.move_plan(current, desired)) == desired


def test_an_order_that_is_already_right_costs_nothing():
    assert remote.move_plan(['a', 'b', 'c'], ['a', 'b', 'c']) == []


def test_moving_one_item_to_the_front_is_one_move_not_a_rewrite():
    """The naive plan rewrites every slot; this is the whole point of the LIS."""
    current = list('abcdefghij')
    desired = ['j', *'abcdefghi']
    assert len(remote.move_plan(current, desired)) == 1


def test_a_reversal_moves_everything_but_one():
    """Nothing keeps its relative order in a reversal except a single item."""
    current = list('abcde')
    assert len(remote.move_plan(current, list(reversed(current)))) == 4


@pytest.mark.parametrize(
    ('current', 'desired', 'expected_moves'),
    [
        (list('abcde'), list('abcde'), 0),
        (list('abcde'), ['b', 'a', 'c', 'd', 'e'], 1),
        (list('abcde'), ['a', 'b', 'e', 'c', 'd'], 1),
        (list('abcdef'), ['f', 'e', 'a', 'b', 'c', 'd'], 2),
    ],
)
def test_the_plan_is_as_short_as_the_reordering_allows(current, desired, expected_moves):
    assert len(remote.move_plan(current, desired)) == expected_moves


def test_a_plan_over_mismatched_sets_is_refused_rather_than_guessed():
    """Adds and removes are separate operations; a reorder must not smuggle one in."""
    with pytest.raises(ValueError, match='same items'):
        remote.move_plan(['a', 'b'], ['a', 'b', 'c'])
    with pytest.raises(ValueError, match='same items'):
        remote.move_plan(['a', 'b', 'c'], ['a', 'b'])


def test_a_big_shuffle_still_plans_far_fewer_moves_than_slots():
    """The cost that matters is request count, so this is the real assertion."""
    current = [f'v{index:03d}' for index in range(300)]
    desired = [*current[150:], *current[:150]]
    assert len(remote.move_plan(current, desired)) == 150
    assert apply_moves(current, remote.move_plan(current, desired)) == desired


def test_batches_are_bounded_so_no_single_request_stands_out():
    assert [len(batch) for batch in remote.batched(list(range(250)))] == [100, 100, 50]
    assert remote.batched([]) == []


def test_the_throttle_puts_a_floor_under_the_gap_between_calls():
    # Readings in order: the first call's stamp, then "now" at the second call
    # (half a second later), then the stamp taken after sleeping.
    slept = []
    clock = iter([0.0, 0.5, 2.0])
    throttle = throttle_module.Throttle(interval_seconds=2.0, sleep=slept.append, clock=lambda: next(clock))

    assert throttle.wait() == 0.0
    assert throttle.wait() == pytest.approx(1.5)
    assert slept == [pytest.approx(1.5)]


def test_a_call_that_is_already_late_does_not_sleep():
    slept = []
    clock = iter([0.0, 99.0, 99.0])
    throttle = throttle_module.Throttle(interval_seconds=2.0, sleep=slept.append, clock=lambda: next(clock))

    throttle.wait()
    assert throttle.wait() == 0.0
    assert slept == []
