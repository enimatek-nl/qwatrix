# Complement conformance status
## The measurement was wrong before this run: the suite is 106 tests, not 50

Complement's own push test leaves a goroutine running past the end of its parent test, and when
that goroutine fails the whole binary aborts — `panic: Fail in goroutine after
TestPushSync/Enabling_a_push_rule_wakes_up_an_incremental_/sync has completed`. Go still prints a
tidy `FAIL github.com/.../tests/csapi 242.921s` summary, which reads exactly like a finished run.
Every run recorded below therefore stopped around the 50th test and reported that as the suite,
while 56 tests — several mandatory for AC-5 (`TestTyping`, `TestLeakyTyping`,
`TestThreadedReceipts`, `TestSync`, `TestSyncFilter`, `TestRelations`, `TestRedact`, the key
uploads, the room-specific-name tests) — had never executed once.

`go test -list` is what found it: the package holds 106 top-level tests, the run reported 50.

`.agent-work/complement-chunks.sh` enumerates the package and runs it in chunks of 8, one process
per chunk, so one test's panic costs at most its own chunk, and a test with no result line is
reported as CRASH instead of disappearing. `.agent-work/complement-score.py` scores a run against
AC-5's mandatory list and diffs it against any earlier run.

| measurement | pass | fail | crash | mandatory passing |
| --- | --- | --- | --- | --- |
| baseline (`complement-baseline-106.txt`) | 45 | 55 | 6 | 27 / 69 |
| + read state, typing, account data, filters (`complement-run-54.txt`) | 54 | 46 | 6 | 34 / 69 |
| + profile-change member events (`complement-run-latest.txt`) | 56 | 44 | 6 | 34 / 69 |
| + room-scoped txn IDs, MSC4115, room summary, createRoom (`complement-run-64.txt`) | 64 | 36 | 6 | 42 / 69 |
| + E2E key endpoints (`complement-run-69.txt`) | 69 | 31 | 6 | 47 / 69 |
| + user directory, lazy loading, departed-member reads, pagination tokens (`complement-run-77.txt`) | 77 | 29 | 0 | 54 / 69 |
| + event relations, to-device messages (`complement-run-81.txt`) | 81 | 25 | 0 | 58 / 69 (84%) |
| + media repo, `/sync` leave section, `contains_url` (`complement-run-87.txt`) | 87 | 19 | 0 | 63 / 69 (91%) |
| + device lists, `/keys/changes`, threaded receipts (`complement-run-89.txt`) | **89** | 17 | 0 | **65 / 69 (94%)** |

Forty-four tests gained over the baseline, **zero regressions in any step**. AC-5's whole-suite
threshold of 70 is met, and the mandatory set is up to 58 of the 69 that exist here.

Two consequences. `TestDisplayNameUpdate` and `TestAvatarUrlUpdate`, named in M1's exit gate and
earlier believed not to exist in this checkout, exist and **fail** — so M1 was signed off
prematurely and is re-opened. And AC-5 names 15 tests this checkout does not contain
(`TestEventAuth`, `TestUnknownEndpoints`, `TestJumpToDateEndpoint`, `TestMediaFilenames`,
`TestRestrictedRooms*`, `TestClientSpacesSummary`, …), which has to be reconciled with the pinned
Complement version rather than counted either way.

Also corrected: the `-tags blacklist_common` I pass matches no build constraint (the convention is
`dendrite_blacklist` and friends), so every host-specific test does run against qwatrix — which is
what we want; `runtime.SkipIf` logs a warning and executes, so the zero skips are genuine.

The upstream suite, run against the container image. `.agent-work/COMPLEMENT-RESULTS.md`
holds the per-run evidence; this file is the current standing and the plan.

## Standing

Complement's `./tests/csapi` package holds **50 top-level tests** containing **189 subtests**
(the sytest-level checks). Both are reported because a top-level test can fail on one subtest
while its siblings pass, and the acceptance criteria speak of tests.

| run | image state | top-level | subtests | skips |
| --- | --- | --- | --- | --- |
| 1 | rooms + sync only (M0–M3) | 5 / 45 fail | not measured (`-v` off) | 0 |
| 2 | + capabilities, join, aliases, presence, body rules | 20 / 30 | — | 0 |
| 3 | run 2 with `-v` | 20 / 30 | 119 / 70 | 0 |
| 4 | + SQLite locking, numeric content rule, join content, `format=event`, profile cache | 23 / 27 | 130 / 59 | 0 |
| 5 | + state-typed `/send` accepted | **23 / 27** | **131 / 58** | 0 |

No run introduced a regression: `comm` over the sorted failure-name lists of consecutive runs is
empty every time.

## Why there are no skips

Complement gates host-specific tests with `runtime.SkipIf(t, runtime.Synapse)` and friends,
which need a `*_blacklist` build tag to know which server is running. Without one it cannot
identify qwatrix, runs every test, and records no skip. That matches the acceptance criterion
"zero skips" — and it means a host-specific test must actually pass here.

## Fixed in this round (each was a protocol error, not a missing feature)

| symptom | diagnosis |
| --- | --- |
| `500 M_UNKNOWN` on join, invite, state writes | `sqlite3: database is locked`. A deferred transaction reads before it writes, discovers the write lock at the `INSERT`, and SQLite refuses *immediately* — `busy_timeout` does not apply to that upgrade. Fixed by `_txlock=immediate` plus a bounded replay of rolled-back write transactions in `Store.Do`. |
| `POST /register` with an invalid username answered the UIA challenge | the specification, and Complement, require the body to be validated *before* the auth exchange: `400 M_INVALID_USERNAME` / `M_USER_IN_USE`. |
| `POST /register` with `user-UPPER` answered `400 M_INVALID_USERNAME` | registration down-cases the username; accounts are stored lowercase and the local part a client gets back is the folded one. |
| login with an uppercase user ID answered `403` | the identifier is folded before lookup — a wrong case on login is a typo, not a request to create an account. |
| `GET /register/available` answered `200 {"available": false}` | an unusable name is `400` with `M_INVALID_USERNAME` or `M_USER_IN_USE` in the error body. |
| `PUT /register` with a sessionless `auth` was accepted | an `auth` dict naming a stage must name a live session, else `401 M_SESSION_INVALID`; otherwise one registration can be paid for with another's completed stage. |
| `GET /profile/{user}/displayname` required a token | profile reads have been public since spec v1.1. |
| `PUT /state/m.room.canonical_alias/` (trailing slash) `404` | the state endpoints are now one `{tail...}` route: the empty state key has several spellings and a state key may contain a slash. |
| `POST /state`-style body rejected | a state write whose body *is* the content (rather than `{"content": …}`) is accepted, because that is what clients send and what Synapse tolerates. |
| a 70 KB event answered `400` | `413 M_TOO_LARGE`: the event size limit is a federation limit every server enforces, checked on the request body before parsing. |
| invalid UTF-8 in a JSON body was accepted | Go's decoder silently replaces it; the specification calls this `M_NOT_JSON`. |
| `[]` or `42` as event content answered `500` | content must be a JSON object: `400`. |
| `POST /rooms/{id}/forget` while joined answered `404` | `400` — the caller is still a member, which is a different problem from an unknown room. |
| `GET /rooms/{id}/aliases` was readable by strangers | it is a room read; a non-member gets `403`. |
| `DELETE /directory/room/{alias}` of an unknown alias answered `200` | `404 M_NOT_FOUND`. |
| `/publicRooms` chunk entries omitted `world_readable` when false | `omitempty` on a boolean made a required key disappear. |

## Endpoints added in this round

`GET /capabilities` (auth-required, reports `m.room_versions`, `m.change_password`,
`m.knockRestricted`), `POST /join/{roomIDOrAlias}`, `PUT`/`DELETE /directory/room/{alias}`,
`POST /publicRooms` with its filter body, and presence:
`GET`/`PUT /presence/{userID}/status` plus `?set_presence=` on `/sync`.

Presence is stored per local user and carries a stream position from the event stream, so a
client's ordinary `since` token decides which changes it has not been told, and it is delivered
only to users who share a room with the subject.

## Second round of protocol errors (each was a missing protocol feature, found by reading the test)

| what the test asked for | what qwatrix did | what the protocol says |
| --- | --- | --- |
| `POST /join/{room}` with `{"foo":"bar"}` | kept only `reason` | a join body **is** the member event's content: keys the server does not know are written to the event anyway, because the sender signed them. Only the fields the server owns (`membership`) are overridden. |
| `GET /state/m.room.member/{me}?format=event` | ignored `format` | `format=event` answers the **whole event** — sender, room_id, hashes — because a client that wants to know *who* set a state event cannot learn it from the content. |
| `PUT /profile/{me}/avatar_url` then `/joined_members` | showed the old avatar | `/joined_members` answers from cached profile columns; a cache is only allowed while it is kept true, so `store.Memberships.RefreshProfile` rewrites every non-forgotten row of the user on a profile change. |
| `GET /joined_members` for a user with no profile | omitted `avatar_url` | both profile keys are always present, `null` when unset — a key that appears only once a value exists cannot be told apart from a server that never answered. |
| `PUT /send/...` with `{"body": 1.1}` or `9007199254740992` | `M_INVALID_PARAM`, and accepted the number | `400 M_BAD_JSON`: event content numbers are restricted to integers an IEEE-754 double holds exactly, because a signature commits to bytes and another server cannot reproduce a number it cannot hold. |
| `GET /members` of a member event with a client key | trimmed content to `membership` | trimming is an optimisation no client asked for; `/members` answers full events unless a client names the extension, and a trimmed event still carries `type` and `sender`. |

`matrix.ValidateEventContent` implements the number rule by walking `json.Token`s — never
`interface{}` — because decoding through `interface{}` routes every number through `float64`,
which is the exact loss being detected.

## The largest single fix: a state-typed event is not a state event

Sixty-odd failures across a dozen tests traced back to one line of mine:

```go
if strings.HasPrefix(eventType, "m.room.") && isStateEventType(eventType) {
    return "", false, matrix.NewInvalidParam("%q is a state event; use the state endpoint", eventType)
}
```

The reasoning — a state event in the timeline would make a room whose timeline and state
disagree — is wrong, and wrong in the direction that breaks clients. **An event is state because
it carries a `state_key`, and because of nothing else.** The type only names the state
definition that *would* apply if there were a key. So `PUT /send/m.room.join_rules/{txn}` is a
perfectly ordinary timeline event that happens to share a name with a state type, and there is
nothing for the timeline and the state to disagree about. Complement's helpers use exactly this
to put an arbitrary event in a room, so refusing it cost every test that sends one.

Fixed by deleting the refusal. `TestFreshSyncFindsRecentEvent` now covers the shape: 25 messages
plus a state-typed `/send`, then a `/sync` with no `since` at all — the way a client looks for
the event it just sent — which must contain it.

## M1 closed: five more protocol errors, all invisible to a passing in-process suite

| what the protocol asks | what qwatrix did | the class of mistake |
| --- | --- | --- |
| `POST /logout` ends the session | revoked the access token, left the device row | half an operation. A token outliving its device is a session that is not in `/devices`, so the user cannot see it, name it, or tell it from one that was never signed out. |
| `logout_devices:false` means "change the password, change nothing else" | still revoked every other session | an overloaded sentinel (`keepDeviceID == ""`) could not express "keep them all", so the flag's `false` case collapsed into the default. |
| the acting session survives a password change | was signed out when the flag was named explicitly | my own test asserted it. Nothing in the spec says so; the promise is about *other* devices, and the caller just proved they know the new password. |
| `POST /login` honours `device_id` and `initial_device_display_name` | assigned the display name to `ClientID`, discarding it | copy-paste into the adjacent field — compiles, and no test read the field back. |
| a UIA challenge always carries `params` | omitted it when empty (`omitempty`) | an optional key that a client must be able to rely on. |
| `DELETE /devices/{id}` with no body is a 401 challenge with flows | answered 403 | refusing where the right answer is instructions: 403 says "you may not", 401 says "here is how". |

Reproduction note: complement's `DELETE` 403 could not be reproduced in-process at all, so it was
reproduced against the container with a small driver (`.agent-work/probe-devices.py`) — the image
that failed predated the device-ownership change, which is why the host-side probe answered 401.

## Third round: three more protocol errors in the room read path

| what the protocol asks | what qwatrix did | why it matters |
| --- | --- | --- |
| deleting an alias the room claims as canonical must clear the claim | deleted the alias, left `m.room.canonical_alias` naming it | the claim *is* an event, so only a new event can correct it. Leaving it points every client at a 404, and clients watch for an empty content object as the signal to drop the name they are showing. |
| forgetting a room ends read access | let a forgotten room's history page on under `shared` visibility | "forget" is the caller's own declaration that the room is no longer their business. A forget that hides the room from one device's list while the history stays one HTTP call away is a UI change sold as a data change. |
| …but a leave that predates a client's token must still reach that incremental `/sync` | dropped forgotten rooms from every `/sync` | a device whose token predates the leave then has no message that would ever remove the room: it keeps a room it cannot open, leave, or forget. Forgetting governs what a *new* sync sees. |
| `GET /event/{id}` answers `404` for what the reader may not see | answered `403 M_FORBIDDEN` | an event ID is a capability nobody handed the caller; `403` confirms the room and event exist, which is the very thing the endpoint must not leak. |
| under `joined`/`invited` visibility a member may not read what predates them | read it | joining a long-lived room handed a newcomer everything sent before their invite — precisely what the creator withheld. Enforced at read time, since the same event must become readable to someone who joins and stays. |

The bound for `invited` is the **invite**, not the join, and that position is nowhere on the
membership row — which stores only the *current* membership, so joining overwrites the invite.
It is recovered from the event stream instead (`Events.FirstPositionOfState`): events are never
deleted, so a user's earliest member event in a room is the invitation itself. Two tests pin the
difference from both sides (`TestInvitedVisibilityReadsFromTheInvite`), because a rule with three
settings and one boundary is exactly where an implementation quietly reads everything.

## Open, by category

* `TestLogout`, `TestChangePassword`, `TestDeviceManagement`: the device survives a password
  change in-process, but complement sees `M_UNKNOWN_TOKEN` on `whoami`, and `/devices` counts one
  device too many after a logout — so something about the UIA-authenticated password change is
  rotating or revoking more than the in-process test exercises.
* `TestMembershipOnEvents`, `TestFetchHistorical*`: read-reachability of events around an invite
  and after leaving, i.e. `history_visibility` edge cases.

**Later milestones, not bugs:** media (`TestContent*`, `TestMediaConfig`, `TestAsyncUpload`,
`TestRoomImageRoundtrip`), push (`TestPushRuleCacheHealth`, `TestChangePasswordPushers`),
E2E keys (`TestKeyChangesLocal`, `TestE2EKeyBackupReplaceRoomKeyRules`), account data and
ignored users (`TestAddAccountData`, `TestInviteFromIgnoredUsersDoesNotAppearInSync`), receipts
and read markers (`TestRoomReceipts`, `TestRoomReadMarkers`), `device_lists` in `/sync`
(`TestDeviceListUpdates`), search, server notices.

## Still failing, grouped

**Not implemented yet (later milestones):** media upload/download/config (M5), pushers and
push rules (M6), `POST /keys/upload` and key backup (M6), account data and ignored users (M6),
receipts / read markers / typing (M4), search and `/hierarchy` (growth), room upgrades and
knocking (M3 growth), `/_synapse/admin/v1/send_server_notice` (growth).

**To diagnose next:** `TestRoomState` (a state write that should be `403` is `200`),
`TestRoomCanonicalAlias` and `TestRoomDeleteAlias` (alias-into-canonical-alias flows),
`TestRoomMembers` (a `POST /rooms/{id}/join` that still fails), `TestRoomForget`
(re-invite flows), `TestDeviceListUpdates` (needs `device_lists` in `/sync`), `TestAddAccountData`.

## What each of the last four rounds added

| round | what was built | the reasoning worth keeping |
| --- | --- | --- |
| 64 | room-scoped transaction IDs, `unsigned.membership`, invited counts, createRoom topic | A retried write must answer with the original event, and a scope that is wrong in one direction loses events while being invisible in tests. |
| 69 | `/keys/upload`, `/keys/query`, `/keys/claim` | Device keys are stored byte-for-byte because a signature covers exact bytes; claims are FIFO because a one-time key handed to two senders is not one-time. |
| 77 | user directory, MSC1337 lazy loading, departed-member reads, pagination tokens | A directory indexes names per room so a private nickname cannot be searched by an outsider. Pagination had two silent bugs: `from` on a backward read must include the event it names, and `end` must be omitted at the end — the omission is the only stop signal a client has. |
| 81 | `/relations`, `/sendToDevice` | Relations needed the relation target denormalised onto the event row at write time; to-device delivery uses a server-side cursor because the sync token cannot name two streams without breaking `/messages`. |

### The measurement-integrity win

The push-rule round did not only add two tests: the six **crashes** were one panic in
`TestPushSync`'s goroutine — a sync that returned no `m.push_rules` reported failure into a `t` that
had already finished, killing the test *process* and taking four mandatory tests (`TestRedact`,
`TestRelations`, `TestRelationsPagination`, `TestRelationsPaginationSync`) with it as unmeasurable.
A crash is not a failure; it is the absence of a measurement, and it can only be found by reading the
whole log for what has no verdict line.

## Fourth round: media, relations, push rules, to-device, device lists, presence

| Measurement | Pass | Fail | Crash | Mandatory AC-5 |
| --- | --- | --- | --- | --- |
| `complement-run-81.txt` | 81 | 25 | 0 | 58 / 69 (84%) |
| `complement-run-87.txt` | 87 | 19 | 0 | 63 / 69 (91%) |
| `complement-run-89.txt` | **89** | 17 | 0 | **65 / 69 (94%)** |


Zero regressions in every run of this round. Features that landed between the 89-pass measurement and
the end of the session, each verified in-process and by targeted Complement runs: the device list
announcement on *join*, presence fan-out to shared rooms, an incremental `/sync` that omits unchanged
rooms, and "no such rule" for a push rule under an unknown kind.

### A measurement that was void, and why it looked like a catastrophe

Run `cc12` scored 41 passes with 48 regressions. It was not a regression: Podman caps the named
volumes it will lock at 2048, Complement leaks one per container, and the twelfth full sweep of the
session hit the ceiling — after which every `Deploy` fails with `allocation failed; exceeded num_locks`
and the verdict file records sixty failures that never happened. The runner now runs
`docker system prune -f --volumes` before each batch, and `complement-score.py` prints a warning
whenever a results file holds fewer than 100 verdicts, because a partial run diffed against a full
baseline reports every test it never reached as a regression.

### The ceiling that is not a bug

Two tests in the AC-5 mandatory list cannot pass without **federation**, because each runs both a
single-server and a two-server deployment in the same test function:

- `TestDeviceListUpdates` — its `hs2` subtests join a room on the *other* homeserver
  (`POST /join/…:hs2?server_name=hs2`), which answers `M_NOT_FOUND` because qwatrix does not federate.
- `TestThreadedReceipts` — the same shape at `thread_notifications_test.go:342`.

Both now pass their single-server subtests; neither can be reported as passing while the two-server
half fails. That leaves `TestSync` and `TestSyncTimelineGap` as the only mandatory tests still within
reach without federation, and both are listed below with what they still need.

### `TestSync`, subtest by subtest

`TestSync` is one test function over seven subtests, so it reports a single FAIL however many of them
pass. Reading it that way, this round took it from seven failing subtests to three:

| Subtest | Now | What it needed |
| --- | --- | --- |
| Can sync a joined room | passes | an incremental `/sync` must not report a room that has not changed |
| Newly joined room is included in an incremental sync | passes | the same, for a room that *did* change once |
| Newly joined room includes presence in incremental sync | passes | a member's presence must reach their co-members |
| Full state sync includes joined rooms | passed | — |
| Get presence for newly joined members … | fails | presence must also start when somebody *joins*, not only when they sync |
| Newly joined room has correct timeline in incremental sync | fails | under investigation |
| sync should succeed even if the token points to a redaction of an unknown event | fails | under investigation |
| Device list tracking / lazy loading | fails | federation half of `TestDeviceListUpdates`-shaped setup |

### Presence, and why a sync is what makes a person online

Presence is the one part of `/sync` that the server has to *initiate*. The rule qwatrix now applies is
that presence is delivered to the users who share a joined room with the subject — which is exactly the
boundary that lets `TestPresence` demand "alice must not be told bob is here" while `TestSync` demands
"alice must be told bob is here", with no contradiction: in the first they share no room.

Two triggers write the record, because the *record* is the event — writing it stamps a stream position,
and that position is what carries the news:

- the first `/sync` of a user with no presence at all marks them online;
- a local user whose `m.room.member` event reaches `join` is marked seen, even if they never sync
  again (a member who joins and stays quiet is still a member the room has met).

Neither overwrites a status the user chose: `MarkSeen` only ever writes when there is no row, so
"unavailable" and a status message survive any number of syncs.

The read side has two kinds of peer, which is the part that was wrong first. A peer whose presence
*changed* since the client's token is news in the ordinary way. A peer who merely became a peer — a
stranger who just joined — has a row older than that token, and a client that is never told about them
renders that person as unknown for as long as they keep quiet. So a room whose membership moved gets
its presence picture re-sent, which stops by itself once the client's token passes the join. The
alternative, stamping every co-member's row on every join, is a write per person per join in the worst
room in the deployment.

## 2026-11-11 — run 97: the complement package that had never been swept (`tests`, root)

**Why this run exists.** Every recorded sweep up to and including run 95 ran `./tests/csapi` only,
because AC-5 phrases its pass threshold as "≥ 70 of the 107 `tests/csapi` tests". Checking AC-5's
*mandatory named set* against the pinned checkout shows that was the wrong scope: 15 of the 95 mandatory
tests live in the root `tests` package, and had never been run at all. The threshold and the mandatory
set are different requirements, and only satisfying the first is how this stayed hidden.

**Command.** `COMPLEMENT_BASE_IMAGE=localhost/qwatrix-complement:latest COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT=host.containers.internal go test -tags blacklist_common -count=1 -timeout 2350s -v ./tests` (image from the code at `3ae8c5d`).

**Result: incomplete — 33 tests decided, 6 passing, then the package aborted.**

```
--- FAIL: TestSendJoinPartialStateResponse (3.69s)
panic: runtime error: index out of range [0] with length 0 [recovered, repanicked]
FAIL	github.com/matrix-org/complement/tests	294.349s
```

The panic is inside complement's test binary — it indexes into something this server's `send_join`
answer did not provide — and a panic there ends the whole package, so 59 tests never ran. Results are in
`.agent-work/complement-run-97-root-package-partial.txt`.

**Mandatory tests this run touched** (`TestWriteMDirectAccountData` passed; `TestContentMediaV1`,
`TestEventAuth`, `TestUnbanViaInvite` failed; **11 mandatory tests still have never been run** because of
the abort). Against run 95, AC-5's mandatory set therefore stands at **79 passing, 5 failing, 11
unmeasured** — not the "2 failing" the csapi-only sweep implied.

**What the failures look like, honestly:** `TestEventAuth` is `GET /_matrix/federation/v1/event_auth`
(T704, open) and `TestUnbanViaInvite` needs co-signed federation invites (T714, open) — both are known
gaps, not new breakage. `TestContentMediaV1` and the media-name tests are not on any open task and need
diagnosis. The rest of the 27 failures are deep federation (`/missing_events`, event-size limits,
corrupted auth chains, `send_leave` v1/v2 rejection), which is the deferred-depth list.

**Action:** the sweep script must cover `./tests ./tests/csapi`, and the sweep must be split so one
panicking test cannot hide 59 others.
