# syntax=docker/dockerfile:1

# qwatrix as an operator runs it: a self-contained image that compiles the server and then
# throws the compiler away.
#
# This is deliberately not the image the test suite uses. `deploy/complement/Dockerfile`
# copies in a binary built outside the image, because Complement rebuilds it for every run and
# would pay for a compile hundreds of times; an operator builds this one once.
#
# The runtime base is Alpine, which means musl. The compile is therefore static: a dynamically
# linked binary inside a musl container fails to start with the unhelpful message "not found",
# which is a confusing thing to discover from a container log.

FROM golang:1.26-alpine AS build

RUN apk add --no-cache ca-certificates git

WORKDIR /src

# The module files come first so that a source-only change reuses the downloaded dependencies
# rather than fetching the world again.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# The version is stated once, in internal/config/config.go, and read out of it here. Passing
# --build-arg VERSION=... publishes exactly that version; leaving it empty takes the source's
# own default. A literal here would be the same fact stated twice, and twice is how a container
# tagged 0.2.0 comes to answer 0.1.0-dev to a federation peer.
ARG VERSION=
RUN set -eu; \
    if [ -z "$VERSION" ]; then \
      VERSION=$(sed -n 's/^[[:space:]]*Version:[[:space:]]*"\([^"]*\)".*/\1/p' internal/config/config.go | head -1); \
    fi; \
    echo "building qwatrix $VERSION"; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/qwatrix ./cmd/qwatrix

FROM alpine:3.23

# ca-certificates is what lets this server verify a peer's TLS certificate when it federates
# out; wget is the health check. Tini would be the usual supervisor, but qwatrix exits cleanly
# on SIGTERM by itself, so the container runtime can be the only process manager.
RUN apk add --no-cache ca-certificates wget

# A non-root uid: the data directory is chowned to it, and nothing in this server needs to be
# root. Bind ports below 1024 by publishing them on the container runtime side, not by
# escalating inside the container.
RUN adduser -D -u 10001 qwatrix

COPY --from=build /out/qwatrix /usr/local/bin/qwatrix

# Everything the server creates lives here: the database, uploaded media, and the signing key.
# Losing this directory means losing the room state and being unable to sign for the domain, so
# it is a volume and not an incidental filesystem.
RUN mkdir -p /data && chown qwatrix:qwatrix /data
VOLUME ["/data"]

USER qwatrix:qwatrix

# 8008 is the client-server port, 8448 the server-server port. Both are the specification's
# defaults; a deployment that publishes something else should pass -addr and -federation-addr.
EXPOSE 8008 8448

ENV QWATRIX_DATA_DIR=/data \
	QWATRIX_ADDR=:8008 \
	QWATRIX_FEDERATION_ADDR=:8448 \
	QWATRIX_LOG_LEVEL=info

# Registration is closed by default, which is the setting an internet-facing container wants;
# an operator who wants open registration says so deliberately with QWATRIX_REGISTRATION=true
# rather than finding out from a stranger's account list.
WORKDIR /data

# /versions is public, needs no account, and is answered by the same handler as everything else,
# which makes it the cheapest signal that is green only when the API is.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD wget -q -O /dev/null http://127.0.0.1:8008/_matrix/client/versions || exit 1

# `-server-name` is not set here on purpose: it is the one value that cannot be a default, and a
# server that guesses its own identity signs federation events for a domain it does not own.
ENTRYPOINT ["/usr/local/bin/qwatrix"]
