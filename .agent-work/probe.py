#!/usr/bin/env python3
"""Probe a running qwatrix the way Complement drives it, over real HTTP."""
import hmac, hashlib, json, sys, urllib.parse, urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18065"
SECRET = "complement"


def mac(nonce, user, password, admin=False):
    body = f"{nonce}\0{user}\0{password}\0{'admin' if admin else 'notadmin'}"
    return hmac.new(SECRET.encode(), body.encode(), hashlib.sha1).hexdigest()


def call(method, path, token=None, body=None, raw=None):
    url = BASE + path
    if token:
        url += ("&" if "?" in url else "?") + "access_token=" + urllib.parse.quote(token)
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req) as res:
            return res.status, json.loads(res.read() or b"{}")
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload or b"{}")
        except json.JSONDecodeError:
            return e.code, {"raw": payload[:200].decode(errors="replace")}


def register(localpart):
    # A nonce is single-use, exactly as Synapse's is, so each registration fetches one.
    nonce = call("GET", "/_synapse/admin/v1/register")[1]["nonce"]
    status, body = call("POST", "/_synapse/admin/v1/register", body={
        "nonce": nonce, "username": localpart, "password": "pw", "admin": False,
        "mac": mac(nonce, localpart, "pw"),
    })
    if "access_token" not in body:
        raise SystemExit(f"register {localpart} failed: {status} {body}")
    return body["access_token"], body["user_id"]


alice, alice_id = register("alice")
bob, bob_id = register("bob")

status, room = call("POST", "/_matrix/client/v3/createRoom", alice, {"name": "probe"})
room_id = room["room_id"]
print(f"createRoom -> {status} {room_id}")

enc = urllib.parse.quote(room_id, safe="")

status, res = call("POST", f"/_matrix/client/v3/rooms/{enc}/join", bob, {})
print(f"POST /rooms/{{id}}/join -> {status} {res}")

status, res = call("POST", f"/_matrix/client/v3/join/{enc}?server_name=hs1", bob, {})
print(f"POST /join/{{id}}?server_name -> {status} {res}")

status, res = call("POST", f"/_matrix/client/v3/join/{enc}", bob, {})
print(f"POST /join/{{id}} -> {status} {res}")

# Alias handling end to end: create an alias two ways, resolve it, join through it.
alias = "#probe:hs1"
status, res = call("PUT", f"/_matrix/client/v3/directories/alias/{urllib.parse.quote(alias, safe='')}",
                   alice, {"room_id": room_id})
print(f"PUT /directories/alias -> {status} {res}")
status, res = call("GET", f"/_matrix/client/v3/directory/room/{urllib.parse.quote(alias, safe='')}")
print(f"GET /directory/room -> {status} {res}")
status, res = call("POST", f"/_matrix/client/v3/join/{urllib.parse.quote(alias, safe='')}", bob, {})
print(f"POST /join/{{alias}} -> {status} {res}")

status, res = call("GET", "/_matrix/client/v3/capabilities", alice)
print(f"GET /capabilities -> {status} {json.dumps(res)[:120]}")
status, res = call("GET", "/_matrix/client/v3/capabilities")
print(f"GET /capabilities (no token) -> {status} {res}")
status, res = call("POST", "/_matrix/client/v3/publicRooms", alice, {"filter": {"generic_search_term": "probe"}})
print(f"POST /publicRooms filter -> {status} {json.dumps(res)[:160]}")

# Profile reads without a token.
status, res = call("GET", f"/_matrix/client/v3/profile/{urllib.parse.quote(alice_id, safe='')}/displayname")
print(f"GET profile displayname (no token) -> {status} {res}")

# Oversized event content must be 413 M_TOO_LARGE.
big = {"body": "x" * 70000}
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc}/state/m.probe/big", alice, big)
print(f"PUT oversized state -> {status} {res}")

# Invalid UTF-8 in a JSON string must be M_NOT_JSON.
status, res = call("POST", "/_matrix/client/v3/register", raw=b'{ "test":"a\x81" }')
print(f"POST /register invalid utf8 -> {status} {res}")

# Infinity is not JSON.
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc}/send/m.room.message/txn-probe-1", alice,
                   raw=b'{"body": Infinity}')
print(f"PUT send Infinity -> {status} {res}")

# --- second round: joining a public room as a stranger, and odd bodies
status, pub = call("POST", "/_matrix/client/v3/createRoom", alice, {"preset": "public_chat", "name": "public probe"})
pub_id = pub["room_id"]
enc_pub = urllib.parse.quote(pub_id, safe="")
print(f"createRoom public -> {status} {pub_id}")
status, res = call("POST", f"/_matrix/client/v3/join/{enc_pub}?server_name=hs1", bob, {})
print(f"POST /join/{{public room}} as stranger -> {status} {res}")
status, res = call("POST", f"/_matrix/client/v3/rooms/{enc_pub}/join", bob, {})
print(f"POST /rooms/{{public}}/join -> {status} {res}")

# A send whose body is valid JSON but not an object.
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc_pub}/send/m.room.message/txn-probe-arr", alice, raw=b'[]')
print(f"PUT send body=[] -> {status} {res}")
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc_pub}/send/m.room.message/txn-probe-num", alice, raw=b'42')
print(f"PUT send body=42 -> {status} {res}")
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc_pub}/state/m.probe", alice, raw=b'[]')
print(f"PUT state body=[] -> {status} {res}")

# Forgetting a room the caller was never in.
status, res = call("POST", f"/_matrix/client/v3/rooms/{enc_pub}/forget", bob, {})
print(f"POST /forget never-member -> {status} {res}")

# A state write the caller has no power for.
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc_pub}/state/m.room.name", bob, {"name": "not mine"})
print(f"PUT state as non-powerful member -> {status} {res}")
status, res = call("PUT", f"/_matrix/client/v3/rooms/{enc_pub}/state/m.room.power_levels/", alice,
                   {"users": {}, "users_default": 0, "events": {}, "events_default": 0,
                    "state_default": 50, "ban": 50, "kick": 50, "redact": 0, "invite": 0})
print(f"PUT /state/m.room.power_levels/ (trailing slash) -> {status} {res}")
