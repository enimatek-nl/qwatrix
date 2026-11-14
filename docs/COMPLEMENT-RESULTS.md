# Upstream complement results

The evidence behind the complement claims in [`FINAL-REPORT.md`](FINAL-REPORT.md) and this file's own
summary: every number below comes from a run against the container image, so runs are comparable —
same suite, same base image, same `COMPLEMENT_SERVER_TRACEBACK=true`.

Since run 97 the measurement is `scripts/complement-sweep.sh <dir>`, which sweeps **both**
packages (`./tests/csapi` and the server-server `./tests`), re-runs any test left without a verdict
one per process before calling it CRASH, merges `results.txt`, and scores AC-5 with
`scripts/ac5-check.py`, which reads the mandatory test names out of
[`ACCEPTANCE-CRITERIA.md`](ACCEPTANCE-CRITERIA.md) so the requirement list cannot drift from the measurement. Runs
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

## What one test took

`TestJumpToDateEndpoint` was the last mandatory test standing, and its thirteen subtests needed four
features between them: application services (only a service may stamp an event with a chosen `?ts=`, which
is how the test places events at exact milliseconds), the client reads `/timestamp_to_event` and
`/context`, those same reads over **federation**, and somewhere to keep a history this server was never
told about — detached, so it moves no state and reaches no `/sync`, and page-able, because that is the only
reason to have fetched it. Its run 115 row is the first in this file where the mandatory column is empty.

## What the remaining failures are

Sixty-nine named tests still fail while the mandatory set passes. They group, and the groups are the
roadmap: knocking (4 — relaying a knock to the other servers in the room, and letting a user invited
to another server's `knock` room join through their own), a restricted join authorised *for* another server (7), proposed MSCs 4289, 4291,
4297 and 4311 (16), relaying presence, typing and device-list updates over federation (3), client features
with no server half — search, URL preview, threads and thread receipts, push rules across upgrades, server
notices, async uploads, room members at a point in time, key-backup room-key rules, transactions across a
refresh (about 20) — and a tail of federation edge cases: unverifiable and rejected auth chains,
partition ordering, partial state in `send_join`, unicode in remote alias names, `is_direct` flags, ACLs.

Nothing in that list is a broken room, a lost message or a lost membership. They are features not built,
in a specification considerably larger than this server, and the mandatory set is what says so.

## The runs, most recent first

`<img>` names the commit the container image was built from, which is the only thing a run can
honestly be attributed to: a run measures the image that exists when its containers start, not the
commit that was HEAD when somebody typed the command. Run 101 is listed and its number is not used —
it was measured against a stale image and re-running it was never finished, so its count is discarded
rather than quoted.

| run | image at | mandatory passing | what the run turned on |
| --- | --- | --- | --- |
| 123 | `b00114d` | 2026-11-14 | **95 / 95** | **0** | **0** | **0** | **128 / 197** — `TestCannotSendNonKnockViaSendKnock`, `TestCannotSendKnockViaSendKnockInMSC3787Room`, `TestKnockRoomsInPublicRoomsDirectory` and `TestKnockRoomsInPublicRoomsDirectoryInMSC3787Room` FAIL → PASS, nothing mandatory regressed | **The first run after knocking, and the first after a knock stopped being room state.** The four tests above are what knocking measures upstream: `send_knock` answers a wrong membership with 400, and a `knock`-rule room appears in the public rooms directory. `TestKnocking` and its MSC3787 twin stay red on two named federated assertions (T806: the knock is not yet relayed to the room's other servers; T807: a user invited to another server's `knock` room is refused when joining through their own), and every subtest a single homeserver can check now passes — which is what the out-of-band membership change bought. That change touched the core event write, so this run's value is mostly negative: 95 of 95 mandatory tests still pass when a knock is stored outside the room's state. The net is +2 of 197 against four names newly passing; the name-for-name diff could not be completed because run 121's per-test list lived under `/tmp` and a host reboot took it, so this run and every later one is archived at `.agent-work/complement-run<run>.txt` instead. |
| 121 | `06e1573` + docs | 2026-11-13 | **95 / 95** | **0** | **0** | **0** | **126 / 197** — `TestCannotSendNonJoinViaSendJoinV1`, `TestCannotSendNonJoinViaSendJoinV2`, `TestCannotSendNonLeaveViaSendLeaveV1`, `TestCannotSendNonLeaveViaSendLeaveV2` FAIL → PASS (24 subtests between them), nothing regressed | **The four membership-validation tests turned on the status code alone.** They ask that a malformed event be answered 400, and this server answered 403 for a `m.room.member` with no state key and for a `state_key` naming somebody other than the sender — because signature verification, which answers 403, ran first and because the room server treats a mismatched state key as a question of authority. The doors now read shape before signatures: `membershipShapeProblem` in `internal/fedapi/membershipshape.go` answers 400 for what no signature could rescue, and 403 stays for standing (a sender whose domain is not the server that signed, an event ID or room that does not match the path). This image also carries `make_leave` and the `get_missing_events` answer, which complement measures nothing else: `TestInboundCanReturnMissingEvents` now reaches its per-event assertions instead of its 404, and still fails on one positional expectation — its expected gap list contains no `m.room.guest_access` and ours does (T754) |
| 119 | `f7ea442` | **95 / 95** | Confirms the outbound half of the key work: a client of this server can now claim a one-time key from a device on another homeserver — a claim that used to go to the *local* store and answer `{}`, which is indistinguishable from an exhausted device, so nothing looked broken — and an unreachable server is named in `failures`. Verdict-for-verdict identical to run 118, which is the expected result: no upstream test crosses that particular border, so the measurement here is that nothing else moved. |
| 118 | `8454ed5` | **95 / 95** | **`TestToDeviceMessagesOverFederation` passes, all three subtests** — to-device messages now cross the federation border as the `m.direct_to_device` EDU, deduplicated on (origin, `message_id`) so a retried key share is delivered once, and queued in the federation ledger, which is what the third subtest measures: it stops the receiving server, restarts the *sender* as well, and expects the key to arrive anyway. The merged pass count moves from 121 to 122 and nothing else changes, run for run. (Run 117 consumed a number without producing a result: the scratch wrapper it was launched from was no longer on disk, so the sweep never started. `scripts/complement-sweep.sh` is the runner.) |
| 116 | `32f3059` | **95 / 95** | Confirms the count after the last unguarded read of room contents was closed: `GET /event_auth/{roomID}/{eventID}` now applies the residency rule every other federation read already did, with the membership-about-your-own-user exception that lets a peer check a join it was handed. Nothing moved, which is what a change to a read nobody in the suite uses should look like | **95 / 95** | none — AC-5 met at run 115 and held here |
| 115 | `7110cfd` | **95 / 95** | **`TestJumpToDateEndpoint` passes, all thirteen subtests** — application services (so events can be placed at chosen milliseconds), the served federation reads, and a joining server that asks a resident peer where its room was and keeps the answer detached. `TestJoinFederatedRoomFromApplicationServiceBridgeUser` also passes outright. No mandatory test is left failing: AC-5 is met, with 121 of 197 recorded verdicts overall | **94 / 95** | `TestJumpToDateEndpoint` (its five federation subtests) |
| 113 | `2796643` | **94 / 95** | `TestSyncTimelineGap` passes, both subtests — a fifty-event gap is now bridged by walking it from its far end. One mandatory test remains, and it waits behind a feature the criteria defer (see below) | **93 / 95** | `TestRestrictedRoomsLocalJoin` passes — a `restricted` room now admits the members its `allow` entry names, on the strength of an authorisation this server signs. It had failed in every run since the suite was first run | **92 / 95** | `TestDeviceListUpdates` passes outright — all ten subtests, first time. It needed device-list changes to cross the border in both directions, and a membership event to be read as two announcements instead of one |
| 114 | `9e69c36` | **94 / 95** | Confirms the count after two new history reads (`/timestamp_to_event`, `/context`). No mandatory test moved. `TestJumpToDateEndpoint` went from refusing everything to **six of its subtests passing** — complement scores a parent by all of its subtests, so the verdict is unchanged and the progress is real |
| 110 | `2d22829` | **91 / 95** | unchanged at the top level, but `TestDeviceListUpdates/when_local_user_leaves_a_room` began passing once device-list changes stopped reaching people who had stopped sharing a room |
| 109 | `1bcfefb` | **91 / 95** | `TestUnknownEndpoints` and `TestMediaWithoutFileName` pass: the first needed an endpoint that had never been served, the second needed this server to ask peers by paths peers actually speak |
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

### Still failing at run 113, and what each one needs

| test | the behaviour, not the symptom |
| --- | --- |
| `TestJumpToDateEndpoint` | needs `GET /_matrix/client/v1/rooms/{roomID}/timestamp_to_event` and the application-service read that the blueprint's set-up uses |

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
