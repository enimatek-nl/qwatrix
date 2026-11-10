# Plan

Ordered milestones; each is independently verifiable and leaves the tree green. Within a
milestone, work is one behaviour at a time (RED → GREEN → refactor, `tdd` skill); the task
queue with live status is `TASKS.md`, evidence lands in `PROGRESS.md`.

## M0 — Foundation + complement tracer bullet (de-risk first)

**Why first:** the single biggest risk (R1) is that complement cannot drive our image on this
podman host. That must fail in hour one, not hour ten.

Deliverables: module + tooling; config; SQLite store with users/devices/tokens; HTTP server
with `/versions`, error contract, health; complement image + entrypoint (TLS from
complement's CA); `tests/harness`; first `tests/complement` test driving
`complement.Deploy` + registration through the real framework.

Exit: `make complement-smoke` passes (Deploy, `/versions`, shared-secret register, room
create+send+sync inside the container) and `go test -race ./tests/...` is green.

## M1 — Accounts, sessions, devices

Registration (standard 3-stage + shared secret), login, logout, whoami, device list/update/
delete, password change + account deactivation with interactive auth, profile
displayname/avatar, `/register/available`.

Exit: `tests/api/{auth,account,profile}_test.go` green; complement `TestRegistration`,
`TestCanRegisterAdmin`, `TestLogin`, `TestLogout`, `TestChangePassword`,
`TestDeactivateAccount`, `TestDeviceManagement`, `TestDisplayNameUpdate`,
`TestAvatarUrlUpdate`, `TestProfile*` pass.

## M2 — Rooms, events, membership, directory

Create-room (presets, versions, initial state, power levels, aliases), state read/write,
message send with txn idempotency, event/members/state reads, invite/join/leave/kick/ban/
unban/forget, redaction, aliases + public rooms directory, `/messages`, event-by-id,
`event_auth`, context, relations, `?limit`/lazy loading plumbing, room summary/hierarchy.

Exit: rooms/state/events/membership/pagination/directory tests green; complement
`TestRoomCreate`, `TestRoomState`, `TestRoomMembers`, `TestRoomAlias`, `TestRoomForget`,
`TestRoomsInvite`, `TestPowerLevels`, `TestRedact`, `TestSendAndFetchMessage`,
`TestTxn*`, `TestFetchEvent*`, `TestPublicRooms`, `TestRoomSummary*` pass.

## M3 — Sync, ephemeral, read state

Full `/sync` (first + incremental + long-poll + filters), typing, receipts, read markers,
presence, `unread_notifications`, `/messages` filters, `TestSyncTimelineGap` semantics.

Exit: `tests/api/sync_test.go` (+ ephemeral) green; complement `TestSync*`, `TestFilter`,
`TestTyping`, `TestLeakyTyping`, `TestPresence*`, `TestRoomReceipts`, `TestThreadedReceipts`,
`TestRoomReadMarkers`, `TestArchivedRoomsHistory`, `TestLeftRoomFixture`,
`TestCumulativeJoinLeaveJoinSync` pass.

## M4 — E2EE key distribution + to-device

`/keys/upload|query|claim|changes`, device-list propagation in sync, OTK/fallback counts,
to-device queue + `/send_to_device` (local), key claim ordering.

Exit: complement `TestUploadKey*`, `TestKeyChangesLocal`, `TestKeyClaimOrdering`,
`TestKeysQueryWithDeviceIDAsObjectFails`, `TestDeviceListUpdates`, `TestToDeviceMessages` pass.

## M5 — Content repository

Upload (sync + size/type limits), download (CS v3 + v1 client media paths), filename
encoding incl. the no-filename and non-ASCII cases, thumbnail generation (PNG/JPEG),
`/media/v3/config`, `?allow_remote`, URL cache table.

Exit: complement `TestMediaConfig`, `TestMediaFilenames`, `TestMediaWithoutFileName*`,
`TestContent*`, `TestLocalPngThumbnail` pass.

## M6 — Remaining client surface

Account data (global/room), tags, filters endpoint, capabilities, pushers + push rules
(+ `TestPushSync`, `TestPushRuleCacheHealth`), `/relations` pagination, `m.direct`,
`/jump_to_date`, unknown-endpoint contract, CORS/rate-limit behaviour, `/search`-lite if
cheap, guest-access rules.

Exit: complement core set for these areas passes; `docs/ENDPOINTS.md` regenerated.

## M7 — Federation surface (as far as it goes)

Keys/identity (`/key/v2/server`, TLS), `/version`, signed request verification + outbound
signing, `/state_ids`, `/state`, `/event`, `/event_auth`, `/timestamp_to_event`, `/query/*`,
`/transactions` ingest, `/make_join`+`/send_join` (v2), `/make_leave`+`/send_leave`,
invite v2, then typing/receipts/device-list EDUs, `/backfill`, `/missings`, federated media.

Exit (staged): `TestVersionStructure`, `TestInboundFederationKeys` →
`TestJoinFederatedRoom*`/`TestOutboundFederationSend`/`TestMessagesOverFederation` →
`TestRemoteTyping`/`TestRemotePresence`/`TestDeviceListsUpdateOverFederation*`.
Each stage is a separate verified commit; M7 does not block M0–M6 delivery.

## M8 — Hardening, compliance sweep, docs

Full complement run + status file; race run; error-path audit (no leaked internals);
restart/persistence tests; log-secrets test; README + ENDPOINTS + FINAL-REPORT.

Exit: every mandatory acceptance criterion in `ACCEPTANCE-CRITERIA.md` verified with recorded
output.

## Working agreements

- One `internal/` package per area, handlers never touch GORM (see `ARCHITECTURE.md` §2).
- Every new endpoint lands with: handler + route registration + test + `docs/ENDPOINTS.md`
  line. `tests/api/endpoint_coverage_test.go` fails if a registered route is undocumented.
- Complement is the arbiter of behaviour; when complement and the spec text disagree we
  follow complement for compliance and record the divergence in `docs/` notes.
- Never mark a task done without the run output in `PROGRESS.md`.
