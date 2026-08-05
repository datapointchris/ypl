# CHANGELOG


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
