// Offline tests for the Worker, with node:sqlite standing in for D1.
// Run: node --test worker/test/
import test from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import worker, { deriveToken, _resetSchemaFlag } from "../src/index.js";

const PASS = "correct horse battery staple";
const BASE = "https://texts-mcp.example.workers.dev";

// Minimal D1 look-alike: prepare/bind/first/all/run and batch.
function fakeD1() {
  const db = new DatabaseSync(":memory:");
  const wrap = (sql, args = []) => ({
    bind: (...a) => wrap(sql, a),
    first: async () => db.prepare(sql).get(...args) ?? null,
    all: async () => ({ results: db.prepare(sql).all(...args) }),
    run: async () => {
      db.prepare(sql).run(...args);
      return { success: true };
    },
    _exec: () => {
      const st = db.prepare(sql);
      return /^\s*(SELECT|WITH)/i.test(sql) ? st.all(...args) : st.run(...args);
    },
  });
  return {
    prepare: (sql) => wrap(sql),
    batch: async (stmts) => {
      db.exec("BEGIN");
      try {
        const out = stmts.map((s) => s._exec());
        db.exec("COMMIT");
        return out;
      } catch (e) {
        db.exec("ROLLBACK");
        throw e;
      }
    },
  };
}

function setup() {
  _resetSchemaFlag();
  return { DB: fakeD1(), PASSPHRASE: PASS, TIMEZONE: "America/New_York", RETENTION_DAYS: "30" };
}

const waits = [];
const ctx = { waitUntil: (p) => waits.push(p) };

async function call(env, method, path, body, token) {
  const headers = { "content-type": "application/json" };
  if (token) headers.authorization = "Bearer " + token;
  const res = await worker.fetch(
    new Request(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }),
    env,
    ctx
  );
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  return { status: res.status, data };
}

async function mcp(env, method, params, id = 1) {
  const mcpToken = await deriveToken(PASS, "texts-mcp:mcp:v1");
  return call(env, "POST", "/mcp/" + mcpToken, { jsonrpc: "2.0", id, method, params });
}

async function tool(env, name, args = {}) {
  const r = await mcp(env, "tools/call", { name, arguments: args });
  assert.equal(r.status, 200, JSON.stringify(r.data));
  assert.ok(!r.data.result.isError, r.data.result.content?.[0]?.text);
  return JSON.parse(r.data.result.content[0].text);
}

test("tokens match the Go bridge's test vectors", async () => {
  assert.equal(await deriveToken(PASS, "texts-mcp:bridge:v1"), "2fcb263fcf2ad5a7dcd3140645a3e79ad3d6138f2aa32933");
  assert.equal(await deriveToken(PASS, "texts-mcp:mcp:v1"), "85e11021d458a0dc801812000fb8056266e73797f19c82eb");
});

test("auth: wrong tokens are refused, root says nothing", async () => {
  const env = setup();
  assert.equal((await call(env, "GET", "/bridge/ping", undefined, "nope")).status, 401);
  assert.equal((await call(env, "GET", "/bridge/ping")).status, 401);
  assert.equal((await call(env, "POST", "/mcp/wrong", { jsonrpc: "2.0", id: 1, method: "ping" })).status, 404);
  const root = await call(env, "GET", "/");
  assert.equal(root.status, 200);
  const noSecret = await call({ ...env, PASSPHRASE: "short" }, "GET", "/bridge/ping");
  assert.equal(noSecret.status, 503);
});

test("bridge flow: session, heartbeat, ingest, MCP tools", async () => {
  const env = setup();
  const bt = await deriveToken(PASS, "texts-mcp:bridge:v1");

  let r = await call(env, "GET", "/bridge/ping", undefined, bt);
  assert.deepEqual([r.status, r.data.ok, r.data.has_session], [200, true, false]);
  assert.equal((await call(env, "GET", "/bridge/session", undefined, bt)).status, 404);

  // The status tool explains what to do before pairing.
  let st = await tool(env, "texts_status");
  assert.equal(st.link.ok, false);
  assert.match(st.link.summary, /pair/);

  const sealed = { v: 1, salt: "c2FsdA==", nonce: "bm9uY2U=", data: "ZGF0YQ==", saved_at: 1234, pairing_id: "p1" };
  assert.equal((await call(env, "PUT", "/bridge/session", sealed, bt)).status, 200);
  r = await call(env, "GET", "/bridge/session", undefined, bt);
  assert.deepEqual(r.data, sealed);
  assert.equal((await call(env, "PUT", "/bridge/session", { v: 2 }, bt)).status, 400);

  assert.equal((await call(env, "POST", "/bridge/heartbeat", { state: "connected", phone_responding: true, bridge_version: "t" }, bt)).status, 200);

  const now = Date.now();
  const batch = {
    conversations: [
      { id: "c1", name: "", is_group: false, protocol: "rcs", unread: true, folder: "inbox", last_message_at_ms: now - 60000,
        participants: [{ id: "1", name: "Me", is_me: true }, { id: "p2", name: "Coach Dana", number: "(555) 010-2030" }] },
      { id: "c2", name: "Robotics parents", is_group: true, folder: "archived", last_message_at_ms: now - 120000,
        participants: [{ id: "p3", name: "Sam", number: "+15550103333" }, { id: "p4", name: "Riley", number: "+15550104444" }] },
      { id: "c3", name: "Old", folder: "inbox", last_message_at_ms: now - 40 * 86400000, participants: [] },
    ],
    messages: [
      { id: "m1", conversation_id: "c1", ts: now - 90000, from_me: false, sender_name: "Coach Dana", sender_number: "(555) 010-2030", text: "Practice moved to 5", kind: "text", status: "received" },
      { id: "m2", conversation_id: "c1", ts: now - 60000, from_me: true, sender_name: "Me", text: "Got it", kind: "text", status: "delivered" },
      { id: "m3", conversation_id: "c2", ts: now - 120000, from_me: false, sender_name: "Sam", sender_number: "+15550103333", text: "Who has the drill?", kind: "text", status: "received" },
      { id: "m4", conversation_id: "c2", ts: now - 3 * 3600000, from_me: false, sender_name: "Riley", text: "Your login code is [code hidden]", code_hidden: true, backfill: true, kind: "text", status: "received" },
      { id: "m5", conversation_id: "c3", ts: now - 40 * 86400000, from_me: false, sender_name: "X", text: "ancient", backfill: true, kind: "text", status: "received" },
      { id: "bad", conversation_id: "", ts: now },
    ],
  };
  r = await call(env, "POST", "/bridge/ingest", batch, bt);
  assert.equal(r.status, 200);
  assert.equal(r.data.texts, 5);
  await Promise.all(waits.splice(0)); // prune old texts

  // Re-sending the same batch must not duplicate anything.
  await call(env, "POST", "/bridge/ingest", batch, bt);
  await Promise.all(waits.splice(0));

  // MCP handshake.
  r = await mcp(env, "initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "t", version: "1" } });
  assert.equal(r.data.result.protocolVersion, "2025-06-18");
  assert.ok(r.data.result.capabilities.tools);
  r = await call(env, "POST", "/mcp/" + (await deriveToken(PASS, "texts-mcp:mcp:v1")), { jsonrpc: "2.0", method: "notifications/initialized" });
  assert.equal(r.status, 202);
  r = await mcp(env, "tools/list", {});
  assert.deepEqual(r.data.result.tools.map((t) => t.name).sort(),
    ["cancel_text", "get_conversation", "get_recent_calls", "get_recent_texts", "list_conversations", "prepare_new_text", "prepare_text", "search_texts", "send_status", "send_text", "texts_status", "view_photos"]);
  r = await mcp(env, "nope/method", {});
  assert.equal(r.data.error.code, -32601);
  const getRes = await worker.fetch(new Request(BASE + "/mcp/" + (await deriveToken(PASS, "texts-mcp:mcp:v1"))), env, ctx);
  assert.equal(getRes.status, 405);

  st = await tool(env, "texts_status");
  assert.equal(st.link.ok, true, st.link.summary);

  // Last hour: two conversations, newest activity first, my text included.
  let recent = await tool(env, "get_recent_texts", { minutes: 60 });
  assert.equal(recent.total_texts, 3);
  assert.deepEqual(recent.conversations.map((c) => c.title), ["Coach Dana", "Robotics parents"]);
  assert.deepEqual(recent.conversations[0].texts.map((t) => t.from), ["Coach Dana", "Me"]);
  assert.equal(recent.conversations[1].archived, true);
  assert.equal(recent.conversations[1].is_group, true);

  recent = await tool(env, "get_recent_texts", { minutes: 60, include_mine: false });
  assert.equal(recent.total_texts, 2);
  recent = await tool(env, "get_recent_texts", { minutes: 600, contact: "riley" });
  assert.equal(recent.total_texts, 2); // group with Riley in it
  recent = await tool(env, "get_recent_texts", { minutes: 600, contact: "555-010-2030" });
  assert.equal(recent.conversations[0].title, "Coach Dana");

  // Old text was pruned.
  const search = await tool(env, "search_texts", { query: "ancient", days: 365 });
  assert.equal(search.results.length, 0);
  const s2 = await tool(env, "search_texts", { query: "DRILL" });
  assert.equal(s2.results[0].conversation, "Robotics parents");

  const convs = await tool(env, "list_conversations", {});
  assert.equal(convs.conversations[0].title, "Coach Dana");
  assert.equal(convs.conversations[0].last_text, "Me: Got it");
  const noArch = await tool(env, "list_conversations", { include_archived: false });
  assert.ok(!noArch.conversations.some((c) => c.archived));

  const conv = await tool(env, "get_conversation", { conversation: "dana" });
  assert.equal(conv.found, true);
  assert.deepEqual(conv.texts.map((t) => t.text), ["Practice moved to 5", "Got it"]);
  const page = await tool(env, "get_conversation", { conversation: "c1", limit: 1 });
  assert.deepEqual(page.texts.map((t) => t.text), ["Got it"]);
  assert.ok(page.older_texts_before);
  const missing = await tool(env, "get_conversation", { conversation: "nobody" });
  assert.equal(missing.found, false);

  // A text that arrives late (phone was offline) still shows in the next hourly window.
  await call(env, "POST", "/bridge/ingest", {
    messages: [{ id: "late", conversation_id: "c1", ts: now - 5 * 3600000, sender_name: "Coach Dana", text: "Sent while you were offline", kind: "text", status: "received" }],
  }, bt);
  recent = await tool(env, "get_recent_texts", { minutes: 60 });
  const dana = recent.conversations.find((c) => c.title === "Coach Dana");
  assert.ok(dana.texts.some((t) => t.text === "Sent while you were offline"));
  assert.equal(dana.late_arrivals, true);

  // Deletes and spam moves remove texts.
  await call(env, "POST", "/bridge/ingest", { deleted_ids: ["m2"], conversations: [{ id: "c2", folder: "hidden" }] }, bt);
  recent = await tool(env, "get_recent_texts", { minutes: 600 });
  assert.ok(!recent.conversations.some((c) => c.title === "Robotics parents"));
  assert.ok(!JSON.stringify(recent).includes("Got it"));

  // A stale heartbeat is reported as the phone being offline.
  const stale = await tool({ ...env }, "texts_status");
  assert.equal(stale.link.ok, true);
  await env.DB.prepare("UPDATE kv SET updated_at = ? WHERE k = 'heartbeat'").bind(now - 3600000).run();
  const off = await tool(env, "texts_status");
  assert.equal(off.link.ok, false);
  assert.match(off.link.summary, /not checked in/);

  // Bad arguments come back as tool errors, not crashes.
  const bad = await mcp(env, "tools/call", { name: "get_recent_texts", arguments: { since: "not a date" } });
  assert.equal(bad.data.result.isError, true);
});

test("photos: upload, missing check, photo_ids on texts, view_photos, delete and prune", async () => {
  const env = setup();
  const bt = await deriveToken(PASS, "texts-mcp:bridge:v1");
  const now = Date.now();
  await call(env, "POST", "/bridge/ingest", {
    conversations: [
      { id: "781", name: "Ftc team", is_group: true, folder: "inbox", last_message_at_ms: now - 60000,
        participants: [{ id: "p1", name: "Coach Nick", number: "+15550101111" }, { id: "p2", name: "Sam", number: "+15550102222" }] },
    ],
    messages: [
      { id: "s1", conversation_id: "781", ts: now - 60000, from_me: false, sender_name: "Coach Nick", text: "This is our schedule", kind: "photo", status: "received" },
      { id: "s2", conversation_id: "781", ts: now - 30000, from_me: false, sender_name: "Sam", text: "Thanks", kind: "text", status: "received" },
    ],
  }, bt);
  await Promise.all(waits.splice(0));

  // A 1x1 PNG.
  const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
  let r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.1", "s1.2"] }, bt);
  assert.deepEqual(r.data.missing, ["s1.1", "s1.2"]);

  const photo = { id: "s1.1", message_id: "s1", conversation_id: "781", ts: now - 60000, mime: "image/png", width: 1, height: 1, data: png };
  r = await call(env, "POST", "/bridge/photo", photo, bt);
  assert.equal(r.status, 200, JSON.stringify(r.data));
  assert.equal((await call(env, "POST", "/bridge/photo", { ...photo, mime: "image/heic" }, bt)).status, 400);
  assert.equal((await call(env, "POST", "/bridge/photo", { ...photo, data: "not base64!" }, bt)).status, 400);
  assert.equal((await call(env, "POST", "/bridge/photo", { ...photo, id: "big", data: "A".repeat(1_700_000) }, bt)).status, 413);
  assert.equal((await call(env, "POST", "/bridge/photo", photo)).status, 401);

  r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.1", "s1.2"] }, bt);
  assert.deepEqual(r.data.missing, ["s1.2"]);

  // Texts list their photos.
  const recent = await tool(env, "get_recent_texts", { since: new Date(now - 3600000).toISOString() });
  const all = JSON.stringify(recent);
  assert.match(all, /"photo_ids":\["s1.1"\]/);
  const conv = await tool(env, "get_conversation", { conversation: "781" });
  const withPhoto = JSON.stringify(conv).includes('"s1.1"');
  assert.ok(withPhoto, JSON.stringify(conv));
  const found = await tool(env, "search_texts", { query: "schedule" });
  assert.ok(JSON.stringify(found).includes('"s1.1"'));

  // view_photos returns the picture itself.
  r = await mcp(env, "tools/call", { name: "view_photos", arguments: { photo_ids: ["s1.1", "nope.0"] } });
  const content = r.data.result.content;
  assert.ok(!r.data.result.isError);
  const img = content.find((c) => c.type === "image");
  assert.deepEqual([img.mimeType, img.data], ["image/png", png]);
  assert.match(content[0].text, /Coach Nick/);
  assert.ok(content.some((c) => c.type === "text" && /nope\.0/.test(c.text)));
  r = await mcp(env, "tools/call", { name: "view_photos", arguments: { photo_ids: ["nope.0"] } });
  assert.ok(r.data.result.isError);

  // Deleting the text on the phone deletes its photo.
  await call(env, "POST", "/bridge/ingest", { conversations: [], messages: [], deleted_ids: ["s1"] }, bt);
  await Promise.all(waits.splice(0));
  r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.1"] }, bt);
  assert.deepEqual(r.data.missing, ["s1.1"]);

  // Old photos are pruned with old texts.
  await call(env, "POST", "/bridge/photo", { ...photo, id: "old.0", ts: now - 40 * 86400000 }, bt);
  await call(env, "POST", "/bridge/ingest", { conversations: [], messages: [] }, bt);
  await Promise.all(waits.splice(0));
  r = await call(env, "POST", "/bridge/photos/missing", { ids: ["old.0"] }, bt);
  assert.deepEqual(r.data.missing, ["old.0"]);
});

test("photos: a preview is replaced by the full photo, never the other way", async () => {
  const env = setup();
  const bt = await deriveToken(PASS, "texts-mcp:bridge:v1");
  const now = Date.now();
  await call(env, "POST", "/bridge/ingest", {
    conversations: [{ id: "c1", name: "", folder: "inbox", last_message_at_ms: now, participants: [{ id: "p1", name: "Coach Nick" }] }],
    messages: [{ id: "s1", conversation_id: "c1", ts: now, from_me: false, sender_name: "Coach Nick", text: "[photo]", kind: "photo", status: "received" }],
  }, bt);
  await Promise.all(waits.splice(0));
  const small = "R0lGODlhAQABAAAAACw=";
  const big = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
  const photo = { id: "s1.0", message_id: "s1", conversation_id: "c1", ts: now, width: 1, height: 1 };

  assert.equal((await call(env, "POST", "/bridge/photo", { ...photo, mime: "image/gif", data: small, thumb: true }, bt)).status, 200);
  let r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.0"] }, bt);
  assert.deepEqual(r.data.missing, [], "a preview counts as there for a preview");
  r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.0"], full: ["s1.0"] }, bt);
  assert.deepEqual(r.data.missing, ["s1.0"], "but not for the full photo");

  r = await mcp(env, "tools/call", { name: "view_photos", arguments: { photo_ids: ["s1.0"] } });
  assert.match(r.data.result.content[0].text, /preview/);

  await call(env, "POST", "/bridge/photo", { ...photo, mime: "image/png", data: big }, bt);
  r = await call(env, "POST", "/bridge/photos/missing", { ids: ["s1.0"], full: ["s1.0"] }, bt);
  assert.deepEqual(r.data.missing, []);
  // A late preview doesn't overwrite the full photo.
  await call(env, "POST", "/bridge/photo", { ...photo, mime: "image/gif", data: small, thumb: true }, bt);
  r = await mcp(env, "tools/call", { name: "view_photos", arguments: { photo_ids: ["s1.0"] } });
  const img = r.data.result.content.find((c) => c.type === "image");
  assert.deepEqual([img.mimeType, img.data], ["image/png", big]);
  assert.doesNotMatch(r.data.result.content[0].text, /preview/);
});

test("calls: upload, status, get_recent_calls with names, missed and called back", async () => {
  const env = setup();
  const bt = await deriveToken(PASS, "texts-mcp:bridge:v1");
  const sealed = { v: 1, salt: "c2FsdA==", nonce: "bm9uY2U=", data: "ZGF0YQ==", saved_at: 1234, pairing_id: "p1" };
  await call(env, "PUT", "/bridge/session", sealed, bt);

  // An older bridge sends no calls state: the status says to update it.
  await call(env, "POST", "/bridge/heartbeat", { state: "connected", phone_responding: true, bridge_version: "t" }, bt);
  let st = await tool(env, "texts_status");
  assert.equal(st.link.ok, true);
  assert.equal(st.link.calls.ok, false);
  assert.match(st.link.calls.summary, /too old/);

  // A bridge that can't read calls says why.
  await call(env, "POST", "/bridge/heartbeat", {
    state: "connected", phone_responding: true, bridge_version: "t",
    calls_state: "no_permission", calls_detail: "Calls are off: allow Call logs.", calls_checked_at_ms: Date.now(),
  }, bt);
  st = await tool(env, "texts_status");
  assert.equal(st.link.calls.ok, false);
  assert.equal(st.link.calls.summary, "Calls are off: allow Call logs.");
  let calls = await tool(env, "get_recent_calls", {});
  assert.match(calls.summary, /No calls.*Calls can't be read right now: Calls are off/);

  await call(env, "POST", "/bridge/heartbeat", {
    state: "connected", phone_responding: true, bridge_version: "t", calls_state: "ok", calls_checked_at_ms: Date.now(),
  }, bt);
  st = await tool(env, "texts_status");
  assert.equal(st.link.calls.ok, true);
  assert.ok(st.link.calls.last_checked);

  const now = Date.now();
  // Mom has a text conversation, so her unnamed call gets her name and the conversation.
  await call(env, "POST", "/bridge/ingest", {
    conversations: [
      { id: "mom", name: "", folder: "inbox", last_message_at_ms: now - 60000,
        participants: [{ id: "1", name: "Me", is_me: true }, { id: "2", name: "Mom", number: "+1 (734) 555-0101" }] },
    ],
    messages: [
      { id: "t1", conversation_id: "mom", ts: now - 10 * 60000, from_me: true, sender_name: "Me", text: "Sorry, in class", kind: "text", status: "delivered" },
    ],
  }, bt);

  const batch = {
    calls: [
      { id: "call-a", ts: now - 20 * 60000, number: "7345550101", name: "", direction: "missed", duration_sec: 0 },
      { id: "call-b", ts: now - 15 * 60000, number: "+17345550199", name: "Coach Nick", direction: "missed", duration_sec: 0 },
      { id: "call-c", ts: now - 5 * 60000, number: "+17345550199", name: "Coach Nick", direction: "outgoing", duration_sec: 135 },
      { id: "call-d", ts: now - 3 * 60000, number: "", name: "", direction: "rejected", duration_sec: 0 },
      { id: "call-e", ts: now - 2 * 60000, number: "+17345550142", name: "Dad", direction: "incoming", duration_sec: 3725 },
      { id: "call-old", ts: now - 5 * 3600000, number: "+17345550142", name: "Dad", direction: "outgoing", duration_sec: 60, backfill: true },
      { id: "", ts: now },
      { id: "call-weird", ts: now - 1000, number: "5", direction: "teleport", duration_sec: -4 },
    ],
  };
  let r = await call(env, "POST", "/bridge/calls", batch, bt);
  assert.equal(r.status, 200);
  assert.equal(r.data.calls, 7);
  // Uploading the same calls again changes nothing.
  r = await call(env, "POST", "/bridge/calls", batch, bt);
  assert.equal(r.status, 200);
  assert.equal((await env.DB.prepare("SELECT COUNT(*) AS n FROM calls").first()).n, 7);
  // Unauthenticated uploads are refused.
  assert.equal((await call(env, "POST", "/bridge/calls", batch, "wrong")).status, 401);

  calls = await tool(env, "get_recent_calls", { minutes: 60 });
  assert.equal(calls.total_calls, 6);
  assert.equal(calls.calls_status.ok, true);
  // Newest first.
  assert.deepEqual(calls.calls.map((c) => c.who), ["5", "Dad", "Private number", "Coach Nick", "Coach Nick", "Mom"]);
  const weird = calls.calls[0];
  assert.equal(weird.direction, "call");
  const dad = calls.calls[1];
  assert.deepEqual([dad.direction, dad.duration, dad.number], ["answered", "1 h 2 min", "+1 734-555-0142"]);
  const mom = calls.calls.find((c) => c.who === "Mom");
  assert.equal(mom.missed, true);
  assert.equal(mom.conversation_id, "mom");
  assert.equal(mom.called_back, false);
  assert.equal(mom.texted_back, true);
  const nickMissed = calls.calls.find((c) => c.who === "Coach Nick" && c.missed);
  assert.equal(nickMissed.called_back, true);
  assert.equal(calls.calls.find((c) => c.who === "Coach Nick" && !c.missed).duration, "2 min 15 s");
  const priv = calls.calls.find((c) => c.who === "Private number");
  assert.deepEqual([priv.direction, priv.missed, priv.called_back], ["declined", true, undefined]);
  assert.match(calls.summary, /^6 calls since .*: 3 missed \(Private number, Coach Nick, Mom\), 1 answered, 1 outgoing, 1 other\. 1 missed call not returned yet\.$/);

  // Filters.
  calls = await tool(env, "get_recent_calls", { minutes: 60, missed_only: true });
  assert.equal(calls.total_calls, 3);
  calls = await tool(env, "get_recent_calls", { minutes: 60, contact: "mom" });
  assert.deepEqual(calls.calls.map((c) => c.who), ["Mom"]);
  calls = await tool(env, "get_recent_calls", { minutes: 60, contact: "555-0199" });
  assert.equal(calls.total_calls, 2);
  calls = await tool(env, "get_recent_calls", { minutes: 60, limit: 2 });
  assert.equal(calls.total_calls, 2);
  assert.equal(calls.truncated, true);
  // The backfilled call from 5 hours ago isn't news.
  calls = await tool(env, "get_recent_calls", { minutes: 60 });
  assert.ok(!calls.calls.some((c) => c.late));

  // A call that reaches us late (phone was offline) shows in the next window.
  await call(env, "POST", "/bridge/calls", { calls: [{ id: "call-late", ts: now - 3 * 3600000, number: "+17345550142", name: "Dad", direction: "missed" }] }, bt);
  calls = await tool(env, "get_recent_calls", { minutes: 30 });
  const late = calls.calls.find((c) => c.late);
  assert.equal(late.who, "Dad");
  // Dad called later and they talked, so it counts as returned.
  assert.equal(late.called_back, true);

  // Old calls are pruned with the texts.
  await call(env, "POST", "/bridge/calls", { calls: [{ id: "ancient", ts: now - 40 * 86400000, number: "1", direction: "missed" }] }, bt);
  await call(env, "POST", "/bridge/ingest", { messages: [{ id: "t2", conversation_id: "mom", ts: now, text: "hi", kind: "text" }] }, bt);
  await Promise.all(waits.splice(0));
  assert.equal(await env.DB.prepare("SELECT id FROM calls WHERE id = 'ancient'").first(), null);

  const bad = await mcp(env, "tools/call", { name: "get_recent_calls", arguments: { since: "nope" } });
  assert.equal(bad.data.result.isError, true);
});
