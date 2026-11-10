# Acceptance criteria

Every criterion has an objective, executable check. `make help` lists the targets.
Results are recorded with real command output in `.agent-work/PROGRESS.md` and
`.agent-work/COMPLEMENT-STATUS.md` — nothing is claimed without being run.

## Mandatory criteria

### AC-1 Toolchain hygiene
- `go build ./...` exit 0.
- `go vet ./...` exit 0.
- `gofmt -s -l cmd internal tests` prints nothing.
- `golangci-lint run ./...` exit 0 (if the binary is installed; otherwise recorded as skipped).

### AC-2 Test-location constraint (user instruction)
- `find . -name '*_test.go' -not -path './tests/*' -not -path './.git/*'` prints nothing.
- `go test ./...` therefore only compiles packages under `tests/`.

### AC-3 Automated test suite green
- `go test -race -count=1 ./tests/...` exit 0 (in-process compatibility + unit suites; the
  `tests/complement` package is build-tagged `complement` so a plain `go test ./...` never
  requires a container runtime).
- Coverage of the implemented surface is reported (`go test -cover ./tests/...`), and every
  implemented endpoint area has at least one behavioural test.

### AC-4 Complement container contract
`make complement-image` builds `localhost/qwatrix-complement:latest` and a real complement
run proves the contract. Concretely, `tests/complement` (out-of-repo style) contains tests
that must pass:
1. `TestComplementCanDeployAndAnswerVersions` — `complement.Deploy(t,1)` succeeds,
   `GET /_matrix/client/versions` is `200` (proves ports, health, readiness probe).
2. `TestComplementSharedSecretRegistration` — `deployment.Register` succeeds (proves the
   `/_synapse/admin/v1/register` HMAC contract, no framework skip).
3. `TestComplementRoomLifecycle` — register → create room → send → sync (proves the
   end-to-end path inside the container).
4. `TestComplementTwoServerDeployment` — `Deploy(t,2)` succeeds and both servers answer
   `/versions` and `/_matrix/federation/v1/version` over HTTPS on 8448 with a certificate
   signed by complement's CA (proves the PKI bootstrap).

### AC-5 Upstream complement suite — measured compliance
`make complement` runs complement's own suite from the pinned checkout and
`.agent-work/COMPLEMENT-STATUS.md` records, per run: complement commit, command, counts of
PASS / FAIL / SKIP with the named test lists.

Rules for the recorded run:
- **Zero skips caused by `/_synapse/admin/v1/register`** (a skip there means every
  admin-flagged registration is untested → AC-4/AC-5 not met). Note that Complement's
  non-admin registrations go through `POST /register`, so that flow must work too.
- The **named core set below must pass**:

  Accounts/sessions: `TestRegistration`, `TestCanRegisterAdmin`, `TestLogin`, `TestLogout`,
  `TestChangePassword`, `TestDeactivateAccount`, `TestDeviceManagement`, `TestDisplayNameUpdate`,
  `TestAvatarUrlUpdate`, `TestProfileDisplayName`, `TestProfileAvatarURL`

  Rooms/events: `TestRoomCreate`, `TestRoomState`, `TestRoomMembers`, `TestGetRoomMembers`,
  `TestGetFilteredRoomMembers`, `TestRoomAlias`, `TestRoomDeleteAlias`, `TestRoomCanonicalAlias`,
  `TestRoomForget`, `TestRoomsInvite`, `TestRoomCreationReportsEventsToMyself`,
  `TestSendAndFetchMessage`, `TestSendMessageWithTxn`, `TestTxnIdempotency`,
  `TestTxnIdempotencyScopedToDevice`, `TestTxnInEvent`, `TestTxnScopeOnLocalEcho`,
  `TestPowerLevels`, `TestDemotingUsersViaUsersDefault`, `TestRedact`, `TestCannotKickLeftUser`,
  `TestCannotKickNonPresentUser`, `TestNotPresentUserCannotBanOthers`, `TestUnbanViaInvite`,
  `TestMembershipOnEvents`, `TestFetchEvent`, `TestFetchEventWorldReadable`,
  `TestFetchEventNonWorldReadable`, `TestFetchMessagesFromNonExistentRoom`, `TestEventAuth`,
  `TestJson`, `TestUnknownEndpoints`, `TestServerCapabilities`, `TestPublicRooms`,
  `TestRoomSummary`, `TestRoomImageRoundtrip`, `TestRoomSpecificUsernameAtJoin`,
  `TestRoomSpecificUsernameChange`

  Sync/read state: `TestSync`, `TestSyncFilter`, `TestSyncLeaveSection`, `TestSyncTimelineGap`,
  `TestSyncOmitsStateChangeOnFilteredEvents`, `TestFilter`, `TestCumulativeJoinLeaveJoinSync`,
  `TestArchivedRoomsHistory`, `TestLeftRoomFixture`, `TestOlderLeftRoomsNotInLeaveSection`,
  `TestRoomMessagesLazyLoadingLocalUser`, `TestRoomReadMarkers`, `TestRoomReceipts`,
  `TestThreadedReceipts`, `TestTyping`, `TestLeakyTyping`, `TestPresence`,
  `TestPresenceSyncDifferentRooms`

  Data/keys/media: `TestAddAccountData`, `TestWriteMDirectAccountData`, `TestPushSync`,
  `TestPushRuleCacheHealth`, `TestUploadKey`, `TestUploadKeyIdempotency`,
  `TestUploadKeyIdempotencyOverlap`, `TestKeyChangesLocal`, `TestKeyClaimOrdering`,
  `TestKeysQueryWithDeviceIDAsObjectFails`, `TestDeviceListUpdates`, `TestToDeviceMessages`,
  `TestMediaConfig`, `TestMediaFilenames`, `TestMediaWithoutFileName`, `TestContent`,
  `TestContentCSAPIMediaV1`, `TestContentMediaV1`, `TestMediaWithoutFileNameCSMediaV1`,
  `TestLocalPngThumbnail`, `TestRelations`, `TestRelationsPagination`,
  `TestRelationsPaginationSync`, `TestClientSpacesSummary`, `TestRoomSummaryAllowedRoomIDs`,
  `TestRestrictedRoomsLocalJoin`, `TestRestrictedRoomsSpacesSummaryLocal`,
  `TestJumpToDateEndpoint`

  Note: `TestTxnIdWithRefreshToken` (refresh tokens) and `TestUrlPreview`, `TestAsyncUpload`,
  `TestSearch`, `TestMSC428x/429x` (unstable room versions), `TestACLs*`, `TestKnocking*` are
  *not* in the mandatory set; they are tracked as growth items.
- The whole-suite pass count must be **≥ 70 of the 107 `tests/csapi` tests** at completion of
  this milestone (measured with `-timeout=60m`), and any test that passed in an earlier
  recorded run must still pass (regression check is part of the status file's history table).
- Federation (`tests/` root, `Deploy(t,2)`) is measured and reported separately; each
  federation test that passes is listed. No federation pass is required for AC-5, but
  `TestVersionStructure` and `TestInboundFederationKeys` are targeted stretch items.

### AC-6 Documentation of what exists
- `docs/ENDPOINTS.md`: every implemented client-server endpoint, its method+path, the spec
  section it implements, and its status (`implemented` / `partial` / `stub`), generated or
  verified by `make endpoints-check` (a test asserts every registered route is listed).
- `README.md`: how to run the server, register a user, and run both test layers.
- `.agent-work/FINAL-REPORT.md`: what shipped, decisions, checks actually run with results,
  known gaps.

### AC-7 Runtime behaviour
- Booting with no arguments works on a fresh directory (dev defaults: loopback address,
  `./qwatrix.db`) and creates nothing outside its data path.
- `SIGTERM`/`SIGINT` shuts down in-flight requests (graceful), and the process exits 0.
- Restarting against an existing database preserves accounts, rooms, and sync tokens
  (verified by `tests/api/restart_test.go`).
- No secrets, tokens, or password hashes in logs (verified by a test that captures startup
  and error-path logs).

## Quality gates during development (not delivery-blocking, still enforced)
- Each behaviour gets a failing test before implementation (TDD; RED evidence recorded in
  `PROGRESS.md` as the actual failing output line).
- `-race` clean on the in-process suites, which exercise concurrent sync/long-poll paths.

## Deferred / explicitly not accepted here
Federation depth beyond AC-4 (full join/invite/leave handshakes, transaction PDU ingest,
backfill/gap-fill, federated media), sliding sync, OIDC/SSO, application services, key
backup + cross-signing, knocking, ACLs, URL preview, search, push gateway delivery. These
are listed in `REQUIREMENTS.md` §11/§8 with `L` priority and recorded as remaining work in
`FINAL-REPORT.md`.
