# syntax=docker/dockerfile:1

# The ypl server. Its migrations are embedded in the binary, so the runtime
# stage holds the binary, yt-dlp, and nothing else this repo produces.

FROM golang:1.26.6-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ypl-api .

FROM alpine:3.22

# ca-certificates because every door out of this process is HTTPS. tzdata
# because a sync run's timestamps are read by a person in a local zone.
RUN apk add --no-cache ca-certificates tzdata

# yt-dlp ships in the image, pinned. ENRICH_VIDEOS_PER_RUN defaults to 30 and
# the server refuses to start without the binary at any count above zero, so an
# image without it would run only the one configuration that reads no video.
#
# Pinned rather than floating so the image is the whole answer to what a read
# runs, and so a yt-dlp that starts failing is a version to move off rather than
# a change nobody chose. The musllinux build is the one that runs on alpine.
# The checksum is the release's own SHA2-256SUMS entry, and a mismatch fails the
# build rather than the read.
ADD --chmod=0755 \
    --checksum=sha256:f3dec9cfeaf304cec98290fe41c6ad465d4b747d302473559643e7af24929722 \
    https://github.com/yt-dlp/yt-dlp/releases/download/2026.08.19/yt-dlp_musllinux \
    /usr/local/bin/yt-dlp

# Running it once proves the binary executes on this base image and that its
# architecture matches the stage's. The alternative place to find out is a read
# on the deployed host.
RUN yt-dlp --version

# The server is a process that starts a third-party binary which makes its own
# network requests, so it runs as nobody with anything to reach. The host
# directory bind-mounted over /var/db/ypl has to be owned by this uid.
RUN adduser -D -u 10001 -h /home/ypl ypl && mkdir -p /var/db/ypl && chown ypl:ypl /var/db/ypl

COPY --from=builder /build/ypl-api /usr/local/bin/ypl-api

# Declared so a compose file mounting the host's database directory needs no
# environment variable to point the server at it. Left unset, the server would
# resolve its own default under the container filesystem, and a database inside
# the image is destroyed by the next pull with nothing reporting it.
ENV DATABASE_PATH=/var/db/ypl/ypl.db

USER 10001:10001
EXPOSE 8080
CMD ["ypl-api"]
