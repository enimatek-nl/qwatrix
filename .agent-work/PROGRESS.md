# Progress

## Where the project stands (2025-11-09, evening)

A Matrix homeserver that runs, deploys as a container, and passes upstream Complement.

| gate | status | command |
| --- | --- | --- |
| builds, vets, formatted | ✅ silent | `make build vet fmt` |
| in-process API suite (`tests/api`, `tests/unit`) | ✅ green, incl. `-race` | `make race` |
| container contract (`tests/complement`) | ✅ 5/5 | `make complement-smoke` |
| upstream Complement `./tests/csapi` | 32/50 top-level, 149/189 subtests, 0 skips | `make complement` |
| endpoint documentation matches the code | ✅ both directions | `go test ./tests/api -run Documentation` |

**M1 (accounts, sessions, devices) is signed off**: its ten Complement exit-gate tests pass
against the container with zero skips. Closing it took five protocol errors that every in-process
test had walked past, because each test asserted what the server did rather than what the protocol
asks (see `.agent-work/COMPLEMENT-STATUS.md`).

Implemented so far: the client-server skeleton (mux, error contract, auth middleware,
user-interactive auth), accounts (registration through both the standard and the Synapse
shared-secret paths, login, logout, devices, password change, deactivation, profiles), the
event domain (GMSL-backed event service, signing identity, room version policy), the room
server (creation, sends, state, membership, redaction, aliases, directory, pagination,
read-reachability), `/sync` with long-polling, and presence.

Not yet: media, push, E2E key support, receipts/typing, account data, search, federation,
room upgrades, knocking.

## The lesson of the evening: run the real suite early and often

The first upstream run scored 5 of 50 top-level tests. Reading the failures rather than the code found fifteen
protocol errors that every in-process test agreed with, because the in-process tests encoded
the same wrong assumptions:

* **Validate the request before authenticating it.** Complement says so out loud: a client
  that sent a bad username wants to hear about the username, not to be asked to authenticate.
* **Accept what clients send, not only what the specification writes.** A state write whose
  body *is* the content, an alias endpoint spelled two ways, an empty state key with a trailing
  slash, an uppercase user ID at the login form — every one of these was a working
  interoperability question that my own tests had answered the strict-and-wrong way.
* **Some answers are required, not stylistic.** `413 M_TOO_LARGE` for an oversized event,
  `M_NOT_JSON` for invalid UTF-8, `404` for deleting an alias that is not there, a
  `world_readable` key that `omitempty` had deleted.
* **`GET /capabilities` blocks five tests.** A missing cheap endpoint costs more than a
  correct-but-unreachable feature.

## The bug worth remembering: `sqlite3: database is locked`

`500 M_UNKNOWN` on ordinary joins and invites under Complement's parallel subtests, and never
in-process. The transaction was opened the default deferred way: it took no lock at `BEGIN`,
read (read lock), then discovered it needed the write lock at the first `INSERT` — at which
point SQLite refuses **immediately** because waiting could not help, and `busy_timeout` never
applies. `500`s on requests with nothing wrong with them.

Fixed in two layers: `_txlock=immediate` so a writer takes the write lock where waiting works,
and a bounded replay in `Store.Do` of a write transaction the database refused (a rolled-back
write is safe to repeat; an error the caller returned is not, and is returned untouched).
`tests/unit/store_concurrency_test.go` drives the concurrent shape.

## How the endpoint document stays honest

`docs/ENDPOINTS.md` is checked against the source by `tests/api/endpoint_coverage_test.go`,
which parses the routes out of the registration calls with `go/ast` and compares them with the
markdown tables in both directions. Verified by injection: adding an undocumented route fails
the suite, and so does documenting one the server dropped. It earns its keep — it caught four
undocumented routes and two stale rows during this round.

## The largest single fix, and the kind of comment that causes them

```go
if isStateEventType(eventType) { return …, "is a state event; use the state endpoint" }
```

An event is state because it carries a `state_key` and because of nothing else; the type only
names the definition that applies *if* there is a key. So a state-typed event sent through
`/send` is an ordinary timeline event, and refusing it broke every test that puts an arbitrary
event in a room. The comment justified it with a worry about the timeline and state disagreeing —
which cannot happen, for exactly the reason the refusal was wrong.

## The second lesson: a failing test names a protocol feature, not a bug

The next batch each turned out to be a protocol capability missing outright, discoverable only by
reading Complement's assertion:

* **a join body is the member event's content** — keys the server has never heard of are written
  anyway, because the sender put them there and signed them;
* **`?format=event` on a state read** answers the whole event, because "who set this?" is not a
  question content can answer;
* **event content numbers must be integers an IEEE-754 double holds exactly**, since a signature
  commits to bytes — so `1.1` and `2^53` are `M_BAD_JSON`, not rounding suggestions;
* **a cached profile must be kept true**, or the cache is not allowed: `/joined_members` answers
  from cached columns, so a profile change rewrites them;
* **a required key that is absent is a wrong answer** — `avatar_url: null` is information, a
  missing `avatar_url` is a shrug.

## Read state, typing, and filters (2026-11-10b)

Measured against the whole suite for the first time (see the measurement correction above):
**45 → 54 passing, 9 newly passing, 0 regressions** (`complement-run-54.txt`). Mandatory AC-5
tests in this checkout: 27 → 34 of 69.

What was built, and the reasoning that shaped it:

* **Typing** (`PUT /rooms/{id}/typing/{userID}`). Emitted **on change, not on every sync.** The
  first implementation put an `m.typing` in every joined room of every sync, which is the only way
  a client can be told typing *stopped* — and it broke `TestSync`, because a room carrying an
  ephemeral event is a room with something new, so no room was ever omitted from an incremental
  sync again. Typing now carries a position borrowed from the end of the event stream, so one
  `since` token orders history, presence, and typing together. The indicator is first-person only,
  and a non-member receives nothing about a room they are not in.
* **Receipts and read markers.** `m.receipt` is keyed by event ID then receipt type. `m.fully_read`
  is *room account data*, not a receipt — it is the reader's own bookmark and is never shown to
  anybody else — while `POST /read_markers` moves both, because clients send them together and a
  room with two read positions has two answers to one question.
* **Account data**, global and per-room. Both spellings of the room path are served: the current
  spec path and the older `/user/{id}/rooms/{roomID}/account_data/{type}` that clients written
  against an earlier draft send. A 404 on the older one is not a declined deprecation, it is a
  client that has lost its data.
* **Saved filters** (`POST`/`GET /user/{id}/filter`, and `?filter=` accepting an ID or inline
  JSON). Two decisions: a filter is stored and returned **verbatim**, because a filter that changes
  by being saved is a client configuration silently drifting; and the filter is applied **before**
  the timeline window is cut, because cutting first gives the client whichever events happened to
  be newest, filtered down afterwards, with `limited` reporting a truncation that never happened.
* **A profile change now re-announces the user in their rooms** as a new `m.room.member` event.
  This was previously decided against — the old comment argued that peers could re-read
  `/profile` — and that argument was wrong: the peers looking at the stale name are exactly the
  ones who will not ask. A member event whose name differs from the *previous* global name is left
  alone, because that is a name chosen for one room and a global rename must not overwrite it.

Two bugs found by writing the tests rather than the code:

* `Save()` on a composite primary key containing a legitimate empty string (global account data
  has no room; an unthreaded receipt has no thread) is treated by GORM as "no key yet" and becomes
  an INSERT — so the second write of any account-data key answered 500 `duplicated key not
  allowed`. Fixed with an explicit upsert.
* The NULL-aggregate trap again: `MAX(...)` over no rows is NULL and scanning that into a Go
  `string` fails the query, so creating an account's *first* filter was a 500. `COALESCE` and an
  `int64`, as everywhere else in this codebase.

`TestDisplayNameUpdate` and `TestAvatarUrlUpdate` — in M1's exit gate, and previously believed not
to exist — were failing all along and now pass, so M1 is signed off again on evidence rather than
on a run that stopped half-way.

## Keys, directory, history, and pagination (2026-11-10c)

Measured runs of the whole suite, each diffed against the one before for regressions:

| measurement | pass | mandatory passing |
| --- | --- | --- |
| `complement-run-54.txt` | 54 | 34 / 69 |
| `complement-run-64.txt` | 64 | 42 / 69 |
| `complement-run-69.txt` | 69 | 47 / 69 |

Zero regressions in every step. What was built and why:

* **Device and one-time keys** (`/keys/upload`, `/keys/query`, `/keys/claim`). Documents are
  stored as the client wrote them, because a device key is signed over its exact bytes — a server
  that "tidied" one hands out a key that fails to verify, and the failure surfaces as somebody's
  colleague's client refusing to talk to them. Claims go out oldest-first and delete what they
  hand over: a one-time key given to two senders is not one-time.
* **The user directory is a list of names, not of accounts.** Each row carries the room it came
  from, so a name said in a private room is searchable only by people who were in it. Indexing
  per-room display names into one field would hand a private disclosure to anybody who typed it.
* **A departed member reads the room they left** (SPEC-216). `history_visibility: joined` says
  "anybody who was in here may read that stretch", so leaving narrows the answer rather than
  closing the door: state and members as of the leave, messages up to the leave. The current-state
  caches cannot answer that, so the state is rebuilt by replaying the room's own state events —
  the events are the history, and nothing here deletes them.
* **Transaction IDs are scoped to the room as well as the device**, which required a primary-key
  change and therefore an explicit SQLite rebuild (rename, create, copy, drop) since GORM will not
  alter constraints. The old table is renamed rather than deleted so a failure leaves the data
  reachable.
* **Pagination tokens were wrong in both directions.** `from` on a backward read now *includes*
  the event it names — a client's token is a sync token, which names the newest event it holds, so
  excluding it lost one event per page and the client could not notice — and `end` is now omitted
  once there is nowhere left to go, which is the specification's only stop signal. `end` is the
  continuation token, so a backward page ends one below its oldest event and no event is served
  twice.

Three measurements worth keeping:

* Complement's `TestRoomCreationReportsEventsToMyself` pins that **writing identical state returns
  the same event ID** — an idempotent operation must not grow the room. That rule now lives in the
  single append path, so retried joins and re-asserted topics are no-ops everywhere.
* `TestRoomSummary` pins that `m.invited_member_count` comes back **down**: an accepted invitation
  that still shows as outstanding is a client offering a decision already made.
* `TestMembershipOnEvents` pins MSC4115's `unsigned.membership`, which is the **reader's**
  membership at the point of each event, not the sender's — the question is "could I have seen
  this at the time".

## Relations, media, push rules, and to-device (2026-11-10d)

Measured: 81 pass, 58/69 mandatory, zero regressions (`complement-run-81.txt`).

* **Push rules** ship defaults, are delivered as `m.push_rules` account data, and a rule write stamps
  the account so a waiting `/sync` wakes. Seeding happens before the *first read of a single rule*,
  not merely before a listing — otherwise the first thing an account does, "overwrite
  `.m.rule.master`", finds nothing to protect and creates a server-owned rule.
* **Event relations** are queried, not scanned: the relation target is copied out of the event
  content when the event is written. A redaction does not clear it, because "what somebody replied
  to" survives the message it pointed at.
* **To-device messages** are delivered against a server-side per-device cursor rather than the
  client's token. Doing it the specification's way needs a token that names two streams at once, and
  these tokens are shared with `/messages` and `/context` as plain integers — every one of those
  readers would have to learn a second syntax to buy a redelivery guarantee. The deviation is
  documented at `store.Device.ToDevicePos`: a device that receives a key and then loses it is not
  handed it again.
* **Media** stores bytes on disk and metadata in the database, writing bytes first and the row second
  so a row never names a file that is not there. The uploaded content type is served verbatim —
  because that is what makes an image render — *with* `Content-Security-Policy: default-src 'none'`
  and `X-Content-Type-Options: nosniff`, because a content type an uploader chose is otherwise a
  script that runs on this origin when somebody opens the URL in a browser.
* **The `/sync` leave section** reports a room only while the leave is news, and shows the room as it
  stood when the reader left. Reporting it on every incremental sync for as long as other people keep
  talking in it makes a client re-learn a fact it already acted on.

Two rules learned about patching with scripts: a struct field renamed by `gofmt`'s realignment will
silently miss a later exact-match replacement, and a repository field added to the `Store` struct but
not to its constructor compiles fine and panics on first use with a nil dereference.

## What is left, from a full measurement

46 failures and 6 crashes out of 106 (`.agent-work/complement-run-54.txt`, and
`complement-score.py` regenerates this grouping from any run). Grouped by what they actually need:

| area | count | mandatory | note |
| --- | --- | --- | --- |
| media (upload, download, config, thumbnails) | 7 | yes | milestone M5 |
| E2E keys (upload, claim, query, backup) | 5 | yes | milestone M4 |
| `/sync` internals (`device_lists`, timeline gap, presence timing) | 4 | yes | `TestSync`, `TestSyncTimelineGap` |
| room versions and create semantics | 3 | yes | `TestRoomCreate` |
| kick/ban/ban-power validation | 3 | yes | small, spec-pinned |
| push rules (needed before threaded receipts) | 2 | yes | milestone M6 |
| user directory | 2 | yes | needs `T310` (done) plus `T312` |
| lazy loading | 2 | yes | `lazy_load_members`, `T311` |
| relations, threads, jump-to-date | 4 | partly | milestone M7 |
| to-device, search, server notices, ignored users, txn idempotency, history paging | 8 | partly | |

The 6 crashes are not failures of their own: they are tests that shared a process with
`TestPushSync`, which panics in a leaked goroutine and takes the process with it. The runner
isolates them into single-test chunks so their real status is measurable.

**AC-5 names 15 tests this pinned Complement does not contain** (`TestEventAuth`,
`TestUnknownEndpoints`, `TestJumpToDateEndpoint`, `TestMediaFilenames`, `TestRestrictedRooms*`,
`TestClientSpacesSummary`, `TestUnbanViaInvite`, …). AC-5's threshold has to be read against the
suite that exists — currently 106 tests — or the criterion cannot be evaluated honestly.

## Next

1. Finish diagnosing the 30 remaining Complement failures (`.agent-work/COMPLEMENT-STATUS.md`
   has the grouped list), starting with the ones that are protocol errors rather than missing
   milestones: `TestRoomState`, `TestRoomCanonicalAlias`, `TestRoomDeleteAlias`,
   `TestRoomMembers`, `TestRoomForget`.
2. M4: typing, receipts and read markers — several failing tests wait on those, and `/sync`
   already has the ephemeral machinery to carry them.
3. M5: media (upload, download, config, thumbnails) — five tests, and the first thing a normal
   client notices.

## Device lists, presence, and the difference between a test and a subtest (2026-11-10e)

Measured: 89 pass, 17 fail, 0 crash of 106; 65 of the 69 mandatory AC-5 tests present in the pinned
Complement checkout (94%); zero regressions (`complement-run-89.txt`, re-measured identically in
`complement-run-89b.txt`). The ladder is 45, 54, 56, 64, 69, 77, 81, 87, 89.

### What landed

- **Media repository** (`internal/store/media.go`, `internal/media`, `internal/csapi/media.go`):
  upload, legacy and authenticated download, config, `contains_url` on `/messages`.
- **Event relations** — the three `/relations/…` routes with pagination in both directions.
- **Push rules** — all five kinds, defaults seeded per account, `/sync` delivery, and the two
  status-code decisions that turned out to be features: a rule under an unknown *kind* answers
  **404 "no such rule"** rather than 400, because clients probe for rules a server may not ship and
  treat 404 as the ordinary negative.
- **To-device messages** — `PUT /sendToDevice/{eventType}/{txnID}`, delivered through `/sync`'s
  `to_device` section with a per-device server-side cursor.
- **Device list changes** — `device_lists.changed` / `.left` on `/sync` and
  `GET /keys/changes?from=&to=`.
- **Presence fan-out** to shared rooms, started by a sync or by a join.
- **An incremental `/sync` omits rooms that have not changed**, unless `full_state` is set.

### The bug worth remembering

`TestKeyChangesLocal` passed while `TestDeviceListUpdates` failed, which pointed at a missing case
rather than a missing feature: the room server announced a membership change to the users who care,
and it worked out who cares by asking which rooms the subject belongs to — **before** writing the
membership that made them a member. A join therefore announced itself to nobody, and every test that
uploaded a key first (so a later, non-join announcement covered it) stayed green. The fix was to move
one call below the `Memberships.Upsert`, with the reason in a comment: the one event that most needs
announcing is the one that has not happened yet when the announcement is written.

Presence has the mirror-image problem. Marking a user online when they sync tells the room about
somebody who then never moves again; so a join marks them too. But the reader side also has two kinds
of peer, and treating them alike is what made a late joiner blind: a peer whose presence *changed*
since the client's token is ordinary news, while a peer who merely *became* a peer has a row older
than that token and would never be reported. A room whose membership has moved gets its presence
picture re-sent — self-limiting, because it stops when the client's token passes the join — which
avoids the alternative of stamping every co-member's row on every join.

### Two mandatory tests that federation owns

`TestDeviceListUpdates` and `TestThreadedReceipts` each run a two-server deployment inside the same
test function, joining a room on `hs2`, which answers `M_NOT_FOUND` because qwatrix does not federate.
Their single-server halves now pass; the tests themselves cannot. `TestSync` and `TestSyncTimelineGap`
are the remaining mandatory tests reachable without federation.

### On measuring with subtests

`TestSync` is one test over seven subtests, so the verdict file prints one FAIL whether one or six of
them fail. This round took it from seven failing subtests to three; the count of passing *tests* did
not move, and would not have moved for two more fixes. Any future round working in that area should
read the subtest lines, not the tally — and note that `TestSync`'s presence expectations require
presence to originate from the server, which no in-process test of my own had ever asked for.

## M7 stage 1: the federation surface begins (2026-11-11)

Two local `/sync` bugs first, both found by reading subtest output rather than the pass count:

- **`limited` on a room joined after the token.** A room the client has only just been allowed into
  cannot be delivered whole, and `limited` is the flag that makes a client page back instead of
  concluding it has seen everything. Complement's
  `TestSync/parallel/Newly joined room has correct timeline in incremental sync` was the reporter.
- **Presence and a peer with several rooms.** A peer's presence row can be older than the reader's
  token while the *room* is news: a stranger who walked in has not changed, but the client has
  become entitled to know them. The classification was being taken from whichever shared room was
  examined first, so a reader sharing an old room and a new one with the same person lost the news.
  `TestAPeerIsMeasuredByEveryRoomYouShare` pins it.

`TestSync` is now down to one failing subtest — the one whose own comment says the event "has to be
poked in from a federated server". Three of the four remaining mandatory-listed names
(`TestSync`'s redaction subtest, `TestSyncTimelineGap`, `TestDeviceListUpdates`,
`TestThreadedReceipts`) need the server-server API, which is milestone M7 rather than a bug.

### M7 stage 1, and why federation starts with keys

`internal/fedapi` now serves the two things a peer needs before it can believe anything: what
implementation this is (`GET /_matrix/federation/v1/version`) and the signing key that every event
will be checked against (`GET /_matrix/key/v2/server`, plus lookup by key ID). The key response is
signed by the key it publishes, so a peer has to believe nothing but the TLS connection it opened.

Supporting pieces:

- `Config.FederationTLS` — a configured certificate pair is used as given; otherwise a self-signed
  one naming the server is minted in the data directory. Peers will reject a self-signed leaf, which
  is the right outcome: an untrusted certificate is a refusal, and federation should refuse rather
  than pretend.
- `Config.Validate` no longer refuses `-federation-addr`, and the Complement entrypoint now turns
  the listener on by default — a closed port answers a peer's first question with a connection
  reset, which is a worse report than an unimplemented route.
- The docs-drift test now extracts routes from `internal/fedapi` as well as `internal/csapi`, so
  federation endpoints cannot drift from `docs/ENDPOINTS.md` any more quietly than client ones can.

Verified: three new in-process tests (`TestFederationVersionIsAdvertised`,
`TestFederationKeysArePublishedAndSigned` — including verifying the published signature with the
published key, `TestFederationKeyCanBeFetchedByID`), the whole in-process suite green, and
complement's own `TestInboundFederationKeys` **passes** against a real container, over TLS with
complement's CA, with its signature checks — the first federation test in the upstream suite to pass.
`TestVersionStructure` and `TestRegistration` were re-run in the same sweep and pass.

## M7 stage 2a: believing a peer (2026-11-11b)

`internal/fedapi` grew its outbound half:

- `Peer` — signed federation requests (`fclient`) over a transport with three certificate stances:
  verify against the system pool, additionally trust `-ca-cert`, or explicitly
  `-federation-insecure-skip-tls-verify` for test networks where peers present self-signed
  certificates. The last one logs a warning at start-up because it removes the anchor that makes a
  key response more than a claim.
- `keyCache` + `notaryFetch` — peer signing keys are fetched from the peer that signed the document
  (the origin, not a notary), self-verified, and cached until the peer's own `valid_until_ts`.
  `NewKeyRing` wires them into gomatrixserverlib's verifier.
- `VerifyRequest` builds a signature check with `StrictValiditySignatureCheck` attached by
  construction. That is not decoration: gomatrixserverlib *calls* the validity function, so a request
  built without it panics — and the function is the specification's rule that a key stops counting
  seven days after publication, which is what limits how far a stolen key can reach back.

### A security claim that was wrong before it was written down

The first version of this code carried a comment saying the self-signature on a key response is "what
stops a man in the middle from answering here is my signing key with a key of his own". The test for
that claim failed, and it failed correctly: an attacker who can answer the request can invent a
fresh key pair, publish its public half, sign the response with its private half, and pass every
check in the document. Self-signature proves **possession**, not **identity**. Identity comes from
the TLS certificate on the connection the answer arrived over — which is precisely why skipping
certificate verification is an explicitly labelled, loudly logged escape hatch rather than a default,
and why `CheckKeyResponse`'s comment now states the boundary instead of overselling the check.

What the check does earn: a response that publishes a key the responder does not hold, or that names
a server nobody asked about, or that publishes nothing at all, is refused rather than cached.

### Verified

`TestFederationBelievesAPeerThatSignsForItself`,
`TestFederationRefusesAKeyResponseThatDoesNotProvePossession`,
`TestFederationRefusesAPeerNamingAnotherServer`, `TestFederationRefusesAKeyWithNoKeysAtAll`,
`TestFederationCachesPeerKeys` (three verifications, one fetch), plus the three stage-1 endpoint
tests: all green, with `go vet` clean and `go test -count=1 ./tests/...` passing.

## M7 stage 2b: `make_join`, interoperating for real (2026-11-11c)

`GET /_matrix/federation/v1/make_join/{roomID}/{userID}` is served, backed by
`roomserver.Service.MakeJoinTemplate`, which answers the three things only this server can know:
where the room's DAG ends (previous event and depth), which events authorise this user joining (the
specification's table: create, join rules, power levels if present, and the user's own prior
membership), and which room version the event has to be built to.

Decisions worth keeping:

- **`sender` is dropped from the template when empty.** An empty sender and no sender are different
  statements, and a peer reading the template as a draft should not have to know which one this
  server means. `origin` and `origin_server_ts` are likewise left out: the peer signs as itself, on
  its own clock — that is the point of the handshake.
- **An unknown room is answered exactly like an unjoinable one** (`403 M_FORBIDDEN`), so `make_join`
  cannot be walked over candidate room IDs to find out which ones exist here.
- **A version the peer cannot speak is answered with the version the room is**, in the body
  (`M_INCOMPATIBLE_ROOM_VERSION` + `room_version`), because that is what a peer acts on
  mechanically. This needed a new error-code path: `APIError.RoomVersion`, carried into the response
  by `httpx.RespondError`.
- **Room versions 1 and 2** wrap each previous/auth event reference in a hash of the event. Rooms
  created here are in a modern version, so plain event IDs are correct for everything this server
  produces; the comment says so rather than leaving a silent gap.

### Evidence

- In-process: `TestMakeJoinOffersAPlaceInTheGraph`, `TestMakeJoinRefusesAnUnknownRoom`,
  `TestMakeJoinNegotiatesTheRoomVersion`, `TestMakeJoinRejectsSomethingThatIsNotAUserID`, plus the
  stage-1 and stage-2a tests. `go vet` clean, `go test -count=1 ./tests/...` green, and the
  docs-drift test now extracts routes from `internal/fedapi` too, with `make_join` documented.
- Against real Complement: `TestSyncTimelineGap` moved from
  `MustJoinRoom: make_join failed: … 404` to
  `MustJoinRoom: send_join failed: … M_UNRECOGNIZED: this server does not implement PUT
  /_matrix/federation/v1/send_join/…`. Complement's gomatrixserverlib client accepted our template,
  filled in its sender, signed the event, and sent it back — which is the interoperability proof,
  obtained from the peer's own code rather than from our test.

### Next, in order

1. **`send_join` v1 and v2** — the acceptance half: verify the event's signature against the peer's
   published key (`fedapi.NewKeyRing` is ready), authorise it against current state, persist it and
   the room's state, and answer with `state`, `auth_chain`, `servers_in_room`. Complement asks for
   **v1** here (`PUT /_matrix/federation/v1/send_join/{roomID}/{eventID}`) even though fclient also
   has v2, so both spellings are needed.
2. `PUT /_matrix/federation/v1/send/{txnID}` transaction ingest with per-PDU results, then
3. `POST /get_missing_events` for gap fill — which is what `TestSyncTimelineGap` needs after the
   join, and with it `TestSync`'s redaction subtest (same shape: an inbound event this server has
   never seen) become reachable.

## T308: the unread badge, and the push-rule evaluator behind it (2026-11-11e)

`unread_notifications` was declared and never computed, so `/sync` reported a badge of zero for every
room — and mandatory `TestThreadedReceipts` died inside complement's own helper on the missing field.
Computing it honestly required the thing that was missing all along: an evaluator for the push rules
the server had only been storing.

**The evaluator** (`internal/pushrules/evaluate.go`). Conditions: `event_match` (dotted keys with
escaped dots, arrays read as their elements joined so a mention list matches a user ID, `\w` as the
specification's word boundary, and a bare pattern looked for *inside* a body because "hi alice" has to
match a rule for "alice"), `contains_display_name`, `room_member_count` (exact, `<`/`>`, `a..b`),
`sender_notification_permission`. Actions: bare `notify`/`dont_notify`, the `set_tweak` objects, and
the Matrix 1.13 combined `{"notify":true,"set_tweak":"highlight"}`. First match wins across kinds in
order, so "mute this room" cannot be out-voted by a broader rule written later.

Two judgement calls worth keeping:

- **A condition this server cannot read makes its rule not fire** — firing on incomprehension would
  make a rule written for a newer server interrupt these users instead. The one exception is
  `notification_content`, inert on purpose: it appears in *this server's own* default rules, where the
  narrower reading would silently disable a rule the user can see and expect to work.
- **An event sent by the reader never notifies**, enforced in the evaluator rather than at callers,
  because every caller would have to remember it and one that forgets tells people their own words are
  waiting for them.

**Defaults changed, and the upgrade path had to change with them.** `.m.rule.is_user_mention` was added
first among the override rules (a mention inside a muted bot's notice is still somebody asking you
specifically), and `.m.rule.master` is now seeded condition-free — master with a condition is not
master, it is a selective mute wearing a master's switch. Because defaults are stored data, adding one
would otherwise reach only accounts created afterwards, so `ensureSeeded` now reconciles; and because
`List` deliberately hides deleted rules, reconciliation reads through a new
`PushRuleRepo.ListIncludingRemoved` — otherwise the reconciliation resurrects every rule a person
deleted, which is exactly what the existing `Removed` tombstone exists to prevent, and which my own
test suite caught.

**The badge** (`internal/readstate/badge.go`). The semantics were read off complement's exact numbers
rather than guessed: an event is read iff its position is at or below `max(unthreaded receipt, receipt
of its own scope)`; an unthreaded receipt therefore reads everything it precedes in every thread, while
a threaded receipt touches only its thread. Unfiltered, the badge covers the whole room *including*
threads, because a client that has never heard of threads must still show a true total; with
`{"room":{"timeline":{"unread_thread_notifications":true}}}` the room badge becomes the main timeline
and threads arrive keyed by root — and only threads with something unread, so a client can never
confuse "all read" with "this server does not do threads".

**A trap that would have been invisible in a unit test**: `Receipt.StreamPos` records *when the receipt
was made* (so an incremental `/sync` can tell which receipts are news) — not how far the reader got.
Using it would mark a whole room read whenever somebody receipts an old message, because "just now" is
later than everything. The badge resolves the receipt's `event_id` to its stream position, and an
unknown event covers nothing.

### Two bugs complement caught that no local test had imagined

1. **Reactions were being counted.** The test sends `m.room.reaction` (the newer spelling), while the
   seeded `.m.rule.reaction` matches `m.reaction` — so it fell through to `.m.rule.fallback` and every
   thumbs-up became a notification (7 where 6 was wanted). Fixed where it belongs: an annotation is
   excluded as a category, both spellings, alongside edits.
2. **A quiet room with unread messages dropped out of `/sync`.** Nothing new had happened since the
   token, so the "unchanged room" rule — pinned by `TestSync` — removed it, and a client polling again
   was told nothing at all about a room whose badge it was still showing. The unread count is a fact
   about the room, so it now counts as content. Both fixes are pinned by
   `TestARoomWithSomethingToReadIsNotReportedAsFinished` and `TestAThreadedReactionIsNotSomethingToRead`,
   each verified to fail when its own fix is reverted.

### Measured

- In-process: 13 badge tests in `tests/api/notifications_test.go`; whole suite green
  (`tests/api` 31.7s, `tests/unit` 0.3s), `golangci-lint run ./...` **0 issues**, vet and `gofmt -s` clean.
- Against real Complement: **`TestThreadedReceipts` passes** (it was never federation-bound — the
  recorded claim that it was is corrected below), and
  `TestThreadReceiptsInSyncMSC4102` now reaches its actual blocker, `POST /join` on a second server.
  `TestSync` is unchanged at exactly one failing subtest, the `send_join` one; `TestPushRules`,
  `TestMarkersReadMarkerAfterPagination`, `TestRoomReadMarkers`, `TestTyping` still pass.

### Correction

`TestThreadedReceipts` is single-server (`Deploy(t, 1)`) and was blocked by the missing badge, not by
federation. The hs2 join that looked like its failure belongs to `TestPushRuleRoomUpgrade`. Mandatory
AC-5 therefore stands at 66 of the 69 present tests, with `TestSync`, `TestSyncTimelineGap` and
`TestDeviceListUpdates` as the genuinely federation-bound remainder.

### New gap this work surfaced

`POST /_matrix/client/v3/rooms/{roomID}/upgrade` is not implemented at all (404), which is what
`TestPushRuleRoomUpgrade` fails on. It was failing before this work as well, so it is a gap rather than
a regression — but a real one: room upgrades and tombstones are how a room moves between room versions.

## M7 stage 2c: the join handshake completes (2026-11-11f)

`PUT /_matrix/federation/v1/send_join/{roomID}/{eventID}` and the v2 spelling are served, and
**complement's own federation client completes a join against qwatrix**: the `M_UNRECOGNIZED` for
`send_join` has gone from the logs and the blocker has moved on to `PUT /_matrix/federation/v1/send/…`.

What the handler does, in the order that matters:

1. **Authenticate the request** with `fclient.VerifyHTTPRequest` — an ed25519 signature over method,
   URI, origin, destination and body, checked against a key fetched from the *origin server itself*.
   Federation has no sessions and no tokens; this signature is the whole proof of who is asking.
2. **Parse the event with the room's own version.** An event is defined by its version's rules, and
   parsing a version 11 event as something else yields a different event ID — a different event.
3. **Check the path's event ID matches the body's.** Honouring the body while the peer believes it
   sent another event is how rooms acquire members nobody intended.
4. **Verify the event's own signature.** The request signature proves who sent it and expires with
   the moment; the event's signature is what every later reader will check, and it has to be right
   before this server repeats it to anybody.
5. **Then, and only then, ask the room** (`roomserver.AcceptFederationJoin`): a join event for this
   room, joining the user whose server signed it, attached to history this server holds, allowed by
   the room's own authorisation rules — the same `events.Authorize` a local join passes through.

Decisions worth keeping:

- **A peer may join its own users and nobody else's.** `make_join` will describe a join for any
  syntactically valid user ID, because it is a description rather than a permission slip; the refusal
  belongs at `send_join`, where the signature identifies who is asking. One compromised homeserver
  must not be able to populate other servers' rooms with identities it does not hold.
- **v1 answers with the legacy `[200, body]` envelope**, v2 with the bare object. gomatrixserverlib
  PUTs to v2 and falls back to v1 on a 404 — which is why a missing v2 shows up in a peer's log as a
  missing *v1* — and a v1 peer handed a bare object would parse the whole response as a status code.
- **Ingest is a separate path from the local write, deliberately.** The local path chooses
  predecessors, authorises, builds and signs; a federated event has had all four decided elsewhere
  and this server's job is to check them. Merging the paths ends with re-signing a peer's event, which
  destroys the signature that says who sent it.
- **An event with unknown predecessors is refused**, not absorbed. Accepting it would fork the room
  onto a branch nothing can reach; fetching them (`/get_missing_events`) is a feature, not a detail
  to paper over.
- **`FederationInsecureSkipTLSVerify` became a real operator flag.** Key verification requires
  *fetching* a peer's key, and Complement's fake server presents a self-signed certificate of its own
  making — not one signed by the Complement authority it mounts — so a strictly verifying server can
  never believe it. The flag is off by default, logs a warning naming exactly what it removes, and the
  Complement entrypoint turns it on because that is the test network it exists for.

### Measured

- In-process, five new tests in `tests/api/federation_sendjoin_test.go` against a peer with its own
  signing key and HTTPS server: a remote join admitted (state and auth chain returned, the join
  present in the room's `/members`); an **unsigned request refused and the room untouched**; a
  **correctly signed join for a user on another domain refused 403**; a join into an **invite-only
  room refused 403**; and the **v1 envelope** shape. Whole suite green, `golangci-lint` clean.
- Against real Complement: `TestInboundFederationKeys`, `TestRegistration`, `TestThreadedReceipts`
  pass; `TestSync`, `TestSyncTimelineGap` and `TestDeviceListUpdates` now fail on
  `PUT /_matrix/federation/v1/send/{txnID}` — one step further than `send_join`.
- Full sweep cc21 (the image before this stage): **90 pass, 16 fail, 0 crash, 0 skip, 0 regressions,
  66 of the 69 present mandatory tests (95%)**, newly passing `TestThreadedReceipts`.

### A tooling bug that hid a win

`complement-score.py RESULTS [BASELINE]` was invoked once with the two files swapped, and scored the
old run against the new one: it printed `pass: 89 … newly passing: none` for a run that had actually
gone to 90 with `TestThreadedReceipts` newly passing. Nothing in the output looked wrong. The header
now names which file it read as the run.

## M7 stage 2d: transaction ingest — the server-server API starts listening (2026-11-11g)

`PUT /_matrix/federation/v1/send/{transactionID}` (and v2) is served, and with it **complement's
mandatory `TestSync` passes** — the subtest that had failed since the first run, on a redaction of an
event this server had never seen.

Everything before this was this server *answering* questions. `/send` is the first endpoint where the
federation tells this server that something happened, which makes it the endpoint where bad things
arrive: forged events, replayed transactions, events for rooms this server has no business knowing.

**Each event is accepted or refused on its own.** A transaction whose third event is bogus keeps the
other two, and the answer reports which one did not land and why. Refusing the whole transaction would
make one bad event from one user a permanent block on a room's progress — and the peer would retry a
failure it cannot fix.

**A transaction delivered twice is applied once.** A peer that did not hear the answer sends the same
transaction again; that is not a mistake to punish but the normal consequence of a timeout. Answers are
cached per (origin, transaction ID), bounded and in-process. The bound is safe to lose on a restart
because the per-event "already have it" check does the idempotency work the cache only makes faster.

**A relayed event is authenticated by its own signature, not by who carried it.** Peers are expected to
forward what they receive, so unlike `send_join` the transaction's origin is *not* required to own the
sender's domain. The event's signature, checked against the sender's published key, is what says who it
came from. Getting this backwards in either direction is serious: trusting the transport lets any
homeserver put words in any user's mouth; requiring origin ownership would make transitive relay
impossible and quietly partition the federation.

**A redaction of an event nobody here has seen is accepted, and applied to nothing.** The target may be
outside what this server keeps; complement sends precisely this to check the server survives it.

**An event that follows history this server does not have is refused** — and that refusal is now the
federation's blocking issue rather than a safety margin, which is worth naming plainly:
`TestSyncTimelineGap` fails on this server's own `M_FORBIDDEN` for a join whose predecessor it was never
told about. The answer is gap filling (`/get_missing_events`), not relaxing the check; inventing a
branch that nothing can reach corrupts the room to make one request succeed.

### EDUs are parsed and dropped, and that is a decision rather than an omission

Typing, receipts, to-device and presence arrive in the same transaction. They are parsed — a transaction
containing one still delivers its events — and applied to nothing, each drop logged at debug with its
type. The uncomfortable property is that the response **has no per-EDU result**, so a peer can never be
told an ephemeral event failed: from the other side this server looks like a homeserver where nobody is
ever typing. A debug line naming the type is the only difference between "chose not to act" and "acted
and lost it", which is why it is there and why T707 stays open in plain sight.

### A refusal needs a name, and room v3+ events do not contain one

The answer keys results by event ID, but from room version 3 onwards an event has no `event_id` field —
the ID is a hash *of* the event, computed under the room version's rules, which is exactly the
information missing when the refusal is "I have never heard of this room". An empty key is matchable by
nothing, so the answer recomputes the ID under this server's default room version. It is a label on a
response, not a security decision: if it is wrong the peer sees an ID it does not recognise, which beats
an empty key — and beats, decisively, a refusal going unreported, which a peer would read as success and
stop retrying.

### Measured

- Four in-process tests in `tests/api/federation_send_test.go`, driven by a peer with its own signing
  key: a federated message reaches the local user's timeline; a transaction delivered twice leaves
  **one** copy; an event whose signature is forged is refused per-event and never reaches the timeline;
  an event for an unknown room is refused. Whole suite green, `golangci-lint` 0 issues, `gofmt -s` clean.
- Against real Complement: **`TestSync` passes** (mandatory), with `TestInboundFederationKeys`,
  `TestRegistration` and `TestThreadedReceipts`. Remaining federation-dependent failures, each named by
  the exact error it now reaches: `TestSyncTimelineGap` (this server's missing-predecessor refusal —
  T710), `TestMessagesOverFederation` (hs2 answers `M_NOT_FOUND` for a room only hs1 knows — T711),
  `TestDeviceListUpdates` (device lists do not cross servers — T712).
- Three helper and library facts that cost a cycle each and are worth remembering: a
  `fclient.FederationRequest` must have its content set *before* signing (the library refuses to modify
  a signed request); the content must be passed as `json.RawMessage`, since a plain `[]byte` marshals to
  a base64 string and arrives as an event that cannot be read; and `HTTPRequest()` emits the library's
  own `matrix://` scheme, so a test driving a plain HTTP client must rewrite scheme and host — but only
  those, because the path is what the signature covers.

## M7 stage 2e: gap filling, and the boundary of what it can do (2026-11-11h)

An event that follows history this server does not have is no longer refused on sight. The refusal names
the missing event **and the events this server holds that it should hang from**, and the transport takes
that question back to the peer that reported the event:

1. `POST /get_missing_events/{roomID}` — the whole run between a point this server holds and the event
   just sent. Tried first because gaps are runs, not single events: a peer that reconnects after a while
   away has a chain, and one request can take all of it.
2. `GET /event/{eventID}` — one event, for a peer that serves only that.

Two decisions in it are worth keeping out of the code comments only:

**An anchor is part of the error, not an afterthought.** The first version carried only the missing event
ID, and a gap of more than one event was then unfillable: an event whose single predecessor is missing has
no held predecessors to offer as a starting point, so the bulk path was skipped exactly when it was needed.
The anchor is the predecessors held **plus the room's current tip** — "where did this server stop following
the room?" is the question the peer can actually answer. Asking without one is asking for the room's whole
history, which is not a question worth asking and not one this server could answer either.

**A batch is stored shallowest-first, and a partly-fillable gap is refused entirely.** A run of missing
events is a chain, and each link is only admissible once the one before it is here; sorting by depth means
a fifty-event gap costs one request instead of fifty round trips. And if the peer remembers only the newer
half, the events it *can* supply still hang from an event neither server has. Storing them would put a
detached fragment in the room — an event whose predecessors are unreachable, which clients walk into when
they paginate. `TestAGapThePeerCanOnlyPartlyBridgeIsStillRefused` pins that no fragment is kept.

**This is the honest boundary, and it is now a measured one.** Against real Complement,
`TestSyncTimelineGap` still fails — but not before this server tried: the log shows it reaching the peer's
`/get_missing_events` and then its `/event` for the specific event. The test's peer deliberately bridges
only the last 10 of a 50-event gap. What closes that is not more fetching; it is asking the peer for the
**state at** the event (`/state_ids`, `/state`) and persisting the event with that state despite a graph
that does not connect — outlier handling, recorded as T713. Refusing, with the reason and the successor
named, is the correct answer until then.

### Measured

- Five in-process gap-fill tests (single event, four-event chain in one request with the answer served
  newest-first to prove the ordering works, unfillable gap refused, partly-bridgeable gap refused with
  nothing kept, and the anchor asserted to be the join this server holds). Whole suite green,
  `golangci-lint` 0 issues, `gofmt -s` clean.
- Complement after the gap-fill image: **`TestSync`, `TestThreadedReceipts` and
  `TestInboundFederationKeys` pass**; `TestSyncTimelineGap` and `TestMessagesOverFederation` remain, at
  T713 (state at event) and T711 (outbound federated join) respectively.
- Full sweep cc24, which is the last complete measurement and the one that includes transaction ingest
  but not gap filling: **91 pass, 15 fail, 0 crash, 0 skip; 67 of the 69 present mandatory AC-5 tests
  (97%); newly passing `TestSync`; no regressions.** Saved as `.agent-work/complement-run-94.txt`.

### Two library traps found the hard way

- `fclient` names the *requesting* server `origin` (`GetEvent(ctx, origin, peer, eventID)`) — the mirror
  image of how this server's own code reads from the peer's side. Swapping them surfaces as
  `no signing identity for server name "<peer>"`, which reads like a key-distribution bug and is a
  argument-order bug.
- Passing `fclient.WithTransport` replaces the library's own destination resolver, which is what turns
  its `matrix://` URLs into HTTPS connections. Every outbound federation call then dies with
  `unsupported protocol scheme "matrix"`. `internal/fedapi` now wraps its TLS-configured transport in a
  round tripper that performs the part of that job its documented policy allows — name as written, HTTPS,
  federation port 8448 when the name carries none — while SRV and `.well-known` resolution stay
  unimplemented and documented as such.

## M7 stage 2f: joining a room that is only somebody else's (2026-11-11i)

`POST /join/{roomID}` for a room this server has never heard of used to answer `M_NOT_FOUND`. It now asks
a peer: `make_join` for a template, check the template, fill in and sign the join through the same event
builder a local join uses (so the client's `reason` is inside what this server signs), `send_join`, verify
the answer, and store the room in one transaction — room row, auth chain, state by depth, and this
server's own join last so the room tip is the join.

What the checks are for, in the order they earned their place:

- **The template is checked before it is signed.** It must be an `m.room.member` for this user, in this
  room, with membership `join`. After signing, this server has authored whatever it says, so a check
  after that point is a complaint, not a defence.
- **A peer's answer is verified rather than believed**: parse under the version the peer named, room ID
  must match, signature must verify, the join must be in the state the peer returns.
- **Authorisation runs against the state *before* the join** — the bug this task found. A `send_join`
  answer contains the join event itself, and a membership event in the authorising state *is* the user's
  previous membership. Authorising against the answer as given asks "may @alice join?" and answers it
  with "@alice is already joined", for any room, on the strength of the event being questioned. Every
  invite-only room is a public room under that rule; it took a test that was honest about *why* it passed
  to notice.
- **A join must carry the auth events the room's rules need.** Authorisation reads an event's own
  `auth_events`, so a join that omits `m.room.join_rules` is evaluated against no join rule — and no join
  rule means "public". This server now computes what the state would require and refuses when the join
  left it out.
- **Two state events for one `(type, state_key)` is a refusal**, because the room's rules then mean
  whichever entry was read last. A peer can hide a ban behind a join, or a foreign power-levels event
  behind the room's own. The exception is the join event itself: a correct answer is pre-join state plus
  the join, so an invited or departed user legitimately appears twice. That over-strict check would have
  broken every rejoin and every invited join, and it took writing the *correct* answer as a test to catch
  it.
- **A room ID on this server's own domain that this server does not hold is `M_NOT_FOUND`, not a
  federation question.** Asking a stranger to vouch for an identity this server owns produces an answer
  that is a lie by construction.

Two findings that are not this task's fix:

- An `invite` must be signed by the invited user's homeserver too. This server does not co-sign invites,
  so invites it sends to remote users will be rejected by them. Tracked as **T714**.
- gomatrixserverlib's `Allowed` reads the auth events an event declares. Any place this server hands it a
  state that already contains the event being authorised is the same bug as above, waiting to be found
  again.

The verification method changed too. Each guard now has a test that is confirmed to fail **with the guard
removed** (`.agent-work/guard-probe.py`); three of them were passing without their guard when checked that
way, and one "test" had never run at all because a `t.Fatalf` sat before the assertion that would have
shown it. A passing test is not evidence until something proves it can fail.
