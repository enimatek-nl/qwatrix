# Upstream complement results

The evidence behind the complement claims in `.agent-work/FINAL-REPORT.md` and this file's own
summary: every number below comes from a run against the container image, so runs are comparable —
same suite, same base image, same `COMPLEMENT_SERVER_TRACEBACK=true`.

Since run 97 the measurement is `.agent-work/complement-sweep.sh <dir>`, which sweeps **both**
packages (`./tests/csapi` and the server-server `./tests`), re-runs any test left without a verdict
one per process before calling it CRASH, merges `results.txt`, and scores AC-5 with
`.agent-work/ac5-check.py`, which reads the mandatory test names out of
`.agent-work/ACCEPTANCE-CRITERIA.md` so the requirement list cannot drift from the measurement. Runs
before 97 covered `./tests/csapi` alone, because that is what `COMPLEMENT_PKG` defaulted to; their
counts are not comparable with later ones and say so.

## The suite this file measures

complement is checked out at `7b38a63c9db3337292bee5887fc8533be3a57b9f` (2026-11-03) for every run
from 97 onward, built with `-tags blacklist_common`. Pinning matters: the suite's mandatory list and
its per-homeserver skip guards change, and a number that moved because the yardstick moved is not
progress.

The tag does **not** satisfy complement's per-homeserver `runtime.SkipIf` guards — those need a
`*_blacklist` tag — so tests a non-Synapse server would normally skip are counted here as real
failures, with a warning line in the log saying complement could not tell which homeserver is
running. The counts below are conservative rather than flattering.

## The runs, most recent first

`<img>` names the commit the container image was built from, which is the only thing a run can
honestly be attributed to: a run measures the image that exists when its containers start, not the
commit that was HEAD when somebody typed the command. Run 101 is listed and its number is not used —
it was measured against a stale image and re-running it was never finished, so its count is discarded
rather than quoted.

| run | image at | mandatory passing | what the run turned on |
| --- | --- | --- | --- |
| 108 | `2a83604` | **89 / 95** | `TestEventAuth` passes in a sweep for the first time, as does `TestUnbanViaInvite` — the two federation tests that have moved most, both fixed since run 105 |
| 107 | `6998aca` | **88 / 95** | `TestUnbanViaInvite` **passes**; `TestEventAuth` fails — and passes three times on its own against the same image, a delivery-latency race written up below |
| 106 | `4671516` | **88 / 95** | `TestUnbanViaInvite` **passes for the first time** (invitations are co-signed by the invited user's server). `TestEventAuth` falls back: complement's mock peer has no invite endpoint, so the first co-signing commit stranded every invitation aimed at it, and `6998aca` sends those unsigned rather than never |
| 105 | `84246dc` | **88 / 95** | the archive regression gone (left-room state reverted) and `TestEventAuth` kept; the seven left are listed below |
| 104 | `85069e7` | 87 / 95 | `TestEventAuth` won by the v2 `send_leave` route; `TestArchivedRoomsHistory` **lost** to the same commit, which had started putting state beside every left room's timeline |
| 103 | `0d29361` | 87 / 95 | the space hierarchy stopped hiding the room its own allow list admits; `TestRestrictedRoomsSpacesSummaryLocal` recovered from its run-102 regression |
| 102 | `f03e9ec` | 86 / 95 | state-shaped event types allowed on the timeline path (`TestRoomForget`), and the room-version advertisement made honest |
| 101 | stale | *(discarded)* | measured an image that did not carry the commit under test; see the note above |
| 100 | pre-hierarchy | 84 / 95 | the last run before the space walk; `tests/csapi` 91 pass / 15 fail, unchanged for runs 95, 98, 99, 100 |
| 99 | — | 81 / 95 | method-mismatch 405s, filename-bearing media downloads, room summary |
| 97 | — | 79 / 95 | first sweep that ran **both** packages; before it `COMPLEMENT_PKG` defaulted to `./tests/csapi`, so every earlier count covers less than the suite |

### Delivery latency, measured twice one way and once another

`TestEventAuth` failed both sweeps above and passed three runs in a row when run on its own against
the same image. The difference is timing, not behaviour: the test gives one state event **one
second** to reach the peer, and this server consulted its federation queue **every two seconds**.
The event was always delivered, often one tick too late — and a sweep is a busier machine than a
single test, which is the only reason it ever passed.

`2a83604` wakes the queue when an event is written instead, keeping the tick as the retry clock.
Runs 106 and 107 are the before picture for that test; run 108 is the check.

### What each run's image carried

Runs up to 105 predate `ef4d6b0` (delivery no longer requiring a separate federation address);
complement passes `-federation-addr :8448`, so that commit was never expected to move a complement
number and runs 106-108 confirm it did not. Run 106 is the first image with invite co-signing, 107 the
first that also sends an invitation unsigned to a peer that cannot co-sign, and 108 the first that
delivers when an event is written rather than when the clock says so.

`ef4d6b0` (the delivery loop no longer requires a separate federation address) is in the tree after
run 105's image and is **unmeasured**. It is expected to move no container number, because
complement's entrypoint always passes `-federation-addr :8448` and so the loop was already running in
every run above — which is exactly why the fault survived all of them. It matters to the default
single-listener deployment, not to this suite. Quote 88 / 95 as the current number, and re-sweep
before attributing any change to that commit.

### Still failing at run 108, and what each one needs

| test | the behaviour, not the symptom |
| --- | --- |
| `TestDeviceListUpdates` | inbound EDUs are parsed by nobody: `handleEDUs` reads nothing, so `m.device_list_update` produces no `device_lists.changed`, and cross-server `POST /keys/query` for remote users is not answered either (T706) |
| `TestSyncTimelineGap` | needs **outlier events** — persisting an event whose history is not yet connected — before a gapped timeline can be filled (T713) |
| `TestRestrictedRoomsLocalJoin` | a restricted join needs the server to evaluate the `m.room.join_rules` allow list and stamp `join_authorised_via_users_server` on the join event (T709, MSC3083) |
| `TestJumpToDateEndpoint` | needs `GET /_matrix/client/v1/rooms/{roomID}/timestamp_to_event` and the application-service read that the blueprint's set-up uses |
| `TestMediaWithoutFileName` | complement names the media origin by its **container host:port** (`host.containers.internal:1024`) while the server's name is `hs1:8008`; no listener of this server carries that host, so the media is not this server's to serve. Deliberately not chased — passing it would mean bending media-origin matching to a container artefact |
| `TestUnknownEndpoints` | wants `405` at `PUT /_matrix/key/v2/query`, the deprecated key notary. Passing it means registering a decoy route for an endpoint this server does not implement, which is a test-suite-shaped answer rather than a true one |

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
