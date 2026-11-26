# syntax=docker/dockerfile:1

# The ypl server, and the command that puts videos enrichment stopped reading
# back in its queue. Migrations are embedded in the binaries, so the runtime
# stage holds those two, yt-dlp, deno, ffmpeg, and nothing else this repo
# produces.

# The tag names the Go release go.mod declares, which is the one CI tests.
# This image sets GOTOOLCHAIN=local and ignores go.mod's toolchain line, so
# the tag alone decides what compiles the binary.
FROM golang:1.26.11-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The release names the image's sha- tag, which GET /api/v1/version answers.
# A build that names none reports dev.
ARG YPL_API_VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${YPL_API_VERSION}" -o ypl-api . && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o reset-enrichment ./cmd/reset-enrichment

# Deno publishes a zip, which ADD does not unpack, so a stage of its own
# unpacks it. The checksum is the release's own .sha256sum.
FROM alpine:3.22 AS deno
ADD --checksum=sha256:e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609 \
    https://github.com/denoland/deno/releases/download/v2.9.8/deno-x86_64-unknown-linux-gnu.zip \
    /tmp/deno.zip
RUN unzip /tmp/deno.zip -d /out

# ffmpeg and ffprobe come from FFmpeg-Builds at a release tag. yt-dlp's own
# builds fork that project. They have applied no patch of their own since
# ffmpeg 8.0, and they build only ffmpeg's master branch.
#
# FFmpeg-Builds deletes a daily build once 14 newer ones exist. It keeps each
# month's last build for 24 months. So a pin names a month's last build, whose
# URL answers for two years, and never a daily build or latest.
#
# The GPL build, because a desk's ffmpeg has libx264, which --recode-video mp4
# encodes with. The shared build, because its libraries serve both tools, where
# the static build carries a copy in each, about 150 MB more. Each binary finds
# its libraries at $ORIGIN/../lib, so bin and lib land side by side. The
# checksum is the release's own checksums.sha256 entry.
FROM alpine:3.22 AS ffmpeg
ADD --checksum=sha256:90a42a7d9689c031beacc3d03b139fd8243351d616988e8a74188df949fe5d08 \
    https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/autobuild-2026-10-31-12-59/ffmpeg-n9.0.2-39-g7adfbe40dc-linux64-gpl-shared-9.0.tar.xz \
    /tmp/ffmpeg.tar.xz
RUN mkdir -p /tmp/ffmpeg /out/bin /out/lib /out/share/doc/ffmpeg && \
    tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ffmpeg --strip-components=1 && \
    cp /tmp/ffmpeg/bin/ffmpeg /tmp/ffmpeg/bin/ffprobe /out/bin/ && \
    cp -P /tmp/ffmpeg/lib/*.so.* /out/lib/ && \
    cp /tmp/ffmpeg/LICENSE.txt /out/share/doc/ffmpeg/

# glibc, because deno publishes no build for musl.
FROM debian:13.8-slim

# ca-certificates because every door out of this process is HTTPS. tzdata
# because an operator setting TZ needs a zone database to resolve it against.
# wget because a container healthcheck probes /ready with it.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata wget && \
    rm -rf /var/lib/apt/lists/*

# yt-dlp ships in the image, pinned. The server reads videos unless ENRICH_PACE
# is off, and refuses to start without the binary when it does, so an image
# without it would run only the one configuration that reads no video.
#
# Pinned rather than floating so the image is the whole answer to what a read
# runs, and so a yt-dlp that starts failing is a version to move off rather than
# a change nobody chose. The release executable carries the yt-dlp-ejs its
# release pins, which the build check below names. The checksum is the
# release's own SHA2-256SUMS entry, and a mismatch fails the build rather than
# the read.
ADD --chmod=0755 \
    --checksum=sha256:249ddb058234276bc449b9cd9492aad9f133bda5923c61a9e39983ae114ef17d \
    https://github.com/yt-dlp/yt-dlp/releases/download/2026.11.23/yt-dlp_linux \
    /usr/local/bin/yt-dlp

# yt-dlp-ejs solves YouTube's player challenges by running its script in deno,
# the one JavaScript runtime yt-dlp enables unasked. Without one, YouTube hands
# a read no format, and audio cannot be fetched.
COPY --from=deno /out/deno /usr/local/bin/deno

# yt-dlp merges a video's best video and audio with ffmpeg, and runs every
# postprocessor through ffmpeg and ffprobe, such as -x. Without them it falls
# back to one pre-merged format, which is rarely the best, and -x fails.
COPY --from=ffmpeg /out/ /usr/local/

# Each tool runs once here, on this base, and prints its version. yt-dlp's
# verbose header names the yt-dlp-ejs it carries, and the deno, ffmpeg and
# ffprobe it found. A grep finding any of them missing fails the build rather
# than a read on the deployed host.
RUN yt-dlp --version && \
    deno --version && \
    ffmpeg -hide_banner -version && \
    ffprobe -hide_banner -version && \
    wget -nv --version && \
    yt-dlp -v 2>&1 | grep -E 'yt_dlp_ejs-0\.8\.0' && \
    yt-dlp -v 2>&1 | grep -E 'JS runtimes: .*deno-2\.9\.8' && \
    yt-dlp -v 2>&1 | grep -E 'exe versions: .*ffmpeg n9\.0\.2-39-g7adfbe40dc.*ffprobe n9\.0\.2-39-g7adfbe40dc'

# The server is a process that starts a third-party binary which makes its own
# network requests, so it runs as nobody with anything to reach. The host
# directory bind-mounted over /var/db/ypl has to be owned by this uid.
RUN useradd --uid 10001 --user-group --create-home --home-dir /home/ypl --shell /usr/sbin/nologin ypl && \
    mkdir -p /var/db/ypl && chown ypl:ypl /var/db/ypl

COPY --from=builder /build/ypl-api /usr/local/bin/ypl-api

# A video enrichment has stopped reading is held back until something clears
# the row, and this is the only thing that reaches it. Without it in the image
# the verdict has no way out on the deployed server.
COPY --from=builder /build/reset-enrichment /usr/local/bin/reset-enrichment

# Declared so a compose file mounting the host's database directory needs no
# environment variable to point the server at it. Left unset, the server would
# resolve its own default under the container filesystem, and a database inside
# the image is destroyed by the next pull with nothing reporting it.
ENV DATABASE_PATH=/var/db/ypl/ypl.db

# The port EXPOSE names, so the two cannot disagree and `docker run -P` cannot
# publish a port with nothing behind it.
ENV PORT=8080

# The zone every timestamp is logged in. Declaring it is what makes overriding
# it a choice, read against the zone database above.
ENV TZ=UTC

USER 10001:10001
EXPOSE 8080
CMD ["ypl-api"]
