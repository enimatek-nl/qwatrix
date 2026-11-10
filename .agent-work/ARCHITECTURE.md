# Architecture

## 1. Shape

```
                        ┌───────────────────────────────────────────────┐
  HTTP :8008 (http) ────┤ csapi (handlers) ─┐                           │
  HTTP :8448 (https) ───┤ fedapi (handlers) ─┤                          │
                        │        │           │   shared middleware       │
                        │      httpx ◄────────┴  (auth, CORS, errors,    │
                        │       │                 rate limit, recover,   │
                        │       │                 request log, JSON)     │
                        ▼       ▼                                         │
                   ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌────────────┐  │
                   │roomserver│ │ syncapi  │ │keyserver │ │ mediaapi   │  │
                   └────┬─────┘ └────┬─────┘ └────┬─────┘ └─────┬──────┘  │
                        │            │            │             │         │
                        └────────────┴─────┬──────┴─────────────┘         │
                                          ▼                              │
                              internal/store (GORM repos, SQLite)        │
                                          ▲                              │
                        internal/events ──┘  (GMSL-backed event domain)   │
                        internal/matrix ──── IDs, errors, versions        │
                        internal/config ──── flags + env                  │
                        internal/app ─────── wiring, listeners, shutdown  │
                        internal/federation ─ signing, keys, client       │
                        cmd/qwatrix ──────── entry point                  │
```

One process, one SQLite file, two listeners. No external services (CN-4).

## 2. Packages and responsibility

| Package | Responsibility | Notes |
| --- | --- | --- |
| `cmd/qwatrix` | flag parsing, config load, `app.New`, run with signal handling | thin: no logic |
| `internal/config` | `Config` struct, defaults, `FromFlags(*flag.FlagSet)`, `FromEnv()`; validation | CN-5: nothing operational hard-coded |
| `internal/matrix` | spec value types: `ErrCode`/`APIError`, ID building/validation (wraps `gomatrixserverlib/spec`), `Versions()` list, localpart rules, `RoomVersion` selection, canonical `RoomID`/`StateKeyTuple` helpers | no I/O |
| `internal/store` | GORM models + one repository per aggregate (interface + impl + constructor), `Store` struct holding `*gorm.DB` (private) and repos (public), `AutoMigrate`, transaction helper `Do(ctx, func(Tx))` | skill's repository pattern |
| `internal/events` | event domain service over GMSL: build+sign a `PDU` from a request, resolve auth events, run `Allowed`, redact, parse power levels/membership/join rules, canonicalise content, enforce size limits, generate event/room/media IDs | the only package allowed to import GMSL event machinery |
| `internal/roomserver` | use-cases: `CreateRoom`, `SendEvent`, `SetState`, `Invite`, `Join`, `Leave`, `Kick`, `Ban`, `Redact`, `Messages`, `State`, `Members`, `Aliases`, `Summary`, `Hierarchy`, `Relations`, `Context` | owns the DAG, state, memberships, stream positions, txn-idempotency, per-room serialisation |
| `internal/syncapi` | `Sync(ctx, req) (Response, error)`: stream→response, filters, batching, `prev_batch`, `limited`, ephemeral, invites/leaves, device lists, to-device, notification counts, long-poll wake-up | consumes only stream positions |
| `internal/keyserver` | device keys, one-time/fallback keys, key query/claim, key-change tracking, to-device queue, cross-signing (later) | |
| `internal/mediaapi` | upload (size/type limits), storage layout, download, thumbnail (pure-Go image decode+scale), async upload (later) | files under `data-dir/media/`, metadata in SQLite |
| `internal/pushapi` | pushers, push rules (server-default ruleset + user overrides), notification-count computation | push *gateway* delivery out of scope |
| `internal/csapi` | HTTP handlers, one file per spec area, `Mux(cfg, deps) http.Handler` | handler = func returning `http.HandlerFunc` taking deps (skill style) |
| `internal/fedapi` | server-server handlers (`/version`, `/key/v2/server`, `/state`, `/event`, `/event_auth`, `/timestamp_to_event`, `/query/*`, `/send`, `/make_join`…) | TLS listener; milestone M7 |
| `internal/federation` | server identity/keyring, request signing & verification, outbound client with host/Well-Known resolution | |
| `internal/httpx` | `RespondJSON`, `RespondError(err)` mapping, `RequireAuth`, `BearerToken`, `CORS`, `RateLimiter`, `Recoverer`, `RequestLogger`, `StreamingResponse` helpers | shared, testable |
| `internal/app` | `App` struct: opens store, builds services, builds both handlers, `Run(ctx)` with `errgroup` (two `http.Server`s + signal ctx + graceful `Shutdown`) | the seam tests use |

**Dependency direction:** `csapi|fedapi → services → store`; `services → events → matrix`.
Handlers never touch GORM; services never touch `http.Request` (they take plain
request/response structs defined next to the handler and passed down).

## 3. Storage model (SQLite via GORM)

`EventRow.id` is the **stream position** (global, monotonic) — the spine of `/sync` and
`/messages`. Ordering of the DAG is *not* taken from it (that is `depth`/`prev_events`).

| Model | Key fields | Indexes / notes |
| --- | --- | --- |
| `User` | `localpart` (pk), `full_id`, `password_hash`, `admin`, `deactivated`, `created_ms`, `user_version` | `full_id` unique |
| `Device` | `id` (pk), `localpart`, `display_name`, `last_seen_ms`, `client_id/ip`, `device_list_seq` | idx `localpart` |
| `AccessToken` | `token` (pk), `device_id`, `localpart`, `created_ms` | idx `device_id` — row presence *is* validity, so `/logout` is a delete (skill's revocation model, no JWT) |
| `Room` | `id` (pk, `!opaque:server`), `version`, `created_ms`, `room_version_creator` | |
| `EventRow` | `id` (int64 pk autoincrement = stream pos), `event_id` (unique), `room_id`, `type`, `state_key` (nullable), `sender`, `depth`, `origin_server_ts`, `redacts`, `json` (canonical signed JSON, no `unsigned`), `prev_events` (JSON), `auth_events` (JSON), `received_ms`, `rejected_reason` | idx `(room_id,id)`, `(room_id,type,state_key,id)`, `event_id` |
| `RoomState` | `(room_id,type,state_key)` pk, `event_id`, `stream_pos` | current resolved state; `prev_content`/`replaces_state` derived from the replaced event |
| `Membership` | `(room_id,localpart)` pk, `membership`, `event_id`, `stream_pos` | denormalised from `m.room.member` for fast authorisation and joins/invites listing |
| `RoomAlias` | `alias` (pk), `room_id`, `creator`, `allowed_in_directory`, `canonical` | |
| `TxnID` | `(localpart,device_id,txn_id)` pk, `event_id`, `payload_hash` | idempotency (FR-27) |
| `Receipt` | `(room_id,receipt_type,thread_id,localpart)` pk, `event_id`, `ts_ms`, `data_json` | |
| `ReadMarker` | `(room_id,localpart)` pk, `fully_read`, `read_receipt`, `read_private_receipt` | |
| `AccountData` / `RoomAccountData` | `(localpart,type)` / `(localpart,room_id,type)` pk, `content` | with `stream_pos` for sync deltas |
| `Tag` | `(localpart,room_id,tag)` pk, `order` | |
| `Filter` | `(localpart,id)` pk, `content` | |
| `DeviceKey` | `(localpart,device_id,key_id)` pk, `json` | identity keys |
| `CrossSigningKey` | `(localpart,usage,key_id)` pk, `json`, `user_id`, `device_id` | later |
| `OneTimeKey` | `(localpart,device_id,key_id)` pk, `json`, `fallback` | consumed on claim |
| `KeyNotifiedUpdate` | `(user_id, notified_user_id)` pk, `stream_pos` | drives `device_lists.changed`/`/keys/changes` |
| `ToDeviceRow` | `id` pk autoincrement, `localpart`, `device_id`, `type`, `body`, `txn_id` | per-device stream |
| `Typing` | `(room_id,localpart)` pk, `expires_ms` | ephemeral rows (cheap; also pruned) |
| `PresenceRow` | `localpart` pk, `presence`, `status_msg`, `last_active_ms`, `stream_pos` | |
| `Pusher` | `(localpart,pushkey,app_id)` pk, `kind`, `data`, `lang`, names | |
| `PushRule` | `(localpart,kind,rule_id)` pk, `priority_class`, `actions`, `enabled`, `conditions`, `actions_json` | defaults seeded lazily |
| `Media` | `media_id` pk, `user_id`, `path`, `content_type`, `size`, `sha256`, `name`, `created_ms`, `urls_cached` | file on disk under `media/<id[0:2]>/<id>` |
| `MediaURLCache` | `url` pk, `expires_ms` | federation media (later) |
| `KeyBackup` | … | later |
| `SyncTokenMeta`? | none — tokens encode a stream position (`s<pos>`), validity = position ≥ oldest retained | |

SQLite pragmas on open: `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`,
`busy_timeout=5000`, single writer via `db.DB.SetMaxOpenConns(1)` for writes and a
read pool of 4 — correctness first, and the write serialisation removes a class of
"stream position interleave" bugs. A `sync.Cond`-based `Notifier` (per-topic: `events`,
`typing`, `receipts`, `todevice`, `keys`) wakes long-pollers after each committed write.

## 4. Key flows

### 4.1 Sending an event (any room mutation)
```
roomserver.mutate(ctx, roomID, actor, build func(*EventDraft)):
  1. per-room mutex (keyed map, refcounted) + store transaction
  2. load current state (RoomState→EventRow→PDU) and memberships
  3. authorise: events.Allows(PDU, stateAsAuthEvents, identityUserIDForSender)
       → gomatrixserverlib.Allowed, mapping NotAllowed → matrix.ErrForbidden
  4. build: events.Build(draft, prevEvents=[tip], depth=tip.depth+1,
            authEvents via AddAuthEvents, roomVersion, serverKey)
       → signed PDU with correct event_id/hash per room version
  5. insert EventRow (stream pos), if state_key != nil: move the replaced event to
     prev_content/replaces_state bookkeeping and upsert RoomState
  6. denormalise membership if m.room.member; bump stream; notify listeners
  7. on error: rollback, return wrapped error (no partial state)
```
Idempotency: for `PUT /send/{type}/{txn}` the `(user,device,txn)` row is read first; a hit
with an identical payload hash returns the stored `event_id`, a hit with a different hash
returns `400 M_TRANSACTION_FAILED` (complement `TestTxnIdempotency*`, `TestTxnScopeOnLocalEcho`).

### 4.2 `/sync`
```
since → streamPos (parse "s<pos>"; unparsable → 401 M_UNKNOWN_POS)
rooms = joined ∪ invited ∪ knocked ∪ left(with events after since)
for each joined room:
  timeline = events in room with id>since, ordered by id, limited by filter.limit
             (from the back when truncated → limited=true, prev_batch=position of first dropped)
  state    = current state minus state events already in the returned timeline,
             filtered to "state the client has not seen" (id>since or state delta)
  ephemeral= typing (unexpired), receipts (id>since)
  unread   = counts from push rules over events since the read marker
invite rooms: invite_state (events with types m.room.create/join_rules/…/membership,
             room_version) — MSC4311 full-event shape where room version demands it
left rooms: limited timeline since the leave event + state at leave time
presence    = changes since since
account_data= global/room account data changes since since
device_lists= key-notified updates since since (changed/left) + empty flag
to_device   = rows for this device with id>since (top-level)
next_batch  = "s<max stream pos observed at response build>"
long poll: if nothing changed and timeout>0 → wait on Notifier with timeout, then re-run
```
Filters (FR-43) support the fields complement exercises: `room.timeline.limit`,
`room.state.lazy_load`, `room.state.types`/`not_types`, `room.ephemeral.*`,
`room.account_data.*`, `room.unread_thread_notifications`? (ignored), `presence.*`,
`event_fields`, `send_to_device.*`, `format` (`client`/empty; `federation` rejected).

### 4.3 Response decoration
Stored `json` is the federation form. At response time the `events` package adds
`unsigned.age` (now − `origin_server_ts`), `unsigned.transaction_id`, `unsigned.prev_content`
+ `unsigned.replaces_state` for replaced state, `unsigned.redacts`, and applies
`event_fields` projection/redacted-event stripping. Nothing else is rewritten, so what a
client stores for E2EE (event ID + hashes) matches what federation would send.

### 4.4 Server identity & federation PKI
`internal/federation` owns an ed25519 signing key pair persisted in the data directory
(`signing.key`, mode 0600) with a `key_id` of the form `ed25519:aBcDeF`. `/key/v2/server`
publishes it, self-signed, `valid_until = now + 7d`. The HTTPS listener on 8448 uses either
the operator's certificate or, inside complement, a leaf generated from
`/complement/ca/*` by `deploy/complement/entrypoint.sh`.

## 5. Cross-cutting behaviour

- **Errors:** services return `*matrix.APIError{Status, Code, Message, RetryAfterMS}`;
  `httpx` serialises it. Anything unrecognised becomes `500 M_UNKNOWN` with the real error
  logged (with a request ID) and a generic message returned (CN-7).
- **Auth middleware:** `httpx.Auth(services)` resolves the token (header then query),
  loads `(user, device)`, bumps `last_seen`, injects into context; handlers use
  `httpx.MustUser(t/r)`. Optional-auth endpoints (login, versions, `/publicRooms`, media
  with `allow_remote`) use a separate wrapper.
- **UIA:** one helper implementing the interactive-auth state machine (`sessions` in
  memory + `m.login.password`/`m.login.dummy`/`m.login.registration_shared_secret` stages),
  used by `/register`, `/account/password`, `/account/deactivate`,
  `/keys/device_signing/upload` (later).
- **Rate limiting:** token bucket per `(scope, subject)` with configurable burst; scopes:
  `login`, `register`, `send`, `media`, `sync`. `429` + `retry_after_ms` + `M_LIMIT_EXCEEDED`.
  Defaults are deliberately high so compliance tests are deterministic.
- **Races/concurrency:** per-room mutation mutex; `sync.Mutex` for in-memory maps;
  notifier via `sync.Cond` + generation counter; long-poll waits always bounded by a timer
  and the request context (skill: no goroutine without an owner — everything hangs off
  `app.Run`'s `errgroup`).
- **IDs:** room `!<random32>:<server>`, event `$<random22>` (opaque, room v2+ format —
  `EventIDFormatV2`), media `<random32>`, device `<BASE64(8)>`, user `@<localpart>:<server>`,
  aliases `#<alias>:<server>`. All validated with GMSL's `spec` helpers.

## 6. Testing architecture (user constraint CN-2)

No `_test.go` outside `tests/`.

| Folder | Kind | What it exercises |
| --- | --- | --- |
| `tests/harness` | helper package (no `_test.go`) | `harness.New(t)`: temp dir + file/`:memory:` SQLite + `app.New` + `httptest.NewServer`; typed CS API client (`Register`, `Login`, `Sync`, `SendEvent`, `MustCreateRoom`, `DoJSON`), clock control, log capture |
| `tests/unit` | fast pure tests | matrix IDs/errors, filters, stream-position math, canonical JSON/canonical content, push-rule evaluation, token parsing, media naming, config |
| `tests/api` | in-process HTTP tests over the real handlers + real SQLite | everything in REQUIREMENTS §2–§7, table-driven, `-race` |
| `tests/complement` | build tag `complement`; out-of-repo complement package | the container contract (AC-4) and our own spec-shaped tests |

The in-process suite is the fast TDD loop (seconds); the complement layers are the
compliance gate (minutes). The harness is used by every `tests/api` test so there is exactly
one way to stand a server up.

## 7. Delivery layout

```
cmd/qwatrix/main.go
internal/{config,matrix,store,events,roomserver,syncapi,keyserver,mediaapi,pushapi,httpx,csapi,fedapi,federation,app}
tests/{harness,unit,api,complement}
deploy/complement/{Dockerfile,entrypoint.sh}
docs/{ENDPOINTS.md, …}
Makefile  go.mod  README.md  .agent-work/
```

`make image` builds the release image; `make complement-image` builds the
complement-specific image (binary copied in, Alpine + openssl, EXPOSE 8008 8448,
HEALTHCHECK on `/_matrix/client/versions`, entrypoint minting the federation leaf).
