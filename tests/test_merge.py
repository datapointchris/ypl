"""The three-way merge, stated as cases rather than as a walk through the code.

Every test here is a claim about what should happen to one playlist across two
machines and a phone. The interesting ones are the pairs that look identical
without a base — a video missing from remote is a deletion or an addition
depending entirely on what was recorded last time.
"""

from ypl import merge


def merged(base, remote, local):
    return merge.merge(list(base), list(remote), list(local))


def test_nothing_changed_anywhere():
    result = merged('abc', 'abc', 'abc')
    assert result.order == list('abc')
    assert not result.changed_here
    assert not result.to_push


def test_a_video_gone_from_remote_but_in_the_base_was_deleted_there():
    result = merged('abc', 'ac', 'abc')
    assert result.order == list('ac')
    assert result.pulled_out == ['b']
    assert not result.to_push


def test_a_video_missing_from_the_base_was_added_here_and_still_has_to_go_up():
    """The same two lists as the test above, and the opposite correct action."""
    result = merged('ac', 'ac', 'abc')
    assert result.order == list('abc')
    assert result.pending_add == ['b']
    assert not result.changed_here


def test_a_video_deleted_here_stays_deleted_and_is_queued():
    result = merged('abc', 'abc', 'ac')
    assert result.order == list('ac')
    assert result.pending_remove == ['b']


def test_a_video_added_on_the_phone_arrives_here():
    result = merged('ab', 'abg', 'ab')
    assert result.order == list('abg')
    assert result.pulled_in == ['g']


def test_a_video_added_at_the_front_on_the_phone_arrives_at_the_front():
    """Appending everything would move it, which reads as ypl reordering the playlist."""
    assert merged('ab', 'gab', 'ab').order == list('gab')


def test_a_video_added_in_the_middle_on_the_phone_lands_where_it_was_put():
    assert merged('abc', 'agbc', 'abc').order == list('agbc')


def test_both_sides_deleting_the_same_video_is_not_a_conflict():
    result = merged('abc', 'ac', 'ac')
    assert result.order == list('ac')
    assert not result.to_push
    assert not result.changed_here


def test_both_sides_adding_the_same_video_needs_nothing_doing():
    """Pushing it again would put the same mix in the playlist twice."""
    result = merged('ab', 'abg', 'abg')
    assert result.order == list('abg')
    assert not result.to_push
    assert result.pulled_in == []


def test_a_first_reconcile_unions_rather_than_deleting():
    """With no base recorded, no absence can be read as a deletion."""
    result = merged('', 'ag', 'ab')
    assert set(result.order) == set('abg')
    assert result.pulled_out == []
    assert result.pending_remove == []


def test_local_order_survives_while_remote_leaves_its_own_alone():
    result = merged('abc', 'abc', 'cba')
    assert result.order == list('cba')
    assert result.order_source == merge.LOCAL


def test_a_remote_reorder_wins_the_whole_playlist():
    """Order is one property, so remote taking it takes all of it."""
    result = merged('abc', 'cba', 'bac')
    assert result.order == list('cba')
    assert result.order_source == merge.REMOTE


def test_an_addition_alone_is_not_a_remote_reorder():
    """Otherwise adding one track on a phone would discard the local ordering."""
    result = merged('abc', 'abcg', 'cba')
    assert result.order_source == merge.LOCAL
    assert result.order == list('cbag')


def test_a_removal_alone_is_not_a_remote_reorder():
    result = merged('abc', 'ac', 'cba')
    assert result.order_source == merge.LOCAL
    assert result.order == list('ca')


def test_a_local_addition_survives_remote_taking_the_order():
    """Remote wins the ordering it knows about; it has no opinion on x at all."""
    result = merged('abc', 'cba', 'abxc')
    assert result.order_source == merge.REMOTE
    assert set(result.order) == set('abcx')
    assert result.order[:3] == list('cba')
    assert result.pending_add == ['x']


def test_the_same_video_twice_is_two_slots_not_one():
    """A set of ids would drop the second copy as though it were noise."""
    result = merged('aba', 'aba', 'aba')
    assert result.order == list('aba')
    assert not result.to_push


def test_deleting_one_of_two_copies_deletes_one_of_two_copies():
    result = merged('aba', 'aba', 'ab')
    assert result.order == list('ab')
    assert result.pending_remove == ['a']


def test_adding_a_second_copy_on_the_phone_brings_a_second_copy_here():
    result = merged('ab', 'aba', 'ab')
    assert result.order == list('aba')
    assert result.pulled_in == ['a']


def test_the_whole_scenario_across_two_machines_and_a_phone():
    """The mini removed c and e and pushed; this machine deleted b offline; the
    phone appended g. Every edit survives, and the base is what makes the
    machine that saw none of it read c and e as remote deletions rather than as
    its own unpushed additions.
    """
    result = merged(base='abcdef', remote='abdfg', local='acdef')
    assert result.order == list('adfg')
    assert result.pulled_in == ['g']
    assert result.pulled_out == ['c', 'e']
    assert result.pending_remove == ['b']
    assert result.pending_add == []


def test_the_same_scenario_with_no_base_resurrects_what_was_deleted():
    """Not a wish — a demonstration of what the base is preventing.

    Without it, c and e read as local additions and go back up to YouTube,
    undoing a deletion made on the other machine.
    """
    result = merged(base='', remote='abdfg', local='acdef')
    assert set(result.pending_add) == {'c', 'e'}


def test_a_reconcile_run_twice_changes_nothing_the_second_time():
    """The base is written from the remote read, so a local deletion survives
    until it is actually pushed rather than being forgotten by the next pull.
    """
    first = merged(base='abcdef', remote='abdfg', local='acdef')
    second = merged(base='abdfg', remote='abdfg', local=first.order)
    assert second.order == first.order
    assert second.pending_remove == ['b']
    assert not second.changed_here


def planned(base, local):
    return merge.push_plan(list(base), list(local))


def test_a_playlist_that_matches_its_base_needs_no_push():
    assert planned('abc', 'abc').empty


def test_videos_added_here_go_up():
    diff = planned('abc', 'abcd')
    assert diff.add == ['d']
    assert diff.remove == []


def test_videos_deleted_here_are_removed_by_position_in_the_base():
    """A video id cannot say which of its copies is meant; a position can."""
    diff = planned('aba', 'ab')
    assert diff.add == []
    assert diff.remove == [2]


def test_a_reorder_alone_is_neither_an_add_nor_a_remove():
    diff = planned('abc', 'cba')
    assert not diff.add and not diff.remove
    assert not diff.empty


def test_additions_are_planned_as_landing_at_the_end():
    """Which is where the batch endpoint puts them, and what the moves assume."""
    diff = planned('ab', 'xaby')
    assert [video_id for video_id, _ in diff.current_after] == ['a', 'b', 'x', 'y']
    assert [video_id for video_id, _ in diff.desired] == ['x', 'a', 'b', 'y']


def test_a_first_push_of_a_new_playlist_is_all_additions():
    diff = planned('', 'abc')
    assert diff.add == ['a', 'b', 'c']
    assert diff.remove == []
