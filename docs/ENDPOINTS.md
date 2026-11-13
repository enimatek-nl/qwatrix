# Implemented endpoints

Every endpoint qwatrix serves, with the section of the [Matrix specification](https://spec.matrix.org/latest/) it
implements. The list is checked by `go test ./tests/api -run TestEveryEndpointIsDocumented`, which fails when the code
registers a route that is not listed here or lists one the code no longer serves — a hand-maintained table is worth
nothing if it can drift from the server it describes.

Two conventions apply to every row and are not repeated:

* **Version prefixes.** Unless marked "exact", an endpoint is served under `/_matrix/client/v3/`, `/_matrix/client/v1/`
  and the legacy `/_matrix/client/r0/` and `/_matrix/client/r0.6.1/` prefixes. Clients in the wild still ask for all
  four, and answering 404 to a client that has not been updated since 2019 is a compatibility failure the
  specification explicitly warns against.
* **Errors.** A failure is a JSON body with `errcode` and `error`, as required by
  [Application Service API § Errors](https://spec.matrix.org/latest/client-server-api/#api-compliance). An unknown path
  under `/_matrix/` answers `M_UNRECOGNIZED`, and a wrong method answers 405 with the same body.

## Discovery and versions

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/_matrix/client/versions` (exact) | [Application versions](https://spec.matrix.org/latest/client-server-api/#matrix-versions) |
| `GET` | `/capabilities` | [Capabilities](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3capabilities) |

`/capabilities` requires a device — capabilities can describe what the caller may do — and reports
`m.room_versions` (default plus the versions this server creates rooms in), `m.change_password`
as enabled because `POST /account/password` exists, and `m.knockRestricted` as enabled.
`m.server_notices` is reported disabled because the admin endpoint behind it is not implemented.

Advertises client-server API versions `v1.0`–`v1.11`. `unstable_features` is deliberately empty: advertising an
unstable flag is a promise that the unstable endpoint prefix works, so a flag appears only when its endpoint does.

## Sessions and accounts

| Method | Path | Spec |
| --- | --- | --- |
| `GET`, `POST` | `/login` | [Login](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3login) |
| `GET` | `/register` | [Registration](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3register) — the flows a client builds its registration screen from |
| `POST` | `/register` | [Registration](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3register) |
| `GET` | `/register/available` | [Check username availability](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3registeravailable) |
| `GET` | `/account/whoami` | [Whoami](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3accountwhoami) |
| `POST` | `/logout` | [Logout](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3logout) |
| `POST` | `/logout/all` | [Logout all](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3logoutall) |
| `POST` | `/account/password` | [Change password](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3accountpassword) |
| `POST` | `/account/deactivate` | [Deactivate account](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3accountdeactivate) |
| `GET` | `/devices` | [List devices](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3devices) |
| `POST` | `/delete_devices` | [Delete devices](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3delete_devices) |
| `GET`, `PUT`, `DELETE` | `/devices/{deviceID}` | [Query / update / delete a device](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3devicesdeviceid) |
| `GET`, `PUT` | `/profile/{userID}/displayname` | [Display name](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3profileuseriddisplayname) |
| `GET`, `PUT` | `/profile/{userID}/avatar_url` | [Avatar URL](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3profileuseridavatar_url) |

Registration uses [User-Interactive API Authentication](https://spec.matrix.org/latest/client-server-api/#user-interactive-api-authentication);
the only stage offered is `m.login.dummy`, and a failed `m.login.password` answers `401` with a body carrying `flows`,
`session` *and* the `errcode`/`error` of the failure, because a client has to tell "not authenticated yet" from
"authentication failed" from one response.

## Shared-secret registration (Synapse-compatible admin API)

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/_synapse/admin/v1/register` (exact) | not in the Matrix spec — the de-facto admin API Complement drives |
| `POST` | `/_synapse/admin/v1/register` (exact) | as above |

This is not standardised, so it is documented here rather than linked away to a document that would only ever
duplicate this paragraph: the `GET` answers a single-use nonce, and the `POST` requires the HMAC-SHA1 `mac` over
`nonce NUL username NUL password NUL ("admin"|"notadmin")` keyed with the registration secret. When no secret is
configured both answer `404 M_UNRECOGNIZED`, which is what makes a test that needs this endpoint skip rather than fail
against a server that has deliberately not enabled it.

## Rooms: creation, events and state

| Method | Path | Spec |
| --- | --- | --- |
| `POST` | `/createRoom` | [Create room](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3createroom) |
| `PUT` | `/rooms/{roomID}/send/{eventType}/{txnID}` | [Sending events](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3roomsroomidsendeventtypetxnid) |
| `GET`, `PUT` | `/rooms/{roomID}/state/{eventType}/{stateKey}` | [Getting / sending state](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidstateeventtypestatekey) |
| `GET`, `PUT` | `/rooms/{roomID}/state/{eventType}` | the empty-state-key form of the row above |
| `GET` | `/rooms/{roomID}/state` | [Full state](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidstate) |
| `GET` | `/rooms/{roomID}/event/{eventID}` | [Getting an event](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomideventeventid) |
| `GET` | `/rooms/{roomID}/messages` | [Message pagination](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidmessages) |
| `GET` | `/rooms/{roomID}/context/{eventID}` | [Event context](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidcontexteventid) |
| `GET` | `/rooms/{roomID}/timestamp_to_event` | [Get the event at a point in time](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidtimestamp_to_event) |
| `POST` | `/rooms/{roomID}/redact/{eventID}` | [Redaction](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidredacteventid) |
| `PUT` | `/rooms/{roomID}/redact/{eventID}/{txnID}` | the transaction-scoped form of the row above |
| `GET` | `/rooms/{roomID}/aliases` | [Room aliases](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidualiases) |

`PUT /send/{eventType}/{txnID}` is idempotent per device: a retry answers with the event ID the first attempt created.
`PUT /state/...` takes its payload under a `content` key, while `PUT /send/...` takes the content *as* the body — the
two differ in the specification and the server does not accept them interchangeably.

## Rooms: membership

| Method | Path | Spec |
| --- | --- | --- |
| `POST` | `/rooms/{roomID}/join` | [Join room](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidjoin) |
| `POST` | `/join/{roomIDOrAlias}` | [Join by ID or alias](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3joinroomidoralias) |
| `POST` | `/rooms/{roomID}/leave` | [Leave room](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidleave) |
| `POST` | `/rooms/{roomID}/forget` | [Forget room](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidforget) |
| `POST` | `/rooms/{roomID}/invite` | [Invite](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidinvite) |
| `POST` | `/rooms/{roomID}/kick` | [Kick](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidkick) |
| `POST` | `/rooms/{roomID}/ban` | [Ban](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidban) |
| `POST` | `/rooms/{roomID}/unban` | [Unban](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3roomsroomidunban) |
| `GET` | `/rooms/{roomID}/members` | [List members](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidmembers) |
| `GET` | `/rooms/{roomID}/joined_members` | [Joined members](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3roomsroomidjoined_members) |
| `GET` | `/joined_rooms` | [Joined rooms](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3joined_rooms) |

`/rooms/{roomID}/join` accepts a room ID or a local alias. Who may perform a membership change is decided by the
room's own events through the authorization rules — never by a special case in a handler.

A `restricted` (MSC3083) room is honoured for a join of a room this server holds: each `allow` entry of the
ratified type `m.room_membership` names one room — in practice a space — and membership of *that* room is
what qualifies, so a space owns the doorways of the rooms it contains without any of them repeating a
membership list. An entry of any other type is ignored, which is the direction that refuses rather than
the one that admits. The join event then carries `join_authorised_via_users_server`, naming one of this
server's own users who is joined to **both** rooms and may issue invites: that user is the witness the
join is checked against, so the joiner cannot be named, and the field is dropped from a client's request
before the event is built — a client allowed to name its own authoriser could enter any restricted room,
signed by itself. What is not implemented is authorising *another server's* join: `/make_join` does not
consult these rules yet, so a remote user who qualifies is refused rather than admitted.

A join for a room this server does not hold is offered to the servers the client named with
`?server_name=` (or `?via=`) and, failing those, to the room ID's own home server. None of them having
admitted the user is reported as an error naming the room; it is never answered with a half-built room.

A room joined from another server is a room whose past this server does not have: joining buys its current
state and none of its history. The events this server later fetches to answer a question about that past
are stored *detached* — readable, pageable, and explicitly not part of the room's graph. Detached history
moves no state, opens no branch, and is never delivered as news: `/sync` does not put it in a timeline, and
the unread badges do not count it, because a user who paged up to last year has not received last year.
`/messages` and `/context` page through it like any other history, which is the only reason it was fetched.

`/rooms/{roomID}/timestamp_to_event?ts=&dir=` searches the room's history for a moment: `dir=f` finds the
earliest event stamped at or after it, `dir=b` the latest at or before. The direction is required because
they are opposite questions, and in a room whose events share a millisecond they find different events —
ties are broken by the order the events were written, oldest going forward and newest going back. Running
past either end finds nothing (`M_NOT_FOUND`) rather than clamping to the nearest event, and the reader is
held to the room's history rule exactly as `/messages` holds it, so the two can never disagree about who
may see the room's past.

`/rooms/{roomID}/context/{eventID}?limit=` returns one event with its neighbours: `events_before` newest-first
and `events_after` oldest-first, each array ordered away from the centre because that is the direction a
client draws, plus the room's state as it stood at the centre so a client that jumped here can render what
it was given. The `limit` covers the two arrays together, and the event itself is always returned, even at
`limit=0`. `start` and `end` name the boundary the response reached rather than the events it carried, so
paging on in either direction continues with the next event instead of repeating the last one.

`/rooms/{roomID}/state/{eventType}` with no state key is the empty state key, which is a real
tuple rather than an absent one; the trailing-slash spelling and a state key containing a
slash are served by the same route.

## Aliases and the room directory

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/directory/room/{alias}` | [Resolve alias](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3directoryroomalias) |
| `PUT`, `DELETE` | `/directory/room/{alias}` | [Create / delete alias](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3directoryroomroomalias) |
| `PUT`, `DELETE` | `/directories/alias/{alias}` | [Create / delete alias](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3directoriesaliasalias) |
| `GET`, `POST` | `/publicRooms` | [Public rooms](https://spec.matrix.org/latest/client-server-api/#post_matrixclientv3publicrooms) |
| `GET`, `PUT` | `/directory/list/room/{roomID}` | [Room directory visibility](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3directorylistroomroomid) |

## Presence

| Method | Path | Spec |
| --- | --- | --- |
| `GET`, `PUT` | `/presence/{userID}/status` | [Get presence](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3presenceuseridsstatus), [set presence](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3presenceuseridsstatus) |

Both require a device, and `PUT` only ever acts on the caller. Presence is stored per local
user and travels on the same stream as events, so a client's single `since` token decides
which presence changes it has already been told — and it is delivered only to users who share
a room with the subject, which is the privacy rule of the feature.
`GET /sync?set_presence=` updates the caller's presence as part of the sync.

## End-to-end encryption keys

| Method | Path | Spec |
| --- | --- | --- |
| `POST` | `/keys/upload` | [upload keys](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3keysupload) |
| `POST` | `/keys/query` | [query keys](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3keysquery) |
| `GET` | `/keys/changes` | [key changes](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3keyschanges) |
| `GET` | `/keys/status` | [key status](https://spec.matrix.org/latest/client-server-api#get_matrixclientv3keysstatus) |
| `POST` | `/keys/claim` | [claim keys](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3keysclaim) |
| `POST` | `/user_directory/search` | [search user directory](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3user_directorysearch) |

## Media

| Method | Path | Spec |
| --- | --- | --- |
| `POST` | `/_matrix/media/v3/upload` | [upload](https://spec.matrix.org/latest/client-server-api/#post_matrixmediav3upload) |
| `POST` | `/_matrix/media/v1/upload` | [upload (legacy prefix)](https://spec.matrix.org/latest/client-server-api/#post_matrixmediav3upload) |
| `GET` | `/_matrix/media/v3/download/{serverName}/{mediaID}` | [download](https://spec.matrix.org/latest/client-server-api/#get_matrixmediav3downloadservernamemediaid) |
| `GET` | `/_matrix/media/v3/download/{serverName}/{mediaID}/{fileName}` | Legacy download naming the file the client wants it saved as — the name comes from the request, the media ID is the only lookup key |
| `GET` | `/_matrix/client/v1/media/download/{serverName}/{mediaID}/{fileName}` | Authenticated download with the same filename rule |
| `GET` | `/_matrix/media/v1/download/{serverName}/{mediaID}` | [download (legacy prefix)](https://spec.matrix.org/latest/client-server-api/#get_matrixmediav3downloadservernamemediaid) |
| `HEAD` | `/_matrix/media/v3/download/{serverName}/{mediaID}` | [download headers](https://spec.matrix.org/latest/client-server-api/#head_matrixmediav3downloadservernamemediaid) |
| `HEAD` | `/_matrix/media/v3/download/{serverName}/{mediaID}/{fileName}` | The same, headers only |
| `GET` | `/_matrix/media/v3/config` | [config](https://spec.matrix.org/latest/client-server-api/#get_matrixmediav3config) |
| `GET` | `/_matrix/media/v1/config` | [config (legacy prefix)](https://spec.matrix.org/latest/client-server-api/#get_matrixmediav3config) |
| `GET` | `/_matrix/client/v1/media/download/{serverName}/{mediaID}` | [authenticated download](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1mediadownloadservernamemediaid) |
| `GET` | `/_matrix/media/v3/thumbnail/{serverName}/{mediaID}` | [thumbnail](https://spec.matrix.org/latest/client-server-api/#get_matrixmediav3thumbnailservernamemediaid) — `width`, `height`, `method=scale|crop`; PNG out, generated once and cached |
| `HEAD` | `/_matrix/media/v3/thumbnail/{serverName}/{mediaID}` | The same, headers only |
| `GET` | `/_matrix/client/v1/media/thumbnail/{serverName}/{mediaID}` | [authenticated thumbnail](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1mediathumbnailservernamemediaid) |
| `HEAD` | `/_matrix/client/v1/media/thumbnail/{serverName}/{mediaID}` | The same, headers only |
| `GET` | `/_matrix/client/v1/room_summary/{roomIDOrAlias}` | [Get a summary of a room](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1room_summaryroomidoralias) — a room ID or an alias, and the join rule plus the rooms a restricted join admits |
| `GET` | `/_matrix/client/v1/rooms/{roomID}/hierarchy` | [Get a hierarchy of space rooms](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidhierarchy) — depth-first walk of the `m.space.child` tree, permission-filtered per room, with `limit`/`max_depth`/`suggested_only` and `next_batch` |
| `GET` | `/_matrix/client/v1/media/config` | [authenticated config](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1mediaconfig) |

Media is stored on disk under `<data-dir>/media` with its metadata in the database; uploads are
capped by `-media-limit` and served with `Content-Security-Policy: default-src 'none'` and
`X-Content-Type-Options: nosniff`. Thumbnails and the URL preview endpoint are not implemented, and
media from another server is answered with 404 rather than fetched.

## Event relations

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/rooms/{roomID}/relations/{eventID}` | [relations](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidrelationseventid) |
| `GET` | `/rooms/{roomID}/relations/{eventID}/{relType}` | [relations of one type](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidrelationseventidreltype) |
| `GET` | `/rooms/{roomID}/relations/{eventID}/{relType}/{eventType}` | [relations of one type and event type](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv1roomsroomidrelationseventidreldtypeeventtype) |

## To-device messages

| Method | Path | Spec |
| --- | --- | --- |
| `PUT` | `/sendToDevice/{eventType}/{txnID}` | [send to device](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3sendtodeviceeventtypetxnid) |

Queued messages are delivered in the `to_device` section of `/sync`.

A recipient who lives on another homeserver is not dropped and not delivered locally: that recipient's
share is separated out and queued for the server named in their user ID, which delivers it to the devices
it holds and verifies the signatures on the payload itself. Two details of that hand-off are this server's
problem even though the far server does the delivering. The client's transaction ID travels with it —
not because the destination is told the client's bookkeeping, but because it is the only identifier both
sides have for "the same send", and the destination needs that to tell a retry from a second key. And a
recipient naming every device with `*` is expanded by the server that owns those devices, since only it
knows that the laptop was added an hour ago.

## Push rules

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/pushrules/` | [get all push rules](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrules) |
| `GET` | `/pushrules` | [get all push rules](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrules) |
| `GET` | `/pushrules/{scope}/{kind}/{ruleID}` | [get a push rule](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrulesscopekindruleid) |
| `PUT` | `/pushrules/{scope}/{kind}/{ruleID}` | [set a push rule](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3pushrulesscopekindruleid) |
| `DELETE` | `/pushrules/{scope}/{kind}/{ruleID}` | [delete a push rule](https://spec.matrix.org/latest/client-server-api/#delete_matrixclientv3pushrulesscopekindruleid) |
| `GET` | `/pushrules/{scope}/{kind}/{ruleID}/actions` | [get actions](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrulesscopekindruleidactions) |
| `PUT` | `/pushrules/{scope}/{kind}/{ruleID}/actions` | [set actions](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3pushrulesscopekindruleidactions) |
| `GET` | `/pushrules/{scope}/{kind}/{ruleID}/enabled` | [is enabled](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrulesscopekindruleidenabled) |
| `PUT` | `/pushrules/{scope}/{kind}/{ruleID}/enabled` | [set enabled](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3pushrulesscopekindruleidenabled) |
| `GET` | `/pushrules/{scope}/{kind}/{ruleID}/pattern` | [get pattern](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3pushrulesscopekindruleidpattern) |
| `PUT` | `/pushrules/{scope}/{kind}/{ruleID}/pattern` | [set pattern](https://spec.matrix.org/latest/client-server-api/#put_matrixclientv3pushrulesscopekindruleidpattern) |

Accounts are seeded with the specification's default rules on first use, and the whole set is
delivered as `m.push_rules` account data in `/sync`.

## Read state, typing, and account data

| Method | Path | Spec |
| --- | --- | --- |
| `PUT` | `/rooms/{roomID}/typing/{userID}` | [send typing](https://spec.matrix.org/latest/client-server-api#put_matrixclientv3roomsroomidtypinguserid) |
| `POST` | `/rooms/{roomID}/receipt/{receiptType}/{eventID}` | [receipt](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3roomsroomidreceiptreceipttypeeventid) |
| `POST` | `/rooms/{roomID}/read_markers` | [read markers](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3roomsroomidread_markers) |
| `GET`, `PUT`, `DELETE` | `/user/{userID}/account_data/{type}` | [global account data](https://spec.matrix.org/latest/client-server-api#get_matrixclientv3useruseridaccount_datatype) |
| `GET`, `PUT`, `DELETE` | `/rooms/{roomID}/account_data/{type}` | [room account data](https://spec.matrix.org/latest/client-server-api#get_matrixclientv3roomsroomidaccount_datatype) |
| `GET`, `PUT`, `DELETE` | `/user/{userID}/rooms/{roomID}/account_data/{type}` | same, older spelling |

None of this is room history, and none of it may reach a member who is not the reader — which is
why receipts, typing indicators, and account data are not events. They travel on `/sync` all the
same: typing as `m.typing` and receipts as `m.receipt` in a room's `ephemeral` batch, the
fully-read marker and account data in the `account_data` batches.

Two rules worth stating because they are easy to get backwards:

* A typing indicator is reported as it stands *now*, including an empty list, rather than only on
  change. A client can only stop showing a spinner if the server says nobody is typing; silence
  is indistinguishable from a sync that has not caught up. Typing lapses on its own after the
  client's `timeout`, because a browser tab that dies must not leave a person typing forever.
  Neither `m.typing` nor `m.receipt` carries a `room_id` down `/sync` — the room is the section
  the event is in, and the spec reserves that field for federation.
* `m.fully_read` is room account data, not a receipt. It is the reader's own bookmark and is never
  shown to anybody else, whereas a read receipt is a statement the room sees. `POST
  /read_markers` moves both, because clients send them together and two read positions in one room
  would disagree.

Account data is namespaced by account, so a room's other members cannot read it, and the answer
to a peer asking is the same as for a key that was never written: the data is keyed by its owner,
so they are being shown their own absent entry rather than being refused somebody else's.

## Filters

| Method | Path | Spec |
| --- | --- | --- |
| `POST` | `/user/{userID}/filter` | [create filter](https://spec.matrix.org/latest/client-server-api#post_matrixclientv3useruseridfilter) |
| `GET` | `/user/{userID}/filter/{filterID}` | [get filter](https://spec.matrix.org/latest/client-server-api#get_matrixclientv3useruseridfilterfilterid) |

`GET /sync?filter=` accepts both spellings the specification allows: a filter ID returned here, or
the filter object inlined as JSON. An ID that names no filter of this account is a `400`, because
treating an unknown ID as "a filter that could not be parsed" and ignoring it would silently
return a stream the client never asked for.

The definition is stored and returned verbatim, including keys this server does not act on. A
client that saves a filter and reads it back must get what it wrote; a lossy round trip would
mean a filter changed by being saved.

Filters are per-account and the ID space is per-account, so `GET` on another user's filter is
refused — reading it would be reading their configuration, and using it on `/sync` would serve
them a stream shaped by somebody else's rules.

## Sync

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/sync` | [Sync](https://spec.matrix.org/latest/client-server-api/#get_matrixclientv3sync) |

`since` tokens are `s` followed by a global stream position; an unparsable token answers `401 M_UNKNOWN_TOKEN`, which
is the specification's signal for a client to discard its token and start again. `timeout` long-polls on an
in-process notification rather than by polling the database.

## Server-server (federation) API

Federation is served over HTTPS on its own address (`-federation-addr`), because it is a second
protocol with a second trust model: nothing here is authenticated by an access token, and a request
that arrives unauthenticated is a peer asking who this server is.

| Method | Path | Spec |
| --- | --- | --- |
| `GET` | `/_matrix/federation/v1/version` | [GET federation version](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1version) |
| `POST` | `/_matrix/key/v2/query` | [Query keys in a batch](https://spec.matrix.org/latest/server-server-api/#post_matrixkeyv2query) — served for this server's own name; other names are left out of the result, as the specification allows, because holding another server's keys is a notary cache this server does not have |
| `GET` | `/_matrix/key/v2/server` | [Get server keys](https://spec.matrix.org/latest/server-server-api/#get_matrixkeyv2server) |
| `GET` | `/_matrix/key/v2/server/{keyID}` | [Get specific server key](https://spec.matrix.org/latest/server-server-api/#get_matrixkeyv2serverkeyid) |
| `GET` | `/_matrix/federation/v1/make_join/{roomID}/{userID}` | [Make a join event](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1make_joinroomiduserid) |
| `PUT` | `/_matrix/federation/v1/send_join/{roomID}/{eventID}` | [Send a join event (legacy envelope)](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv1send_joinroomideventid) |
| `PUT` | `/_matrix/federation/v2/send_join/{roomID}/{eventID}` | [Send a join event](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv2send_joinroomideventid) |
| `PUT` | `/_matrix/federation/v1/send_leave/{roomID}/{eventID}` | [Send a leave event (legacy)](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv1send_leaveroomideventid) — a membership ended by a peer, verified like a relayed event
| `PUT` | `/_matrix/federation/v2/send_leave/{roomID}/{eventID}` | [Send a leave event (v2)](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv2send_leaveroomideventid) — as v1, with the answer as an object rather than the legacy unsigned pair |
| `GET` | `/_matrix/federation/v1/event_auth/{roomID}/{eventID}` | [Get the auth chain of an event](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1event_authroomideventid) — the events that authorised one event, transitively
| `GET` | `/_matrix/federation/v1/event/{eventID}` | [Get a single event](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1eventeventid) |
| `GET` | `/_matrix/federation/v1/timestamp_to_event/{roomID}` | [MSC3059 — get the event closest to a timestamp](https://github.com/matrix-org/matrix-spec-proposals/pull/3059) |
| `GET` | `/_matrix/federation/v1/state_ids/{roomID}` | [Get the current state of a room by ID](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1state_idsroomid) |
| `GET` | `/_matrix/federation/v2/state_ids/{roomID}` | [Get the current state of a room by ID, with presentation](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv2state_idsroomid) |
| `GET` | `/_matrix/federation/v1/state/{roomID}` | [Get the current state of a room](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1stateroomid) |
| `GET` | `/_matrix/federation/v2/state/{roomID}` | [Get the current state of a room, signed](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv2stateroomid) |
| `GET` | `/_matrix/federation/v1/backfill/{roomID}` | [Backfill](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1backfillroomid) |
| `GET` | `/_matrix/federation/v2/event_auth/{roomID}/{eventID}` | [Get the auth chain of an event (v2)](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv2event_authroomideventid) — as v1, plus the `rooms` set for auth events missing from the room (always empty here)
| `PUT` | `/_matrix/federation/v1/invite/{roomID}/{eventID}` | [Sign an invite for our user (legacy)](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv1inviteroomideventid) |
| `PUT` | `/_matrix/federation/v2/invite/{roomID}/{eventID}` | [Sign an invite for our user](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv2inviteroomideventid) |
| `PUT` | `/_matrix/federation/v1/send/{transactionID}` | [Send a transaction](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv1sendtransactionid) |
| `PUT` | `/_matrix/federation/v2/send/{transactionID}` | [Send a transaction](https://spec.matrix.org/latest/server-server-api/#put_matrixfederationv2sendtransactionid) |
| `GET` | `/_matrix/federation/v1/user/devices/{userID}` | [Get device lists](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1userdevicesuserid) — only for this server's own users |
| `POST` | `/_matrix/federation/v1/user/keys/query` | [query user device keys](https://spec.matrix.org/latest/server-server-api#post_matrixfederationv1userkeysquery) |
| `POST` | `/_matrix/federation/v1/user/keys/claim` | [claim user keys](https://spec.matrix.org/latest/server-server-api#post_matrixfederationv1userkeysclaim) |

These two are what an encrypted conversation between two homeservers needs before it can start: the
first returns the device key documents of this server's users, and the second *spends* a one-time key so
that a device on the far side can wrap the first message of an Olm session to it.

Both answer for the domain in the user ID and for nobody else's, which is the same rule every other key
path here runs on and for the same two reasons. A device's keys are believed by the client that receives
them because they came from the domain in the user ID, so a server that answered for another domain would
let any peer mint an identity for any user. And a one-time key is spent by being handed over, so serving
one for a user this server does not host would be spending a secret that is not this server's to spend.
A user this server does not host is therefore left out of the answer entirely rather than answered with
an empty set: "no devices" and "not mine to say" are different facts, and the first one tells a client to
stop trying to encrypt to somebody it can reach.

A peer that claims a key and then vanishes has used it, and that is the protocol working rather than
leaking: reserving a key in case the asker comes back would let any peer freeze this server's devices'
key pools by asking and not finishing. A device notices its stock running low through
`GET /keys/status`, which answers how many unsent one-time keys *this* device holds — keyed by algorithm,
so a client that only knows `signed_curve25519` need not know what else exists.

The sending side is the same pair of requests in the other direction, and it is the half that decides
whether this server's own users can begin a conversation. A claim for a device that lives elsewhere is
forwarded to the server named in that user ID, and only there: device IDs are chosen by whoever registers
them, so two servers hand out the same identifier routinely, and a lookup that fell through to a local
device with a stranger's name would spend a local key and tell the client it had started a session with
somebody it has never met.

A peer that cannot be asked is named in the response's `failures` map rather than left out of it. The two
ways a claim comes back empty are not the same news — a device that has run out of prepaid keys needs its
owner to open it, while a homeserver that was unreachable needs nothing from anybody and the client should
ask again shortly — and an empty answer alone cannot tell them apart. Nothing is retried, either: a claim
that is resent after it succeeded spends a second key, so the client that wants another attempt simply
makes one.
| `GET` | `/_matrix/federation/v1/media/download/{mediaID}` | [MSC2448](https://github.com/matrix-org/matrix-spec-proposals/pull/2448) — signed; files this server originates only |
| `GET` | `/_matrix/federation/v1/download/{serverName}/{mediaID}` | [Download media from its origin](https://spec.matrix.org/latest/server-server-api/#get_matrixfederationv1downloadservernamemediaid) — signed, and only for this server's own files; it is also the first path this server tries when fetching somebody else's, before the legacy `/_matrix/media/v3/download/...` and the MSC2448 draft it also serves |

The version string and the published signing keys are also answered on the **client** listener. They
are public facts about this server rather than something a caller earns by being a peer, and a
deployment is usually probed at one address; nothing else in the server-server API is — transactions,
joins, device lists and media all require a peer that has proved who it is.

The key response is signed by the key it publishes, so a peer needs to believe only the certificate
of the connection it opened. An unknown key ID answers `404 M_NOT_FOUND`: "this server never had
that key" is a fact a peer can act on, while an empty `200` is indistinguishable from a server that
has not finished answering.

When `-tls-cert`/`-tls-key` are not given the listener generates a self-signed certificate for the
server name in the data directory. Peers will reject it, which is the intended outcome: an untrusted
certificate is a refusal, and federation should refuse rather than pretend.

## Planned, and deliberately not implemented

Federation is being built in stages. Served today: implementation version, the signing-key publishing
and lookup a peer must believe before anything else, the join handshake — `make_join` offers an
unsigned join event, `send_join` accepts the signed result and returns the room's state and
authentication chain so the peer can check the room for itself — transaction ingest, by which a
peer delivers events this server then authorises against the room, and the same handshake in the
outbound direction, by which this server joins one of its users into a room that only a peer holds, and
device-list lookup, by which a client asking about a user who lives elsewhere is answered from that
user's own homeserver.

A device list is asked for with a signature and answered only for this server's own users. Both halves
of that matter: the answer decides whom a client encrypts to, so an unsigned question could be answered
by whoever is watching the connection, and a server willing to describe `@alice:other.example` is a
server where any peer can mint a device for any user anywhere. A peer that cannot be reached leaves that
user with no devices in the answer rather than failing the query, because a query usually covers a whole
room at once and one dead homeserver must not silence every encrypted room its users share.

Delivery runs the other way too, and through a ledger rather than inline. When this server originates
an event it writes a copy-to-send row for every other homeserver with a member in that room, in the same
transaction as the event, and a worker keeps trying until each peer confirms it. A peer that is
unreachable therefore cannot make a local user's `PUT /send` fail — the message was accepted and is
true — and cannot make it disappear either, because the obligation was written down before anything could
be sent. Failed attempts back off up to ten minutes apart; a delivery is abandoned only after many
hundreds of attempts and a week, and abandoning one is logged as an error, because a message that was
given up on is the fact an operator most needs to find out about.

Not done: this server sends its own events and not other servers' ones. A room spanning three homeservers
therefore hears each participant's messages from the servers its members are directly connected to, and
forwarding is the work that closes the rest.

Ingest answers event by event: a transaction whose third event is bogus keeps the other two and
reports which one did not land. It is idempotent by transaction ID, because a peer that did not hear
the answer sends the transaction again, and a room where every message appears twice is a broken room.
An event is refused if its own signature cannot be verified, if it follows history this server does not
have (that is a gap to fill, not a branch to invent), or if the room's rules would not allow it.

Ephemeral events (EDUs) are parsed — a transaction containing one still delivers its events — and two
kinds are acted on. `m.device_list_update` is applied for users of the server that signed it, so that
somebody who shares a room learns that the keys they hold moved elsewhere. `m.direct_to_device` is
delivered into the recipient's device queue, and it is the only way an end-to-end key share crosses
from one homeserver to another — without it, encryption works between two devices on this server and
nowhere else.

Delivery is remembered rather than trusted. A message carries an idempotence identifier chosen by the
sending server, and a peer that retries must not hand a client the same Megolm key twice: a replayed
message key can be replayed as a message. The memory is keyed on the sender *and* that identifier,
because two servers are each free to invent `message_id: 1`.

Everything else — typing indicators, remote receipts, presence — is discarded, which the response
cannot report because ephemeral events are acknowledged as a whole or not at all; the drop is therefore
logged by type rather than silent, and it remains open work.

The sending side queues a device message in the same durable ledger as an event, for the reason the
specification gives for retrying ephemeral events at all: a key share dropped because a peer was briefly
asleep is not a notification that arrived late, it is a message that can never be read.

An event that follows history this server does not have is not refused straight away: the refusal names
the missing event, and the transport asks the peer that reported it for exactly that event
(`GET /_matrix/federation/v1/event/{eventID}`) before trying again — a bounded number of times, because
a peer can always claim to be missing history. A fetched event is verified like any other and must
belong to the room whose gap is being filled; otherwise gap filling would be a door into every other
room. When nothing can be fetched the peer gets the original refusal, naming what is missing, and the
event is not stored: a room assembled from events that do not attach to known history has sections its
own members cannot read.

That limit is stated plainly because it is a real one: when a peer can supply only *part* of a run of
history, the events it supplies still hang from something neither server has, and this server refuses the
lot. Closing such a gap is not a matter of fetching more — it needs the peer's `/state_ids` and `/state`
answer to be stored as the state *at* the event (an outlier event), which is separate work. A fragment of
history kept because it was available is a hole a client can fall into.

An outbound join is a peer's answer, and a peer's answer is an input from a party that benefits from
this server being wrong, so it is checked as one. Every state and auth event must parse under the
version the peer named, carry this room's ID, and verify against its sender's published key. The room's
rules are then applied by this server, to the state as it stood *before* the join: some peers answer
`send_join` with the join event already inside the state they return, and a membership event found in
the authorising state counts as the user's previous membership, so authorising against the answer as
given would admit anyone anywhere on the strength of the very event being questioned. Peers differ on
that point — Synapse includes the join, complement's reference federation server deliberately builds
its state before inserting it — so its presence is not required: a peer that replied 200 has accepted
the join, which this server knows independently, having built the event from the peer's own `make_join`
template and verified it against the peer's signing key.

Two of these refusals are worth naming because they are how a peer's lie would otherwise be read as a
fact. A join whose `auth_events` leave out an event the room's rules require — most importantly the join
rules — is refused, because an absent join rule does not refuse anybody, it reads as "public". And a
state holding two events for the same `(type, state_key)` is refused, because then the room's rules mean
whichever entry was read last; the single exception is the join event itself, since a correct answer
carries the pre-join state plus the join, and so legitimately holds two membership events for a user who
was invited or had left.

Next come state and event reads (`/state`, `/state_ids`, `/event`, `/backfill`), the EDUs above, and
device list and to-device propagation between servers.

One limitation deserves its own sentence because it is not this server being cautious: an `invite`
membership event has to be signed by the invited user's homeserver as well as the sender's, and this
server does not yet co-sign the invites it sends. A remote server will therefore reject an invitation
issued here to a remote user.

A signed federation request is authenticated twice over: the request signature covers the method, URI,
origin, destination and body, and the event carries its own signature so that it stays checkable by
servers that never saw this conversation. Both are verified before a join touches a room, and the
room's own authorisation rules are then applied by the room server — a private room does not become
public because the request arrived on the federation port, and a server may submit a join only for a
user on its own domain.

Every read of room contents (`/event`, `/timestamp_to_event`, `/state_ids`, `/state`, `/backfill` and
`/event_auth`) is gated by one rule: **a server may read what its users are in.** A peer with no joined user in the room is refused even
for an event whose ID it already knows, because event IDs are not secrets and "may I have that event?" is
otherwise a way to read a room without ever joining it. An event this server has never heard of is a `404`
and an event it holds but will not show is a `403`, in that order, so the difference between the two is
only visible to a server that is already entitled to the room. A rejected event is answered as if it did
not exist: keeping a falsehood because a peer sent it is bookkeeping, not a reason to spread it.

`/event_auth` carries one exception, and it is this room's membership in the asking server: a peer being
shown the join, leave, ban or invite of **one of its own users** may fetch the rules that authorised that
decision even before it is in the room. The event has already been delivered to it, and refusing its
authority would leave the peer holding an event it cannot check — which is not a safety gain. What the
exception is not is a key: the peer may read the rules behind that membership and nothing else in the room.

`/backfill` gives a window of history ending just before a point the peer names, newest-first, so the
asking server can chain it onto what it holds and ask again until a window comes back empty. `since` is
accepted as an event ID or as one of this server's own stream tokens — both are things a peer can legitimately
hold — and a `since` this server does not recognise is refused rather than answered with the newest window,
because silently restarting from the tip is how a backfill loop spins forever. The window is capped at 100,
which is the specification's own cap for this endpoint.

`/state_ids` and `/state` are the same read in two shapes — IDs, and events — and both go through one
function, so a peer that asked one then the other cannot be shown a state that moved between the questions.
Version 2 of `state_ids` adds `presentation`; this server only ever answers with the state it acts on, which
is the whole of it, so it says `full` and means it. The auth chain that travels with them is the closure
across the whole state, with each event named once, because a peer counting the chain would otherwise be
counting something that depends on the order the state happened to be written in.

`/timestamp_to_event` answers where this server's copy of the room's history was, not where all of it
was — a resident server that has backfilled nothing points at the nearest event it holds. That is the
contract: the asking server is expected to backfill around the answer it gets.

A window served by `/backfill` includes what this server itself fetched from somebody else. Passing on
history this server merely remembers is honest because the asking peer verifies every event it is given —
the same rule that lets this server accept events it did not authorize. What this server refuses to do is
place a fetched event in the room's graph: fetched events move no state, open no branch, and are not
answered for by `/send`.

The client's `/timestamp_to_event` asks a peer when the room's home server is not this one, and keeps what
comes back. It does so *before* consulting its own store, because a store holding only this week's events
is exactly the store that answers a question about last March with this week's join event and looks
confident doing it. If the peer cannot be reached, the local answer is given: a server that cannot reach a
neighbour is entitled to answer from what it knows, but not to pretend that is everything.

`make_join` is a description of what a valid join would look like, not a permission slip: the answer
to "may this user join?" is asked again, of the signed event, at `send_join`. An unknown room and an
unjoinable one are answered the same way so the endpoint cannot be used to ask which room IDs exist
here.

## Application services

An application service is a program this server has been told to trust — typically a bridge to another
network. Trust is configured with files, read at start-up from
`-app-service-config-files` / `QWATRIX_APP_SERVICE_CONFIG_FILES` (comma-separated, globs allowed), in
Synapse's own registration format so an existing registration can be dropped in unchanged. A file that
cannot be read or understood stops the server: a delegation nobody read is not a delegation.

| What | How |
|---|---|
| Authentication | the registration's `as_token`, presented as an ordinary access token. It is not stored anywhere, so revoking it means editing the file |
| Who it speaks for | its own `sender_localpart`, or any local user named by `?user_id=` **inside a `users` namespace**; anything else is `403 M_FORBIDDEN` |
| Virtual users | the account and its device are created the moment the service first speaks for that user, with no password — and a deactivated account stays deactivated, because that was this server's decision, not the service's |
| `?ts=` on `PUT /send/{txnID}` and `PUT /state/{type}/{key}` | honoured for a service and **ignored for everybody else** (MSC3316): importing history means saying when a message happened, and letting any client choose that would let it rewrite when a room began or retroactively claim it was there first |
| `/sync` | refused with `403`: it is one person's devices catching up, and a service is told what it cares about over its own URL instead |

Namespace patterns are matched against the **whole** user ID and anchored, so a namespace naming `alice`
does not also cover `alice-evil`. `rooms` and `aliases` namespaces are read and reported but not yet
enforced anywhere, because nothing this server does today consults them.

**Not implemented:** the outbound half. Events are not pushed to the service's `url`, so a service
learns nothing it has to be told about; ephemeral events, `de.sorunome.msc2409.push_ephemeral`,
`org.matrix.msc3202` encryption and the transaction endpoint are all absent. A bridge that only sends
works; one that also receives does not, and it will fail quietly rather than loudly, which is worth
knowing before trusting one.

Deliberately out of scope for the foreseeable future: OIDC/SSO, third-party invites, `/search`, sliding
sync, and room version 12's state-DAG variant — the reasoning is in [REQUIREMENTS.md](REQUIREMENTS.md).
