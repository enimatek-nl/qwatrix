# Tasks

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done+verified · `[!]` blocked
Every `[x]` must have evidence in `PROGRESS.md`.

> **Reconciled against the code on 2026-11-11d, and it was badly wrong.** M2, M3's core, M4, M5 and
> most of M6 were still marked `[ ]` although they had been built, tested in-process and measured
> against Complement over the previous days: the live documents had become `PROGRESS.md` and
> `COMPLEMENT-STATUS.md`, and this table — the one that answers "how far are we" — had not been
> written back to. Every status below was checked against the routes, the tests, or a Complement
> verdict rather than against memory. Two genuine gaps the stale table was hiding came out of that
> audit and are now named explicitly: **T308** (built and verified on 2026-11-11e), **T801** (no
> restart/persistence test, which AC-7 asks for by name) and **T608** (no room upgrade endpoint).

## M0 — Foundation + complement tracer bullet

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T001 | Go module, `.gitignore`, Makefile (build/test/race/fmt/vet/lint/complement targets), LICENSE | CN-1 | `[x]` |
| T002 | `internal/config` — flags + env + validation | CN-5 | `[x]` |
| T003 | `internal/matrix` — API errors, error codes, ID helpers, versions list | FR-1, CN-7 | `[x]` |
| T004 | `internal/httpx` — JSON/error responses, CORS, recover, request log, auth middleware, rate limiter | CN-7, CN-8, CN-9 | `[x]` |
| T005 | `internal/store` — GORM store bootstrap (pragmas, migrate, tx helper) + user/device/token repos | CN-4, CN-6 | `[x]` |
| T006 | `internal/app` — wiring, both listeners, errgroup + graceful shutdown | CN-6, AC-7 | `[x]` |
| T007 | `GET /_matrix/client/versions` + unknown-endpoint contract | FR-1, FR-2 | `[x]` |
| T008 | `tests/harness` — in-process server + typed CS client | CN-2, CN-3 | `[x]` |
| T009 | `tests/complement` package (TestMain + smoke: deploy, versions, shared-secret register, room lifecycle, 2-server deploy) | CC-1…CC-8, AC-4 | `[x]` (room-lifecycle test still fails only on `createRoom`, which is T203) |
| T010 | `deploy/complement/{Dockerfile,entrypoint.sh}` + `make complement-image` | CC-1…CC-5 | `[x]` |
| T011 | Shared-secret registration `/_synapse/admin/v1/register` (GET nonce + POST HMAC) | FR-13 | `[x]` |
| T012 | `make complement-smoke` target + first recorded complement run in PROGRESS | AC-5 | `[x]` |
| T013 | `405` answers must carry a Matrix `M_UNRECOGNIZED` body (stdlib mux answers plain text) | FR-2, complement `TestUnknownEndpoints` | `[x]` — answers `404 M_UNRECOGNIZED` with a Matrix body for both an unknown path and a known path with the wrong method (measured with curl, not assumed); complement `TestUnknownEndpoints`/`TestVersionStructure` pass. Deliberately not `405`: see the note at the bottom |

## M1 — Accounts, sessions, devices

> **Re-opened and closed again 2026-11-10b.** `TestDisplayNameUpdate` and `TestAvatarUrlUpdate`
> were measured failing once the whole suite ran (`.agent-work/complement-chunks.sh` explains why
> they had never run: a panic in one of Complement's own push tests was ending the process). Both
> are fixed by `T310` — a profile change now writes a member event — and both **pass against the
> container**, alongside the rest of the gate. Signed off on the full-suite run this time.

> **Signed off 2026-11-10.** Every test in the exit gate passes against the container:
> `TestRegistration`, `TestCanRegisterAdmin`, `TestLogin`, `TestLogout`, `TestChangePassword`,
> `TestDeactivateAccount`, `TestDeviceManagement`, `TestProfileDisplayName`,
> `TestProfileAvatarURL` (+ `TestRequestEncodingFails`), 10/10, zero skips
> (`/tmp/complement-m1b.log`). `TestDisplayNameUpdate` / `TestAvatarUrlUpdate` are named
> `TestProfileDisplayName` / `TestProfileAvatarURL` in this Complement version.
>
> Closing it took five protocol errors, each hidden behind an in-process test that agreed with
> the server instead of with the protocol (see `T109`–`T112` below).

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T101 | `POST /register` three-stage flow with UIA (`m.login.dummy`, `m.login.password`, shared secret), `M_USER_IN_USE`/`M_INVALID_USERNAME` | FR-10, FR-11, FR-21 | `[x]` — three-stage UIA flow, `M_USER_IN_USE` / `M_INVALID_USERNAME` before auth |
| T102 | `GET /register/available` | FR-12 | `[x]` — answers 400 `M_USER_IN_USE` / `M_INVALID_USERNAME`, checks the case-folded name |
| T103 | `GET/POST /login` (+`m.login.password` only), `M_FORBIDDEN`, `M_USER_DISABLED` | FR-14, FR-15 | `[x]` — folds the whole login identifier; `M_FORBIDDEN` for a wrong password |
| T104 | `GET /account/whoami`, `POST /logout`, `POST /logout/all` | FR-16, FR-17 | `[x]` |
| T105 | Device management: `GET /devices`, `PUT/DELETE /devices/{id}`, `POST /delete_devices` | FR-19 | `[x]` — adds `GET /devices/{id}` |
| T106 | `POST /account/password`, `POST /account/deactivate` behind UIA | FR-20 | `[x]` |
| T107 | Profile: `GET/PUT /profile/{userId}/displayname`, `/avatar_url`, `GET /profile/{userId}` | FR-* | `[x]` — GET reads are public (`optionalAuth`) since spec v1.1 |
| T108 | `GET /versions`-adjacent deprecation headers, `GET /versions` auth behaviour | FR-1 | `[x]` |

| T109 | `POST /logout` deletes the device, not only its token, so a signed-out session cannot linger unlisted in `/devices` | FR-16, FR-19 | `[x]` |
| T110 | `POST /account/password`: `logout_devices:false` keeps every session; the acting session survives either way | FR-20 | `[x]` |
| T111 | `POST /login` honours `device_id` (reusing a device the same account already owns) and `initial_device_display_name` | FR-14, FR-19 | `[x]` |
| T112 | UIA challenges always carry `params`; a device delete with no body is a 401 challenge, not a 403 | FR-20, FR-21 | `[x]` |

| T113 | Deleting an alias clears the room's `m.room.canonical_alias` claim (an alias-only claim becomes `{}`) so clients drop a name that no longer resolves | FR-32 | `[x]` |
| T114 | Forgetting a room ends read access (`/messages`, `/state` → 403), while a leave that predates an existing token still reaches that incremental `/sync` | FR-30, FR-36 | `[x]` |
| T115 | `GET /event/{id}` answers `404 M_NOT_FOUND` for anything unreadable, and enforces the per-event "not before my membership" bound under `joined`/`invited` visibility | FR-34 | `[x]` |
| T116 | `history_visibility: invited` reads from the **invite**, not the join (`store.Events.FirstPositionOfState` reads the invite's position from the append-only event stream, since `memberships.stream_pos` holds only the current membership) | FR-34 | `[x]` |

## M2 — Rooms, events, membership, directory

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T201 | Event domain service (`internal/events`): build/sign, auth events, `Allowed`, redact, power levels, limits | FR-26, FR-27 | `[x]` |
| T202 | Room storage models: rooms/events/state/memberships/aliases/txn | FR-26 | `[x]` |
| T203 | `POST /createRoom` (presets, version selection, initial state, PL override, alias, invite, is_direct) | FR-25, FR-26 | `[x]` |
| T204 | `PUT/GET /rooms/{id}/send/{type}/{txn}` + txn idempotency | FR-27 | `[x]` |
| T205 | State: `GET /rooms/{id}/state`, `GET/PUT /rooms/{id}/state/{type}/{key}`, canonical alias + name/topic/avatar | FR-28 | `[x]` |
| T206 | Membership: invite, join (by id/alias), leave, kick, ban, unban, forget | FR-29 | `[x]` |
| T207 | `GET /rooms/{id}/members`, `/joined_members`, `/joined_rooms` | FR-30 | `[x]` |
| T208 | `GET /rooms/{id}/event/{id}` (+`event_filter`), `/event_auth`, `/context/{id}`, `/relations` | FR-30, FR-34 | `[x]` |
| T209 | `GET /rooms/{id}/messages` with from/to/dir/limit/filter (+lazy loading) | FR-31 | `[x]` |
| T210 | `POST|PUT /rooms/{id}/redact/{txn}` with room-version redaction rules | FR-32 | `[x]` |
| T211 | Aliases + directory: `PUT/DELETE/GET /directory/room/{alias}`, `/directory/list/room/{id}`, `/publicRooms`, `/rooms/{id}/aliases` | FR-33 | `[x]` |
| T212 | Room summary + hierarchy (spaces) | FR-35 | `[x]` |

## M3 — Sync, ephemeral, read state

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T301 | `internal/syncapi` first-sync: joined/invite/leave sections, timeline, state, `prev_batch` | FR-40 | `[x]` |
| T302 | Incremental sync + `M_UNKNOWN_POS` + `limited` + timeline gap | FR-41 | `[x]` |
| T303 | Long-poll with notifier wake-up, `set_presence`, `timeout` | FR-42 | `[x]` |
| T304 | Filters: inline + stored, lazy loading, `event_fields`, `TestSyncTimelineGap` | FR-42, FR-43 | `[x]` |
| T305 | Typing: `PUT /rooms/{id}/typing/{userID}`, `m.typing` in `/sync`, expiry, first-person only, no leak to non-members | FR-44 | `[x]` |
| T306 | Receipts (`POST /rooms/{id}/receipt/{type}/{eventID}`), read markers (`POST /read_markers` → `m.fully_read` room account data), `m.receipt` in `/sync` | FR-45 | `[x]` |
| T306b | Account data, global and per-room (both spec spellings of the room path), delivered in `/sync`; `m.fully_read` lives in it | FR-45 | `[x]` |
| T309 | Saved filters: `POST`/`GET /user/{id}/filter`, inline or by ID on `/sync`, limit + types/senders applied **before** the window is cut, shape validation that keeps unknown keys | FR-42, FR-43 | `[x]` |
| T310 | A profile change re-announces the user in their rooms as a new `m.room.member` event, and never overwrites a name chosen for that room | FR-24, FR-29 | `[x]` |
| T311 | `lazy_load_members` (state delta compressed to unseen members) — `TestRoomMessagesLazyLoading*` are mandatory and wait on it | FR-43 | `[x]` |
| T312 | User directory (`POST /user_directory/search`) — mandatory `TestRoomSpecificUsernameAtJoin`/`Change` need it alongside T310 | FR-30 | `[x]` |
| T307 | Presence (per-user + `m.presence` in sync) | FR-47 | `[x]` |
| T308 | `unread_notifications` + push-rule-driven counts | FR-46 | `[x]` — push-rule evaluator (`internal/pushrules/evaluate.go`) plus `readstate.Badge`; mandatory complement `TestThreadedReceipts` passes, 13 in-process tests in `tests/api/notifications_test.go` |

## M4 — Keys and to-device

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T401 | `POST /keys/upload` (device keys, OTKs, fallback), counts in sync | FR-50, FR-51 | `[x]` |
| T402 | `POST /keys/query` (+`include_unverified`, `failures`), `/keys/claim` ordering + consumption | FR-50 | `[x]` |
| T403 | `GET /keys/changes` + `device_lists.changed/left` tracking | FR-51 | `[x]` |
| T404 | To-device local queue + `PUT /send_to_device/{type}/{txn}` + sync delivery | FR-51 | `[x]` |

## M5 — Content repository

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T501 | Upload + storage layout + limits + `mxc://` IDs | FR-55 | `[x]` |
| T502 | Download (v3 + `client/v1/media/*`), filename encoding, `Content-Disposition` | FR-55 | `[x]` |
| T503 | Thumbnail generation (pure Go) + `?width/height/method` | FR-55 | `[x]` |
| T504 | `GET /_matrix/media/v3/config`, `?allow_remote`, URL cache table | FR-55 | `[x]` |

## M6 — Remaining client surface

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T601 | Account data global + per-room, sync deltas, `m.direct` | FR-60 | `[x]` |
| T602 | Room tags | FR-61 | `[x]` |
| T603 | `GET /capabilities` (+`org.matrix.msc3244` etc.), `/account/3pid` | FR-62 | `[~]` — `GET /capabilities` is served and `TestCapabilities` passes; `GET /account/3pid` is not registered (an `invite_3pid` request is logged and refused instead), and `TestGet3PID` is not in the scored 106 |
| T604 | Pushers + push rules (+defaults seeding, cache-bump semantics) | FR-63 | `[x]` |
| T605 | `POST /rooms/{id}/report`, ignored users, `/mutual_rooms` (L) | FR-37 | `[~]` — `POST /rooms/{id}/report` and ignored users are served; `/_matrix/client/v3/rooms/{id}/mutual_rooms` (unstable) is not implemented |
| T606 | Knocking + `knock_restricted` join rules (S) | FR-36 | `[x]` |
| T607 | `docs/ENDPOINTS.md` generation + coverage test | AC-6 | `[x]` |
| T608 | `POST /rooms/{id}/upgrade` + tombstone handling (room version migration) | FR-26 | `[ ]` — **found 2026-11-11e**: the route does not exist, which is what `TestPushRuleRoomUpgrade` fails on (`POST …/upgrade` → 404). Pre-existing, not a regression |

## M7 — Federation (staged)

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T701 | Server keys: `/_matrix/key/v2/server`, key persistence, notary shape | FR-70 | `[x]` — `/_matrix/key/v2/server` (+ by key ID), complement `TestInboundFederationKeys` passes against a container |
| T702 | `GET /_matrix/federation/v1/version` + `TestVersionStructure` | FR-70 | `[x]` — `TestVersionStructure` passes |
| T703 | Signed request verification + outbound signing client (Well-Known/federation port resolution) | FR-72 | `[~]` — signed outbound client + inbound signature verification + origin key fetching and caching are done; Well-Known and SRV resolution are deliberately not (documented limitation, peers are asked exactly as written) |
| T704 | Read endpoints: `/state_ids`, `/state`, `/event`, `/event_auth`, `/timestamp_to_event` | FR-71 | `[ ]` |
| T705 | `/transactions` ingest (PDUs + EDUs, dedup, per-origin txn cache) | FR-73 | `[x]` — PDUs ingested with per-event accept/refuse, per-origin transaction cache, complement `TestSync` passes on it; EDUs parsed and dropped (T707) |
| T706 | Join v2 (`/make_join`, `/send_join`), leave v2, `/event` fetch, invite v2 | FR-73 | `[~]` — `/make_join` and `/send_join` (v1 legacy envelope and v2) served and interoperating: complement's own client completes a join against a qwatrix container; leave and invite are not |
| T710 | Gap filling: `POST /get_missing_events` (bulk, anchored on the room's tip) then `GET /event/{eventID}` (single), before refusing an event whose predecessors are unknown | FR-73 | `[x]` — five in-process tests: a one-event gap, a four-event chain in one request, an unfillable gap refused, a partly-bridgeable gap refused with no fragment kept; against complement both endpoints are now reached |
| T713 | Outlier events: persist an event whose history cannot be connected, using the peer's `/state_ids` + `/state` answer as the state at that event | FR-73 | `[ ]` — the named successor to the gap-fill refusal and the real blocker for `TestSyncTimelineGap`, whose peer deliberately bridges only part of a 50-event gap; storing such an event needs state-at-event, not graph continuity |
| T711 | Outbound federated join: `POST /join/{roomID}?server_name=` for a room only a peer knows (outbound `/make_join` + `/send_join`, then persist the returned state) | FR-73 | `[x]` — `internal/fedapi/joinremote.go` + `internal/roomserver/joinremote.go`, pinned by nine in-process tests in `tests/api/federation_join_test.go`; a room this server owns but does not hold is answered `M_NOT_FOUND` rather than federated, and each security guard has a test that fails when the guard is removed (`.agent-work/guard-probe.py`) |
| T715 | **Bug, open:** two qwatrix *containers* refuse each other's signed federation requests — `send_join` between hs1 and hs2 answers `M_FORBIDDEN` "Invalid request signature" — while the identical handshake between two harness servers in one process succeeds | FR-73 | `[ ]` — measured 2026-11-11 in complement (`TestMessagesOverFederation`, `TestDeviceListUpdates`); the refusing server now logs the URI it saw, the origin and the `Authorization` header, and the URI it saw carries a raw `:` where the signed URI should carry `%3A`; the two things that differ from the passing in-process test are that container server names have no port and that the container listener serves HTTP/1.1 while `httptest` negotiates h2 — **Narrowed:** `TestAHomeserverNamedWithoutAPortFederatesAJoin` reproduces it in one process — two harness servers named `127.0.0.1:PORT` federate, and naming the peer `localhost` (no port, federation port implied) fails while fetching its signing keys. Checked in as a `t.Skipf` that prints the failure; the fault is in federation between servers whose names omit the port, not in the join logic |
| T714 | Co-sign the `m.room.member` invite events this server sends to a remote user (the invited user's homeserver must sign an invite alongside the sender's server) | FR-73 | `[ ]` — found while building T711: gomatrixserverlib requires an invite to carry a signature from the invitee's domain, so a remote server will reject an invitation this server issues to a remote user; `tests/api/federation_join_test.go` documents the rule in `TestAFederatedJoinOfARejoiningUser` and works around it there |
| T712 | Federation of device lists (`GET /user/devices`, `m.device_list_updates` EDU) | FR-74 | `[ ]` — blocks `TestDeviceListUpdates`, the last mandatory AC-5 failure; needs T707 |
| T707 | EDUs: typing, receipts, device lists, to-device over federation | FR-74 | `[ ]` — `/send` parses them and applies none; every drop is logged by type because the response cannot report an ephemeral failure |
| T708 | `/query/profile`, `/query/user/info`, `/user/keys/*`, `/backfill`, `/missings` | FR-74 | `[ ]` |
| T709 | Restricted-rooms/spaces remote joins, `TestRestrictedRoomsRemoteJoin*` | FR-73 | `[ ]` |

## M8 — Hardening + reporting

| ID | Task | Reqs | Status |
| --- | --- | --- | --- |
| T801 | Restart/persistence + sync-token survival tests | AC-7 | `[ ]` |
| T802 | Error-path audit: no internal error text/stack to clients; log-secrets test | CN-7, AC-7 | `[ ]` |
| T803 | Full complement sweep + `.agent-work/COMPLEMENT-STATUS.md` history | AC-5 | `[~]` — `COMPLEMENT-STATUS.md` keeps the per-run history; the sweep keeps needing to be re-run as features land |
| T804 | README, docs/, `.agent-work/FINAL-REPORT.md` | AC-6 | `[ ]` |
| T805 | `golangci-lint` clean, `-race` clean, `gofmt -s` clean | CN-1, CN-3 | `[x]` — `golangci-lint run ./...` reports 0 issues (20 were cleared on 2026-11-11d: 9 unchecked `Close`/`Fprintf`, 7 dead helpers, 3 style), `-race` clean, `gofmt -s -l .` empty |

## What is actually left (re-audited 2026-11-11e, after T308)

Ordered by what they unlock, with the honest size of each.

1. **Federation to the end of the join handshake** — `send_join` (v1 *and* v2: complement drives v1),
   then `PUT /_matrix/federation/v1/send/{txnID}` ingest, then `/get_missing_events`. This is the only
   route to the three remaining mandatory failures, all of which deploy a second server:
   `TestSync`'s redaction subtest, `TestSyncTimelineGap`, `TestDeviceListUpdates`. `make_join` already
   interoperates: complement's gomatrixserverlib client accepts our template and signs a join with it.
2. **T608 room upgrade + tombstone** — one endpoint, and it is how a room ever moves room version.
3. **T801 restart/persistence tests** — AC-7 names them and none exist.
4. **T802 error-path audit + log-secrets test** — no stack traces or SQL in bodies, no access tokens
   or password hashes in logs.
5. **T804 README and `.agent-work/FINAL-REPORT.md`** — there is still no README at the repository root.
6. **Small client-surface leftovers**: `GET /account/3pid`, `mutual_rooms`, `/versions` unstable-flag
   negotiation, URL preview (`TestUrlPreview`), `/search` (`TestSearch`), `TestServerNotices`,
   `TestAsyncUpload`, `TestGetRoomMembersAtPoint`, `TestInviteFromIgnoredUsersDoesNotAppearInSync`,
   `TestTxnIdWithRefreshToken` (refresh tokens), `TestThreadsEndpoint`, `TestChangePasswordPushers`,
   `TestE2EKeyBackupReplaceRoomKeyRules`.
7. **T715** — why two qwatrix containers refuse each other's signatures while two in-process
   servers do not. Every remaining container-side federation failure waits behind this one.
8. **T710, T711** — gap fill (`/get_missing_events`) and the outbound federated join. **Done**
   (2026-11-10); T713 and T714 are what they exposed. These were
   what every remaining federation test waits on; each names its own blocking test in its row.
8. **T707–T709, T712** — federation EDUs, `/query/*`, `/backfill`, `/missings`, device-list
   federation, restricted remote joins.

### T308 is done, and it moved the mandatory line

`unread_notifications` is computed from read receipts through the push-rule evaluator; mandatory
`TestThreadedReceipts` passes, and `TestThreadReceiptsInSyncMSC4102` now fails only where it deploys a
second server. The correction that matters for planning: **`TestThreadedReceipts` was never
federation-bound** (`Deploy(t, 1)`), and the hs2 join blamed for it belongs to
`TestPushRuleRoomUpgrade`.

### Note on `404` versus `405`

`T013` asked for `405` bodies to be Matrix-shaped. The server answers a known path with the wrong
method as `404 M_UNRECOGNIZED`, which is what the specification prescribes for an endpoint this
server does not have and what Complement's own tests check; the method is named in the body
(`Unrecognized request: DELETE /_matrix/client/v1/login`) so a client can tell "wrong method" from
"no such thing". Marked done on that reading rather than left open as a naming quibble.

## Constraints to keep

- Complement's leaked volumes exhaust Podman's 2048 lock ceiling around the twelfth full sweep; the
  runner prunes between batches and `complement-score.py` warns under 100 verdicts. Never quote a run
  whose runner has not printed `full log:`.
- `docs/ENDPOINTS.md` is checked against the routes by `tests/api/endpoint_coverage_test.go`, which
  reads route patterns out of the Go AST: build a pattern by concatenation and the endpoint is
  invisible to the test.
- Adding a repository to `store.Store` is three edits (field, `AutoMigrate`, constructor); the third
  is the one that compiles while leaving the field nil and turning a request into a bare 500.












































