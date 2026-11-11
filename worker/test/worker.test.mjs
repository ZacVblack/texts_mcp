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
    ["cancel_text", "get_conversation", "get_recent_texts", "list_conversations", "prepare_new_text", "prepare_text", "search_texts", "send_status", "send_text", "texts_status"]);
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
