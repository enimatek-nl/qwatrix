# qwatrix — final report

Measured state as of 2026-11-11. Every number below came from a command that was actually run; where a
claim could not be verified it says so.

## 1. What this is

A Matrix homeserver in a single Go binary: one process, one SQLite file, no external services. It
implements the client-server API a real client needs and a working, tested subset of the
server-server API.

| | |
|---|---|
| Go files / lines | 118 files, 36 597 lines (23 221 in `internal/`, 12 335 of them tests) |
| Documented endpoints | 92 rows in `docs/ENDPOINTS.md`, enforced by a test in both directions |
| In-process tests | 199 `Test…` functions under `tests/` |
| Tasks | 82 of 101 complete (`.agent-work/TASKS.md`) |
| Statements covered | 70.1 % of `./internal/...` by `go test -coverpkg=./internal/... -cover ./tests/...` |

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
| AC-3 suite green | `go test -race -count=1 ./tests/...` | exit 0 — `tests/api` 467 s, `tests/unit` 3.9 s, `tests/runtime` (non-race run) 7.3 s |
| AC-3 coverage | `go test -coverpkg=./internal/... -cover ./tests/...` | 70.1 % of `./internal/...` statements |
| AC-4 container contract | `make complement-smoke` (5 tests in `tests/complement`) | pass: versions on the expected port, shared-secret registration, admin registration, room lifecycle, two-server deployment with complement's CA |
| AC-5 upstream suite | `.agent-work/complement-sweep.sh` (both packages, per-test retries) | see §5 |
| AC-6 documentation | `docs/ENDPOINTS.md` + `TestEveryEndpointIsDocumented`, `README.md`, this file | present; the doc test checks both directions (no undocumented route, no advertised route that is not served) |
| AC-7 runtime | `tests/runtime` against the compiled binary | pass: boots with no arguments and creates `qwatrix.db` in the working directory; creates **nothing** in a working directory when `-data-dir` is elsewhere; an in-flight `/sync` survives `SIGTERM` **and** `SIGINT` and answers 200 with a `next_batch`; exit code 0; boot and error-path output carries no shared secret, password or access token |
| AC-7 persistence | `tests/api/persistence_test.go` (6 tests, real process restart) | pass: rooms/history, sessions and devices, sync progress, client state, uploaded media; plus a test that the replaced server has actually stopped answering, because without it every durability assertion could pass by reading the server that never restarted |

Two audit notes about the tests themselves, because they changed the answers:

- The log-secrets test was vacuous until the harness was put at `LevelDebug`: an operator does not run a
  homeserver at error level, and a planted `slog.Info` password leak passed the first version.
- `README.md`'s registration recipe was run with `curl` against a live server rather than written from
  the code — the first draft was wrong about the User-Interactive Authentication exchange.

## 5. Complement: measured compliance

`.agent-work/COMPLEMENT-STATUS.md` keeps the per-run history; `.agent-work/complement-sweep.sh` produces
the numbers and `.agent-work/ac5-check.py` scores them against the mandatory set parsed out of
`.agent-work/ACCEPTANCE-CRITERIA.md` itself, so the requirement and the measurement cannot drift apart.

The sweep exists because of two measurement failures that had already happened:

1. Complement's own binaries sometimes panic (a goroutine failing after its parent test finished, or a
   test indexing into something this server did not provide), which aborts every later test **in that
   package** while `go test` still prints a summary that reads like a complete run. One sweep reported
   50 tests for a package holding 106.
2. Every sweep ran `./tests/csapi` only, because AC-5 states its threshold as "70 of the 107
   `tests/csapi` tests" — but AC-5's mandatory *named* set draws on both packages, and 15 mandatory
   tests lived in `./tests` and had never been run. Run 98 was the first sweep of both, with
   per-test retries: **197 verdicts, none unmeasured.**

`tests/csapi` (run 99, current code): **91 pass, 15 fail**, unchanged from before the outbound-federation
work — no regressions.

`./tests` (server-server, run 98, before remote media and the 405 fix): 8 pass, 83 fail. That package is
the federation suite, and it is the honest picture of how much of federation is still missing; its
failures concentrate on `get_missing_events` edge cases, event-size limits, corrupted auth chains,
`send_leave`, restricted rooms, and remote media.

### AC-5 mandatory set

Run 98 measured **79 of 95 passing, 16 failing, 0 unmeasured**. Fifteen of the sixteen were then
diagnosed to a specific missing capability, and four of them (remote media) were fixed afterwards —
those four plus the 405 rule are being re-measured in run 99.

| Mandatory test | Cause, from its own log line | Status |
|---|---|---|
| `TestContentMediaV1`, `TestMediaFilenames`, `TestMediaWithoutFileName`, `TestMediaWithoutFileNameCSMediaV1` | `GET /client/v1/media/download/hs1/…` → 404: files hosted elsewhere were never fetched | **fixed after run 98**; re-measured in run 99 |
| `TestLocalPngThumbnail` | same for `media/thumbnail` — the thumbnail half of MSC2448 is not implemented | open (T719) |
| `TestEventAuth` | `MustLeaveRoom: send_leave failed` — federated leave is not implemented | open (T720) |
| `TestUnbanViaInvite` | the invited remote user never appears: invites are not co-signed | open (T714) |
| `TestSyncTimelineGap` | `MustSendTransaction … contained events the server rejected` — needs outlier events and per-event state snapshots | open (T713) |
| `TestDeviceListUpdates` | `device_lists.changed` never names a remote user: needs `m.device_list_updates` EDUs, which are the only thing that can carry a key upload across servers | open (T712b) |
| `TestRoomSummaryAllowedRoomIDs` | `GET /_matrix/client/v1/room_summary/{roomID}` → 404: only `/rooms/{id}/hierarchy` is registered | open (T721) |
| `TestClientSpacesSummary`, `TestRestrictedRoomsSpacesSummaryLocal`, `TestRestrictedRoomsLocalJoin` | restricted rooms / space-hierarchy join rules | open (T709) |
| `TestUnknownEndpoints` | 405-vs-404 rule now implemented; the one remaining assertion wants 405 for `PUT /_matrix/key/v2/query`, which this server honestly does not have | partially fixed, residual recorded |
| `TestJumpToDateEndpoint`, `TestSyncOmitsStateChangeOnFilteredEvents` | complement blueprint construction fails / `POST /join/{roomID}` refused in that fixture | open (T723) |

`tests/csapi` failures (15) in full, each a named missing feature rather than a crash:
`TestAsyncUpload`, `TestChangePasswordPushers`, `TestDeviceListUpdates`, `TestE2EKeyBackupReplaceRoomKeyRules`,
`TestGetRoomMembersAtPoint`, `TestInviteFromIgnoredUsersDoesNotAppearInSync`, `TestMessagesOverFederation`,
`TestPushRuleRoomUpgrade`, `TestSearch`, `TestServerNotices`, `TestSyncTimelineGap`,
`TestThreadReceiptsInSyncMSC4102`, `TestThreadsEndpoint`, `TestTxnIdWithRefreshToken`, `TestUrlPreview`.

`TestMessagesOverFederation` is the clearest single example of what this milestone moved: it used to
fail at the join. It now fails only in its two `backfill` subtests —
`Fetched 9 events from the /messages endpoint that included events -1 to -1` against
`expected 20 events in timeline` — that is history from *before* the join point, which is
`GET /_matrix/federation/v1/backfill` (T704), not delivery.

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
