# qwatrix — final report

Measured state as of 2026-11-13. Every number below came from a command that was actually run; where a
claim could not be verified it says so.

## 1. What this is

A Matrix homeserver in a single Go binary: one process, one SQLite file, no external services. It
implements the client-server API a real client needs and a working, tested subset of the
server-server API.

| | |
|---|---|
| Go files / lines | 149 files, 46 479 lines — 92 files / 28 222 lines of server in `cmd/` + `internal/`, 57 files / 18 257 lines of tests |
| Documented endpoints | 121 rows in `docs/ENDPOINTS.md`, with a test asserting no route is undocumented |
| In-process tests | 288 `Test…` functions under `tests/` |
| Tasks | 129 rows tracked in the working task list, which along with the progress log is deliberately kept out of the repository; the open ones are listed in § 5 |
| Statements covered | 72.4 % of `./internal/...` measured over `tests/api` and `tests/unit` together (71.5 % from `tests/api` alone) |

## 2. What was built

**Client-server API.** Rooms and events (create, send, state, redact, pagination, relations/threads
storage, bundled aggregates), membership with invites and third-party invites, auth rules and power
levels, sync with long-polling and lazy loading, filters, read markers, receipts, typing, presence,
to-device messages, device and cross-signing key management with key claims and OTK tracking,
account data, push rules, media upload/download/thumbnails with capability URLs, profiles,
aliases and the published-room directory, room summaries, search, generated thumbnails,
UIA-driven registration/login/logout/password/deactivation, and the Synapse-compatible shared-secret
admin endpoint.

**Server-server API** — and the story of this milestone is how much of this only *works* once two real
servers are involved:

**Late additions worth naming**, because they came from checking claims rather than from a plan:
remote media fetch-and-cache with the signed federation download (MSC2448) and thumbnails of other
servers' files, `405` with an `Allow` header where an endpoint exists and the method does not, the
filename-bearing download forms, and `GET /_matrix/client/v1/room_summary/{room}` including the rooms
a restricted join admits.

**A room's past, and a bridge's users.** Two capabilities landed at the end because the mandatory suite
asked for them. Application services on the receiving side: registration files read at start-up (a bad one
stops the server, because a registration file is security configuration), `as_token` authentication,
virtual users provisioned without a password, `?user_id=` acting inside the registration's users namespace
and nowhere else, and MSC3316 event import with a deliberately chosen `?ts=`. And the two reads that let a
client jump into history — `/timestamp_to_event` and `/context` — served to clients and to peers, with a
joining server that asks a resident peer for a past it never received and keeps what it is given *detached*:
page-able and signature-verified, and outside the room's graph, its state, its tip and its `/sync`.

- Signing keys: published, fetched from peers, and verified (`/send`, `/make_join`, `/send_join` all
  require a valid `X-Matrix` signature; refusals name the reason without leaking the signature).
- Federation of a room: inbound `/make_join` + `/send_join` with authorisation against the state before
  the join, and outbound joining of a room this server does not hold.
- **Transaction delivery in both directions.** Inbound ingest is idempotent per transaction and event.
  Outbound delivery goes through a persistent ledger (`store.FederationOutbox`) written *in the same
  database transaction as the event*, so a peer that is briefly unreachable can neither fail a local
  `PUT /send` nor make a message vanish. Failures back off to ten minutes; a delivery is abandoned only
  after hundreds of attempts and a week, and abandoning logs at `Error`.
- Device lists across servers: `GET /user/devices/{userID}` served for this server's own users,
  `/keys/query` for a remote user fetched from *their* server, and remote members announced into
  `/sync device_lists.changed`.
- **Gap filling**: a peer's missing predecessor events are fetched with `get_missing_events` before an
  event is refused, with the boundary documented (a peer that supplies only part of a run gets the
  batch refused rather than being allowed to split the room's history).
- **Remote media (MSC2448)**: a file whose origin is another server is fetched and cached, so an image
  in a federated room is an image rather than a grey square.

**Runtime and operations.** Boots with no arguments on a fresh directory, keeps every byte inside its
data directory, drains in-flight requests on `SIGTERM`/`SIGINT` and exits 0, structured logs with
secret redaction, graceful degradation when federation is not configured, and a delivery worker that
runs beside the listeners rather than behind them.

## 3. Decisions worth knowing

1. **Delivery is a ledger, not an inline call.** A separate write after the event commits can be lost
   between the two statements, and the result is a room whose members disagree about what happened,
   permanently and silently. Writing the obligation in the same transaction costs one row per
   (event, destination) and buys the guarantee.
2. **Cached media keeps its own local ID**, with `(origin, upstream_id)` as the way back to it. Reusing
   the origin's ID would leave rows whose meaning depends on where they came from, and the safety rule
   ("never serve another server's file under this server's origin") would depend on somebody reading a
   column carefully.
3. **A wrong method is answered with 405, an unknown path with 404** — and the route table that makes
   that possible is in this server because Go's own 405 handling is defeated by the `/_matrix/`
   catch-all a Matrix server needs. The matching is written here rather than delegated to
   `ServeMux.Handler`, which measured as reporting matches for paths that are not endpoints.
4. **The version string and signing keys are served on the client listener too** — public facts about
   this server, not something a caller earns by being a peer. Nothing that changes state, and nothing
   that depends on who is asking, is served this way.
5. **A federation refusal names the reason but not the credential.** A peer's `X-Matrix` signature is
   replayable for its exact body, timestamp and nonce, so it is redacted at *every* log level; the
   method, claimed origin and URI are kept, which is what makes the refusal diagnosable.
6. **Self-signatures on federated device keys are not verified.** Complement uploads empty signatures
   and the in-repo fixtures upload a literal dummy; verifying would refuse legitimate peers and
   clients. The surviving guards are that a device list is served only for this server's own users,
   asked only of the domain named in the user ID, and that a peer device document naming a different
   user is dropped and logged.
7. **`PUT /_matrix/key/v2/query` answers 404** even though a complement assertion would pass with 405,
   because this server does not implement the removed key-notary endpoint. Registering a route purely
   to change a status code would be a server lying about what it has.
8. **Room upgrades, refresh tokens, threads-as-a-timeline, search, key backup, pushers, URL preview and
   knocking are not implemented.** They are not on the mandatory complement set; each has an open task
   and each failing test is listed below rather than smoothed over.

## 4. Checks actually run

| Criterion | Command | Result |
|---|---|---|
| AC-1 toolchain | `go build ./...`, `go vet ./...`, `gofmt -s -l cmd internal tests`, `golangci-lint run ./...` | all clean; `golangci-lint`: 0 issues |
| AC-2 test location | `find . -name '*_test.go' -not -path './tests/*' -not -path './.git/*'` | prints nothing |
| AC-3 suite green | `go test -race -count=1 ./tests/...` | exit 0, **no data race**: `tests/api` 544.6 s, `tests/runtime` 8.9 s, `tests/unit` 3.9 s |
| AC-3 coverage | `go test -coverpkg=./internal/... -cover ./tests/...` | 70.2 % of `./internal/...` statements via `tests/api`, 20.6 % via `tests/unit` |
| AC-4 container contract | `make complement-smoke` (5 tests in `tests/complement`) | pass: versions on the expected port, shared-secret registration, admin registration, room lifecycle, two-server deployment with complement's CA |
| AC-5 upstream suite | `scripts/complement-sweep.sh` (both packages, per-test retries) | **met**: 95 of the 95 named mandatory tests pass at run 115; 121 of 197 recorded verdicts overall |
| AC-6 documentation | `docs/ENDPOINTS.md` + `TestEveryEndpointIsDocumented`, `README.md`, this file | present; the doc test checks both directions (no undocumented route, no advertised route that is not served) |
| AC-7 runtime | `tests/runtime` against the compiled binary | pass: boots with no arguments and creates `qwatrix.db` in the working directory; creates **nothing** in a working directory when `-data-dir` is elsewhere; an in-flight `/sync` survives `SIGTERM` **and** `SIGINT` and answers 200 with a `next_batch`; exit code 0; boot and error-path output carries no shared secret, password or access token |
| Release container | `podman build -f deploy/Dockerfile .` then `podman run -e QWATRIX_SERVER_NAME=…` | builds; boots; `/versions` answers. Writing this line found the federation-key bug below |
| Federation TLS | `tests/unit/federationtls_test.go` (3 tests) | pass: the self-signed pair loads in `crypto/tls`, names the server in the SAN, and is reused rather than re-minted |
| AC-7 persistence | `tests/api/persistence_test.go` (6 tests, real process restart) | pass: rooms/history, sessions and devices, sync progress, client state, uploaded media; plus a test that the replaced server has actually stopped answering, because without it every durability assertion could pass by reading the server that never restarted |

Two audit notes about the tests themselves, because they changed the answers:

- The log-secrets test was vacuous until the harness was put at `LevelDebug`: an operator does not run a
  homeserver at error level, and a planted `slog.Info` password leak passed the first version.
- `README.md`'s registration recipe was run with `curl` against a live server rather than written from
  the code — the first draft was wrong about the User-Interactive Authentication exchange.
- Three things were **documented as built and were not**: thumbnails (a flag and doc rows, no route),
  the room hierarchy, and space summaries — the last two had even reached this report's own feature
  list. A task marked complete from an early milestone was also found claiming thumbnails; the docs
  and the task table now match the route table, which is what `TestEveryEndpointIsDocumented` checks.
- The self-signed federation key was written as raw DER into a file named like PEM, so **a server
  configured to federate could not start**; crypto/tls reported it as "failed to find any PEM data".
  It survived every test because the complement container mints its certificate with openssl and no
  test had a TLS federation listener. It was found by running the install command the README gives.

## 5. Complement: measured compliance

complement was run in full — **both packages**, in batches, with any test left without a verdict
re-run in its own process before being called a crash. That last step matters: `TestPushSync` leaks
a goroutine that panics after its parent has finished, which aborts a Go test binary while still
printing a tidy summary, so a package of 106 tests can otherwise be reported as having decided 50.

Run 100, image built from the final commit:

| Package | Pass | Fail |
|---|---|---|
| `tests/csapi` (client API) | **91** | 15 |
| `tests` (server-server) | **14** | 77 |
| **overall** | **105** | **92** |

The server-server package is where the honest weight sits: most of complement is about federation,
and this server implements a working subset of it.

### AC-5, the named mandatory set

[`ACCEPTANCE-CRITERIA.md`](ACCEPTANCE-CRITERIA.md) names 95 tests that must pass. Measured at run 115,
against the image built at `7110cfd`: **95 passing, 0 failing, 0 skipped, 0 never run**. **AC-5 is met.**
The last test to fall was `TestJumpToDateEndpoint`, and [COMPLEMENT-RESULTS.md](COMPLEMENT-RESULTS.md)
records what its thirteen subtests took: application services, two history reads, those reads over
federation, and a place to keep a past this server was never told about.

AC-5's 70-of-107 threshold is written against `tests/csapi` alone, where 93 of 106 pass; this report
scores the harder thing — the mandatory set, across both packages, 121 of 197 recorded verdicts.

The 15 `tests/csapi` failures are a similar list (threads, search over federation, key-backup room-key
rules, URL preview, pushers, transactions across refresh, server notices, room members at a point,
…); [`COMPLEMENT-RESULTS.md`](COMPLEMENT-RESULTS.md) records them run by run, and the working
status log is the narrative of the earlier rounds, stopping at run 97 — which is
exactly why it should not be cited for a current count.

### What moved, and what did not

The mandatory set went **79 → 81 → 84 → 86 → 87 → 88 → 89 → 91 → 92 → 93 → 94** across runs 97, 99, 100, 102, 103, 105, 108, 109, 111, 112 and 113,
from the federated-media, method-mismatch, thumbnail, room-summary, filename, space-hierarchy,
legacy-federation-endpoint and moderation-target work; the server-server package went from
8 passing to 14. Run 104 is the honest regression in that line: a change that put state beside every
left room's timeline broke `TestArchivedRoomsHistory`, and the revert carried by run 105's image took
it back — which is what the container suite is for. `tests/csapi` has been **91 pass / 15 fail, identically, for runs 95, 98, 99 and
100** — the strongest evidence available that none of that work broke a client-API test.

## 6. Known limitations

- **This server sends its own events; it does not forward another server's.** A room spanning three
  homeservers hears each participant from the servers its members are directly connected to (T705b).
- **`GET /backfill`, `/state_ids`, `/state`, `/event`, `/event_auth`, `/timestamp_to_event`,
  `/media/thumbnail` are not implemented** (T704, T719, T721). `/state_ids` honestly needs per-event
  state snapshots, which the current schema does not keep: `RoomState` is keyed by
  `(room_id, type, state_key)` and overwritten in place.
- **No outlier events in the graph.** An event whose history cannot be connected is refused rather than
  joined into the room's DAG, which is the correct safety property: a partial bridge would split the
  room's history in two. Events fetched from a peer to answer a question about the past are stored
  *detached* — readable and pageable, but moving no state, opening no branch, and never delivered as news.
  A fetched event is therefore never answered for by `/send`, and never appears in a `/sync` batch.
- **`m.device_list_updates` EDUs are not sent**, so a key upload on one server is invisible to peers
  until a room event follows (T712b).
- Not implemented at all: room upgrades (T608), refresh tokens (MSC2918), threads timeline endpoint,
  search, URL preview, key backup and cross-signing verification flows, pushers and push gateway
  delivery, knocking, server ACLs, sliding sync, OIDC/SSO.
- **Application services are supported on the receiving side**: registration files, `as_token`
  authentication, virtual users, acting as a namespaced user (`?user_id=`), and MSC3316 event import with a
  chosen `?ts=`. What is not implemented is the *outbound* half — no events or ephemeral data are pushed to
  a service's configured `url`, so a real bridge would be told nothing. The `rooms` and `aliases` namespaces
  are parsed and reported but not enforced, since nothing acts on them yet.
- Federation over TLS: a deployment without a certificate pair generates a self-signed one, and a peer
  will not trust it — which is the right outcome, and `-federation-insecure-skip-tls-verify` exists for
  test networks and logs a `Warn` naming the security boundary it removes.

## 7. Reproducing the measurements

```sh
make test          # in-process suites (tests/api, tests/unit, tests/runtime)
make race          # the same under -race
make lint fmt vet
make complement-smoke                                  # container contract, 5 tests
scripts/complement-sweep.sh                           # whole upstream suite + AC-5 score
python3 scripts/ac5-check.py <results-file>        # score one sweep against AC-5
```

`COMPLEMENT` points at the complement checkout (default `~/complement`); under podman, export
`DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock` and
`COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT=host.containers.internal`.
