"""The `remote.Backend` implementation, over youtubei on `www.youtube.com`.

Not YouTube Music, which is where this started and could not go. The account
that owns this channel is a brand account, and a brand account has no Music
presence: `music.youtube.com` answers its cookies with the account menu a
signed-out visitor gets. The main site answers the same cookies with the full
edit surface, so the write path lives here.

Not the official Data API either. `playlistItems.insert` costs 50 units of a
per-project 10,000/day, which is 200 writes a day permanently — a cap no OAuth
setup lifts. `edit_playlist` carries an `actions` array holding a hundred
additions, so the same work is a couple of dozen requests rather than eighteen
days of queue draining.

That trade is deliberate and it is not free, so everything here is built to stay
at human scale rather than to go as fast as it can: every call goes through
`Throttle`, batches are bounded, and a 429 stops the run instead of retrying
into it.

Nothing is stored but the browser's name and the page id. The cookies are read
out of the browser on every run, which is what makes signing in once mean once:
Google rotates `__Secure-1PSIDTS` and `__Secure-3PSIDTS` while you stay signed
in, so a session copied to a file at two o'clock is a signed-out visitor by
five — and the copy says nothing when it stops working.
"""

import hashlib
import json
import re
import time
from typing import Any

import httpx

from ypl.remote import CREATE_INTERVAL_SECONDS
from ypl.remote import DEFAULT_PRIVACY
from ypl.remote import MAX_BATCH
from ypl.remote import RemoteAccount
from ypl.remote import RemoteAuthError
from ypl.remote import RemoteError
from ypl.remote import RemoteItem
from ypl.remote import RemoteRateLimitedError
from ypl.remote import batched
from ypl.throttle import Throttle

ORIGIN = 'https://www.youtube.com'

API_BASE = f'{ORIGIN}/youtubei/v1'

# The web client identifies itself by name and version in every request body.
# No API key: youtubei stopped requiring one, and yt-dlp — which tracks these
# constants far more actively than this tool can — no longer sends it either.
CLIENT_NAME = 'WEB'
CLIENT_VERSION = '2.20260114.08.00'

# The browser sends one hash per SID cookie it holds, space-joined, and YouTube
# accepts the request if any of them checks out. Sending only the first was what
# the YouTube Music backend did, computed from `__Secure-3PAPISID` — the right
# cookie for the 3P scheme and the wrong one for the scheme it was labelled
# with. It was tolerated; it is not what the client sends.
#
# `SAPISID` is absent on some accounts and `__Secure-3PAPISID` stands in for it,
# which is why the first entry has two names to try.
SID_SCHEMES = (
    ('SAPISIDHASH', ('SAPISID', '__Secure-3PAPISID')),
    ('SAPISID1PHASH', ('__Secure-1PAPISID',)),
    ('SAPISID3PHASH', ('__Secure-3PAPISID',)),
)

# Signed-in-ness, as distinct from holding a SID cookie. YouTube clears this one
# on sign-out and does not reliably clear `__Secure-3PAPISID`, so a jar carrying
# a SID and no `LOGIN_INFO` is a session that has already ended.
LOGIN_COOKIE = 'LOGIN_INFO'

# What a refusal looks like when it arrives with a 200. `edit_playlist` reports
# failure in the body rather than in the status, so a push against a playlist
# this identity cannot write returns success-shaped nothing unless this is read.
STATUS_SUCCEEDED = 'STATUS_SUCCEEDED'

# What makes `browse` answer a playlist with its videos rather than with the
# page furniture alone. A fixed value — the web client's own — and it asks for
# unavailable videos to be included, which a merge needs: a video that has gone
# private is still a slot, and omitting it reads as a deletion.
PLAYLIST_ITEMS_PARAMS = 'wgYCCAA='

# Sent because youtubei answers a non-browser agent differently, and httpx
# would otherwise announce itself as one.
USER_AGENT = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15'

# `ytcfg.set({...})` appears several times in a page and the useful keys are
# spread across them, so every match is merged rather than the first taken.
YTCFG_PATTERN = re.compile(r'ytcfg\.set\s*\(\s*(\{.+?\})\s*\)\s*;')

REQUEST_TIMEOUT_SECONDS = 30.0


class YouTubeiError(RemoteError):
    """A youtubei response that could not be read as the thing it should be.

    Separate from `RemoteError` so that a shape change in YouTube's payloads is
    distinguishable from a refusal: the first needs this module updating, the
    second is the account saying no.
    """


def sid_authorization(cookies: dict[str, str], origin: str = ORIGIN, now: float | None = None) -> str:
    """The `Authorization` header value for a cookie jar.

    One `SCHEME timestamp_sha1(timestamp SID origin)` per SID cookie present,
    space-joined. Timestamped, so it is computed per request and never stored —
    which is the whole reason the pasted-headers flow this replaces was a
    delivery mechanism for a value that expired before it was useful.
    """
    stamp = str(int(now if now is not None else time.time()))
    authorizations = []
    for scheme, names in SID_SCHEMES:
        sid = next((cookies[name] for name in names if cookies.get(name)), '')
        if not sid:
            continue
        digest = hashlib.sha1(f'{stamp} {sid} {origin}'.encode()).hexdigest()  # noqa: S324 - Google's scheme, not a choice
        authorizations.append(f'{scheme} {stamp}_{digest}')
    if not authorizations:
        raise RemoteAuthError('that browser is not signed in to YouTube — no SAPISID cookie. Sign in and try again')
    return ' '.join(authorizations)


def cookie_header(cookies: dict[str, str]) -> str:
    return '; '.join(f'{name}={value}' for name, value in cookies.items())


def page_headers(cookies: dict[str, str]) -> dict[str, str]:
    """What a plain page load carries — no youtubei auth, just the session."""
    return {'cookie': cookie_header(cookies), 'user-agent': USER_AGENT}


def page_config(html: str) -> dict[str, Any]:
    """The `ytcfg` a page was rendered with, merged across every block."""
    config: dict[str, Any] = {}
    for block in YTCFG_PATTERN.findall(html):
        try:
            config.update(json.loads(block))
        except json.JSONDecodeError:
            # One unparsable block does not make the others worthless, and the
            # keys wanted here are spread across several.
            continue
    return config


def request_headers(cookies: dict[str, str], page_id: str = '', visitor_id: str = '') -> dict[str, str]:
    """What every youtubei call carries.

    `x-goog-pageid` is the fix this whole rebuild is downstream of. Without it
    the request authenticates as the Google account rather than as the channel,
    and a brand account's own playlists come back `owned: false` with no
    `setVideoId` on any item — every write then failing with `STATUS_FAILED`.
    yt-dlp calls the same value a delegated session id and needs it for exactly
    this, reading private playlists of a secondary channel.

    `x-goog-authuser` goes with it: a page id without an auth user selects a
    channel under an unspecified account.

    `x-goog-visitor-id` is not optional either, whatever its name suggests.
    Without it `browse` answers PERMISSION_DENIED for every playlist that is
    not public — which is most of them here, and precisely the ones this tool
    exists to write to. It is read off the page a playlist was loaded from.
    """
    headers = {
        'content-type': 'application/json',
        'origin': ORIGIN,
        'x-origin': ORIGIN,
        'referer': f'{ORIGIN}/',
        'user-agent': USER_AGENT,
        'authorization': sid_authorization(cookies),
        'cookie': cookie_header(cookies),
        'x-youtube-client-name': '1',
        'x-youtube-client-version': CLIENT_VERSION,
    }
    if visitor_id:
        headers['x-goog-visitor-id'] = visitor_id
    if page_id:
        headers['x-goog-pageid'] = page_id
        headers['x-goog-authuser'] = '0'
    return headers


def request_context(client_version: str = CLIENT_VERSION, visitor_id: str = '') -> dict[str, Any]:
    """The client block youtubei wants wrapped around every request body.

    Language and timezone are pinned rather than inherited so that titles come
    back in one language regardless of where this runs, and so a playlist read
    on the laptop and pushed from the Arch box cannot disagree about what a
    video is called.
    """
    client = {
        'clientName': CLIENT_NAME,
        'clientVersion': client_version,
        'hl': 'en',
        'gl': 'US',
        'timeZone': 'UTC',
        'utcOffsetMinutes': 0,
    }
    if visitor_id:
        client['visitorData'] = visitor_id
    return {'client': client}


def continuation_query(node: Any) -> dict[str, Any] | None:
    """The request body that fetches the next page, or None at the end.

    Two things here are easy to get wrong and both fail silently, answering 200
    with no items rather than an error.

    The token is not at a fixed depth. A playlist's own continuation arrives
    wrapped in a `commandExecutorCommand` holding several commands, one of them
    a page-reload signal, so every command is tried in turn.

    `clickTracking` belongs in the request *root*, beside `continuation`, and it
    has to be the `clickTrackingParams` of the same command that carried the
    token — not the endpoint's, and not folded into `context` where the client's
    own fixed one already lives.
    """
    for renderer in find_all(node, 'continuationItemRenderer'):
        endpoint = renderer.get('continuationEndpoint') or {}
        commands = list((endpoint.get('commandExecutorCommand') or {}).get('commands') or [])
        commands.append(endpoint)
        for command in commands:
            token = (command.get('continuationCommand') or {}).get('token')
            if not token:
                continue
            query: dict[str, Any] = {'continuation': token}
            tracking = command.get('clickTrackingParams')
            if tracking:
                query['clickTracking'] = {'clickTrackingParams': tracking}
            return query
    return None


def find_all(payload: Any, key: str) -> list[dict[str, Any]]:
    """Every dict stored under `key`, at any depth.

    A walk rather than a fixed path through `twoColumnBrowseResultsRenderer` →
    `tabs` → `sectionListRenderer` → `itemSectionRenderer` → and so on, because
    YouTube reshuffles those wrapper renderers regularly and the leaf ones are
    what carry meaning. A fixed path turns a cosmetic change on their side into
    an empty playlist here, which merges as "every video was deleted remotely"
    and is the one failure this tool must never have.
    """
    found: list[dict[str, Any]] = []
    if isinstance(payload, dict):
        for name, value in payload.items():
            if name == key and isinstance(value, dict):
                found.append(value)
            else:
                found.extend(find_all(value, key))
    elif isinstance(payload, list):
        for entry in payload:
            found.extend(find_all(entry, key))
    return found


def renderer_title(renderer: dict[str, Any]) -> str:
    title = renderer.get('title') or {}
    runs = title.get('runs') or []
    if runs:
        return runs[0].get('text') or ''
    return title.get('simpleText') or ''


def items_from(payload: Any) -> list[RemoteItem]:
    """The slots a browse response describes.

    `setVideoId` is the handle for removing or moving one specific slot and is
    the reason this read exists at all — yt-dlp does not return it, so a push
    reads the playlist through this backend rather than trusting the mirror.
    An item without one is kept rather than dropped: it is still a video in the
    playlist, and dropping it would make the merge see a remote deletion.
    """
    return [
        RemoteItem(
            video_id=renderer['videoId'],
            set_video_id=renderer.get('setVideoId') or '',
            title=renderer_title(renderer),
        )
        for renderer in find_all(payload, 'playlistVideoRenderer')
        if renderer.get('videoId')
    ]


def appended_items(payload: dict[str, Any]) -> list[Any]:
    """What a continuation response actually adds.

    Continuation responses repeat a whole page render in `contents` on top of
    the items they append, so reading the payload as a whole counts the first
    hundred twice. Only `appendContinuationItemsAction` is new.
    """
    items: list[Any] = []
    for action in payload.get('onResponseReceivedActions') or []:
        append = action.get('appendContinuationItemsAction') or {}
        items.extend(append.get('continuationItems') or [])
    return items


def accounts_from(payload: dict[str, Any]) -> list[dict[str, Any]]:
    """Every identity these cookies can act as, flattened out of the switcher.

    Each is `{name, page_id, has_channel, selected}`. The page id is absent for
    the personal Google account — it is the default identity, so there is
    nothing to delegate to — and present for every brand account under it.
    """
    accounts = []
    for item in find_all(payload, 'accountItem'):
        endpoint = item.get('serviceEndpoint') or {}
        select = endpoint.get('selectActiveIdentityEndpoint') or endpoint.get('selectAccountEndpoint') or {}
        identity = select.get('supportedTokens') or []
        page_id = ''
        for token in identity:
            page_id = (token.get('pageIdToken') or {}).get('pageId') or page_id
        accounts.append(
            {
                'name': renderer_title(item) or (item.get('accountName') or {}).get('simpleText') or '',
                'page_id': page_id or select.get('pageId') or '',
                'has_channel': bool(item.get('hasChannel')),
                'selected': bool(item.get('isSelected')),
            }
        )
    return accounts


def channel_account(accounts: list[dict[str, Any]]) -> dict[str, Any]:
    """The identity that owns a channel, which is the one worth signing in as.

    `hasChannel` is the whole test. The personal account on this jar reports it
    false and owns nothing; the brand account reports it true and owns the
    channel and every playlist. Picking by "currently selected" is what the
    YouTube Music backend effectively did, and the browser was left selected on
    the personal one.
    """
    with_channel = [account for account in accounts if account['has_channel']]
    if not with_channel:
        raise RemoteAuthError('none of the accounts in that browser owns a YouTube channel — sign in to the one that does')
    if len(with_channel) > 1:
        named = ', '.join(account['name'] for account in with_channel)
        raise RemoteAuthError(f'that browser is signed in to more than one channel ({named}) — ypl cannot choose between them')
    return with_channel[0]


class YouTubeiBackend:
    """Writes through the web client's own endpoints.

    Every call is throttled, because the whole argument for using this rather
    than the sanctioned API is that it can do the same work in far fewer
    requests — which only holds if it does not then spend that saving on speed.
    """

    def __init__(
        self,
        cookies: dict[str, str],
        page_id: str = '',
        throttle: Throttle | None = None,
        create_throttle: Throttle | None = None,
        client: httpx.Client | None = None,
    ):
        if not cookies.get(LOGIN_COOKIE):
            raise RemoteAuthError('that browser has no YouTube session — sign in and run `ypl auth` again')
        self.cookies = cookies
        self.page_id = page_id
        # Both learned from one page load and reused after that, because they
        # describe the browsing session rather than any playlist. Defaults are
        # a fallback for the moment before `bootstrap` has run.
        self.visitor_id = ''
        self.client_version = CLIENT_VERSION
        self.bootstrapped = False
        self.throttle = throttle or Throttle()
        # Separate, because creation is the only endpoint with a measured limit
        # and sharing one floor would either crawl every add or outrun the one
        # limit we know about.
        self.create_throttle = create_throttle or Throttle(CREATE_INTERVAL_SECONDS)
        self.client = client or httpx.Client(timeout=REQUEST_TIMEOUT_SECONDS)

    def bootstrap(self) -> None:
        """Learn the client version and visitor id from one page load.

        Both are pinned to the moment: YouTube ships a new `INNERTUBE_CLIENT_VERSION`
        most days, and the visitor id identifies this browsing session. Reading
        them rather than hardcoding them is what keeps this working without a
        release every time YouTube deploys — the constants here are only a floor.

        Costs one request per backend, and never repeats. A page that cannot be
        read is not fatal: the constants stand in, and the call that needed them
        will say so itself.
        """
        if self.bootstrapped:
            return
        self.bootstrapped = True
        self.throttle.wait()
        try:
            response = self.client.get(ORIGIN, headers=page_headers(self.cookies), follow_redirects=True)
        except httpx.HTTPError:
            return
        if response.status_code >= 400:
            return
        config = page_config(response.text)
        self.visitor_id = config.get('VISITOR_DATA') or self.visitor_id
        self.client_version = config.get('INNERTUBE_CLIENT_VERSION') or self.client_version
        # The browser already knows which channel it is acting as, so a missing
        # page id is recoverable rather than fatal. Worth recovering because the
        # symptom of not sending one is not an error: `browse` answers 200 with
        # the page furniture and no videos, which reads as an empty playlist.
        self.page_id = self.page_id or config.get('DELEGATED_SESSION_ID') or ''

    def call(self, endpoint: str, body: dict[str, Any], throttle: Throttle | None = None) -> dict[str, Any]:
        """One youtubei request, paced and translated.

        A 401 is a dead cookie and a 429 is a throttle, and the difference is
        the only one the queue acts on: one clears by waiting and the other
        never does.
        """
        self.bootstrap()
        (throttle or self.throttle).wait()
        payload = {'context': request_context(self.client_version, self.visitor_id), **body}
        try:
            response = self.client.post(
                f'{API_BASE}/{endpoint}',
                json=payload,
                headers=request_headers(self.cookies, self.page_id, self.visitor_id),
                params={'prettyPrint': 'false'},
            )
        except httpx.HTTPError as error:
            raise RemoteError(f'could not reach YouTube: {error}') from error

        if response.status_code == 429:
            raise RemoteRateLimitedError('YouTube asked us to slow down — stopping rather than retrying into it')
        if response.status_code == 401:
            raise RemoteAuthError('YouTube refused the session (401) — the browser cookies are no longer signed in')
        if response.status_code == 403:
            # Not an auth error, however much it reads like one. Measured on a
            # live session whose `accounts_list` answered in the same minute:
            # youtubei returns PERMISSION_DENIED for a request it will not serve
            # rather than for a caller it does not know. Calling it auth would
            # send you to sign in again over a request that was never going to
            # work.
            raise RemoteError(f'YouTube would not serve {endpoint} for this request (403)')
        if response.status_code >= 400:
            raise RemoteError(f'YouTube returned HTTP {response.status_code} for {endpoint}')
        try:
            return response.json()
        except json.JSONDecodeError as error:
            raise YouTubeiError(f'{endpoint} did not answer with JSON') from error

    def edit(self, playlist_id: str, actions: list[dict[str, Any]]) -> None:
        """Send an `edit_playlist` batch and hold it to its own status.

        The refusal arrives as `STATUS_FAILED` with a 200, so a push against a
        playlist this identity cannot write reports success and changes nothing
        unless the body is read. That was the shape of every write this tool
        ever made before the page id.
        """
        payload = self.call('browse/edit_playlist', {'playlistId': playlist_id, 'actions': actions})
        status = payload.get('status') or ''
        if status != STATUS_SUCCEEDED:
            raise RemoteError(f'YouTube refused the edit: {status or "no status"}')

    def account(self) -> RemoteAccount:
        """Which channel this session acts as.

        The one call that distinguishes a browser holding cookies from a
        browser holding a session: the jar is read locally, and only YouTube
        answering proves the cookie is alive.
        """
        payload = self.call('account/accounts_list', {})
        accounts = accounts_from(payload)
        if not accounts:
            raise RemoteAuthError('YouTube answered as a signed-out visitor — the browser cookies are no longer a session')
        if self.page_id:
            for account in accounts:
                if account['page_id'] == self.page_id:
                    return RemoteAccount(name=account['name'], handle=account['page_id'])
            raise RemoteAuthError(f"the stored page id {self.page_id} is not one of this browser's accounts — run `ypl auth` again")
        chosen = channel_account(accounts)
        return RemoteAccount(name=chosen['name'], handle=chosen['page_id'])

    def playlist_items(self, playlist_id: str) -> list[RemoteItem]:
        """Read the playlist the way the write path needs it, following pages.

        Read immediately before writing rather than out of the mirror, so that
        a push acts on what is there now and on handles that are still valid.

        `params` is what makes `browse` answer with videos. Without it the same
        call returns the page furniture and nothing else, which is what made
        this look like the wrong endpoint for a long time. The value is a fixed
        one — it is the tab the web client asks for, and it includes videos that
        have gone unavailable, which matters because dropping them would look
        to the merge exactly like somebody deleting them.

        A short read is an error rather than a short list, for the same reason.
        The merge treats what is missing from a remote read as deleted on
        YouTube, so returning the first hundred of a thousand would queue nine
        hundred removals — reading less than there is has to be louder than
        reading nothing at all.
        """
        payload = self.call('browse', {'browseId': f'VL{playlist_id.removeprefix("VL")}', 'params': PLAYLIST_ITEMS_PARAMS})
        lists = find_all(payload, 'playlistVideoListRenderer')
        if not lists:
            raise YouTubeiError(f'YouTube served no video list for {playlist_id} — is it a playlist this account owns?')

        items = items_from(lists[0])
        query = continuation_query(lists[0])
        while query:
            block = appended_items(self.call('browse', query))
            fresh = items_from(block)
            if not fresh:
                raise YouTubeiError(
                    f'{playlist_id} has more slots than the {len(items)} read, and YouTube did not serve the rest. '
                    'Refusing a partial read, because the merge would take the missing ones for deletions.'
                )
            items.extend(fresh)
            query = continuation_query(block)
        return items

    def create_playlist(self, title: str, description: str = '') -> str:
        body = {
            'title': title,
            'description': description,
            'privacyStatus': DEFAULT_PRIVACY,
        }
        payload = self.call('playlist/create', body, throttle=self.create_throttle)
        playlist_id = payload.get('playlistId')
        if not playlist_id:
            raise RemoteError(f'playlist was not created: {payload.get("status") or payload}')
        return playlist_id

    def delete_playlist(self, playlist_id: str) -> None:
        """Remove the playlist from YouTube.

        Checked on `command` rather than on `status`, which this endpoint does
        not return — unlike every `edit_playlist` action. A refusal arrives as
        a status code instead: an id that is not a playlist is a 400, and one
        this identity may not delete is a 403, both of which `call` has already
        turned into errors by here.
        """
        payload = self.call('playlist/delete', {'playlistId': playlist_id})
        if 'command' not in payload:
            raise RemoteError(f'YouTube did not confirm deleting {playlist_id}')

    def rename_playlist(self, playlist_id: str, title: str) -> None:
        self.edit(playlist_id, [{'action': 'ACTION_SET_PLAYLIST_NAME', 'playlistName': title}])

    def add_items(self, playlist_id: str, video_ids: list[str]) -> None:
        for batch in batched(video_ids, MAX_BATCH):
            actions = [{'action': 'ACTION_ADD_VIDEO', 'addedVideoId': video_id} for video_id in batch]
            self.edit(playlist_id, actions)

    def remove_items(self, playlist_id: str, items: list[RemoteItem]) -> None:
        missing = [item.video_id for item in items if not item.set_video_id]
        if missing:
            raise RemoteError(f'cannot remove {", ".join(missing)} — no setVideoId, so this account does not own the playlist')
        for batch in batched(items, MAX_BATCH):
            actions = [
                {'action': 'ACTION_REMOVE_VIDEO', 'setVideoId': item.set_video_id, 'removedVideoId': item.video_id} for item in batch
            ]
            self.edit(playlist_id, actions)

    def move_item(self, playlist_id: str, item: RemoteItem, before: RemoteItem | None) -> None:
        """Move one slot in front of another, or to the end when `before` is None.

        One request per move — there is no batch form. That asymmetry is why
        reordering is the expensive operation here while adding a hundred
        videos is one call, and why the push computes the shortest move
        sequence rather than rewriting the order slot by slot.

        `movedSetVideoIdSuccessor` names what the slot lands in front of, which
        is what `ACTION_MOVE_VIDEO_BEFORE` means. The predecessor field belongs
        to the after-variant and setting it here moves the item to the wrong
        side of its neighbour.
        """
        if not item.set_video_id:
            raise RemoteError(f'cannot move {item.video_id} — no setVideoId, so this account does not own the playlist')
        action: dict[str, Any] = {'action': 'ACTION_MOVE_VIDEO_BEFORE', 'setVideoId': item.set_video_id}
        if before is not None:
            if not before.set_video_id:
                raise RemoteError(f'cannot move ahead of {before.video_id} — no setVideoId, so this account does not own the playlist')
            action['movedSetVideoIdSuccessor'] = before.set_video_id
        self.edit(playlist_id, [action])
