# qwatrix

A Matrix homeserver in one Go binary: one process, one SQLite file, no external services, no
Redis, no Postgres, no message queue. Start it with no arguments and you have a working
homeserver on loopback that clients can register with, sync from, and federate over.

It implements the Matrix client-server API used by real clients (rooms, events, membership,
sync with long-polling, filters, read markers, receipts, typing, presence, to-device messages,
end-to-end encryption key management, push rules, media including generated thumbnails, account
data, relations/threads, search, and room summaries) and a working subset of the server-server API.
`docs/ENDPOINTS.md` lists every endpoint with its status, and a test asserts that no route is
undocumented; `.agent-work/FINAL-REPORT.md` records what was verified and what is knowingly
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

## Register a user

Registration is open by default. User-Interactive Authentication is a conversation, so the first
request is answered with the challenge and the client completes the stage it names:

```sh
# 1. Ask to register; take the session ID out of the 401 challenge.
session=$(curl -s -X POST http://127.0.0.1:8008/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"correct horse battery staple"}' \
  | sed -n 's/.*"session":"\([^"]*\)".*/\1/p')

# 2. Complete the dummy stage and get a token.
curl -s -X POST http://127.0.0.1:8008/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"alice\",\"password\":\"correct horse battery staple\",
       \"auth\":{\"type\":\"m.login.dummy\",\"session\":\"$session\"}}"
```

That returns `user_id`, `access_token` and `device_id`. Log in the same way with
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
.agent-work/complement-sweep.sh                  # every package, panic-proof, + AC-5 score
```

The sweep script exists because complement's own test binaries sometimes panic (a panic aborts
everything after it in that package while `go test` still prints a summary that looks complete),
and because AC-5's mandatory test list draws on **both** packages. It enumerates each package with
`-list`, runs chunks in separate processes, retries anything left undecided on its own, and scores
the result against `.agent-work/ACCEPTANCE-CRITERIA.md`.

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
list for `/keys/query`, and gap filling for missing events.

Deliberately not there yet, and written down rather than hidden: this server sends its own events
but does not forward another server's, `GET /backfill` and `/state_ids` are not implemented (so a
peer cannot read history from before your join), `m.device_list_updates` EDUs are not sent, and
restricted-room joins and knock/ACL federation are not implemented.

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
them fail. `.agent-work/FINAL-REPORT.md` lists them with the measured
complement results, and `.agent-work/TASKS.md` tracks each one.
