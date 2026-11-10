#!/usr/bin/env python3
"""Reproduce complement's DELETE /devices/{id} sequence against a running qwatrix."""
import hmac
import hashlib
import json
import sys
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18099"
SECRET = sys.argv[2] if len(sys.argv) > 2 else "complement"


def call(method, path, body=None, token=None, raw=None, ctype="application/json"):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", ctype)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:  # noqa: BLE001
        return 0, str(e)


def register(username, password):
    status, text = call("GET", "/_synapse/admin/v1/register")
    nonce = json.loads(text)["nonce"]
    mac = hmac.new(SECRET.encode(), "\0".join([nonce, username, password, "notadmin"]).encode(),
                   hashlib.sha1).hexdigest()
    return call("POST", "/_synapse/admin/v1/register",
                {"nonce": nonce, "username": username, "password": password,
                 "admin": False, "mac": mac})


status, text = register("m1user", "pw")
print("register:", status, text[:120])

# A second session, which is the one complement deletes.
status, text = call("POST", "/_matrix/client/v3/login", {
    "type": "m.login.password",
    "identifier": {"type": "m.id.user", "user": "@m1user:hs1"},
    "password": "pw",
    "device_id": "second_device",
    "initial_device_display_name": "device display",
})
print("login:", status, text[:160])
second = json.loads(text)

status, text = call("GET", "/_matrix/client/v3/account/whoami", token=second["access_token"])
print("whoami(second):", status, text[:120])

# The main client, used to ask.
status, text = call("POST", "/_matrix/client/v3/login", {
    "type": "m.login.password",
    "identifier": {"type": "m.id.user", "user": "@m1user:hs1"},
    "password": "pw",
})
main = json.loads(text)
print("login(main):", status, main.get("device_id"))

for label, raw in (("no body", None),
                   ("empty object", b"{}")):
    status, text = call("DELETE", "/_matrix/client/v3/devices/second_device",
                        token=main["access_token"], raw=raw)
    print(f"DELETE with {label}: {status} {text[:200]}")

status, text = call("DELETE", "/_matrix/client/v3/devices/second_device",
                    token=main["access_token"], raw=b"not json at all")
print(f"DELETE with junk body: {status} {text[:200]}")
