# qwatrix

A Matrix homeserver in one Go binary: one process, one SQLite file, no external services, no
Redis, no Postgres, no message queue. Start it with no arguments and you have a working
homeserver on loopback that clients can register with, sync from, and federate over.

It implements the Matrix client-server API used by real clients (rooms, events, membership,
sync with long-polling, filters, read markers, receipts, typing, presence, to-device messages,
end-to-end encryption key management, push rules, media including generated thumbnails, account
data, relations/threads, search, and room summaries) and a working subset of the server-server API.
`docs/ENDPOINTS.md` lists every endpoint with its status, and a test asserts that no route is
undocumented; [`docs/FINAL-REPORT.md`](docs/FINAL-REPORT.md) records what was verified and what is knowingly
incomplete.

## Run it

```sh
go run ./cmd/qwatrix          # or: make build && ./bin/qwatrix
```

With no arguments it uses the development defaults:

| Setting | Default |
|---|---|
| Server name | `localhost:8008` |
| Client API listens on | `127.0.0.1:8008` |
| Federation | **off** — set `-federation-addr` to enable it |
| Data directory | `.` — holding `qwatrix.db`, `media/`, `signing.json` |
| Registration | **on** (`-registration=false` to disable) |
| Log level | `info` |

It creates nothing outside its data directory, and `SIGTERM`/`SIGINT` finish in-flight requests
before exiting with status 0. Both claims are tested (`tests/runtime`).

```sh
curl -s http://127.0.0.1:8008/_matrix/client/versions   # readiness probe
```

## Run it in a container

```sh
podman build -t ghcr.io/enimatek-nl/qwatrix:latest -f deploy/Dockerfile .
podman run -d --name qwatrix -p 8008:8008 -p 8448:8448 \
  -v qwatrix-data:/data \
  -e QWATRIX_SERVER_NAME=example.org \
  ghcr.io/enimatek-nl/qwatrix:latest
```

`QWATRIX_SERVER_NAME` (or `-server-name`) is required: it is the identity the server signs
federation events with, and a server that guesses it signs for a domain it does not own. The image
listens on `8008` (client) and `8448` (federation), keeps everything under `/data` — which is a
volume, because that is where the room state, the media and the signing key live — runs as uid
10001, and leaves registration closed until `QWATRIX_REGISTRATION=true` says otherwise.
`make image` builds the same image with whichever of podman or docker is configured.

The other image, `deploy/complement/Dockerfile`, is the one the test suite runs: it copies in a
binary built outside the image, because complement rebuilds the image for every deployment.

## Register a user

Registration is open by default. User-Interactive Authentication means a request may be answered
with a 401 challenge listing the stages — but a request that already carries the stage it intends to
satisfy is completed on the spot, which is what a script (and complement) does:

```sh
curl -s -X POST http://127.0.0.1:8008/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"correct horse battery staple",
       "auth":{"type":"m.login.dummy"}}'
```

A client that does not know the stages yet sends no `auth` and gets `{"flows":[{"stages":[…]}],
"session":"…"}` back; it then completes a stage and echoes that `session`. Both forms return
`user_id`, `access_token` and `device_id`. Log in the same way with
`POST /_matrix/client/v3/login`, and use the token either as `Authorization: Bearer <token>` or,
less well, as `?access_token=`.

For automated deployments there is also the shared-secret admin endpoint, which is what test
harnesses use to create users with an explicit admin flag:

```sh
./bin/qwatrix -registration-secret "$SECRET"
# POST /_synapse/admin/v1/register with nonce, username, password, admin and an HMAC-SHA1 MAC
```

## Configure it

Precedence is **built-in default < `QWATRIX_*` environment < flag**, with no silent disagreement:
a flag you did not pass never overwrites an inherited environment variable.

```
-addr                        client API listen address          (127.0.0.1:8008)
-server-name                 Matrix server name to answer for   (localhost:8008)
-data-dir / -db              state directory / database path    (./qwatrix.db)
-federation-addr             server-server listen address       (empty = federation off)
-tls-cert / -tls-key         PEM pair for the federation listener
-ca-cert                     extra CA to trust when federating
-federation-insecure-skip-tls-verify   disable peer TLS verification (test networks only)
-registration                allow POST /register               (true)
-registration-secret         shared secret for the admin endpoint (empty = disabled)
-app-service-config-files      application service registrations to trust (comma-separated, globs ok)
-room-version                default room version               (11)
-sync-max-timeout            longest permitted /sync long-poll  (1m)
-sync-timeline-limit         default /sync timeline batch size  (10)
-media-limit                 maximum upload size                (100 MiB)
-thumbnails                  generate thumbnails                (true)
-rate-limit / -rate-limit-rate / -rate-limit-burst               (on, 20/s, burst 60)
-log-level                   debug | info | warn | error        (info)
-version                     version string reported to peers
```

Each has an environment spelling: `QWATRIX_SERVER_NAME`, `QWATRIX_ADDR`,
`QWATRIX_FEDERATION_ADDR`, `QWATRIX_TLS_CERT`, `QWATRIX_TLS_KEY`, `QWATRIX_CA_CERT`,
`QWATRIX_FEDERATION_INSECURE_SKIP_TLS_VERIFY`, `QWATRIX_DATA_DIR`, `QWATRIX_DB`,
`QWATRIX_REGISTRATION_SECRET`, `QWATRIX_ROOM_VERSION`, `QWATRIX_LOG_LEVEL`, `QWATRIX_VERSION`,
`QWATRIX_REGISTRATION`, `QWATRIX_THUMBNAILS`, `QWATRIX_RATE_LIMIT`, `QWATRIX_MEDIA_LIMIT`.

## Test it

Two layers, and they test different things.

**In-process** — a real homeserver, real HTTP listeners, real SQLite, in one process, no
container runtime:

```sh
make test     # go test ./tests/...   (tests/api, tests/unit, tests/runtime)
make race     # the same under -race, which the sync long-poll paths are worth running
make lint     # golangci-lint
make fmt vet
```

- `tests/api` — behavioural compatibility over the client API.
- `tests/unit` — unit tests of the internal packages.
- `tests/runtime` — the compiled binary: boots with no arguments, keeps its files inside its data
  directory, and lets an in-flight `/sync` finish when it is signalled.

**Containers** — the upstream [complement](https://github.com/matrix-org/complement) suite
against a real image:

```sh
make complement-smoke   # this repo's own container contract (ports, PKI, registration, 2 servers)
make complement         # complement's tests/csapi suite
make complement COMPLEMENT_PKG=./tests           # complement's server-server package
scripts/complement-sweep.sh                     # every package, panic-proof, + AC-5 score
```

The sweep script exists because complement's own test binaries sometimes panic (a panic aborts
everything after it in that package while `go test` still prints a summary that looks complete),
and because AC-5's mandatory test list draws on **both** packages. It enumerates each package with
`-list`, runs chunks in separate processes, retries anything left undecided on its own, and scores
the result against [`docs/ACCEPTANCE-CRITERIA.md`](docs/ACCEPTANCE-CRITERIA.md).

Set `COMPLEMENT` to point at your complement checkout (default `~/complement`), and
`COMPLEMENT_BASE_IMAGE` for the image (default `localhost/qwatrix-complement:latest`).

## Federation

Enabled by giving the server-server API an address:

```sh
./bin/qwatrix -server-name matrix.example -federation-addr 0.0.0.0:8448 \
  -tls-cert fullchain.pem -tls-key privkey.pem
```

Without a certificate pair it generates a self-signed one, which a peer will not trust — and
should not: an untrusted certificate is a refusal, not a formality. `-federation-insecure-skip-tls-verify`
exists for test networks and removes a security boundary.

Working today, between two real servers: inbound and outbound `/make_join` + `/send_join`,
transaction ingest **and** delivery (locally originated events reach peers through a persistent
ledger that survives a restart), server signing keys both ways, fetching a remote user's device
list for `/keys/query`, to-device messages — the path an end-to-end key share takes between homeservers,
remembered per sender so a retried share is not delivered twice — gap filling for missing events, and the
reads a peer makes when it is the one being asked — `/event`, `/timestamp_to_event`, `/state_ids`,
`/state`, `/backfill` and `/event_auth`, each answering only what the asking server's own users are in.

Joining a room buys its current state and none of its history, so a question about the past of a
room hosted elsewhere is put to a server that has it, and what comes back is kept *detached*:
page-able through `/messages` and `/context`, and outside the room's graph, its state, its tip and
its `/sync`, because a user who paged up to last year has not received last year.

Deliberately not there yet, and written down rather than hidden: this server sends its own events
but does not forward another server's, typing and receipts are not relayed, and authorising
another server's restricted-room join — plus knock and ACL federation — are not implemented.

## Layout

```
cmd/qwatrix          the binary: environment, flags, signals
internal/app         composition root: builds and wires every service, owns the listeners
internal/config      defaults, QWATRIX_* environment, flags, validation
internal/csapi       client-server API: routes and handlers
internal/fedapi      server-server API: inbound federation, outbound peer client, delivery outbox
internal/roomserver  the room DAG: authorisation, event construction, state resolution, joins
internal/syncapi     /sync, long-polling, filters, notifications
internal/store       SQLite (GORM): rooms, events, state, membership, keys, device lists, outbox
internal/keyserver   device keys, cross-signing storage, key claims, /keys/query
internal/devicelists device-list change tracking for /sync and /keys/changes
internal/accounts    users, devices, sessions, UIA, shared-secret registration
internal/media       uploads, thumbnails, download endpoints
internal/events      event building/signing helpers over gomatrixserverlib
internal/uia         User-Interactive Authentication stages and sessions
internal/identity    the server's signing identity (the key in signing.json)
internal/filters     /filter definitions and the filtering applied to /sync
internal/directory   room aliases and the published-room directory
internal/presence    presence state and its fan-out
internal/pushrules   push rule storage, evaluation, and the /pushrules API
internal/readstate   read markers, receipts, notification counts
internal/todevice    to-device message queues (m.room_key invitations and the like)
internal/notifier    the wake-up signal shared by /sync long-pollers
internal/httpx       middleware, request decoding, error responses
internal/matrix      protocol constants, identifiers, the Matrix error type
tests/               harness (in-process server), api, unit, runtime, complement (contract)
docs/ENDPOINTS.md    every endpoint and its status; enforced by a test
```

## Known limitations

Room upgrades, the room hierarchy (so space summaries are not served), URL preview, refresh tokens
(MSC2918), the threads timeline endpoint, key backup and cross-signing, knocking, server ACLs, push
gateway delivery, and sliding sync are not implemented, and a handful of complement tests covering
them fail. Application services are supported on the receiving side — registration files,
`as_token` authentication, virtual users, acting as a namespaced user, `?ts=` event import — but
nothing is ever pushed to a service's configured `url`, so a real bridge would be told nothing. [`docs/FINAL-REPORT.md`](docs/FINAL-REPORT.md) lists them with the measured
complement results.
