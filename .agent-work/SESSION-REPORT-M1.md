# Session report — M1 signed off, room read path corrected

Date: 2026-11-10. Suite numbers are measured, not estimated: `/tmp/complement-full{4..8}.log`.

## Result

| | before this session | after |
| --- | --- | --- |
| Complement top-level | 20 / 50 | **32 / 50** |
| Complement subtests | 119 / 189 | **149 / 189** |
| skips | 0 | 0 |
| regressions introduced | — | **0** (`comm` over consecutive failure lists) |
| in-process suite (`go test -race ./tests/...`) | green | green |
| `go build ./...`, `go vet ./...`, `gofmt -l` | clean | clean |

**M1 (accounts, sessions, devices) is signed off**: 10/10 of its Complement exit-gate tests pass
against the container (`/tmp/complement-m1b.log`).

## Protocol errors fixed (11)

Each was invisible to a passing in-process suite, because each in-process test asserted what the
server did rather than what the protocol asks.

1. `POST /logout` deletes the device, not only its token.
2. `logout_devices:false` keeps every session; the acting session survives either way.
3. `POST /login` honours `device_id` (reusing a device the same account owns) and
   `initial_device_display_name` — which had been assigned to `ClientID` and silently discarded.
4. UIA challenges always carry `params`; `DELETE /devices/{id}` with no body is a 401 challenge
   with flows, not a 403.
5. `m.room.canonical_alias` may only claim an alias that resolves back to this room
   (`M_BAD_ALIAS`), and a malformed one (`M_INVALID_PARAM`).
6. Deleting an alias clears the room's claim to it, as an event (`{}` when that alias was all it
   claimed).
7. Forgetting a room ends read access (`403`).
8. …while a leave that predates a client's token still reaches that incremental `/sync`.
9. `GET /event/{id}` answers `404` for anything unreadable, never `403`.
10. Under `joined` visibility a member may not read what predates their join.
11. Under `invited` visibility the bound is the **invite**, not the join — recovered from the
    append-only event stream (`Events.FirstPositionOfState`), because `memberships.stream_pos`
    stores only the current membership.

Plus, found while diagnosing: a state-typed event sent through `/send` is a legitimate timeline
event (an event is state because it carries a `state_key` and because of nothing else), so the
refusal was deleted; and event-content numbers must be integers an IEEE-754 double holds exactly
(`matrix.ValidateEventContent`, walking `json.Token`s so no number ever passes through `float64`).

## Method that worked

Read Complement's assertion, never its test name. Where an in-process reproduction disagreed with
the container, reproduced against the container with a small driver
(`.agent-work/probe-devices.py`, `.agent-work/probe-sync.py`) rather than reasoning about it —
that is what showed one "403 bug" had already been fixed by an unrelated change.

## Where the remaining 18 failures are

16 are unimplemented milestones with nothing left to diagnose: media, push, E2E keys, account
data/ignored users, receipts and read markers (`TestRoomReceipts`, `TestRoomReadMarkers` —
**mandatory for AC-5**, milestone M4), `device_lists` in `/sync`, search, server notices.
Two are protocol work: `TestRoomCreate`, `TestMembershipOnEvents`.

## Next

M4 (typing, receipts, read markers) is the mandatory path: `/sync` already carries ephemeral and
account-data batches, so the transport exists and the storage and four endpoints do not.
