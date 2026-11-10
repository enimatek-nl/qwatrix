# Reference analysis

## A. complement — the contract a homeserver image must satisfy

Derived by reading `complement@9a18f5c` (not the README alone): `README.md` §Image
requirements, `config/config.go`, `internal/docker/{builder,deployer,deployment}.go`,
`client/auth.go`, `test_main.go`, `test_package.go`, `OUT-OF-REPO-TESTS.md`.

### A.1 Container contract (hard requirements)

| Item | Value | Source |
| --- | --- | --- |
| Client API port | `8008`, **plain HTTP** | `deployer.go` `endpoints(..., 8008, 8448)`; README §Image requirements |
| Federation port | `8448`, **HTTPS** (complement uses `FedBaseURL` = `https://…` with `InsecureSkipVerify` + `ServerName: hsName`, so the cert must contain `subjectAltName=DNS:<SERVER_NAME>`) | `deployment.go` `transportFor` |
| Readiness | `GET /_matrix/client/versions` → `200`, plus `HEALTHCHECK` healthy, within `COMPLEMENT_SPAWN_HS_TIMEOUT_SECS` (30 s default) | `deployer.go` `waitForPorts`/health wait |
| Identity | env `SERVER_NAME=hs1|hs2|…` at container start; the container's network alias equals that name, so federation peers resolve `hs1` directly | `deployer.go` `env`, `NetworkingConfig.Aliases` |
| Restartability | `ENTRYPOINT`/`CMD` runs repeatedly (`Restart`, `StopServer`, `StartServer`, `PauseServer`, `UnpauseServer` are used by tests) — data must live on the container filesystem/volume, no host assumptions | `Deployment` interface |
| Registration | Two paths, and Complement picks between them: `deployment.Register` uses the **standard** `POST /register` flow (`m.login.dummy`, 401-then-200) and only uses **shared-secret** registration when `RegistrationOpts.IsAdmin` is true. Shared-secret: `GET /_synapse/admin/v1/register` → `200 {"nonce":…}`, `POST` with `{nonce,username,password,mac,admin}` → `200 {user_id,access_token,device_id}`; a non-200 on GET makes the framework **skip** dependent tests. Both paths are therefore load-bearing (`/register` for almost every user, the shared secret for every admin test). Verified against our image: see `PROGRESS.md` 2026-11-09 M0. | `internal/docker/deployment.go` `Register`, `client/auth.go` |
| HMAC details | `SharedSecret = "complement"`; `mac = hex(HMAC-SHA1(key=secret, msg = nonce ‖ 0x00 ‖ user ‖ 0x00 ‖ pass ‖ 0x00 ‖ "admin"/"notadmin"))` | `client/auth.go` |
| Fallback registration | If shared secret is unavailable complement uses `POST /register` with a single `m.login.dummy` stage (no session exchange when the server does not demand a session first) | `client/auth.go` `RegisterUser` |
| Federation PKI | CA material copied into every container at `/complement/ca/ca.crt` and `/complement/ca/ca.key`; homeserver must sign its own leaf and trust the CA outbound | `deployer.go` `MountCACertPath`, `MountCAKeyPath` |
| App services | registration YAML dropped in `/complement/appservice/` (directory is created & populated before start) | `MountAppServicePath` |
| Host reachability | `host.docker.internal` (override `COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT`; **podman needs `host.containers.internal`**) is mapped to `host-gateway`; complement runs a mock homeserver ("conduit"-style) on the host that our HS must be able to call | `deployer.go` `extraHosts`, `config.go` |
| Resources | `CapAdd: NET_ADMIN`, `PublishAllPorts: true`, optional CPU/memory caps, labels `complement_context`, `complement_blueprint`, `complement_pkg`, `complement_hs_name` | `deployer.go` `deployImage` |
| Cleanup | complement removes labelled containers/images/networks at start and end of a package run | `builder.Cleanup` |

### A.2 How complement drives a suite

- Entry point: `complement.TestMain(m, namespace)` in `tests/main_test.go`; tests call
  `complement.Deploy(t, nServers)` → `hs1…hsN`, or `complement.OldDeploy(t, blueprint)` for
  pre-seeded blueprints (`b.Alice`, `b.OneToOneRoom`, `b.HomeserverWithAppservice`, …).
- `deployment.Register(t, "hs1", helpers.RegistrationOpts{Localpart: …, IsAdmin: …})`
  → `*client.CSAPI`. `client.CSAPI` wraps requests (`Do`/`MustDo`), adds
  `access_token`, tracks devices, and offers helpers (`MustSync`, `MustCreateRoom`,
  `MustSendEvent`, `TxnID`, `SharedSecretRegistration`).
- Assertions: `must.MatchResponse`/`must.MatchJSON`/`should.*`/`match.*`. `must.*` is
  strict about JSON shapes — extra fields are tolerated, missing/wrong ones are not.
- `COMPLEMENT_*` env knobs that matter here: `BASE_IMAGE`, `SPAWN_HS_TIMEOUT_SECS`,
  `ALWAYS_PRINT_SERVER_LOGS`, `DEBUG`, `HOST_MOUNTS`, `ENABLE_DIRTY_RUNS`,
  `SHARE_ENV_PREFIX`, `CONTAINER_CPU_CORES`, `CONTAINER_MEMORY`,
  `HOSTNAME_RUNNING_COMPLEMENT`, `POST_TEST_SCRIPT`.
- Suite size at the pinned commit: **107 test functions in `tests/csapi`**, plus ~30
  federation/media/restricted tests at `tests/` root and per-MSC packages.
- Tests may be **skipped** by the framework when the image lacks a capability (shared-secret
  registration, appservices). Skips are not passes; the status file tracks them.
- Complement uses **wrong-method answers** as an assertion: `TestUnknownEndpoints` expects
  `405` *with a Matrix `M_UNRECOGNIZED` body* for e.g. `PUT /_matrix/client/v3/login`, and
  `404 M_UNRECOGNIZED` for unknown paths. A stdlib mux answers 405 with plain text, so the
  mux needs a method-not-allowed shim (task T013).

### A.3 Out-of-repo test packages (what `tests/complement/` follows)

`OUT-OF-REPO-TESTS.md`: a separate Go module path imports
`github.com/matrix-org/complement` (+ `client`, `helpers`, `match`, `must`, `ct`, `should`),
defines `TestMain` → `complement.TestMain(m, "<unique-namespace>")`, and runs with
`COMPLEMENT_BASE_IMAGE=<image> go test ./tests/complement`. Namespace must be unique versus
parallel packages (`csapi`, `fed` are taken).

## B. matrix-spec v1.19 — facts pinned by reading the source

- Spec release `v1.19` (2026-07-08); client-server API versions `v1.0`…`v1.11` are the
  `versions` values to advertise; room versions `1`…`12` exist upstream.
- Per-endpoint OpenAPI 3.1 files live in `data/api/client-server/*.yaml` (73 files) and
  `data/api/server-server/*.yaml`; the prose is `content/client-server-api/_index.md` +
  `modules/*.md`. These are the field-level oracle used while implementing (grep by path).
- Error model: `{"errcode": "...", "error": "..."}` with the error codes enumerated in
  `_index.md` §Standard error codes — `M_UNKNOWN_TOKEN`, `M_MISSING_TOKEN`, `M_FORBIDDEN`,
  `M_UNKNOWN`, `M_UNRECOGNIZED`, `M_LIMIT_EXCEEDED` (+`retry_after_ms`), `M_TOO_LARGE`,
  `M_EXCLUSIVE`, `M_BAD_JSON`, `M_NOT_JSON`, `M_INVALID_PARAM`, `M_USER_IN_USE`,
  `M_INVALID_USERNAME`, `M_WEAK_PASSWORD`, `M_SESSION_INVALID`/`_MODIFIED`/`_NOT_FOUND`,
  `M_THREEPID_IN_USE`, `M_USER_DISABLED`, `M_RESOURCE_LIMIT_EXCEEDED`, `M_TOO_MANY_REQUESTS`,
  `M_IncompatibleRoomVersion` (note the capitalisation), `M_CANNOT_LEAVE_SERVER`…
- Access tokens: `Authorization: Bearer <token>` preferred, `?access_token=` still accepted
  (deprecated but universally used); failures must carry `M_UNKNOWN_TOKEN` and set
  `errcode` even at `401`.
- `unsigned` fields (`age_ts`→`age`, `transaction_id`, `prev_content`, `redacts`,
  `replaces_state`) are added at *response* time and are not part of the stored/persisted
  federation JSON — this drives the storage design (persist canonical signed JSON, decorate
  on the way out).

## C. gomatrixserverlib — the pieces qwatrix builds on

Pinned `v0.0.0-20260716140101-4fe595dc7f58` (same version complement itself depends on).

| Need | API |
| --- | --- |
| Room version metadata | `RoomVersions()`, `MustGetRoomVersion(v)`, `StableRoomVersions()`, `IRoomVersion` (event/ID format, redaction, canonical-JSON checks, `CheckCreateEvent`) |
| Event creation + signing | `IRoomVersion.NewEventBuilder()` → fill → `(*EventBuilder).Build(now, origin, keyID, privateKey)` → `PDU` (assigns event ID per format, hashes `prev_events`/`auth_events`, signs with the server key) |
| Auth event selection | `(*EventBuilder).AddAuthEvents(provider)`, `StateNeededForAuth`, `AuthEventReferences` |
| Authorization rules | `Allowed(event PDU, authEvents AuthEventProvider, userIDQuerier spec.UserIDForSender) error` |
| Content parsing | `NewPowerLevelContentFromEvent/AuthEvents`, `NewMemberContentFromAuthEvents`, `NewJoinRuleContentFromAuthEvents`, `NewCreateContentFromAuthEvents` |
| Redaction | `IRoomVersion.RedactEventJSON`, `PDU.Redact()` |
| Canonical JSON | `CanonicalJSON`, `EnforcedCanonicalJSON(input, roomVersion)` |
| Signing/verification | `SignJSON`, `VerifyJSON`, `ServerKeys`/`ServerKeyFields`/`VerifyKey` for `/_matrix/key/v2/server` |
| Identifiers | `spec.NewUserID`, `spec.NewRoomID`, `spec.EventID` validation, `spec.ServerName` |
| Federation client (later) | `gomatrixserverlib/fclient` + `keyring` |

Consequence: qwatrix does **not** reimplement reference hashing, canonical JSON, event-ID
formats, the auth rules table, or state resolution — it wires them to its own storage and
API layers. Everything above the event layer (accounts, rooms, sync, media, keys, HTTP) is
qwatrix code.

## D. lattice (user's previous homeserver) — reference, deliberately not code source

Used **only** for environment facts, verified by reading its `Makefile`,
`Dockerfile`, `deploy/complement/*`:

1. complement is driven from a complement checkout, not as a module dependency
   (`cd $(COMPLEMENT) && COMPLEMENT_BASE_IMAGE=… go test ./tests/csapi`), because
   complement's own `go.mod` owns the test binaries.
2. The complement image copies a **binary built outside the image** (fast rebuilds) onto an
   Alpine base with `openssl` — needed to mint the federation leaf certificate from
   complement's CA at container start, with a self-signed fallback when no CA is present.
3. Entrypoint contract: read `SERVER_NAME`, generate `subjectAltName=DNS:$SERVER_NAME`
   (a bare CN is rejected by modern Go verifiers), import the CA into the system trust
   store, then exec the server with `-server-name`, client `:8008`, federation `:8448`.
4. `tests/{unit,compat,federation,integration}` confirms the dedicated-test-folder layout the
   user asked for; the same split is adopted here (`tests/{unit,api,complement,harness}`).

**Not reused:** its schema, its handler organisation, its coverage tooling, or any source
text. qwatrix's architecture is derived from the spec and complement's contract
(`ARCHITECTURE.md`), and the two codebases are expected to differ in structure.
