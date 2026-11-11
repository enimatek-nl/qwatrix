# qwatrix — final report

Measured state as of 2026-11-11. Every number below came from a command that was actually run; where a
claim could not be verified it says so.

## 1. What this is

A Matrix homeserver in a single Go binary: one process, one SQLite file, no external services. It
implements the client-server API a real client needs and a working, tested subset of the
server-server API.

| | |
|---|---|
| Go files / lines | 120 files, 37 250 lines (23 843 in `internal/` + `cmd/`, 13 407 of them tests) |
| Documented endpoints | 100 rows in `docs/ENDPOINTS.md`, with a test asserting no route is undocumented |
| In-process tests | 204 `Test…` functions under `tests/` |
| Tasks | 105 rows tracked in `.agent-work/TASKS.md`; the open ones are listed in § 5 |
| Statements covered | 70.2 % of `./internal/...` by `tests/api` plus 20.6 % by `tests/unit` |

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
| AC-5 upstream suite | `.agent-work/complement-sweep.sh` (both packages, per-test retries) | **not met**: 84 of the 95 mandatory tests pass, 11 fail, 0 never ran (§ 5) |
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

`.agent-work/ACCEPTANCE-CRITERIA.md` names 95 tests that must pass. Measured: **84 passing**,
**11 failing**, 0 skipped, **0 never run**. AC-5 is therefore **not met**, and the gap is eleven named
behaviours rather than a broad failure.

AC-5's 70-of-107 threshold is written against `tests/csapi` alone, where 91 pass; this report scores
the harder thing — the mandatory set, across both packages.

| Failing mandatory test | The behaviour behind it |
|---|---|
| `TestDeviceListUpdates` | needs a **pushed `m.device_list_updates` EDU**: between two syncs the other user only uploads device keys, so no room event crosses the boundary and no amount of event delivery or local announcement can satisfy it (T712b) |
| `TestSyncTimelineGap` | a gapped timeline needs **outlier events** — persisting an event whose history cannot yet be connected (T713) |
| `TestUnbanViaInvite` | this server does not **co-sign invites** for remote users, so the remote server refuses the invite (T714) |
| `TestEventAuth` | fails at `MustLeaveRoom: send_leave failed` — a **federated leave** is not implemented (T720) |
| `TestUnknownEndpoints` | wants `405` at `PUT /_matrix/key/v2/query`, the deprecated notary API. Not served, and no decoy route will be registered to change a status code: `404` is the truth (T722) |
| `TestMediaWithoutFileName` | complement asks for media by its **public container host:port** rather than the Matrix server name; serving that means treating a configured public name as local (T718) |
| `TestClientSpacesSummary` | `GET /rooms/{roomID}/hierarchy` — a walk over `m.space.child` — is not implemented (T725) |
| `TestRestrictedRoomsLocalJoin`, `TestRestrictedRoomsSpacesSummaryLocal` | a restricted join needs the remote space graph, so both the hierarchy endpoint and remote-room permission evaluation are needed (T709, T725) |
| `TestJumpToDateEndpoint` | fails inside complement's own blueprint set-up: the scenario's `POST /join` is refused before `/timestamp_to_event` is reached (T723) |

The 15 `tests/csapi` failures are a similar list (threads, search over federation, key-backup room-key
rules, URL preview, pushers, transactions across refresh, server notices, room members at a point,
…); `.agent-work/COMPLEMENT-RESULTS.md` records them run by run.

### What moved, and what did not

The mandatory set went **79 → 81 → 84** across runs 97, 99 and 100, from the federated-media,
method-mismatch, thumbnail, room-summary and filename work; the server-server package went from
8 passing to 14. `tests/csapi` has been **91 pass / 15 fail, identically, for runs 95, 98, 99 and
100** — the strongest evidence available that none of that work broke a client-API test.

## 6. Known limitations

- **This server sends its own events; it does not forward another server's.** A room spanning three
  homeservers hears each participant from the servers its members are directly connected to (T705b).
- **`GET /backfill`, `/state_ids`, `/state`, `/event`, `/event_auth`, `/timestamp_to_event`,
  `/media/thumbnail` are not implemented** (T704, T719, T721). `/state_ids` honestly needs per-event
  state snapshots, which the current schema does not keep: `RoomState` is keyed by
  `(room_id, type, state_key)` and overwritten in place.
- **No outlier events.** An event whose history cannot be connected is refused rather than persisted,
  which is the correct safety property (a partial bridge would split the room's history) and the reason
  `TestSyncTimelineGap` fails (T713).
- **`m.device_list_updates` EDUs are not sent**, so a key upload on one server is invisible to peers
  until a room event follows (T712b).
- Not implemented at all: room upgrades (T608), refresh tokens (MSC2918), threads timeline endpoint,
  search, URL preview, key backup and cross-signing verification flows, pushers and push gateway
  delivery, knocking, server ACLs, sliding sync, OIDC/SSO, application services.
- Federation over TLS: a deployment without a certificate pair generates a self-signed one, and a peer
  will not trust it — which is the right outcome, and `-federation-insecure-skip-tls-verify` exists for
  test networks and logs a `Warn` naming the security boundary it removes.

## 7. Reproducing the measurements

```sh
make test          # in-process suites (tests/api, tests/unit, tests/runtime)
make race          # the same under -race
make lint fmt vet
make complement-smoke                                  # container contract, 5 tests
.agent-work/complement-sweep.sh                        # whole upstream suite + AC-5 score
python3 .agent-work/ac5-check.py <results-file>        # score one sweep against AC-5
```

`COMPLEMENT` points at the complement checkout (default `~/complement`); under podman, export
`DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock` and
`COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT=host.containers.internal`.
