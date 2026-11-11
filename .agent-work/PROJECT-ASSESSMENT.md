# Project Assessment — qwatrix

## Assignment

Greenfield implementation of a Matrix homeserver in Go, in an empty repository
(`/home/enimatek/Projects/qwatrix`, no commits). It must be spec-compliant with
<https://spec.matrix.org/latest/> (spec release **v1.19**, dated 2026-07-08, client-server
API versions up to **v1.11**) and be verifiable against the
[complement](https://github.com/matrix-org/complement) black-box compliance suite.

Explicit user constraints:

1. Go as the implementation language (mandated by the `backend` skill).
2. Tests must live in a dedicated test folder, not mixed with the Go source files.
3. Compliance is proven by running complement against the implementation.

## Classification

**Greenfield**, with strong available references:

| Reference | Type | Use |
| --- | --- | --- |
| `matrix-spec` (cloned: `/home/enimatek/Projects/refs/matrix-spec`) | authoritative spec source (markdown + OpenAI 3.1 YAML per endpoint) | field-level request/response contracts, error codes |
| `complement` (cloned: `/home/enimatek/Projects/refs/complement`, user copy: `~/complement` @ `7b38a63`) | compliance test framework + test suite | image contract, out-of-repo test API, assertion behaviour |
| `lattice` (`/home/enimatek/Projects/lattice`, the user's earlier homeserver) | prior art by the same author | **environment/integration knowledge only**: complement Makefile invocation, complement container contract, entrypoint TLS bootstrap, test-folder layout |

**Independence rule:** qwatrix is written from scratch. No code is copied from lattice.
Only environment facts (how complement is driven on this machine, which env vars and ports
it uses) and the user's established project conventions are reused. See
`REFERENCE-ANALYSIS.md`.

## Environment

| Item | Value | Notes |
| --- | --- | --- |
| Go | `go1.26.10` | module proxy reachable, `gomatrixserverlib` downloads fine |
| Container runtime | podman **5.8.9** emulating the Docker CLI/API | `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` (rootless, socket active) |
| Docker API served by podman | `ApiVersion 1.44`, `MinAPIVersion 1.24` | complement negotiates with `client.WithAPIVersionNegotiation()`, so 1.44 is accepted (its README asks for 1.45 as a guideline) |
| Evidence the pairing works | local image store already contains `localhost/complement-lattice:*` images built hours ago | complement has run against a homeserver image on this host with podman before |
| Complement docs for podman | `README.md` "Running using Podman": needs `BUILDAH_FORMAT=docker` (OCI format drops `HEALTHCHECK`) and `COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT=host.containers.internal` | adopted in the Makefile |
| Rootful socket | `/run/podman/podman.sock` exists but is not readable by this user | rootless is the path; user is in `wheel` if escalation is ever needed |
| Other | 28 cores, 62 GiB RAM, 625 GiB free | parallel complement runs and heavy test suites are affordable |
| Known host caveat | rootless `pasta` networking has bitten complement federation tests upstream | documented fallback: `slirp4netns` in `/etc/containers/containers.conf` |

## Stack decision

Follows the `backend` skill (stdlib `http.NewServeMux` with method-prefixed routes, GORM +
gormlite pure-Go SQLite, repository pattern, `context.Context` everywhere, explicit error
handling, table-driven tests, race detector, gofmt + golangci-lint).

| Concern | Choice | Why |
| --- | --- | --- |
| HTTP | stdlib `net/http` + `http.NewServeMux` (Go 1.22 patterns) | skill mandate; Matrix routes are flat and path-typed; no framework lock-in |
| Storage | GORM + `github.com/ncruces/go-sqlite3/gormlite` | skill mandate; one file, `CGO_ENABLED=0` static binary → trivial container image |
| Matrix event domain | `github.com/matrix-org/gomatrixserverlib` (GMSL) + `/spec` | canonical JSON, reference hashing, event signing, event-ID/room-ID/user-ID validation, **event authorization rules** (`Allowed`), redaction rules, state-resolution algorithms, room-version metadata — the parts where a subtle bug means non-compliance |
| Auth | opaque random access tokens persisted in a `access_tokens` table | **Deviation from the skill's JWT recipe:** the Matrix spec requires opaque, revocable `access_token`s (`M_UNKNOWN_TOKEN`); a self-verifying JWT cannot be revoked on `/logout` and leaks nothing by being verifiable. The skill's "session row = revocation" model is kept, minus the JWT. |
| Passwords | `golang.org/x/crypto/bcrypt` | same as the skill's auth recipe |
| OpenAPI docs | **not generated** (no swaggo) | **Deviation from the skill:** the Matrix specification *is* the OpenAPI document; generating a second, hand-drifted copy would be a liability. `docs/` links to the spec sections per endpoint instead. |
| Room version default | `11`, advertising `1`–`12` | v12 imposes strict event byte limits and MSC4291 rules; v11 is the safest default that is still current. v12 rooms are still creatable on request. |

## Risks

| # | Risk | Mitigation |
| --- | --- | --- |
| R1 | podman's Docker-API shim may reject calls complement makes (`CapAdd: NET_ADMIN`, `PublishAllPorts`, `CopyToContainer`, `ImageBuild` with `HEALTHCHECK`) | **Tracer bullet first**: get `/versions` + shared-secret registration into a complement image and run a real `complement.Deploy` before writing further features. Fallbacks: rootful socket, `WithDeployment` custom deployer for our own tests. |
| R2 | Scope: the CS API is ~234 endpoints, the SS API ~30 more; full compliance is a multi-week effort | Milestone plan (`PLAN.md`) with a measured complement pass-list; every milestone independently shippable and verified |
| R3 | gormlite (WASM SQLite) is slower than cgo SQLite under complement's `perf_*`-style tests | Batched writes, `journal_mode=WAL`, `synchronous=NORMAL`, narrow indexes, and stream-position based queries instead of graph walks |
| R4 | Writing tests only under `tests/` forbids package-internal white-box tests | Tests exercise exported constructors only (aligns with the TDD skill: behaviour through public interfaces); `internal/...` stays importable from `tests/` inside the module |
| R5 | complement's suite changes under a moving `main` | Pin the complement commit in `.agent-work/COMPLEMENT-RESULTS.md`; Makefile takes `COMPLEMENT=` path/checkout |

## Loaded skills

`autonomous-development` (workflow), `backend` (Go conventions — mandate),
`tdd` (mandatory methodology for implementation), `frontend` — **not** applicable
(a homeserver has no UI here).
