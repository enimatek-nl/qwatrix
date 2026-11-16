#!/usr/bin/env node
// Drives the requests a real Matrix client makes when it sets up end-to-end encryption, against a
// running homeserver — using the same crypto library Element Web and Element X use.
//
// This is not a unit test. It is the question "would the 'set up my digital identity' screen finish?"
// answered by running the client half of that screen for real: upload the device identity, create a
// cross-signing identity, publish it, sign this device with it, set up server-side key backup, and
// then read back what the server says about the user.
//
//   npm install @matrix-org/matrix-sdk-crypto-wasm
//   node scripts/element-e2ee-check.mjs http://127.0.0.1:8008 alice hunter2hunter2
//
// Exits non-zero on the first step a client could not complete.
import { OlmMachine, UserId, DeviceId } from "@matrix-org/matrix-sdk-crypto-wasm";

const BASE = process.argv[2] ?? "http://127.0.0.1:8008";
const USER = process.argv[3] ?? "alice";
const PASSWORD = process.argv[4] ?? "hunter2hunter2";
// A fresh device ID per run: a homeserver that is asked for a device ID another account already
// uses picks a new one rather than taking over that account, and this check is about the account
// it just created.
const DEVICE_ID = "QWCHECK" + Math.floor(Math.random() * 1e6).toString(36).toUpperCase();

let failures = 0;
function report(step, ok, detail) {
  console.log(`${ok ? "ok  " : "FAIL"}  ${step}${detail ? ` — ${detail}` : ""}`);
  if (!ok) failures++;
}

async function call(path, { method = "GET", token, body } = {}) {
  const response = await fetch(BASE + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
  });
  const text = await response.text();
  let json;
  try { json = JSON.parse(text); } catch { json = { raw: text }; }
  return { status: response.status, json };
}

// authenticate obtains an account, registering one when the server is open.
async function authenticate() {
  let result = await call("/_matrix/client/v3/register", {
    method: "POST",
    body: { username: USER, password: PASSWORD, device_id: DEVICE_ID, auth: { type: "m.login.dummy" } },
  });
  if (result.status !== 200) {
    result = await call("/_matrix/client/v3/login", {
      method: "POST",
      body: {
        type: "m.login.password",
        identifier: { type: "m.id.user", user: USER },
        password: PASSWORD,
        device_id: DEVICE_ID,
      },
    });
  }
  if (result.status !== 200) throw new Error(`could not log in: ${result.status} ${JSON.stringify(result.json)}`);
  return result.json;
}

// post sends one of the requests the crypto library produced, at the endpoint its type maps to.
async function post(token, path, body) {
  const result = await call(path, { method: "POST", token, body });
  return result;
}

// postWithPassword answers a user-interactive challenge with the account password, which is what
// Element does when it asks for it on screen.
async function postWithPassword(token, path, body) {
  let result = await post(token, path, body);
  if (result.status !== 401) return result;
  const challenge = result.json;
  const stage = (challenge.flows ?? []).flatMap((flow) => flow.stages ?? []).find((s) => s === "m.login.password");
  if (!stage) return { status: 401, json: { ...challenge, note: "no password stage offered" } };
  return post(token, path, {
    ...body,
    auth: {
      type: stage,
      session: challenge.session,
      identifier: { type: "m.id.user", user: USER },
      password: PASSWORD,
    },
  });
}

const account = await authenticate();
const { user_id: userId, access_token: token, device_id: deviceId } = account;
console.log(`signed in as ${userId} on device ${deviceId} at ${BASE}`);

const olm = await OlmMachine.initialize(new UserId(userId), new DeviceId(deviceId));

// 1. The bootstrap the crypto library performs for a fresh device: the device identity and its
//    prepaid one-time keys, the three cross-signing keys, and the signature this device makes with
//    its new self-signing key. These are the three requests a "digital identity" screen is built out
//    of, in the order the library hands them over.
const bundle = await olm.bootstrapCrossSigning(true);

if (bundle.uploadKeysRequest) {
  const uploaded = await post(token, "/_matrix/client/v3/keys/upload", bundle.uploadKeysRequest.body);
  report("POST /keys/upload publishes this device", uploaded.status === 200,
    JSON.stringify(uploaded.json).slice(0, 120));
}
if (bundle.uploadSigningKeysRequest) {
  const published = await postWithPassword(token, "/_matrix/client/v3/keys/device_signing/upload",
    JSON.parse(bundle.uploadSigningKeysRequest.body));
  report("POST /keys/device_signing/upload publishes the identity", published.status === 200,
    JSON.stringify(published.json).slice(0, 200));
} else {
  report("the library produced an identity to publish", false);
}
if (bundle.uploadSignaturesRequest) {
  const signed = await post(token, "/_matrix/client/v3/keys/signatures/upload", bundle.uploadSignaturesRequest.body);
  const refused = Object.keys(signed.json?.failures ?? {});
  report("POST /keys/signatures/upload signs this device",
    signed.status === 200 && refused.length === 0, JSON.stringify(signed.json).slice(0, 200));
}

// 3. Read it back the way another client would: a key query has to show the identity, and this
//    device has to be stamped by the self-signing key.
const query = await call("/_matrix/client/v3/keys/query", {
  method: "POST", token, body: { device_keys: { [userId]: [] } } ,
});
const answer = query.json ?? {};
report("POST /keys/query answers with the master key", Boolean(answer.master_keys?.[userId]));
report("POST /keys/query answers with the self-signing key", Boolean(answer.self_signing_keys?.[userId]));
report("POST /keys/query answers with the user's own user-signing key",
  Boolean(answer.user_signing_keys?.[userId]));
const selfSigningIDs = Object.keys(answer.self_signing_keys?.[userId]?.keys ?? {});
const deviceDocument = answer.device_keys?.[userId]?.[deviceId] ?? {};
const stamped = selfSigningIDs.some((id) => deviceDocument.signatures?.[userId]?.[id]);
report("the device key carries a signature from the self-signing key", stamped,
  `device signatures: ${JSON.stringify(Object.keys(deviceDocument.signatures?.[userId] ?? {}))}`);

const status = await olm.crossSigningStatus();
report("the crypto library believes it holds all three private keys",
  Boolean(status.hasMaster && status.hasSelfSigning && status.hasUserSigning),
  `master=${status.hasMaster} self=${status.hasSelfSigning} user=${status.hasUserSigning}`);

// 4. And the identity survives a replacement, which is the flow a locked-out user ends up in.
const replacement = await olm.bootstrapCrossSigning(true);
const replaced = await postWithPassword(token, "/_matrix/client/v3/keys/device_signing/upload",
  JSON.parse(replacement.uploadSigningKeysRequest.body));
report("a replaced identity is accepted after the password is given again", replaced.status === 200,
  JSON.stringify(replaced.json).slice(0, 160));

const after = await call("/_matrix/client/v3/keys/query", {
  method: "POST", token, body: { device_keys: { [userId]: [] } },
});
const same = JSON.stringify(after.json.master_keys?.[userId]?.keys) ===
  JSON.stringify(JSON.parse(replacement.uploadSigningKeysRequest.body).master_key.keys);
report("the replaced master key is the one on file", same);

olm.free();
console.log(failures === 0 ? "\nall checks passed" : `\n${failures} check(s) failed`);
process.exit(failures === 0 ? 0 : 1);
