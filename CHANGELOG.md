# CHANGELOG


## v2.0.1 (2026-08-07)

### Bug Fixes

- **sync**: Unsync playlists another channel owns
  ([`2163bf8`](https://github.com/datapointchris/ypl/commit/2163bf8a244a8e5f8426c6a332d05aabef7900d9))

Two playlists saved from other channels were bound before ownership was consulted, so they carried
  #YPL-SYNCED:yes. Every run queued a reconcile against them, and the write client cannot read a
  playlist the account does not own — YouTube answers with no video list. The same two failures had
  been logged every half hour since, and no amount of syncing cleared them.

The mirror sweep already knows who owns each playlist, so the flag is corrected from what it just
  read, before any work is queued and at no request cost. Demotion needs a positive answer on both
  ids: owned_by is false for an unknown account channel exactly as it is for a genuinely foreign
  one, and trusting it there would unsync the whole library on a run that could not work out who we
  are.

### Chores

- **lint**: Disable SC1091/SC1090 from the forge toolchain
  ([`140f5a6`](https://github.com/datapointchris/ypl/commit/140f5a682ccd2d2c31a2b83c48fa7e7c54b21ea4))


## v2.0.0 (2026-08-07)

### Bug Fixes

- **remote**: Bound one run's requests where no config can reach
  ([`8929979`](https://github.com/datapointchris/ypl/commit/89299793137f7af00cbde65c616b70e4ab6dc8f0))

Three bounds existed and none of them was a guarantee.

`request_interval_seconds = 0` passed validation, which removed the pacing entirely — the one
  setting that can turn this into something shaped like a scraper. It is a floor now: raising it is
  allowed, lowering it past a second does nothing.

`sync_minutes = 0` meant no ceiling rather than no time, so a run could go on indefinitely by config
  alone.

And the budget is only checked *between* work items, while a single item can be many requests:
  binding one 960-video playlist is ten paged reads charged as one. A playlist large enough never
  reaches a check at all.

So the real bound goes where every request actually passes, in the backend, counted before the
  request rather than after: 400 in one run, whatever the config says and whatever the caller
  passes. It raises a RemoteRateLimitedError subclass so every existing caller already handles it
  correctly — the run stops and leaves the rest, because everything re-derives from stored state and
  a short run is never a lost update.

The bootstrap page load counts too. It is a request to YouTube like any other.

### Chores

- **ci**: Re-trigger builds dropped by the Actions outage
  ([`67bff03`](https://github.com/datapointchris/ypl/commit/67bff0363491b3fc6fcd400f0d0460991f786cbf))

GitHub Actions was degraded from 15:22 UTC on 2026-08-06: the CI run for 30c4f86 never acquired a
  hosted runner, and the four pushes after it created no runs at all. main has been unvalidated and
  unreleased since. An empty commit is the only trigger these workflows expose.

### Features

- **cli**: A bare ypl answers where things stand
  ([`1420a6b`](https://github.com/datapointchris/ypl/commit/1420a6b845eae777e5e2b3b97a7af4450487abba))

It printed thirty-nine commands in six panels, which is the answer to "what can this do" — not the
  question anyone types a bare command to ask. Now it says whether you are signed in, when it last
  synced, how many playlists are here, and the one thing to run next.

One line rather than a menu, because every state has exactly one sensible answer and offering the
  others is what turns a first run into reading. On a machine that has never been set up that line
  is `ypl auth --browser safari`, which with `ypl sync` is the whole setup.

This departs from the fleet's no-args-shows-help rule in `~/dev/standards/cli-design.md`, under the
  override that rule already carries for a tool whose identity is one read-only action: the glance
  takes no options of its own, writes nothing, and `--help` still answers what it used to. Exit 0
  rather than 2, because it ran and answered. The test records the departure and the reasoning
  rather than deleting the rule it breaks.

- **cli**: Cut the surface to what earns its place
  ([`38ab5e6`](https://github.com/datapointchris/ypl/commit/38ab5e6948814a43a1e61e96e7a329652392fc4d))

Twelve commands go. Each was either a second way to do something the tool already does, or a verb
  acting on state you could not see in the command.

`playlists urls` becomes `playlists show --urls`, which is the same read with the same --sort and
  one fewer command to know about. `enrich` was the tail of every sync already. `config
  init|example|path` wrote and located a file that is hand-edited and optional; `config show` says
  what is in effect, which is the only one of the four anybody needed.

`playlists promote|demote` are a field. A playlist stays here by carrying `#YPL-SYNCED:no` in its
  file, and the file is the whole store — so changing your mind is an edit to the thing itself
  rather than a pair of commands that reach in and set it.

`use`, `drop`, `later` and `sooner` were built on a remembered current playlist, which is why their
  names say nothing: `later` is `playlists order` with an invisible target and `drop` is `playlists
  remove` with one. The pointer goes with them, and so does everything that existed to serve it —
  the completion helper, the current-playlist resolver, and the state file.

`plays add` stays, against the plan, because deleting it leaves `history` with no caller at all:
  `plays list` would be permanently empty and `ypl next` would rank on nothing. It is the write half
  of the play history and the `on_log` hook `menu next` is documented against, not spare surface.

Also removed the code left with no callers — `set_synced`, `move_entry`, `config.write_example` and
  its template — and the help text throughout that still named commands that no longer exist.

- **cli**: Sync is the only command that reaches YouTube
  ([`605eec8`](https://github.com/datapointchris/ypl/commit/605eec88a3d42cbd5ebbceec87ec5d30176ee895))

`remote pull`, `remote plan` and `remote apply` were the same loop `ypl sync` already runs, driven
  by hand in a fixed order. They are gone, and with them the `remote` group — `auth` moves to the
  root, which is where the two-command setup was always meant to be: `ypl auth --browser safari`,
  then `ypl sync`.

What `plan` answered moves into `ypl status`, and costs nothing to answer there. The membership half
  of a push is the local file against the recorded base, both of which are files here, so `status`
  now says what each playlist would send rather than only naming it. `plan` spent a request per
  playlist to learn the same counts plus whether YouTube had moved underneath them, and staleness is
  the sync's business — the sync is what settles it.

Collapsing pull into apply also changes an outcome rather than preserving it. A playlist YouTube had
  changed used to be refused, and told you to run the other command; now the reconcile happens first
  and the push sends what the merge settled. Nothing is ever pushed against a base the run has not
  read.

The suite loses a third of its tests. Sixty-one of them existed only once a whole sync was stood up
  — an account feed, a mirror, a fake backend, a timer and a lock, so that one invocation could be
  asserted on. They tested the wiring rather than the behaviour, and the behaviour is covered where
  it lives.

And a guarantee they were quietly breaking: no test may start yt-dlp, mpv or a service manager. Once
  sync became the only way to drive a reconcile, tests that had stubbed the playlist reads still
  left `fetch_video` real, so the enrich tail at the end of every run went to YouTube on the
  signed-in account, unthrottled — the suite disables the pacing. tests/conftest.py refuses those
  processes by name now, so it fails loudly rather than making the request.

BREAKING CHANGE: `ypl remote auth` is now `ypl auth`, and `ypl remote pull|plan|apply` are gone —
  `ypl sync` does all three. `ypl status --json` carries objects in `pending_push` rather than
  names.

- **remote**: Read a whole playlist, however many videos it holds
  ([`4fe5fcd`](https://github.com/datapointchris/ypl/commit/4fe5fcd198695ba6557f6a7de7b0b10e1263f290))

Reads stopped at the first hundred slots, so every playlist bigger than that failed rather than
  sync. WSC is 960 videos; it now reads 960, each with its setVideoId handle.

Three things were wrong, and each failed by answering 200 with no items rather than by erroring,
  which is why they took so long to separate:

- `browse` needs `params: wgYCCAA=` alongside `browseId`. Without it the call returns page furniture
  and no videos at all, which is what made a page scrape look like the only way to read a playlist.
  The same value asks for unavailable videos to be included — one that has gone private is still a
  slot, and omitting it reads to the merge as a deletion. - A continuation's `clickTracking` goes in
  the request root beside `continuation`, not inside `context`, and it must be the tracking of the
  same command that carried the token. - The client version and visitor id cannot be constants.
  YouTube ships a version most days and the visitor id is per-session, so both are read from ytcfg
  on one page load per backend.

The page scrape goes with them: one `browse` call now returns the first hundred slots and the token
  for the rest.

A missing page id is recovered from the browser's own DELEGATED_SESSION_ID during that bootstrap.
  Not sending one is not an error either — it is another 200 with no videos — and the stored one had
  been silently cleared by an older build on the sync timer, which is what hid the real cause for
  hours. A page id that was chosen at sign-in still wins over the browser's selection.

Verified against the account: create, add, read back with handles, move, remove, rename and delete
  all succeed on a scratch playlist, and seven mirrored playlists read to exactly their mirrored
  counts.

### Refactoring

- **sync**: Delete the privacy subsystem
  ([`30c4f86`](https://github.com/datapointchris/ypl/commit/30c4f86ed9cf6fe4aec35ad549df0954b50e43f2))

Privacy was never what stood between this tool and the playlists it could not write to. A browse for
  a non-public playlist came back with no contents — for its owner, with a live session — and that
  was read as YouTube refusing to serve it. The cause was the identity every request carried:
  reading as the channel rather than as the Google account behind it returns those playlists in
  full, with a handle on every slot.

Twenty-three of forty-two playlists were written off as permanently read-only on that misreading,
  and a subsystem grew to keep them out of adopting, reconciling and pushing. All of it comes out:
  not_public, the withheld list and its report, the availability field and its yt-dlp parse, and the
  three filter sites in the work queue.

New playlists are still created private. Nothing else here reads, reports or changes who can see a
  playlist.

BREAKING CHANGE: `ypl sync --json` no longer carries a `withheld` key, and `ypl status` no longer
  prints a `Not public` row.

- **sync**: Every playlist YouTube holds is a file here
  ([`68a0e20`](https://github.com/datapointchris/ypl/commit/68a0e20e439c1557254091787b62bbd3777d3edd))

Adopting was an opt-in step guarding a decision nobody wanted to make. One sync, two stores, no
  opt-in: the sweep writes a file for every mirrored playlist that has none, and changes flow both
  ways from then on.

Ownership stops being a guess. It decided which playlists to adopt by comparing the account name
  against the playlist's channel, and the two reads never agreed — the account menu says `Chris
  Birch` for a channel called `iChrisBirch` — so every playlist this account owns was judged to be
  somebody else's. It is a channel id match now, read once per run off the account's own Liked
  videos, which is the same free request path everything else uses.

What ownership decides is no longer whether a file is written but whether it is synced. A playlist
  on someone else's channel gets `#YPL-SYNCED:no` at the moment its file is created, and is built
  from the mirror rather than the write backend, with no base and no push to prepare. A rule applied
  where the file is made cannot leave a file quietly queuing pushes that will be refused.

The declined list goes with it. It existed because sync would re-adopt a playlist deleted here, and
  it recorded an intention to have a playlist there but not here. That state no longer exists:
  deleting is deleting, and a file that vanishes on its own is a playlist missing its file, which
  the next sync writes again.

BREAKING CHANGE: `ypl remote adopt` is gone — `ypl sync` does it for every playlist, every run. `ypl
  sync --json` reports `bound` where it reported `adopted`, `ypl status --json` no longer carries
  `declined`, and $XDG_DATA_HOME/ypl/declined.json is obsolete and can be deleted.

### Breaking Changes

- **sync**: `ypl remote adopt` is gone — `ypl sync` does it for every playlist, every run. `ypl sync
  --json` reports `bound` where it reported `adopted`, `ypl status --json` no longer carries
  `declined`, and $XDG_DATA_HOME/ypl/declined.json is obsolete and can be deleted.


## v1.0.0 (2026-08-06)

### Bug Fixes

- **remote**: Confirm a delete by its command, not by a status
  ([`2dc1921`](https://github.com/datapointchris/ypl/commit/2dc19210dcf318615e84d473f806f6c18adefb50))

`playlist/delete` answers with a `command`, not with the `status` every edit_playlist action
  returns, so checking for one made every successful delete raise. Found against the account: the
  delete had worked and ypl reported it refused.

A real refusal arrives as a status code instead — 400 for an id that is not a playlist, 403 for one
  this identity may not delete — both of which are already errors by the time the body is read.

### Features

- **remote**: Write through youtubei as the channel, not the account
  ([`9627d8c`](https://github.com/datapointchris/ypl/commit/9627d8cf95b845487d221fbda637bd1dba7fb538))

Every request ypl made authenticated as the personal Google account rather than as the brand account
  that owns the channel and all forty-two playlists. That is why no write ever succeeded: playlists
  read back owned by somebody else, no setVideoId on any item, STATUS_FAILED on every edit.

`ypl remote auth --browser safari` now asks YouTube which identities the browser's cookies reach and
  records the one that owns a channel, so every later request can name it in `x-goog-pageid`.
  Verified against the account: it signs in as iChrisBirch and reads playlists with real slot
  handles.

YouTube Music could not be fixed the same way — a brand account has no Music presence and answers
  with the signed-out menu — so ytmusicapi goes and the backend speaks youtubei on www.youtube.com
  over httpx.

Nothing about the session is stored any more. The cookies are read from the browser on every run,
  which removes both the credential on disk and the stale-session failure that had sync mirroring
  all afternoon while the write path was quietly signed out.

Three protocol corrections found while building it:

- The authorization header carries one hash per SID cookie the jar holds, not one. The old single
  SAPISIDHASH was computed from `__Secure-3PAPISID`, which is the wrong cookie for that scheme. - A
  move names `movedSetVideoIdSuccessor`. The predecessor field belongs to the after-variant and
  would land every moved item one position out. - `x-goog-visitor-id` is required. Without it
  youtubei answers PERMISSION_DENIED for every playlist that is not public.

A playlist is read from its own page rather than from `browseId: VL<id>`, which is the
  music.youtube.com convention and returns page furniture with no videos on the main site. Paging
  past the first hundred slots is not solved: the page's continuation token answers 200 with no
  items. Rather than return a short list — which the merge would read as a pile of remote deletions
  and push back as removals — a truncated read raises.

BREAKING CHANGE: `ypl remote auth` no longer accepts pasted request headers and has no --replace
  flag. Sign in with --browser. The session file at $XDG_CONFIG_HOME/ypl/ytmusic.json is obsolete
  and can be deleted.

### Breaking Changes

- **remote**: `ypl remote auth` no longer accepts pasted request headers and has no --replace flag.
  Sign in with --browser. The session file at $XDG_CONFIG_HOME/ypl/ytmusic.json is obsolete and can
  be deleted.


## v0.12.7 (2026-08-06)

### Bug Fixes

- **sync**: Count only the playlists you could make public
  ([`c5c9157`](https://github.com/datapointchris/ypl/commit/c5c91578d79e8b3248962548787a7efd4145a4d0))

`Not public 23` on an account where twenty-one is the number that means anything: `Liked videos` and
  `Watch later` are in the playlists feed, report no availability like every private playlist does,
  and have no privacy setting to change. They were being named under a heading whose whole purpose
  is to say what to go and fix.

Left out of the report rather than out of `not_public`, which is the filter: Music serves them no
  better for being YouTube's own, and the reconcile and push paths still need to skip them. Same
  reason a saved playlist is left out of the sweep silently — a standing property of the thing is
  not news, and repeating it every half hour is how the counts `ypl status` exists to show got
  buried in the first place.


## v0.12.6 (2026-08-06)

### Bug Fixes

- **sync**: The feed lists saved playlists, so check ownership
  ([`3f5b5d8`](https://github.com/datapointchris/ypl/commit/3f5b5d8cb343073d8c48a40460d7860f0d4be01f))

The two failures left after the privacy fix, refused by YouTube Music on every run: `Rick Roderick:
  Nietzsche and the Postmodern Condition` and `The Robert Greene Podcast`. Their channels are `The
  Partially Examined Life` and `Robert Greene` — playlists saved from other people's channels, not
  this account's. Their ids do not even carry this channel's prefix.

The sweep's comment claimed the feed was the authority on ownership because a playlist is in it
  "because this account has it". Has, not owns. Binding one queues pushes nothing here can perform,
  which is exactly why `remote adopt` with no name has always filtered on `owned_by` through
  `adoptable_playlists` — the sweep is the path that never did, so the two disagreed and the
  automatic one was wrong.

The account identity was already being read to test the session and then thrown away; it is kept and
  passed to the queue now. Left out silently, like the system lists and for the same reason: it is a
  standing property of somebody else's playlist rather than news. They stay mirrored and readable,
  which is the whole reason to have them.

That was also the last thing making a scheduled run exit 1 — a timer draining a queue that could
  never empty, reporting failure every half hour for two playlists nothing was ever going to write
  to.


## v0.12.5 (2026-08-06)

### Bug Fixes

- **config**: Show every setting, not two of them
  ([`dd9b717`](https://github.com/datapointchris/ypl/commit/dd9b717e852cfff8633c79b843428f61357384b7))

`config show` says it prints the settings in effect including defaults, and printed two of the
  seven. The five it left out include `background_sync`, which the README tells you to set to turn
  the timer off — so there was no way to confirm it had taken — and `sync_minutes`, which decides
  how long a run lasts. Walked off the dataclass now, so a setting added later cannot go missing
  again, and rendered as it would be written in the TOML so a value can be copied back rather than
  translated.

### Documentation

- **readme**: Say where the scheduled runs write their log
  ([`0736808`](https://github.com/datapointchris/ypl/commit/07368080421f2ee5f08d2f3f65ffa56e089c3bd7))

The bug that hid for a day left its only trace there: every timer-driven sync failed for want of
  yt-dlp on PATH while `ypl status` reported a healthy schedule, and nothing pointed a reader at the
  one file that said so.


## v0.12.4 (2026-08-05)

### Bug Fixes

- **sync**: Give the timer a PATH that finds yt-dlp
  ([`f819e04`](https://github.com/datapointchris/ypl/commit/f819e048ff5f513ee139f69c3ccb8f5ea11c922c))

Every timer-driven sync on this machine has failed, from the first one:

yt-dlp is not on PATH — install it to read playlists

launchd starts an agent with `/usr/bin:/bin:/usr/sbin:/sbin` and nothing else. Homebrew's yt-dlp is
  in `/usr/local/bin`, so a scheduled run found no reader and exited 1 every thirty minutes while
  every run at the prompt worked. `ypl` itself is scheduled by absolute path for precisely this
  reason, and then the run shells out to `yt-dlp` by name.

The unit carries a PATH now, built from where those binaries actually are rather than by copying the
  installing shell's whole PATH: what a run needs is its tools, and an inherited PATH also bakes in
  whatever else was set the day the timer went in. `ensure` asks whether an existing unit can reach
  them, so the machines already running a broken one repair themselves — the command matches and the
  interval matches, so nothing else would have noticed. It asks that rather than comparing the PATH
  string, so an unrelated change to the shell's PATH does not unload and reload the unit on the next
  sync.

The bug that hid it: `installed()` read the command and the PATH out of the systemd *timer* file,
  where neither lives. Both came back empty, so every comparison failed and the unit was rewritten
  on every sync — which also means the command check added a commit ago never worked on Linux. A
  launch agent holds all of it in one file; the systemd pair splits the interval from the command
  and the environment.

### Refactoring

- **sync**: Drop an in_sync nothing reads
  ([`83184e5`](https://github.com/datapointchris/ypl/commit/83184e5fee3af8ddbd6bc5cafc94d7ecc15644dd))

Defined, documented as what makes the run log readable at a glance, and called from nowhere. Putting
  it in the payload instead would store a derived answer beside the three fields it derives from,
  which is the objection that keeps the push queue out of storage.


## v0.12.3 (2026-08-05)

### Bug Fixes

- **sync**: Leave the playlists Music will not serve out of it
  ([`a8da618`](https://github.com/datapointchris/ypl/commit/a8da618419ce9f3e64bfa4addaff2e9270b478ff))

The first real run against the account failed on 23 of its 42 playlists, every one of them the same
  way: `Unable to find 'contents'`. It is not the session — that run reported `signed_in: true`, 16
  playlists reconciled through the same client, and the response YouTube does return carries
  `logged_in: 1` and `has_unlimited_entitlement: True`.

It is privacy. Measured on the account: `BE HAPPY`, `Meditation` and `Yoga` read `availability:
  public` and adopt fine; `Art` and `Computers` report no availability at all — private — and
  YouTube Music answers a browse for either with a page holding a responseContext, a trackingParams
  and nothing else. No contents, no error, for the owner with a live session. So a non-public
  playlist cannot take part in the remote half at all, and one adopted before it went private can no
  longer be reconciled or pushed either, which is why this filters all three rather than only the
  sweep.

Left out rather than attempted, and reported as a standing fact rather than a failure, for the
  reason `skipped` already exists: nothing here can change it, so a run that called it a failure
  would report the same 23 playlists every half hour for as long as the timer runs — and did, on top
  of burying the counts `ypl status` exists to show.

Decided from the mirror sweep that just ran rather than from a stored column: yt-dlp reports
  `availability` on the same free request the mirror already makes, so this costs no request, needs
  no re-sync, and re-answers itself the moment a playlist's privacy changes on YouTube.


## v0.12.2 (2026-08-05)

### Bug Fixes

- **sync**: Schedule the ypl symlink, not what it points at
  ([`e606965`](https://github.com/datapointchris/ypl/commit/e606965c3a77db01d2d906e71e581686f9e2b03a))

`~/.local/bin/ypl` is a link into uv's tool directory, and resolving it put that directory's
  internal layout into the launch agent. uv promises to keep the link; where it points is uv's
  business, and a version of it that reorganises leaves a timer naming a path that no longer exists.
  Absolute without following symlinks — absoluteness was the point, since launchd and systemd run
  with almost no PATH.

### Documentation

- **readme**: Mention --version among the reads
  ([`e3d83b2`](https://github.com/datapointchris/ypl/commit/e3d83b20fbe8a7d7b66c48825b75b19e54bdc731))


## v0.12.1 (2026-08-05)

### Bug Fixes

- **status**: Point at the command, not a bare count
  ([`279e96e`](https://github.com/datapointchris/ypl/commit/279e96e4983d6c6ab6d4e81461b4f8974821e59c))

The trailer under a truncated failure list said how many more there were and left the reader to work
  out where. `~/dev/standards/cli-design.md` settles that one: no remainder counts, the trailer is
  the command that shows the rest.


## v0.12.0 (2026-08-05)

### Bug Fixes

- **sync**: Replace a timer naming a ypl that moved
  ([`7790ec0`](https://github.com/datapointchris/ypl/commit/7790ec05d24506fe9bf3593e3720cb5d78744df1))

`ensure` compared only the interval, so a unit naming a `ypl` that has since moved was left alone —
  firing, failing, and reported by `ypl status` as scheduled every 30 minutes, which is worse than
  no timer at all. It compares the command now and reinstalls when it disagrees, so a machine
  repairs its own timer on the next sync.

Which matters because the suite installed one here. `isolated_home` redirected the three XDG
  variables, and a launch agent's path comes from HOME, so every test that invoked `sync` on a mac
  wrote ~/Library/LaunchAgents/com.ichrisbirch.ypl.plist and had launchctl load it: this machine has
  been running a ypl timer against a checkout's venv, logging into a pytest temporary directory that
  no longer exists. HOME is redirected too now, and `run_manager` is stubbed for the whole suite
  rather than by the timer tests that opted in — the tests that installed it were about adoption and
  had never heard of the scheduler.

A virtualenv's `ypl` is also passed over when an installed one is on PATH. Developing means `uv run
  ypl sync`, which puts the checkout first, and a unit bound to it dies with the next rebuild of
  that directory.

### Features

- **cli**: Answer --version, like the rest of the fleet
  ([`6c4aa3f`](https://github.com/datapointchris/ypl/commit/6c4aa3fc476e1f7b11b0ab6e05d84f342d54d132))

Every other CLI here says which build is running; ypl was the one that could not, which matters most
  for the tool with a timer — a background sync and a prompt can be two different versions and
  nothing said so. One line, `ypl <version>`, with the commit appended when uv installed from a git
  ref rather than a release, because a version alone does not identify a build that tracks a branch.

`status` also stops reprinting the run it is summarising. The three things a run did were rendered
  with the lists themselves rather than their lengths, so a sweep that took over seventeen playlists
  printed seventeen names where it meant "17 adopted", and a run where every playlist failed printed
  one line each — pushing the playlist and enrichment counts, which is what the command is for, off
  the screen. Five failures now, then how many more. `--json` still carries all of them, and so does
  the log.


## v0.11.2 (2026-08-05)

### Bug Fixes

- **remote**: Rebuild the session from the browser before every run
  ([`4d62be8`](https://github.com/datapointchris/ypl/commit/4d62be80546850c3358b93237a9cc0e16f2ce228))

Why the session was signed out three hours after signing in: Google rotates `__Secure-1PSIDTS` and
  `__Secure-3PSIDTS` while you stay signed in, and the session file is a photograph of them. yt-dlp
  re-reads the cookie jar on every call, which is why `ypl sync` went on mirroring private playlists
  all afternoon while the write path had quietly become a signed-out visitor.

The browser is already recorded — that is what `auth.json` is for — so the session is now rebuilt
  from it before anything that might write. It costs no request, the cookies come out of a local
  file, and it makes signing in once actually mean once for as long as the browser stays signed in.

A browser that cannot be read leaves the stored session alone. It may still be good, and stale beats
  absent when nothing else can sign in.


## v0.11.1 (2026-08-05)

### Bug Fixes

- **remote**: Treat a signed-out session as one, not as an odd error
  ([`6dcccef`](https://github.com/datapointchris/ypl/commit/6dcccef2d1033ac6c4722adf621a0d5346f33eed))

The first real sync failed twenty-five playlists with four kilobytes of YouTube JSON each, and the
  fact explaining all of them — the stored session is signed out — appeared nowhere. Three separate
  holes let that happen.

`account()` is meant to be the check that a session works, and a signed-out session does not fail
  it. It answers, with the account menu a logged-out visitor gets: Get Music Premium, Settings,
  Terms, and no account header. ytmusicapi raises a navigation KeyError whose message says nothing
  about authentication, so it was translated to a generic error, kept by `remote auth`, and believed
  by everything after.

The sync then asked forty playlists individually instead of asking once. It now checks the session
  before any of the work and skips the whole remote half with one line naming the command that fixes
  it.

And the reads that did succeed were the public playlists, which come back anonymously with no
  `setVideoId` on any slot — enough to look like a reconcile and useless to a push. Adoption now
  refuses a read where no slot carries a handle, and a base that has none counts as drift, so the
  seventeen already written repair themselves on the next signed-in run rather than sitting there
  matching the mirror forever.

Failures are also truncated. Forty four-kilobyte messages is a log nobody opens twice, which for an
  unattended sync is the same as no log.


## v0.11.0 (2026-08-05)

### Bug Fixes

- **sync**: One sync at a time, so the timer cannot double a run
  ([`6e6718f`](https://github.com/datapointchris/ypl/commit/6e6718f533c87a44d0318b227a8b7df192f3e1bb))

Nothing stopped two syncs running together, and the timer makes that reachable: it fires at startup
  and every half hour, onto whatever is already going. Two runs would adopt the same playlists twice
  and write the same M3U files and bases from both processes. The mirror survives it — that is WAL —
  but the files and the request budget do not.

An advisory flock, released by the kernel when the process dies, rather than a pid file that a crash
  leaves behind and that then stops every later run until someone notices. For something unattended,
  that is never.

A second run exits 0 saying so: the timer landing on a run in progress is the system working, and
  failing would fill the agent log with errors about a sync that is happening.

### Features

- **status**: Say whether a sync is going on right now
  ([`d39e18f`](https://github.com/datapointchris/ypl/commit/d39e18f8665acaaf99f7e590db07a11b13293079))

`ypl status` could only describe runs that had already finished, which leaves the one question a
  background process actually raises unanswered: is it working at this moment, or stuck. A run in
  flight and a run that died look identical from the last log line.

The lock already knows. Taking it and dropping it again is the check, and it needs no pid file to go
  stale.


## v0.10.1 (2026-08-05)

### Bug Fixes

- **sync**: The sync installs its own timer, so there is no schedule verb
  ([`6b3143c`](https://github.com/datapointchris/ypl/commit/6b3143cb8889ee76af4db94f05e47cae48701264))

`ypl schedule install` was one more thing to remember, which is the exact problem the timer exists
  to remove. The first `ypl sync` on a machine now sets it up as a side effect and every run after
  that finds it already there, unmentioned.

Turning it off is a setting rather than a verb — `background_sync = false` removes the timer on the
  next run — because nothing installed it and so nothing should have to uninstall it.

A machine where launchd or systemd will not co-operate still syncs: the run happening now matters
  more than the ones that would have followed it, so a failed install is silence rather than an
  error.


## v0.10.0 (2026-08-05)

### Bug Fixes

- **db**: Let a reader in while the background sync writes
  ([`4a6a05e`](https://github.com/datapointchris/ypl/commit/4a6a05ef9e4be378ce9bac4d9f8240e44ba0e82d))

The mirror had one writer and it was always the person typing. Now a timer spends most of a run
  writing enrichment rows, and under the default journal `ypl playlists show` blocks behind it and
  fails after five seconds — a command failing in your face because of a process nobody asked to
  start.

WAL, so a reader carries on against the last committed state, and a busy timeout long enough to
  outlast a write rather than Python's five seconds. A tracklist arriving a minute later costs
  nothing; a command that will not answer costs the whole point of running this unattended.

### Features

- **sync**: One command that runs itself, in both directions
  ([`7b54481`](https://github.com/datapointchris/ypl/commit/7b5448116e56cb9dd8fd9ecc84c243514718cae9))

Syncing was four commands in a fixed order — sync, adopt, pull, apply — plus enrich, and a sync you
  have to drive is one that silently stops happening. `ypl sync` is now the whole loop, `ypl
  schedule install` runs it at startup and on an interval, and `ypl status` is how you find out
  whether it is working.

The order is what makes it cheap. The mirror read costs no quota and is also the change detector, so
  a playlist whose mirror still matches its file is never read through the write client — a library
  where nothing moved overnight spends no write requests at all. Reconciling precedes pushing
  because a push against a stale base refuses by design. Enrichment is last: it is the only
  unbounded step, and the only one where stopping early costs nothing.

Work is a queue derived on every run rather than stored, for the reason the push queue is: what is
  owed is a fact about the files, the bases and the mirror, and a written-down copy is a second
  answer that can disagree. It is ordered by what you would notice missing — a wrong local file
  first, an edit that has not landed second, a playlist that has never been here third.

One budget covers every action, priced by what each costs: a read is 1, a write 5, a playlist
  creation 25, since that is the only endpoint with a limit anyone has measured. A run stops when
  the budget is spent and the next one continues, which is safe only because every step re-derives
  its own remaining work.

Two failure modes that only appear once nobody is watching:

- A playlist deleted here would be re-adopted by the next run, so the deletion is recorded in
  `declined.json` — data, since nothing can rebuild an intention. - A video that will never extract
  would be retried every run forever, so a full extraction saying it is gone is recorded in a new
  `enrich_failures` table. A table rather than a flag on `videos` because a new table reaches an
  existing mirror, and because `is_unavailable` is what the flat listing says and sync rewrites it
  every run.

Enrichment is ordered by playlists that live here before the rest of the mirror, so `Liked videos`
  at five thousand entries cannot swallow days of budget ahead of the playlists that actually get
  played.


## v0.9.0 (2026-08-05)

### Features

- **remote**: Take over the playlists YouTube already holds
  ([`e2bac6a`](https://github.com/datapointchris/ypl/commit/e2bac6a190f386638416d579ba67df6f71ebce84))

A reconcile only reaches a local file carrying a remote id, and that id was only ever written by a
  creation ypl performed — so the whole merge layer served the playlists this tool made and none of
  the ones the account actually has. `ypl remote adopt` writes the file, binds it, and records the
  read as the base, after which a playlist made in the web player is an ordinary local playlist that
  happens to already exist.

Written from the write backend's own read rather than from the mirror, which it has to be twice
  over: the base needs each slot's setVideoId, and a mirror hours old would record videos YouTube no
  longer holds, which the first push would faithfully put back.

Adopting also makes both stores hold one playlist, and a name matching both was an ambiguity error —
  so adopting the account would have made every playlist unreachable by name. `known_playlists` now
  drops a mirrored row whose id a local file is bound to. That retires an existing bug too: a
  playlist created here, pushed, then re-mirrored by `ypl sync` used to go ambiguous with itself.

The sweep covers what the signed-in account owns, matched through the slug because yt-dlp's channel
  and YouTube Music's account name agree on nothing else. Someone else's playlist is left out of it
  — nothing here can write to that — but is still adopted when named, since a collaborative playlist
  cannot be told apart from it. Liked videos and Watch later are refused either way.


## v0.8.0 (2026-08-05)

### Features

- **playlists**: Kebab-case what ypl names, keep what YouTube named
  ([`dd8a8fc`](https://github.com/datapointchris/ypl/commit/dd8a8fcfd883cac20cd94653f10329e352d50684))

A playlist made here is named its own slug — `create 'Six Hour Work'` writes `six-hour-work`, and
  that is the title it gets on YouTube. The casing alone then says which playlists were assembled
  here, among forty made by hand in the web player, with no field for it to keep in step.

The other direction is the same rule read backwards: anything arriving from YouTube keeps its name
  verbatim, because re-casing `DRIVE TIME` would rewrite someone's own playlist under them on their
  own account.

`local.authored_name` is the one boundary this passes through, so an adopted name simply never goes
  near it. The resolver and the shell completion now compare slugified forms on both sides, which is
  what keeps the rule from costing anything at the prompt: `drive time` finds `DRIVE TIME`, and `Six
  Hour Work` finds `six-hour-work`.


## v0.7.1 (2026-08-05)

### Bug Fixes

- **auth**: One login, not two
  ([`5368669`](https://github.com/datapointchris/ypl/commit/5368669c22d5e8f9b0a3d514ba2838b8235ac8a4))

`ypl remote auth --browser safari` signed in, and `ypl sync` then said it had nothing to read. Both
  were working exactly as built, which was the problem: auth stored the YouTube Music session, every
  read borrows cookies through yt-dlp instead, and nothing connected the two — so the tool held two
  unrelated ideas of being logged in and signing in only ever set one.

Signing in now records which browser it read from, beside the session file it wrote, and every read
  falls back to it. An explicit --browser still wins, then the config, then this. A `sync --browser`
  that came back with playlists records it too, since that also proves the browser holds a session.

The config keeps precedence over what was inferred: it is the setting a person wrote down.


## v0.7.0 (2026-08-05)

### Bug Fixes

- **reads**: Pace the bulk reads, and stop when YouTube pushes back
  ([`8c0c43e`](https://github.com/datapointchris/ypl/commit/8c0c43e6a8cdc9ab39c0b7aa81eed197e0bfabf5))

`enrich --all` over a library is thousands of sequential extractions, and they carry your cookies.
  Reads cost no API quota, which is why nothing here was ever paced — but quota is not the only
  thing forty or four thousand back-to-back signed-in requests spend, and a burst is the shape that
  gets an account looked at. The write path has been careful about exactly this since it was built;
  the read path was not, and it is the one that makes the requests.

So both bulk loops — `enrich` over videos and a bare `sync` over every playlist in the account — go
  through the same floor the write path uses, at `request_interval_seconds` from the config, two
  seconds by default.

And yt-dlp saying "Sign in to confirm you're not a bot" or 429 is now its own error, distinct from a
  video that simply cannot be read. One video failing is skipped and reported; this stops the run.
  Answering "slow down" with more requests is the worst available move, and enrichment is resumable
  by construction, so stopping costs time and nothing else.

Throttle moves out of `remote` into its own module, so a read command can pace itself without
  importing the write path. Its sleep resolves at call time rather than as a default argument, which
  is what lets a test replace it — bound at import, the suite really slept, and a suite that sleeps
  is a suite that gets its pacing deleted.

### Features

- **cli**: Complete playlist names, and titles inside the current one
  ([`0ab474d`](https://github.com/datapointchris/ypl/commit/0ab474d7c25d24232dcf32d910d6676ba78e2248))

`ypl playlists edit <TAB>` offers the playlists that exist. Nothing here should ever be retyped: the
  names are long, they have spaces, and the tool already knows all of them.

Commands that write offer only local playlists, because offering a mirrored one to `ypl playlists
  edit` would be a lie about what it can do. The fragment arguments on `drop`, `later` and `sooner`
  complete against the titles inside the current playlist, which is the same idea as the fragment
  itself: what you can say about the thing playing is its name.

Matching is anywhere in the title rather than at the start, because these titles begin with the
  artist or the event and the part you remember is rarely the first word.

Every failure answers with nothing. A completion runs on a keystroke, in a shell that will print a
  traceback into the middle of the line being typed, and no reason for one is worth that.

Run `ypl --install-completion` once per machine.


## v0.6.0 (2026-08-05)

### Features

- **videos**: Hand over the library as something choosable
  ([`91cfa6b`](https://github.com/datapointchris/ypl/commit/91cfa6b884e04122827fb351b2f6053199849311))

`ypl videos list` is the read curation runs on. One row per mix: how long it is, whose channel it
  came from, which of your playlists already hold it, and the artists inside it, commonest first.

Collapsed rather than complete, because complete is unusable — forty tracks each across a library of
  thousands is megabytes, and no amount of it answers the question being asked. What answers it is
  which artists are in there.

Nothing here labels genre or tempo, and nothing will: a chapter marker does not carry either.
  Knowing that Shimza is uptempo and Bonobo is not is the reader's job, so this hands over the
  artists and stops. The playlists a video already sits in are carried for the same reason — you
  named those, so they are a judgement that already exists and costs nothing to pass on.

Filters are the questions curation actually asks: long enough to work to, containing this artist
  anywhere in the tracklist, from this playlist. The sort vocabulary is the same one as everywhere
  else minus `position`, which means nothing across playlists, and a test pins that it invented no
  names.

`enrich --all` comes with it, because none of this exists until the tracklists do and the default
  batch of fifty was never going to get there.


## v0.5.0 (2026-08-05)

### Features

- **playing**: Change the playlist without naming anything
  ([`895eb9e`](https://github.com/datapointchris/ypl/commit/895eb9ec24a9086af5c21452ad0a88f5924ae568))

`ypl drop`, `ypl later`, `ypl sooner` act on the playlist you are listening to, on the video that is
  playing, and take no arguments at all in the ordinary case. The other half of the id problem: the
  editor buffer handles rearranging eighty videos, and this handles the one you just heard and do
  not want.

Two facts make that work, and only one of them is stored. Which playlist is on goes in
  $XDG_STATE_HOME, set by `ypl play` or by `ypl use` for playback ypl has no part in. Where you are
  in it is read live from mpv's socket, and is simply unavailable when the player is a browser tab
  or a phone, because YouTube exposes nothing that would answer it.

That asymmetry is why every verb also takes a fragment of a title — `ypl drop wagram`. It is the
  answer for the case the socket cannot cover, and it is still not an id. A fragment matching two
  videos lists them and changes nothing.

`drop` sends mpv to the next video as well, since continuing to play what you just deleted is not
  what dropping it meant, and `--keep-playing` says otherwise. Moving past either end clamps rather
  than refusing: `ypl later` on the last video is a reasonable thing to type without checking first.


## v0.4.0 (2026-08-05)

### Features

- **playlists**: Rearrange a playlist in your editor
  ([`7e6860d`](https://github.com/datapointchris/ypl/commit/7e6860dc501bb178c9c193f6445e86cdd4273ee8))

`ypl playlists edit NAME` opens one line per video in $EDITOR — the id first, then the title and
  duration. Move lines to reorder, delete a line to remove, paste a URL to add, save to apply. `git
  rebase -i`'s shape, for the same reason: rearranging a list is something an editor is already
  better at than any command could be, and the id sits on the line without ever being typed.

Typing eleven characters to identify a mix is what made editing a playlist while it played
  intolerable, and no number of extra commands fixes that. This does, for bulk work; the
  current-playlist verbs will do it for tweaks.

An empty buffer aborts rather than emptying the playlist, since deleting every line by accident must
  not be how an authored playlist is lost, and a non-zero exit from the editor — `:cq` — aborts for
  the same reason. A line that is not a video is refused with its line number and nothing is
  written.

The buffer takes the playlist's own ids as known, so whatever the file holds parses back even if it
  does not match the eleven-character rule. A tool has to accept its own output.

Reads the buffer from stdin when something is piped in, which is both how this is tested without an
  editor and how Claude rewrites an order in one shot.


## v0.3.0 (2026-08-05)

### Features

- **sync**: Mirror the whole account, not one URL at a time
  ([`8e52d67`](https://github.com/datapointchris/ypl/commit/8e52d67b0bb47ffb90ab7a24bc2cb930f4a0b251))

`ypl sync` with no URL lists the playlists the signed-in account has and mirrors every one of them.
  Pasting forty-two URLs in from a browser was never a reasonable way to start using this, and
  nothing about the design required it — the account has always been able to say what it holds.

Read from YouTube's own playlists feed rather than the YouTube Music library, because they are not
  the same list: the library call returns one podcast queue on this account while the feed returns
  the forty-odd playlists actually saved. Music's library is a view of Music, and these playlists
  were made on YouTube.

Both steps are yt-dlp reads, so a whole library costs no quota, which is what makes syncing
  everything the default rather than something to ration. A playlist YouTube will not serve is
  collected and named at the end rather than ending the run — one collaborative list gone private
  must not cost the other forty their sync.


## v0.2.0 (2026-08-05)

### Features

- **remote**: Sign in from a browser, without DevTools
  ([`2b944bf`](https://github.com/datapointchris/ypl/commit/2b944bf9e8505026fedd011394873fea2f5cf376))

`ypl remote auth --browser safari` reads the cookies of a browser already signed in to YouTube and
  builds the session from them. No Network tab, no hunting for a request, no pasting a header block
  into a terminal and guessing at when EOF arrives.

The paste was never the point. Those headers carry three facts and the cookie jar holds all of them:
  the cookie itself, which account is selected, and a SAPISIDHASH — and that last one, which looks
  like the important one, is the least. ytmusicapi recomputes it from the cookie before every
  request because it is timestamped; the stored value exists only so the file is recognised as
  browser auth rather than OAuth.

Cookies come from yt-dlp, which already decrypts Safari's binary format, Chrome's keychain-encrypted
  database and Firefox's sqlite, and which is already a hard dependency because every read goes
  through it. It has no "dump the cookies" mode, so the jar is written as a side effect of a run
  aimed at an unresolvable host: the export happens before the network does, so nothing is requested
  from YouTube by a command reading a local file.

With no --browser it falls back to `cookies_from_browser` from the config, which already names the
  browser holding a YouTube session, and only then to a paste on stdin. The paste instructions now
  say to press Enter before Ctrl-D, because EOF only registers at the start of a line and a paste
  without a trailing newline swallows the first one.


## v0.1.1 (2026-08-05)

### Bug Fixes

- **remote**: Tell the truth about finding the request headers
  ([`ca33ba5`](https://github.com/datapointchris/ypl/commit/ca33ba576b30fd5d46c0aeda77cf44b424d1a3f6))

The sign-in instructions sent you to DevTools to filter for `browse`, and the panel stays empty: the
  Network tab records only while it is open, and Music is a single-page app that makes no requests
  once it has loaded. An empty panel then reads as the instructions being wrong rather than as the
  page having nothing to say.

So: open the panel, then click something to make the page talk. And filter for `/youtubei/` rather
  than one endpoint — every authenticated request carries the same credentials, and ytmusicapi only
  needs `cookie` and `x-goog-authuser` to be somewhere in the paste.

### Documentation

- **readme**: Stop pinning a release tag that does not exist
  ([`86ac480`](https://github.com/datapointchris/ypl/commit/86ac4805eaaa505d33f46680572530cc5a5b220d))

The install example named v0.2.0, which was a guess at what the first release would be called. It is
  v0.1.0, and naming any version there rots it on the next release — dotfiles resolves the newest
  tag from the releases API rather than carrying a number.

- **repo**: Follow the rename to ypl
  ([`fe79634`](https://github.com/datapointchris/ypl/commit/fe79634cb506622ee998bdb1e154e16b743a8f0d))

The repository is `ypl` now, at `~/tools/ypl` with the other personal CLIs, so the tool, the
  command, the module, the distribution and the repo all finally spell the same thing.

`repo` comes back off the update config, since the default — the tool name — is now correct.
  CHANGELOG entries keep their old commit URLs; they are generated history and GitHub redirects
  them.


## v0.1.0 (2026-08-05)

### Bug Fixes

- Clear the lint findings CI caught on the legacy splitter
  ([`0a354cd`](https://github.com/datapointchris/ypl/commit/0a354cd10dcc7fe82ba75e64e59495077fbaa5eb))

The toolchain commit carried no Python files, so the pre-commit ruff hooks had nothing to check and
  CI was the first thing to run them.

B020 was the one with teeth: the split loop bound its control variable to the same name as the
  iterable it consumed, so the progress line reported the whole video list rather than the chunk
  just added. Renaming the control variable fixes the count as well as the lint.

B019 replaced @lru_cache on a method with an instance dict, and the stale top-level [tool.ruff]
  ignore key is gone — ruff has been reading it from lint.ignore for some time.

- **config**: Report a broken config file instead of a traceback
  ([`2529361`](https://github.com/datapointchris/ypl/commit/2529361054bd6d6c419cefa5c0d6961a89fe72c0))

A hand-edited config.toml with a syntax error crashed every command that loads settings, with a
  tomllib traceback and exit 1. It is a usage error - the file only got that way by hand - so it
  exits 2 and names the file, the parser's complaint, and `ypl config example`.

enrich_batch_size is validated rather than trusted. A 0 or a string would have reached the SQL LIMIT
  clause, where 0 silently means "enrich nothing" and looks like the library is already done.

- **output**: Stop Rich breaking paths and ids across lines
  ([`f756b1d`](https://github.com/datapointchris/ypl/commit/f756b1d84ce7f18aa0173e08ff6f4ba8c1a8b378))

CI caught this on an 80-column runner: the config error printed the file as `.../ypl/config\n.toml`,
  because Rich hard-wraps at the terminal width and will break mid-token to do it. Every message on
  this console carries something meant to be copied - a config path, a playlist id, a video id - so
  a wrap that lands inside one defeats the reason it was printed. Same defect class as the
  ellipsis-truncated id column.

soft_wrap lets the terminal wrap the line instead, which does not insert a newline and leaves the
  token whole on the clipboard. Tables keep the wrapping console, since their columns manage width
  themselves.

The regression test pins a width narrower than any real path. Verified it fails without the fix
  rather than trusting that it would.

- **tests**: Build the long socket path instead of inheriting it
  ([`881e515`](https://github.com/datapointchris/ypl/commit/881e5150723ba3f1f3ec31b730cb6d1780b74a8e))

The socket-length test leaned on pytest's tmp_path already being over the 104-byte limit. That is
  true under macOS's private temp directory and false on Linux, so it passed locally and failed in
  CI, where the path came to 74 bytes and the socket was addressable after all.

The fixture now nests the state directory deep enough to overflow on either platform, so the test
  asserts the behaviour rather than the environment.

### Chores

- Add .planning to gitignore
  ([`6aeb28b`](https://github.com/datapointchris/ypl/commit/6aeb28b8a27946ecbdd30392df1beea363575c21))

- Add .planning to gitignore
  ([`ca2f6bd`](https://github.com/datapointchris/ypl/commit/ca2f6bdd20da2de92345ba3e02c87ab2b79703ba))

- Add TODO and ignore service account credentials
  ([`bb10841`](https://github.com/datapointchris/ypl/commit/bb10841d6b21469e1d000e7e64d6ff67a74ed4ba))

- Clean up generated gitignore
  ([`01343a9`](https://github.com/datapointchris/ypl/commit/01343a9201e5eec12195f5e10525c4c1381380c8))

- Stop tracking runtime progress state
  ([`52e2057`](https://github.com/datapointchris/ypl/commit/52e20578d396d6fe9cce512c9096ff606472e26b))

The split checkpoint file is state the tool writes, not source, so every run dirtied the working
  tree. Ignore the pattern rather than the one filename, since the rework moves this under
  $XDG_STATE_HOME.

- Update readme
  ([`6edaafc`](https://github.com/datapointchris/ypl/commit/6edaafce21406885ce2f5a18aa71b25d9480fddd))

- **toolchain**: Adopt the generated configs and CI
  ([`8d95cb8`](https://github.com/datapointchris/ypl/commit/8d95cb8bc5a67bde28497ee4e795305a5e56fbc2))

Deploys forge toolchain 11: pre-commit hooks, .editorconfig, .markdownlint.json, .shellcheckrc, the
  ruff/pytest/pyright pyproject merge, and the generated validate.yml.

Migrates the build off poetry to uv_build with a PEP 621 [project] table, which the generated
  uv-lock hook requires. Drops the black, isort, bandit and refurb config with the tools themselves
  — ruff replaces all four.

Registers the repo as active in ~/dev/repos.json with python and actions components; every generator
  reads the registry.

### Documentation

- Describe ypl, and retire the stale TODO list
  ([`08ab878`](https://github.com/datapointchris/ypl/commit/08ab8788bb0e28e423b704d3e301c2aa4552ac6b))

The README still described the argparse splitter. It now states the quota arithmetic that drives the
  whole design, since that is the thing nobody would guess from the code.

TODO.md held two entries: switching to the dataset library, which the rework decided against by
  using sqlite3 directly, and a link to the google-api-python-client OAuth guide. The link moved
  into the icb item for the write backend, where it sits next to the decision it informs. Intentions
  belong in icb, not a markdown file that rots.

- **cli**: Stop promising ypl never writes to YouTube
  ([`7cdd2b5`](https://github.com/datapointchris/ypl/commit/7cdd2b573541288559dd601b70149db2d65413a0))

The root help stated it as a rule and the Building panel repeated it. Playlists are now marked for
  syncing on creation, and `ypl remote` is being built to push them, so the honest framing is that
  organizing is local and instant while going back up is separate, queued and deliberately slow.

### Features

- **next**: Suggest what to put on, from listening history
  ([`87eae45`](https://github.com/datapointchris/ypl/commit/87eae45a19ae3dbef6c1907a8ef375cb753e3937))

ypl next answers "and specifically, what now?" — least recently listened to, never-played first. It
  is the resolver `menu next` delegates to, so a listen pursuit names a mix rather than the word
  "listen"; the register snippet is in the README. `ypl plays add` records a listen and `ypl plays
  list` shows them.

History is logged, never inferred. ypl play hands mpv the whole list at once and blocks, so it
  cannot know which of it was actually played — menu next's on_log is the mechanism, the same one
  icb tasks complete {id} uses.

History is a file, not a table, and that is a correction made before it shipped. The repo's own rule
  is that the mirror holds state because free reads rebuild it, while anything authored lives under
  $XDG_DATA_HOME. Nothing rebuilds a record of what you listened to, so a plays table would have
  made the deletable copy the only copy. It is plays.jsonl beside the playlists, appended one line
  per listen, and append order settles two listens logged in the same second without depending on a
  one-second clock.

The schema now runs on every open with every statement idempotent, so a new table reaches an
  existing mirror without a migration system. Verified against a real pre-existing database: rows
  survived and the seeded lookup did not double.

next draws afresh among everything tied at the same rank, since on an untouched library that is all
  of it and a stable sort would name the same mix forever. menu next caches its own draw, which is
  where within-session stability comes from.

- **play**: Play a playlist and report the current track
  ([`dd95dfa`](https://github.com/datapointchris/ypl/commit/dd95dfa0bac3046673a392753fba3f67b0b3974d))

ypl play runs mpv in the foreground over either kind of playlist, and ypl now reads mpv's IPC socket
  to say what is playing. Because the mirror holds a tracklist with real timestamps, now reports the
  track inside a two-hour mix rather than the name of the mix, which is the payoff for storing
  chapters at all.

URLs go to mpv as arguments rather than as a playlist file, so --sort and --limit mean the same
  thing here as everywhere else and a mirrored playlist plays without writing a file first. --audio
  drops the video window; mpv_arguments in the config is the escape hatch for everything else.

Every playback opens the IPC socket: it costs one argument when unused and is the only thing now can
  read. A socket left by a crashed mpv is cleared first, but only after checking that nothing
  answers on it, so a running player is never cut off.

The socket path is length-checked before mpv sees it. A unix socket address is 104 bytes on macOS,
  and over that mpv logs `Could not create IPC socket` and plays on regardless — leaving now
  reporting nothing with no way to tell why. Found by running against a real mpv from a deep temp
  directory.

now exits 1 with nothing on stdout when nothing is playing, so a status bar can run it unguarded. On
  Arch none of this is needed: waybar's mpris module shows mpv already once mpv-mpris is installed.

The IPC layer is tested against a fake mpv socket rather than a mocked one, because what breaks here
  is protocol-shaped — event lines arriving before the reply, replies coming back out of order,
  properties mpv declines to answer.

- **playlists**: Build and edit local playlists
  ([`9a519f2`](https://github.com/datapointchris/ypl/commit/9a519f2b62223f007c48da0fd861d68704ff7738))

Playlists you make are M3U files under $XDG_DATA_HOME/ypl/playlists, written by playlists
  create/add/remove/delete. Nothing here reaches YouTube, so curation is instant and unlimited while
  the 200-writes-a-day API quota stays spent only on a deliberate push.

An entry is a video and only a video. A mix holding forty tracks is one entry, because a video is
  the smallest thing that can be played; the tracklist is metadata about it in the mirror, which is
  what ordering and similarity will be computed from. Track-level entries would need EXTVLCOPT
  start/stop directives that VLC honours and Kodi does not, trading away the plays-everywhere
  property that chose M3U in the first place.

The file is the whole store — no sidecar and no table. Provenance rides on #YPL- comment lines that
  every player skips, so mpv --playlist plays a generated file with no ypl involvement at all.

One resolver now serves both stores: a name is searched across the mirror and the playlist
  directory, and ambiguity is an error rather than a guess. Writes scope themselves to local
  playlists so a copy named after its mirrored source stays editable. enrich upserts, because
  enrichment is a fact about a video rather than about its membership of a mirrored playlist.

- **playlists**: Split and reorder local playlists
  ([`a797241`](https://github.com/datapointchris/ypl/commit/a797241aebf3359abbcdf0736554321fa85729a6))

playlists split cuts a long playlist into several local ones by --size or --parts, and playlists
  order rearranges one you built, in place or --into a new name. Both write M3U files only.

Parts come out even rather than as full chunks and a stub: 140 videos at a size of 90 is two parts
  of 70, not a 90 and a 50. The legacy argparse splitter intended this but compared the remainder
  against half the total rather than half the target, so the branch never fired, and it divided by
  zero on a playlist shorter than the target. Both are fixed here rather than ported.

split defaults to playlist order where the legacy tool always shuffled — a deterministic default is
  easier to reason about and --sort random is one flag away. Every part is checked for a collision
  before any is written, so a split that would half-overwrite an earlier one fails having changed
  nothing.

The sort vocabulary grows longest, shortest and title, which order needs and every other command
  gets for free. SORT_CLAUSES is the SQL half and LOCAL_SORT_KEYS the Python half; a test asserts
  they cover the same names, because a name in one and not the other is a KeyError the first time it
  meets a local playlist.

Selections that drop deleted and private videos now say so. A split of 100 that quietly yields 95
  reads as a bug in the split.

- **playlists**: Sync new playlists to YouTube by default
  ([`be8ac94`](https://github.com/datapointchris/ypl/commit/be8ac94eae7571531f30d04cfeb6b8b08f48bc2c))

A playlist made here is meant to end up on the phone, so it is marked for syncing on creation and
  goes up on the next drain. --local keeps one on this machine, and promote/demote change it later.
  Created PRIVATE, which still appears in your own library on every signed-in device.

Syncing everything rather than promoting selectively also keeps the merge path exercised instead of
  rehearsed, which is the point at which sync bugs are cheap to find.

Two directives rather than one: "should be synced" and "is bound to a remote playlist" are different
  facts with a real window between them, so a playlist reads as local, pending, or synced. Written
  either way rather than only when true, so a hand-edited file says what it is instead of falling
  back to the default. A file written before any of this existed reads as synced, which matches how
  playlists are made now.

Playlist creation gets its own far slower throttle. It is the one endpoint with a limit anyone has
  measured — roughly twenty in fifteen minutes — and making everything sync is exactly what makes
  creation the most frequent write. A single creation still goes straight through; only a burst is
  spaced.

`kind` in the listing stays which store holds a playlist, because that is what --source filters on;
  where it sits on the way to YouTube is its own field.

- **release**: Publish releases, and name the distribution ypl
  ([`08d9a9c`](https://github.com/datapointchris/ypl/commit/08d9a9ccd3de6f88bd44112be953c4083f784f9e))

The fleet installs the personal Python CLIs from a release tag over git, not from PyPI, so there has
  to be a release to install. python-semantic -release on push to main, gated on validate, same
  shape as every other Python tool here.

The distribution is renamed from youtube-playlists to ypl, because uv keys a tool's receipt
  directory on the distribution name and pyselfupdate reads the running version from it. Installed
  under the old name, `ypl update` would look for a distribution called ypl, not find itself, and do
  nothing. The repository keeps its name, which is why the update config now states `repo` — it
  defaults to the tool name, and there is no repo called ypl.

- **remote**: Add the write backend, and drop the Data API
  ([`38fa038`](https://github.com/datapointchris/ypl/commit/38fa038c0ea4db09800e02aa01f74504ca14b478))

Settles the deferred backend decision in favour of ytmusicapi, and removes the machinery built for
  the other answer: the legacy argparse splitter with its 24-hour quota loop and checkpointing, and
  the google-api-python-client and google-auth-oauthlib dependencies.

The reasoning is volume, not convenience. playlistItems.insert costs 50 units of a per-project
  10,000/day, so the sanctioned path is 200 writes a day permanently and an 1,800-video
  reorganisation is eighteen days of draining — a daemon making requests around the clock for weeks,
  which is the shape that looks least like a person. The web client protocol batches: one request
  carries an actions array of a hundred additions, so the same work is a couple of dozen requests,
  fewer than doing it by hand in the browser.

That argument only holds while the saving is not spent on speed, so every call goes through a
  floor-interval throttle, batches are bounded at 100, and a rate-limit response raises rather than
  retries. The one documented limit is on playlist creation — roughly 20 in 15 minutes, clearing in
  hours — and retrying into it is what would turn a throttle into a pattern.

Reordering is the asymmetry worth designing around: add and remove carry an actions array, but
  edit_playlist takes a single moveItem with no batch form, so every move is its own request.
  move_plan computes the shortest move sequence via the longest already-ordered subsequence, which
  makes moving one video to the front of a 200-track playlist one request instead of 200.

The backend is a Protocol and the ytmusicapi implementation is its own module, so the Data API stays
  a one-module swap.

- **remote**: Push local playlists up to YouTube
  ([`56754ac`](https://github.com/datapointchris/ypl/commit/56754ac15a228711ba6eaeaa625710917aa812be))

`ypl remote plan` says what would change there; `ypl remote apply` does it. Two verbs and no
  `--apply` flag, and no `push` either — a third verb that also writes would be a second word for
  apply's job. The plan is the dry run by construction: same reads, same arithmetic, stopping before
  the first write.

A playlist that has never been up is created and bound to its new remote id before a single video
  goes into it. A creation that succeeded and was not written down is a playlist on YouTube that
  nothing here knows about, and the next run would make a second one — there is a test that fails
  the fill and then asserts the second run adds videos rather than creating again.

Everything else is a diff against the base. Additions go up a hundred at a time, removals go by the
  handle recorded for that slot, and the order is fixed with the fewest moves that produce it —
  planned against a read taken after the additions, because a video added a moment ago has no handle
  until it is read back.

A playlist YouTube has changed since the last reconcile is skipped rather than guessed at, and the
  run exits 1 saying so. Resolving that drift inside a push would be a second merge implementation
  on the write side, deciding conflicts without the reconcile's record. Pull first, push second.

The fake backend the tests run against now holds a playlist and mutates it on every write, so an
  assertion after apply is about what YouTube ended up with rather than about which methods were
  called.

- **remote**: Reconcile playlists with YouTube
  ([`b39b0bd`](https://github.com/datapointchris/ypl/commit/b39b0bd01f1570599e43ac93e894baac538da2f8))

`ypl remote pull` reads YouTube, merges it into the local file against the base, and records the
  read as the new base. Videos deleted there leave the file, videos added there arrive in it, and
  changes made here stay made and go up on the next drain.

The merge is pure and lives on its own, because it is the part that has to be reasoned about exactly
  and a test that has to build a playlist file to state a case is a test nobody writes. Membership
  merges per video; order is settled once for the whole playlist, since per-item order merging turns
  a moved track into an argument with no answer.

Three things the merge gets right that the obvious version does not. A reorder is judged only on the
  videos both sides still hold, or every addition would count as a reorder and hand YouTube the
  ordering of the whole playlist. A video arriving from remote is anchored after whatever precedes
  it there rather than appended, or a track added at the front of a playlist on a phone lands at the
  back of it here. And videos are keyed by occurrence rather than by id, because a playlist may hold
  the same mix twice and a set of ids reads the second copy as noise.

Nothing is queued at pull time. What has to go up is whatever the file and the base disagree about,
  so `remote plan` re-derives it and a pull run twice cannot double-queue anything. The file is
  saved before the base: the reverse order leaves a base describing a remote state the file was
  never merged against, and every video the merge was about to drop then reads as a local addition
  and goes back up to YouTube.

- **remote**: Record what YouTube held at the last reconcile
  ([`dec9505`](https://github.com/datapointchris/ypl/commit/dec9505727dd781121306317373cb41ace806cb0))

The third state a remote-wins merge needs. Local [A, B, C] against remote [A, C] has two possible
  histories — B was deleted on the phone, or B was added here and never pushed — and they are the
  same two lists with opposite correct actions. YouTube exposes no per-item modification time and a
  removal leaves nothing behind, so the only way to tell them apart is to have written down what was
  there last time.

One JSON file per playlist under $XDG_DATA_HOME/ypl/remote, beside the playlists rather than in the
  mirror: the mirror rebuilds from a free `ypl sync`, and this rebuilds from nothing, because
  re-reading YouTube answers what is there now rather than what was there then. It carries each
  slot's setVideoId, which is the handle the push path needs and which yt-dlp never returns, so it
  doubles as the handle map.

An unreadable base raises rather than reading as an absent one. Absent says nothing has been
  reconciled, so everything local is new; unreadable says a snapshot exists and cannot be trusted,
  and treating it as the former would queue deletions on YouTube for videos that were only ever
  added on the phone. Writes go through a temp file and a rename for the same reason — a truncated
  base is unrecoverable.

Deleting a playlist now deletes its base. Otherwise it outlives the file it describes, and the next
  playlist to slug the same way adopts it and reads as having had every one of those videos deleted
  here.

- **remote**: Store the YouTube session, once per machine
  ([`657e022`](https://github.com/datapointchris/ypl/commit/657e0226169fcfa7e5dcee9060d922207ad856a1))

`ypl remote auth` takes the request headers from a signed-in music.youtube.com tab, pasted or piped,
  and writes them to $XDG_CONFIG_HOME/ypl/ytmusic.json. Nothing else in the write path can be run
  against a real account until this exists.

ytmusicapi's own setup writes that file at whatever the umask allows. The cookie in it is the entire
  credential, so setup is called for its parsing only and the file is opened at 0600 here — it never
  exists world-readable, not even for the moment before a chmod.

A headers block that parses is not yet a session that works, so the command asks YouTube whose
  account it reaches and prints the answer. One YouTube rejects is deleted rather than stored to
  fail later, somewhere further from the paste; one that could not be checked at all — a throttle, a
  dead network — is kept, because that failure says nothing about the credential.

Browser headers rather than OAuth: ytmusicapi's OAuth flow now needs a TV-type Google client of your
  own, which is the Data API project setup this backend exists to avoid.

- **ypl**: Add the sync and read layer over a local mirror
  ([`45cb7e9`](https://github.com/datapointchris/ypl/commit/45cb7e9b7c97743e7d556448c3cbb0ae49334f1d))

First slice of the rework: a Typer CLI that mirrors playlists locally and reads them back, with no
  path to writing YouTube at all.

Reads go through the yt-dlp binary rather than the Data API. Two reasons, and the second is the
  decisive one: reads through yt-dlp cost no quota, and chapters are not exposed by the Data API
  under any part/field combination. Chapters are the whole basis for a mix tracklist — sampling
  Cercle, Anjunadeep and Boiler Room sets, two in three carry them, and the third has a timestamped
  description. yt-dlp is shelled out to rather than depended on, because it ships breaking fixes
  constantly and must track YouTube independently of this repo's lock file.

Three levels: playlist -> video -> track. Membership rows are replaced wholesale on re-sync since a
  reorder shifts every later position, while videos are upserted so enrichment already paid for
  survives. Deleted and private videos are kept rather than dropped, or every position after one
  would silently shift.

Effects are split impure -> pure -> impure per python.md: ytdlp reads, tracklist parses with no I/O,
  service writes. That is what makes the parser's real-world spread affordable to cover — dash
  variants, leading track numbers, hyphenated artist names, bracketed timestamps, and the DJ
  convention where an unidentified track is called ID. ID is deliberately not stored as an artist:
  similarity is computed on shared artists, so it would make every unidentified track in the library
  look like one prolific artist.

Paths follow the XDG split in data.md. The mirror is state and rebuilds from a free sync; local
  playlists will be authored M3U under XDG_DATA_HOME, which is why they are not rows in a database
  that should not be synced.

videos show takes ignore_unknown_options: video ids are base64url, so about one in thirty starts
  with a hyphen and Click read it as an unknown option. Found by running it, not by reading it.

### Refactoring

- Move main script into project
  ([`f0ceb03`](https://github.com/datapointchris/ypl/commit/f0ceb03128935244febe3169ea1551da3675da3e))

- **models**: Define the watch URL in one place
  ([`9c1c129`](https://github.com/datapointchris/ypl/commit/9c1c1294a17cc14e849438bab095c9e6a859b269))

The video watch URL was spelled out as an f-string in three modules. It is what local playlist files
  contain, what gets piped to other tools, and what yt-dlp is handed, so the three have to agree on
  the same string.

### Testing

- Pin the behaviour the stubs were hiding
  ([`a72d2ad`](https://github.com/datapointchris/ypl/commit/a72d2adee4b74552e6abf9a0558d3014f3c4b160))

A mutation pass over the suite — break the code, check the tests notice — found three assertions
  that a stub could satisfy on its own.

track_at was only ever exercised with a well-formed chapter tracklist, where the WHERE clause
  already narrows to one row and the ordering never matters. A description-derived tracklist leaves
  end_seconds NULL on every track, so all of them match at once and the latest start is the one
  playing. Now covered, along with a finished track not playing on and an unplaceable one being
  skipped.

ypl now was only tested through --json, leaving its human output — a nested f-string doing quote
  juggling — never run. Its title precedence was untested too: the mirror and mpv both offer one,
  and only the mirror's is right.

play never had a failing mpv, so a playlist that would not play looked like it played.

16 of 18 mutations are now caught. The two survivors are equivalent mutants rather than gaps: mpv
  never sends data alongside a non-success reply, and a NULL start_seconds already fails the `<=`
  comparison under SQL's three-valued logic, so the explicit IS NOT NULL is documentation rather
  than a filter.
