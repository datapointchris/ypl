# ypl

Organize and play YouTube playlists of long DJ mixes.

Most of the library this was built for is mixed sets — one video holding twenty to forty tracks by
different artists. YouTube already carries those tracklists as chapter markers, timestamped
description lines or one of its top comments, so ypl reads them and keeps them where they can be searched,
compared and rearranged.

ypl is two programs. A server keeps the channel's playlists mirrored and reads a tracklist for each
mix. A command-line client on every machine plays from it and changes it, so several machines see
one library rather than each keeping a copy that drifts.

## The server

The server in `api/` keeps the channel's playlists mirrored through the Data API, reads a tracklist
for each mix with `yt-dlp`, and answers the HTTP API the Go client speaks. Its reference — the
quota a run spends, the settings it reads, and every route — is [`api/README.md`](api/README.md),
beside the code it describes.

## The command-line client

`cli/` holds no database and reads nothing from YouTube. Every answer comes from the server.

It installs from this repository's `cli/v*` releases, which carry a binary for each platform the
release builds, with a checksum file beside them. `ypl update` moves an installed binary to the
newest release, and a daily check says when one is out.

Nothing about a deployment is built into the binary. The server's address and the identity provider
that signs its tokens each name one installation, so they are read from
`$XDG_CONFIG_HOME/ypl/config.toml` or from the environment:

```toml
api_base = "https://ypl.example.com"
issuer = "https://auth.example.com"
```

`YPL_API_BASE`, `YPL_OIDC_ISSUER` and `YPL_CLIENT_ID` override the file. `ypl config show` prints
every setting with the layer that set it, because a value alone does not say whether it came from
the file or from an export made months ago.

`ypl auth login` authenticates the machine with the OAuth 2.0 device authorization grant: the CLI
prints a code and a URL, and approving it in a browser anywhere logs this machine in. That is what
makes it work over SSH on a machine with no browser. The token goes in the OS keychain, or in a
mode-600 file on a host that has none, and it is refreshed under a lock so two commands at once
cannot spend the same refresh token twice. The client id is `ypl-cli-<machine>`, one per machine, so
a token can be revoked for one machine without touching the others.

A first run installs the newest `cli/v*` release, checks it against its checksums, and then tells it
where the server is:

```bash
tag=$(gh release list --repo datapointchris/ypl --json tagName \
  --jq '[.[] | select(.tagName | startswith("cli/"))][0].tagName')
platform="$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
gh release download "$tag" --repo datapointchris/ypl --pattern "ypl_*_${platform}.tar.gz" --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt && tar -xzf ypl_*_"${platform}".tar.gz ypl && install ypl ~/.local/bin/

ypl config example > "$(ypl config path)"   # fill in api_base and issuer
ypl auth login                              # approve the code in a browser
ypl server status                           # what the server holds
```

`ypl --help` lists every command as the line to type, in sections: playing first, then one for each
thing the library holds, then the server and setup. It is not repeated here, because a list in
markdown goes stale and `--help` cannot. A bare `ypl` prints that help.

A playlist is named by its title or its YouTube id at every command that takes one, and the title's
case, spacing and punctuation do not have to be reproduced. `ypl playlists show 'sunday morning'`
finds Sunday Morning. Tab completes a playlist wherever one is named, offering each as its title
slugged — `sunday-morning`, with the title beside it — which needs no quoting and reaches the same
playlist at a rename or a delete as much as at a read. `ypl completion <shell>` prints the script,
and its `--help` says where each shell loads it from. A playlist the server does not recognize is
answered with every playlist it does, in the same form.

A read also takes part of a title, so `ypl playlists show morning` finds it too — but only a read.
A rename, a delete or an edit takes the id or the whole title, because a fragment that happens to
match one playlist matches it unambiguously, and there is nothing for the two-matches refusal to
catch. A fragment sent to one of those is refused by the title it is part
of, rather than by a sentence saying nothing was found.

`ypl playlists create` and `ypl playlists rename` make their change on YouTube in the request that
asks for it, and so does `ypl playlists delete`, which asks first and needs `--yes` where there is
nobody to ask. `--no-input` forbids the question everywhere and takes that path from a terminal.

`ypl playlists edit` is the one changing verb that does not reach YouTube: it sets the order the
server holds, and the next sync run pushes that order. Where an edit is refused, the buffer is kept
in a file and the refusal names it, because by then the editor has closed and that file is the only
copy of the rearranging.

`ypl play <playlist>` runs mpv in the foreground on the playlist's videos, in its order, leaving out
the ones YouTube will not serve. With no playlist it plays a draw of up to 100 mixes, made the way
`ypl next` makes one: never-played first, in a new order each time, then the ones heard least
recently. `--audio` drops the video window and `--mpv` passes an argument
straight through. A player that failed is exit 1 and mpv's own status is written to stderr, because
mpv spends 2 on a file it cannot open and this tool spends 2 on an invocation it would not accept.
It opens mpv's IPC socket, which is what lets `ypl now` report the track inside a two-hour mix
rather than the name of the mix. `ypl now` writes its answer and then exits 1 when nothing is
playing, so a status bar can run it unguarded in either mode.

`ypl play` records what it plays. It reads mpv's socket while the player runs, and once a mix has
played for 20 minutes, or half its length when that is shorter, it tells the server. That is what
`ypl next` reads to stop suggesting the same mix. A seek forward is not listening, so it does not
count. `ypl plays add <video>` records a mix heard somewhere `ypl play` could not see, by id or by a
link it was copied from. `ypl plays delete <play>` takes one back, asking first, and the play's
handle is never given to another.

Every read takes `--json`, which writes a stable shape to stdout and nothing else. A collection with
nothing in it is `[]` rather than `null`, so one filter works on every answer. Exit codes are 0 for
success, 2 for an invocation the CLI would not accept, and 1 for a command that ran and failed;
`ypl auth status` and `ypl next` exit 1 to report a real state rather than a failure, so a status bar
can run either unguarded.

## License

[MIT](https://tldrlegal.com/license/mit-license)
