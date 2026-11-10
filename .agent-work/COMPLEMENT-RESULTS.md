# Upstream complement results

The evidence behind `.agent-work/COMPLEMENT-STATUS.md`. Each run is one `make complement`
against the container image, so the numbers are comparable: same suite, same base image, same
`COMPLEMENT_SERVER_TRACEBACK=true`.

## Run 1 — 2025-11-09 20:16, commit state after M0–M3 (rooms + sync)

`COMPLEMENT_RUN='' make complement` over the whole `./tests/csapi` package.

| measure | value |
| --- | --- |
| tests discovered | 107 |
| failures | 45 |
| passing (by subtraction) | 62 |
| zero skips | yes — no `--- SKIP` line in the log |
| duration | 285.7s |

### Failures, with the first failing assertion

Failures are grouped by root cause because several tests share one cause; the milestone is
the one whose acceptance criteria cover it.

**Endpoints not implemented yet (expected at this stage — M5/M6 or growth items)**

| test | first failure |
| --- | --- |
| TestAddAccountData | `PUT /user/{user}/account_data/test.key` → 404 |
| TestContent, TestContentCSAPIMediaV1, TestRoomImageRoundtrip, TestAsyncUpload, TestMediaConfig | `POST /_matrix/media/v3/upload`, `GET /_matrix/media/v3/config` → 404 |
| TestChangePasswordPushers | `POST /pushers/set` → 404 |
| TestPushRuleCacheHealth | `PUT /pushrules/global/sender/…` → 404 |
| TestKeyChangesLocal | `POST /keys/upload` → 404 |
| TestE2EKeyBackupReplaceRoomKeyRules | `POST /room_keys/version` → 404 |
| TestPresence | `POST /join/{roomID}?server_name=hs1` → 404 (see the join gap below) |
| TestSearch | search is a growth item, not in AC-5 |
| TestServerNotices | `POST /_synapse/admin/v1/send_server_notice` → 404, wants 403 |
| TestInviteFromIgnoredUsersDoesNotAppearInSync | needs ignored_user_list account data |

**Missing endpoints that are cheap and were fixed in the next commit**

| test | first failure | fix |
| --- | --- | --- |
| TestServerCapabilities, TestRoomMembers, TestRoomDeleteAlias, TestPowerLevels, TestDemotingUsersViaUsersDefault | `GET /capabilities` → 404 | implement `GET /capabilities` with `m.room_versions` and `m.change_password` |
| TestPublicRooms | `POST /publicRooms` → `M_UNRECOGNIZED` (only GET was registered) | register POST and read its filter body |
| TestRoomCanonicalAlias | `PUT /directory/room/{alias}` → 404 | `PUT`/`DELETE` on that path create and delete an alias, which only `/directories/alias/{alias}` was registered for |
| TestPresence, TestDeviceListUpdates, TestFetchEvent, the history-visibility tests | `POST /join/{roomIDOrAlias}?server_name=hs1` → 404 | register `POST /join/{roomIDOrAlias}` and resolve aliases through the alias store |
| TestProfileDisplayName, TestProfileAvatarURL | `GET /profile/{user}/displayname` → 401 | profile reads have been public since spec v1.1, so `GET` is now `optionalAuth` |

**Protocol errors in behaviour this server already had**

| test | symptom | diagnosis |
| --- | --- | --- |
| TestRegistration | `POST /_synapse/admin/v1/register` with `user-UPPER-shared-SECRET` → 400 `M_INVALID_USERNAME` | Synapse's shared-secret protocol verifies the MAC over the username as sent but **stores it downcased**; the server must fold the case after verification |
| TestLogin | login with an uppercase local part → 403 | accounts are stored lowercase and clients send what a human typed, so the local part is now folded before lookup |
| TestRequestEncodingFails | `POST /register` with invalid UTF-8 in a JSON string → 401 UIA challenge | Go's decoder silently replaces invalid UTF-8; the request body is now rejected as `M_NOT_JSON` before any state machine runs |
| TestJson | `{"body": Infinity}` → 500 | same body-reading path, now `M_BAD_JSON`/`M_NOT_JSON` with 400 |
| TestEvent, "Large State Event" | a 70 KB event → 400, wants 413 | the event content limit is a federation limit every server enforces: now 413 `M_TOO_LARGE` above 65536 bytes |
| TestRoomState | `POST /rooms/{roomID}/leave` → 500 | the membership handler filled `TargetLocalpart` from the session with the **full user ID**, which the room server then built into `@alice:hs1:hs1`; join/leave/forget act on the caller and take no target |
| TestRoomAlias, TestRoomForget | `GET /directory/room/{alias}` 404 / `POST /rooms/{id}/forget` 404 where 400 was expected | both follow from the alias and membership gaps above; re-verified in the next run |
| TestRoomCreate, TestRoomMembers, TestRoomReceipts, TestRoomReadMarkers, TestMembershipOnEvents, TestFetchHistorical*, TestDeviceListUpdates | `SendEventSynced waiting for event ID …` | the room existed but the test's `POST /join/…` had 404'd, so the sender was never a member; the same join gap |

**Also visible**

| test | note |
| --- | --- |
| TestRoomState | `POST /rooms/{id}/leave` 500 (above) |
| TestServerNotices | wants 403 for an admin endpoint called by a non-admin; AC-6 growth item |
| TestChangePassword | a `runtime.SkipIf([venator])` warning: complement cannot identify the server without a `*_blacklist` build tag and so runs every host-specific test |

### Observations for the next round

1. `SendEventSynced` timeouts dominate: any endpoint a test needs *before* it can send is
   worth more than a correct-but-unreachable feature.
2. Complement discovers the server name from `PUT /_synapse/admin/v1/send_server_notice/:user_id`,
   which it calls during deployment. A `404 M_UNRECOGNIZED` answer is acceptable — no test
   failed on the deploy step — but TestServerNotices wants 403.
3. Tests gated on `runtime.SkipIf([dendrite|synapse|venator])` cannot skip because
   `COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT` does not name a known server; they run and must
   pass. That is why AC-5's zero-skip rule holds for these.
