#!/usr/bin/env python3
"""Reproduce complement's "power_levels write must come back down /sync" sequence."""
import hmac
import hashlib
import json
import sys
import urllib.parse
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18100"
SECRET = "complement"


def call(method, path, body=None, token=None, raw=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:  # noqa: BLE001
        return 0, str(e)


def register(username):
    password = "pw-" + username
    status, text = call("GET", "/_synapse/admin/v1/register")
    nonce = json.loads(text)["nonce"]
    mac = hmac.new(SECRET.encode(), "\0".join([nonce, username, password, "notadmin"]).encode(),
                   hashlib.sha1).hexdigest()
    status, text = call("POST", "/_synapse/admin/v1/register",
                        {"nonce": nonce, "username": username, "password": password,
                         "admin": False, "mac": mac})
    if status != 200:
        raise SystemExit(f"register {username} failed: {status} {text[:160]}")
    return json.loads(text)


alice = register("probealice")
bob = register("probebob")
print("registered")

status, text = call("POST", "/_matrix/client/v3/createRoom", {"preset": "private_chat"}, token=alice["access_token"])
room_id = json.loads(text)["room_id"]
print("room:", room_id[:30])

call("POST", f"/_matrix/client/v3/rooms/{urllib.parse.quote(room_id, safe='')}/invite",
     {"user_id": bob["user_id"]}, token=alice["access_token"])
call("POST", f"/_matrix/client/v3/rooms/{urllib.parse.quote(room_id, safe='')}/join", {}, token=bob["access_token"])

# The power_levels rewrite complement performs: everybody back to 0, aliases restricted.
content = {
    "ban": 50, "kick": 50, "redact": 50, "events_default": 0, "state_default": 50,
    "users_default": 0, "invite": 0, "history_default": 0,
    "events": {"m.room.name": 0, "m.room.power_levels": 100, "m.room.history_visibility": 100,
               "m.room.canonical_alias": 50, "m.room.avatar": 50, "m.room.tombstone": 100,
               "m.room.aliases": 50, "m.room.server_acl": 100, "m.room.encryption": 100},
    "users": {alice["user_id"]: 0, bob["user_id"]: 0},
    "notifications": {"room": 50},
}
path = f"/_matrix/client/v3/rooms/{urllib.parse.quote(room_id, safe='')}/state/m.room.power_levels/"
status, text = call("PUT", path, content, token=alice["access_token"])
print("PUT power_levels:", status, text[:140])
if status != 200:
    raise SystemExit("the power_levels write was refused")
event_id = json.loads(text)["event_id"]

for attempt in range(3):
    status, text = call("GET", "/_matrix/client/v3/sync?timeout=0", token=alice["access_token"])
    body = json.loads(text)
    joined = body.get("rooms", {}).get("join", {})
    room = joined.get(room_id)
    if room is None:
        print(f"sync {attempt}: room missing from join; rooms={list(joined)}")
        continue
    events = room.get("timeline", {}).get("events", [])
    ids = [e.get("event_id") for e in events]
    print(f"sync {attempt}: timeline has {len(ids)} events, limited={room['timeline'].get('limited')}")
    print("   contains our event:", event_id in ids)
    if not ids:
        print("   state section events:", len(room.get("state", {}).get("events", [])))
    types = [e.get("type") for e in events]
    print("   types:", types[-6:])
    break
