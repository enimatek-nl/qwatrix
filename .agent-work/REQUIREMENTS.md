# Requirements

Sources: matrix-spec v1.19 (`/home/enimatek/Projects/refs/matrix-spec`), complement image
contract (`complement/README.md` §Image requirements, `client/auth.go`,
`internal/docker/deployer.go`), user instructions, `backend` skill mandates.

Priority: **M** = mandatory, **S** = should (compliance depth), **L** = later (tracked, not this milestone).

## 1. Functional — discovery & versions

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-1 | `GET /_matrix/client/versions` returns `200` `{"versions":[...], "unstable_features":{...}}`, no auth, listing every supported version `v1.0`…`v1.11` (M) | unit + api test; complement HS-readiness probe |
| FR-2 | Unknown `/_matrix/*` paths answer `404 {"errcode":"M_UNRECOGNIZED"}`; unknown non-matrix paths answer plain 404 (M) | api test + complement `TestUnknownEndpoints` |
| FR-3 | `GET /.well-known/matrix/client` served when configured (S) | api test |

## 2. Functional — registration, login, sessions

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-10 | `POST /register` completes the `m.login.dummy` three-stage flow (stages → session → `200 {user_id, access_token, device_id}`) (M) | api test + complement `TestRegistration` |
| FR-11 | Duplicate localpart → `400 M_USER_IN_USE`; illegal localpart → `400 M_INVALID_USERNAME`; too-long → `400 M_EXCLUSIVE`-family error per spec (M) | api table test |
| FR-12 | `GET /register/available` reports availability, `M_USER_IN_USE` when taken (S) | api test |
| FR-13 | `GET /_synapse/admin/v1/register` returns `{"nonce":...}` and `POST` accepts the Synapse shared-secret HMAC (secret `complement`, HMAC-SHA1 over `nonce\0user\0pass\0admin|notadmin`) with `admin` flag honoured (M — complement's primary registration path) | api test + every complement test that calls `deployment.Register` |
| FR-14 | `GET /login` advertises `m.login.password` (+`m.login.dummy`/`m.login.application_service` as configured) (M) | api test |
| FR-15 | `POST /login` with correct password → token+device; wrong password → `403 M_FORBIDDEN`; deactivated → `403 M_USER_DISABLED`; unknown user → `403 M_FORBIDDEN` (M) | api table test |
| FR-16 | `GET /account/whoami` returns `user_id`, `device_id`, `is_guest:false` (M) | api test |
| FR-17 | `POST /logout` revokes exactly one device's token; `POST /logout/all` all of them; further calls with the revoked token → `401 M_UNKNOWN_TOKEN` (M) | api test |
| FR-18 | Tokens accepted from `Authorization: Bearer` **and** `?access_token=` (both are used by clients and by media/federation-adjacent endpoints) (M) | api test |
| FR-19 | Device management: `GET /devices`, `PUT /devices/{id}`, `DELETE /devices/{id}`, `POST /delete_devices` (S) | api test + complement `TestDeviceDeletion` |
| FR-20 | `POST /account/password`, `POST /account/deactivate` (UIA `m.login.password`), password change invalidates tokens except the acting device (S) | api test |
| FR-21 | Interactive auth (`m.login.password`, `m.login.dummy`, `m.login.email/registration`-shaped 401 body with `session`, `M_UNAUTHORIZED`/`M_FORBIDDEN`/`M_MISSING_PARAM`) (S) | api test |

## 3. Functional — rooms and events (client-server)

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-25 | `POST /createRoom` honours `preset` (`private_chat`, `public_chat`, `trusted_private_chat`), `name`/`topic`/`visibility`/`room_alias_name`/`room_version`/`creation_content`/`initial_state`/`power_level_content_override`/`invite`/`is_direct`, and rejects unknown `room_version` with `M_UNSUPPORTED_ROOM_VERSION` (M) | api test + complement `TestRoomCreation*` |
| FR-26 | Rooms are created as a valid Matrix DAG: `m.room.create`, creator `m.room.member` (`join`), `m.room.power_levels`, `m.room.join_rules`, `m.room.history_visibility`, `m.room.guest_access` — signed by the server key, correct `depth`, `prev_events`, `auth_events`, `event_id` per room version (M) | api test (field-level) + complement `TestRoomCreateEvents` |
| FR-27 | `PUT/GET /rooms/{id}/send/{type}/{txnId}` sends message events with per-device `txn_id` idempotency (replay → same `event_id`; reused txn with different body → `400 M_TRANSACTION_FAILED`) and enforces event authorization (M) | api test + complement `TestSendEventWithUTANonce`? (no) — `TestPartialState`/`TestMessagePagination` |
| FR-28 | `GET/PUT /rooms/{id}/state/{type}/{state_key}` + `GET /rooms/{id}/state` + `GET /rooms/{id}/state_event_auth`… (state read/write with power-level enforcement) (M) | api test + complement `TestState*` |
| FR-29 | Membership: `/invite`, `/join/{idOrAlias}`, `/rooms/{id}/leave`, `/kick`, `/ban`, `/unban`, `/forget`; `308`/alias handling for joins we do not host is out of scope in single-server mode; permission failures → `403 M_FORBIDDEN`; joining a `public` room without invite works, `invite` room without invite does not (M) | api test + complement `TestRoomJoin`, `TestRoomBan`, `TestRoomKick`, `TestInboundInvite`… |
| FR-30 | `GET /rooms/{id}/members` (+`not_membership`, `since`-token filtering), `/joined_members`, `/joined_rooms`, `/rooms/{id}/event/{eventId}`, `/rooms/{id}/event_auth/{eventId}` (M) | api test |
| FR-31 | `GET /rooms/{id}/messages` with `from`/`to`/`dir`/`limit`/`filter` and `chunk`/`start`/`end` (M) | api test + complement `TestMessagePagination` |
| FR-32 | `POST /rooms/{id}/redact/{txnId}` (and `PUT` form) with power-level + sender rules, correct redaction algorithm per room version, `unsigned.redacts`/`redacts` field (M) | api test + complement `TestRedaction*` |
| FR-33 | Aliases & directory: `PUT/DELETE /directory/room/{alias}`, `GET /directory/room/{alias}`, `GET/PUT /directory/list/room/{roomId}`, `GET /publicRooms` (+`/network/{name}`), `GET /rooms/{id}/aliases`, canonical alias in state (M) | api test + complement `TestRoomAlias`, `TestRoomDirectory` |
| FR-34 | `GET /rooms/{id}/context/{eventId}` (+`filter`, `event_filter`), `GET /rooms/{id}/relations/...` (S) | api test |
| FR-35 | `GET /rooms/{id}/hierarchy` (space hierarchy, `MAX_DEPTH`/`M_FORBIDDEN`), `GET /rooms/{id}/summary` / `room_types` filtering (S) | api test + complement `TestRoomHierarchy` |
| FR-36 | Knocking: `/knock`, `/accept_knock`-less server side, join rules `knock`/`knock_restricted` (L/S) | complement `TestKnock*` |
| FR-37 | `POST /rooms/{id}/report` (L) | — |

## 4. Functional — sync, read state, ephemeral

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-40 | `GET /sync` returns `next_batch`, `rooms.join/invite/leave`, `state`/`timeline` (`events`,`prev_batch`,`limited`), `device_one_time_keys_count`, `device_lists`, `to_device`, `presence`, `account_data`, `device_lists.op_sync` optional; first sync (no `since`) returns full room state and a timeline of ≤10 events (M) | api test + nearly every complement test |
| FR-41 | Incremental `since` semantics: only new events, state deltas since the token, `limited=true` when events were dropped, invalid token → `401 M_UNKNOWN_POS` (M) | api test + complement `TestStateAfterLeft`… |
| FR-42 | `timeout` long-poll with timely wake-up (no busy wait), `set_presence`, `filter` (inline JSON or filter ID), `since`+`timeout=0` fast path (M) | api test (long-poll wake-up timing) |
| FR-43 | `POST /user/{userId}/filter`, `GET /user/{userId}/filter/{filterId}`, `GET .../urlcache` (S) | api test |
| FR-44 | Typing: `PUT /rooms/{id}/typing/{userId}` (+`/rooms/{id}/typing` read path in sync) (S) | api test + complement `TestTypingDeviceLists`? (federation later) |
| FR-45 | Receipts: `POST /rooms/{id}/receipt/{receiptType}/{threadId}/{eventId}`, `POST /rooms/{id}/read_markers`, `GET /rooms/{id}/read_markers`, `GET /rooms/{id}/receipts` (S) | api test + complement `TestReadMarkers`, `TestReceipts` |
| FR-46 | `unread_notifications` + `notification_count`/`highlight_count` per joined room (S) | api test |
| FR-47 | Presence `GET/PUT /presence/{userId}/status`, `m.presence` in sync (S) | api test + complement `TestPresence` |

## 5. Functional — end-to-end encryption key distribution

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-50 | `POST /keys/upload` accepts device keys + one-time keys + fallback keys, returns counts; `POST /keys/query` returns claimed keys and `failures`; `POST /keys/claim` consumes one-time keys; `GET /keys/changes` (M) | api test + complement `TestKeysQuery`, `TestKeysClaim`, `TestOTKCount` |
| FR-51 | `device_lists.changed`/`left` in `/sync` when tracked users' devices change; `device_one_time_keys_count` + fallback key counts (M for E2EE-shaped tests) | api test |
| FR-52 | Cross-signing `POST /keys/device_signing/upload`, `GET /keys/query` incl. master/self-signing, key backup `/keys/backup/*` (L) | complement `TestCrossSigning*` |

## 6. Functional — media (content repository)

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-55 | `POST /_matrix/media/v3/upload` (`?filename=`), `GET /_matrix/media/v3/download/{server}/{mediaId}`, `GET /_matrix/media/v3/thumbnail/...`, `GET /_matrix/media/v3/config`, `GET /_matrix/client/v1/media/download/...` with `?allow_remote` and `?timeout_ms`; `M_NOT_FOUND` for unknown media; filename/`Content-Disposition` encoding incl. non-ASCII and no-filename paths (S) | api test + complement `TestMedia*` |

## 7. Functional — account data, tags, push, misc

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-60 | Account data (global + per-room) `PUT/GET` + `DELETE`, surfaced in `/sync` deltas (S) | api test |
| FR-61 | Room tags `GET/PUT/DELETE /user/{id}/rooms/{roomId}/tags[/{tag}]` (S) | api test |
| FR-62 | `GET /capabilities` (room versions, changes, `m.version`), `GET /account/3pid` (empty ok), `GET /versions`-adjacent `GET /login` deprecation headers (S) | api test |
| FR-63 | Pushers `GET /pushers`, `POST /pushers/set`, `GET/PUT/DELETE /pushrules/...` (S) | api test + complement `TestPushers`? |
| FR-64 | `GET /account/whoami` `is_guest`, guest access rules per `m.room.guest_access` (L) | complement guest tests |

## 8. Functional — federation (server-server) surface

| ID | Requirement | Verify |
| --- | --- | --- |
| FR-70 | Serves HTTPS on 8448 with a certificate naming `SERVER_NAME`; `GET /_matrix/federation/v1/version`; `GET /_matrix/key/v2/server` with signed notary keys and `verify_keys`/`old_verify_keys` (M for two-server complement deployments to even start) | complement two-server deploys |
| FR-71 | `GET /_matrix/federation/v1/state_ids/{roomId}`, `/state/{roomId}`, `/event/{eventId}`, `/event_auth/{roomId}/{eventId}`, `/timestamp_to_event` (S) | complement federation tests |
| FR-72 | Signed request verification (`Authorization: origin=…,key=ed25519:…`, `/federation/1.0/` URI, valid per GMSL key fetching) and outbound request signing (S) | complement federation tests |
| FR-73 | Join handshake v2 `/make_join`+`/send_join`, invite v2 `/make_invite`/`/send_invite`? (invite is `/invite` v2 + `/exchange_invite`), leave `/make_leave`+`/send_leave`, `/event` + `/send/{txnId}` transaction receiving with PDU processing, `/backfill`, `/missings`? (L) | complement federation tests |
| FR-74 | `/query/profile`, `/query/user/info`, `/query/media/download`, `/media/download`, `/user/keys/*` (device lists + key claim/claim over federation), `/send_to_device` (L) | complement federation tests |

## 9. Technical / non-functional constraints

| ID | Requirement | Verify |
| --- | --- | --- |
| CN-1 | Go implementation; `go build ./...`, `go vet ./...`, `gofmt -s -l` clean; `golangci-lint` clean (M) | make targets |
| CN-2 | All Go test files live **only** under `tests/` (no `_test.go` beside implementation files) — user instruction (M) | `find . -name '*_test.go' -not -path './tests/*'` must be empty |
| CN-3 | Tests are behavioural and table-driven where they express a set of cases; run with `-race` (M, `backend` skill) | `make test`/`make race` |
| CN-4 | One static binary (`CGO_ENABLED=0`), storage in one SQLite file; no external service dependency (M, skill mandate) | image build |
| CN-5 | Configuration via flags **and** environment; nothing operational hard-coded — server name, addresses, DB path, shared secret, room-version default, rate limits, media limits (M, skill mandate) | api test + entrypoint |
| CN-6 | `context.Context` first argument on all I/O paths; all errors wrapped with `%w`; goroutines owned by an `errgroup` with cancellation and graceful shutdown (M, skill mandate) | review + `go vet` |
| CN-7 | Spec-shaped errors for every failure path: `{"errcode","error"}` JSON, correct status; no Go/SQLite error text leaked to clients (M) | api tests + review |
| CN-8 | Rate limiting with spec status `429 M_LIMIT_EXCEEDED` + `retry_after_ms`, generous defaults so compliance tests are not flaky (S) | api test |
| CN-9 | CORS per spec (incl. `Access-Control-Allow-Origin: *`, `?access_token` allowed header) (S) | api test |
| CN-10 | Documented endpoints & compliance status in the repo (`docs/ENDPOINTS.md`, `.agent-work/COMPLEMENT-STATUS.md`) (M) | files |

## 10. Complement integration requirements

| ID | Requirement | Verify |
| --- | --- | --- |
| CC-1 | Image `EXPOSE`s 8008 (client, plain HTTP) and 8448 (federation, HTTPS) (M) | Dockerfile + running suite |
| CC-2 | `HEALTHCHECK` that is green only when the API answers; healthy within `COMPLEMENT_SPAWN_HS_TIMEOUT_SECS` (default 30 s) (M) | build with `BUILDAH_FORMAT=docker`, running suite |
| CC-3 | Accepts `SERVER_NAME` at runtime as the identity of the homeserver, and `ENTRYPOINT` may run multiple times (fresh volumes per container) (M) | running suite |
| CC-4 | Registration secret `complement` at `/_synapse/admin/v1/register` (M) | running suite |
| CC-5 | Derives a TLS certificate for `SERVER_NAME` from the CA complement copies to `/complement/ca/{ca.crt,ca.key}` and trusts that CA outbound (M for federation tests) | two-server complement tests |
| CC-6 | Application-service registrations appear as YAML in `/complement/appservice/` (L) | complement `b.HomeserverWithAppservice` |
| CC-7 | Our own complement-style tests live in `tests/complement/` and run with `COMPLEMENT_BASE_IMAGE=… go test ./tests/complement` (out-of-repo pattern) (M — user instruction) | make target |
| CC-8 | Upstream complement suite runnable via `make complement` from a complement checkout (pinned commit recorded) (M) | make target + status file |

## 11. Explicitly out of scope (recorded decisions)

- Application services / AS API (L, CC-6 tracks the container side only).
- Sliding sync (MSC4186), matrix hooks, OIDC provider, SSO identity providers.
- Third-party ID / identity-server integration, email verification, 3PID invites.
- Search (`/search`) and the event-index backends beyond simple room/message search.
- Room version 12 state-DAG variant `org.matrix.msc4242.12`, historical/blocked room versions.
- Media URL pre-authenticated (`/_matrix/media/downloads/`) and `mxc_uri` federation media (L).
- Client-bandwidth optimisations: lazy-loading member events is implemented, but not
  MSC3391 partial state, sticky events, or the newer state-dag optimisations.
