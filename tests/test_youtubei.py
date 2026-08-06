import json

import httpx
import pytest

from ypl import youtubei
from ypl.remote import RemoteAuthError
from ypl.remote import RemoteError
from ypl.remote import RemoteItem
from ypl.remote import RemoteRateLimitedError
from ypl.throttle import Throttle

SIGNED_IN = {
    'SAPISID': 'sap',
    '__Secure-1PAPISID': 'one',
    '__Secure-3PAPISID': 'three',
    'LOGIN_INFO': 'yes',
}


def video(video_id, set_video_id='', title='Title'):
    renderer = {'videoId': video_id, 'title': {'runs': [{'text': title}]}}
    if set_video_id:
        renderer['setVideoId'] = set_video_id
    return {'playlistVideoRenderer': renderer}


def wrapped_list(items, token=''):
    contents = list(items)
    if token:
        contents.append({'continuationItemRenderer': {'continuationEndpoint': {'continuationCommand': {'token': token}}}})
    return {'contents': {'twoColumnBrowseResultsRenderer': {'tabs': [{'playlistVideoListRenderer': {'contents': contents}}]}}}


def item_list(items, token=''):
    """A `playlistVideoListRenderer` holding these items, and maybe a next page."""
    contents = list(items)
    if token:
        contents.append(
            {
                'continuationItemRenderer': {
                    'continuationEndpoint': {'commandExecutorCommand': {'commands': [{'continuationCommand': {'token': token}}]}}
                }
            }
        )
    return {'playlistVideoListRenderer': {'contents': contents, 'isEditable': True}}


def page_html(items, token='', visitor='VISITOR', decoy_token='DECOY'):
    """A playlist page as YouTube serves one.

    Carries a second continuation outside the item list, because the real pages
    do: it belongs to a recommendations shelf, and following it appends videos
    that are not in the playlist.
    """
    data = {
        'contents': {
            'twoColumnBrowseResultsRenderer': {
                'tabs': [
                    {
                        'tabRenderer': {
                            'content': {
                                'sectionListRenderer': {
                                    'contents': [
                                        {'itemSectionRenderer': {'contents': [item_list(items, token)]}},
                                        {
                                            'continuationItemRenderer': {
                                                'continuationEndpoint': {'continuationCommand': {'token': decoy_token}}
                                            }
                                        },
                                    ]
                                }
                            }
                        }
                    }
                ]
            }
        }
    }
    return f'<script>ytcfg.set({{"VISITOR_DATA": "{visitor}"}});</script><script>var ytInitialData = {json.dumps(data)};</script>'


def continuation_response(items, token=''):
    """A continuation answer, which repeats a page render alongside what it adds."""
    return {
        'contents': {'twoColumnBrowseResultsRenderer': {'tabs': []}},
        'onResponseReceivedActions': [{'appendContinuationItemsAction': {'continuationItems': [item_list(items, token)]}}],
    }


class Recorder:
    """An httpx client that answers from a script and keeps what it was sent."""

    def __init__(self, responses, page=None):
        self.responses = list(responses)
        self.page = page
        self.requests = []
        self.gets = []

    def get(self, url, params=None, headers=None, follow_redirects=False):
        self.gets.append({'url': url, 'params': params, 'headers': headers})
        return httpx.Response(200, text=self.page or '', request=httpx.Request('GET', url))

    def post(self, url, json=None, headers=None, params=None):
        self.requests.append({'url': url, 'body': json, 'headers': headers})
        status, payload = self.responses.pop(0) if self.responses else (200, {'status': youtubei.STATUS_SUCCEEDED})
        return httpx.Response(status, json=payload, request=httpx.Request('POST', url))


def backend(responses=(), page_id='', cookies=None, page=None):
    return youtubei.YouTubeiBackend(
        dict(cookies if cookies is not None else SIGNED_IN),
        page_id=page_id,
        throttle=Throttle(0),
        create_throttle=Throttle(0),
        client=Recorder(responses, page=page),
    )


def test_one_authorization_hash_per_sid_cookie_the_jar_holds():
    """The browser sends all three, and YouTube accepts on any of them.

    Sending only `SAPISIDHASH` computed from `__Secure-3PAPISID` — which is what
    the YouTube Music backend did — pairs a scheme with the wrong cookie. It was
    tolerated rather than correct.
    """
    value = youtubei.sid_authorization(SIGNED_IN, origin='https://www.youtube.com', now=1000)
    schemes = [part.split(' ')[0] for part in value.split(' ') if 'HASH' in part]
    assert schemes == ['SAPISIDHASH', 'SAPISID1PHASH', 'SAPISID3PHASH']


def test_each_hash_is_computed_against_its_own_cookie():
    import hashlib

    value = youtubei.sid_authorization(SIGNED_IN, origin='https://www.youtube.com', now=1000)
    parts = dict(zip(value.split(' ')[0::2], value.split(' ')[1::2], strict=True))
    for scheme, sid in (('SAPISIDHASH', 'sap'), ('SAPISID1PHASH', 'one'), ('SAPISID3PHASH', 'three')):
        expected = hashlib.sha1(f'1000 {sid} https://www.youtube.com'.encode()).hexdigest()
        assert parts[scheme] == f'1000_{expected}'


def test_the_three_p_cookie_stands_in_when_sapisid_is_missing():
    """Some accounts have no bare SAPISID, and YouTube itself falls back this way."""
    value = youtubei.sid_authorization({'__Secure-3PAPISID': 'three'}, now=1000)
    assert value.startswith('SAPISIDHASH ')
    assert 'SAPISID3PHASH' in value


def test_a_jar_with_no_sid_cookie_is_an_auth_error_not_an_empty_header():
    with pytest.raises(RemoteAuthError):
        youtubei.sid_authorization({'LOGIN_INFO': 'yes'})


def test_the_page_id_and_auth_user_travel_together():
    """A page id without an auth user names a channel under no particular account."""
    headers = youtubei.request_headers(SIGNED_IN, page_id='PAGEID')
    assert headers['x-goog-pageid'] == 'PAGEID'
    assert headers['x-goog-authuser'] == '0'


def test_no_page_id_means_neither_header():
    headers = youtubei.request_headers(SIGNED_IN)
    assert 'x-goog-pageid' not in headers
    assert 'x-goog-authuser' not in headers


def test_a_browser_holding_cookies_but_no_login_is_not_a_session():
    """YouTube clears LOGIN_INFO on sign-out and does not reliably clear the SID."""
    with pytest.raises(RemoteAuthError):
        youtubei.YouTubeiBackend({'__Secure-3PAPISID': 'three'})


@pytest.mark.parametrize('given', ['PL123', 'VLPL123'])
def test_a_stored_id_reaches_the_page_without_its_browse_prefix(given):
    """Ids arrive from yt-dlp, from an M3U file and from `playlist/create`, and
    only some of them carry the `VL` a browse call wanted."""
    reader = backend(page=page_html([video('a', 'h1')]))
    reader.playlist_items(given)
    assert reader.client.gets[0]['params'] == {'list': 'PL123'}


def test_items_are_found_wherever_youtube_has_moved_the_renderers():
    """The walk is the point: a fixed path turns their reshuffle into data loss.

    An empty read merges as "every video was deleted remotely", which is the one
    failure this tool must never have, so the parse holds on to the leaf
    renderers rather than to the wrappers around them.
    """
    buried = {'someNewWrapper': {'andAnother': [wrapped_list([video('a', 'h1'), video('b', 'h2')])]}}
    assert [item.video_id for item in youtubei.items_from(buried)] == ['a', 'b']


def test_an_item_without_a_handle_is_kept_rather_than_dropped():
    """It is still a video in the playlist, and dropping it reads as a deletion."""
    items = youtubei.items_from(wrapped_list([video('a', 'h1'), video('b')]))
    assert [(item.video_id, item.set_video_id) for item in items] == [('a', 'h1'), ('b', '')]


def test_a_playlist_is_read_from_its_page_not_from_a_browse_call():
    """`browseId: VL<id>` is the music.youtube.com convention.

    On the main site it answers with the page furniture and no videos at all,
    so the read that carries handles is the page itself.
    """
    reader = backend(page=page_html([video('a', 'h1'), video('b', 'h2')]))
    items = reader.playlist_items('PL1')
    assert [(item.video_id, item.set_video_id) for item in items] == [('a', 'h1'), ('b', 'h2')]
    assert reader.client.gets[0]['params'] == {'list': 'PL1'}
    assert reader.client.requests == []


def test_reading_a_playlist_follows_every_continuation():
    reader = backend(
        [
            (200, continuation_response([video('b', 'h2')], token='evenmore')),
            (200, continuation_response([video('c', 'h3')])),
        ],
        page=page_html([video('a', 'h1')], token='more'),
    )
    assert [item.video_id for item in reader.playlist_items('PL1')] == ['a', 'b', 'c']


def test_the_recommendations_continuation_is_never_followed():
    """A playlist page carries a second token belonging to a shelf below it.

    Following it appends videos that are not in the playlist, which merges as a
    pile of local additions and pushes them onto YouTube.
    """
    reader = backend(page=page_html([video('a', 'h1')], decoy_token='DECOY'))
    assert [item.video_id for item in reader.playlist_items('PL1')] == ['a']
    assert reader.client.requests == []


def test_a_short_read_is_refused_rather_than_returned():
    """The merge takes what is missing from a remote read for a remote deletion.

    So a hundred slots read out of a thousand would queue nine hundred removals
    on YouTube. Reading less than there is has to be louder than reading none.
    """
    reader = backend([(200, {'onResponseReceivedActions': []})], page=page_html([video('a', 'h1')], token='more'))
    with pytest.raises(youtubei.YouTubeiError, match='Refusing a partial read'):
        reader.playlist_items('PL1')


def test_a_page_with_no_video_list_is_an_error_not_an_empty_playlist():
    """Reading nothing and reading an empty playlist must never look the same."""
    with pytest.raises(youtubei.YouTubeiError):
        backend(page='<script>var ytInitialData = {"contents": {}};</script>').playlist_items('PL1')


def test_a_page_that_carries_no_data_at_all_says_so():
    with pytest.raises(youtubei.YouTubeiError):
        backend(page='<html>signed out</html>').playlist_items('PL1')


def test_the_visitor_id_is_taken_from_the_page_and_sent_on_later_calls():
    """Not optional despite the name: without it `browse` answers
    PERMISSION_DENIED for every playlist that is not public."""
    reader = backend([(200, continuation_response([video('b', 'h2')]))], page=page_html([video('a', 'h1')], token='more'))
    reader.playlist_items('PL1')
    assert reader.client.requests[0]['headers']['x-goog-visitor-id'] == 'VISITOR'


def test_a_refusal_arriving_with_a_200_is_still_a_refusal():
    """`edit_playlist` reports failure in the body, which is how every write this
    tool ever made reported success and changed nothing."""
    with pytest.raises(RemoteError, match='STATUS_FAILED'):
        backend([(200, {'status': 'STATUS_FAILED'})]).add_items('PL1', ['a'])


def test_a_move_names_the_successor_not_the_predecessor():
    """`ACTION_MOVE_VIDEO_BEFORE` means "in front of", so the field is the successor.

    Setting the predecessor field lands the item on the wrong side of its
    neighbour, one position out, on every move.
    """
    mover = backend()
    mover.move_item('PL1', RemoteItem('a', 'h1'), RemoteItem('b', 'h2'))
    action = mover.client.requests[0]['body']['actions'][0]
    assert action == {'action': 'ACTION_MOVE_VIDEO_BEFORE', 'setVideoId': 'h1', 'movedSetVideoIdSuccessor': 'h2'}


def test_moving_to_the_end_names_no_neighbour_at_all():
    mover = backend()
    mover.move_item('PL1', RemoteItem('a', 'h1'), None)
    assert mover.client.requests[0]['body']['actions'][0] == {'action': 'ACTION_MOVE_VIDEO_BEFORE', 'setVideoId': 'h1'}


def test_writing_without_a_handle_says_what_is_actually_wrong():
    """No setVideoId means this identity does not own the playlist.

    Worth saying rather than letting YouTube answer STATUS_FAILED, because that
    is the exact symptom the page id exists to cure and the message is the only
    thing that connects them.
    """
    with pytest.raises(RemoteError, match='does not own'):
        backend().remove_items('PL1', [RemoteItem('a', '')])


def test_adds_are_batched_a_hundred_at_a_time():
    """The whole argument for this backend over the Data API is the actions array."""
    adder = backend([(200, {'status': youtubei.STATUS_SUCCEEDED})] * 3)
    adder.add_items('PL1', [f'v{index}' for index in range(250)])
    sent = [len(request['body']['actions']) for request in adder.client.requests]
    assert sent == [100, 100, 50]


def test_a_429_stops_the_run_rather_than_being_retried_into():
    with pytest.raises(RemoteRateLimitedError):
        backend([(429, {})]).rename_playlist('PL1', 'Monday')


def test_a_401_is_an_auth_error():
    with pytest.raises(RemoteAuthError):
        backend([(401, {})]).rename_playlist('PL1', 'Monday')


def test_a_403_is_not_treated_as_a_dead_session():
    """Measured on a live session: youtubei answers PERMISSION_DENIED for a
    request it will not serve, not only for a caller it does not know. Calling
    it auth sends you to sign in again over something signing in cannot fix."""
    with pytest.raises(RemoteError) as raised:
        backend([(403, {})]).rename_playlist('PL1', 'Monday')
    assert not isinstance(raised.value, RemoteAuthError)


def test_the_channel_owning_account_is_chosen_over_the_one_that_is_selected():
    """The personal account is the selected one and owns nothing.

    Picking by selection is what the YouTube Music backend effectively did, and
    it is the whole reason forty-two playlists read back as somebody else's.
    """
    accounts = [
        {'name': 'Chris Birch', 'page_id': '', 'has_channel': False, 'selected': True},
        {'name': 'iChrisBirch', 'page_id': 'PAGEID', 'has_channel': True, 'selected': False},
    ]
    assert youtubei.channel_account(accounts)['page_id'] == 'PAGEID'


def test_a_jar_reaching_no_channel_at_all_refuses_to_guess():
    accounts = [{'name': 'Chris Birch', 'page_id': '', 'has_channel': False, 'selected': True}]
    with pytest.raises(RemoteAuthError, match='owns a YouTube channel'):
        youtubei.channel_account(accounts)


def test_two_channels_is_a_question_ypl_refuses_to_answer_for_you():
    accounts = [
        {'name': 'One', 'page_id': 'a', 'has_channel': True, 'selected': True},
        {'name': 'Two', 'page_id': 'b', 'has_channel': True, 'selected': False},
    ]
    with pytest.raises(RemoteAuthError, match='more than one channel'):
        youtubei.channel_account(accounts)


def test_the_account_switcher_is_flattened_out_of_whatever_wraps_it():
    payload = {
        'contents': [
            {
                'accountItem': {
                    'accountName': {'simpleText': 'iChrisBirch'},
                    'hasChannel': True,
                    'isSelected': False,
                    'serviceEndpoint': {'selectActiveIdentityEndpoint': {'supportedTokens': [{'pageIdToken': {'pageId': 'PAGEID'}}]}},
                }
            }
        ]
    }
    assert youtubei.accounts_from(payload) == [{'name': 'iChrisBirch', 'page_id': 'PAGEID', 'has_channel': True, 'selected': False}]


def test_every_request_carries_the_client_context():
    """youtubei answers a body with no context with an error rather than a page."""
    writer = backend()
    writer.rename_playlist('PL1', 'Monday')
    assert writer.client.requests[0]['body']['context']['client']['clientName'] == 'WEB'


def test_a_network_failure_is_a_remote_error_not_a_traceback():
    class Broken:
        def post(self, *args, **kwargs):
            raise httpx.ConnectError('no route to host')

    broken = youtubei.YouTubeiBackend(dict(SIGNED_IN), throttle=Throttle(0), client=Broken())
    with pytest.raises(RemoteError, match='could not reach YouTube'):
        broken.rename_playlist('PL1', 'Monday')


def test_a_response_that_is_not_json_says_so():
    class Garbage:
        def post(self, url, **kwargs):
            return httpx.Response(200, text='<!doctype html>', request=httpx.Request('POST', url))

    with pytest.raises(youtubei.YouTubeiError):
        youtubei.YouTubeiBackend(dict(SIGNED_IN), throttle=Throttle(0), client=Garbage()).rename_playlist('PL1', 'Monday')


def test_a_created_playlist_hands_back_its_id():
    maker = backend([(200, {'playlistId': 'PLNEW'})])
    assert maker.create_playlist('Monday') == 'PLNEW'


def test_a_creation_that_did_not_create_anything_is_an_error():
    with pytest.raises(RemoteError):
        backend([(200, {'status': 'STATUS_FAILED'})]).create_playlist('Monday')


def test_new_playlists_are_private():
    """Privacy is not ypl's business past this one default."""
    maker = backend([(200, {'playlistId': 'PLNEW'})])
    maker.create_playlist('Monday')
    assert maker.client.requests[0]['body']['privacyStatus'] == 'PRIVATE'


def test_a_delete_is_confirmed_by_the_command_it_answers_with():
    """`playlist/delete` returns no `status`, unlike every edit_playlist action.

    Checking for one made every successful delete look like a refusal.
    """
    backend([(200, {'responseContext': {}, 'command': {}})]).delete_playlist('PL1')


def test_a_delete_that_confirms_nothing_is_an_error():
    with pytest.raises(RemoteError):
        backend([(200, {'responseContext': {}})]).delete_playlist('PL1')


def test_renaming_sends_the_title_action():
    renamer = backend()
    renamer.rename_playlist('PL1', 'Tuesday')
    assert renamer.client.requests[0]['body']['actions'] == [{'action': 'ACTION_SET_PLAYLIST_NAME', 'playlistName': 'Tuesday'}]


def test_the_stored_page_id_is_sent_on_every_call():
    writer = backend(page_id='PAGEID')
    writer.rename_playlist('PL1', 'Monday')
    assert writer.client.requests[0]['headers']['x-goog-pageid'] == 'PAGEID'


def test_an_account_call_refuses_a_stored_page_id_the_jar_cannot_reach():
    """The browser signed out of the brand account, or was pointed at a new profile."""
    payload = {
        'accountItem': {
            'accountName': {'simpleText': 'Chris Birch'},
            'hasChannel': False,
            'serviceEndpoint': {'selectAccountEndpoint': {'pageId': ''}},
        }
    }
    with pytest.raises(RemoteAuthError, match='not one of'):
        backend([(200, {'contents': [payload]})], page_id='GONE').account()


def test_a_signed_out_answer_is_an_auth_error_rather_than_an_empty_list():
    """YouTube answers a dead cookie with a page, not with a failure."""
    with pytest.raises(RemoteAuthError, match='signed-out visitor'):
        backend([(200, {'contents': []})]).account()


def test_the_request_body_is_json_youtube_would_accept():
    writer = backend()
    writer.rename_playlist('PL1', 'Monday')
    assert json.dumps(writer.client.requests[0]['body'])
