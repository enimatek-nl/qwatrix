# syntax=docker/dockerfile:1

# qwatrix as Complement sees it: a container that listens on the two ports Complement
# reaches, answers for the server name it is handed at run time, and keeps its own storage.
#
# The binary is built outside the image and copied in, on purpose. Complement rebuilds the
# image for every run and builds a snapshot image per blueprint on top of it, so a compile
# step here would be paid for hundreds of times. It also keeps the compiler out of the
# image that is being tested.
#
# Build it with `make complement-image`, which cross-compiles with CGO off — the runtime
# base is musl, and a dynamically linked binary inside it dies with the unhelpful message
# "not found".

FROM alpine:3.23

# openssl mints the federation certificate from the certificate authority Complement copies
# into every container; ca-certificates is what makes outbound federation able to verify a
# peer; wget is the health check.
RUN apk add --no-cache openssl ca-certificates wget

COPY bin/qwatrix /usr/local/bin/qwatrix
COPY deploy/complement/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 8008 is the client-server port Complement calls over plain HTTP, 8448 is the
# server-server port it reaches over HTTPS. Both are the specification's defaults, and
# Complement will not look at any other port.
EXPOSE 8008 8448

# The database, media and signing keys all live here. Complement starts a fresh container
# per deployment, so this is intentionally disposable.
# The registration files complement copies into the container before it starts. The value is a glob,
# and a glob rather than a path on purpose: most blueprints configure no application service at all,
# and a server that failed to start because a file was missing would make every one of them fail.
ENV QWATRIX_APP_SERVICE_CONFIG_FILES=/complement/appservice/*.yaml \
	QWATRIX_DATA_DIR=/data \
	QWATRIX_ADDR=:8008 \
	QWATRIX_REGISTRATION_SECRET=complement \
	QWATRIX_REGISTRATION=true \
	QWATRIX_RATE_LIMIT=false \
	QWATRIX_LOG_LEVEL=info

VOLUME ["/data"]

# The health check is the cheapest signal that is up only when the API is: /versions is
# public, needs no account, and is answered by the same handler as everything else.
#
# Complement waits for this to go green (and for /versions to answer) before it starts a
# test, within COMPLEMENT_SPAWN_HS_TIMEOUT_SECS. The short interval is what keeps that wait
# close to the server's real start-up time rather than the check's period.
HEALTHCHECK --interval=2s --timeout=3s --start-period=1s --retries=20 \
	CMD wget -q -O /dev/null http://127.0.0.1:8008/_matrix/client/versions || exit 1

ENTRYPOINT ["/entrypoint.sh"]
