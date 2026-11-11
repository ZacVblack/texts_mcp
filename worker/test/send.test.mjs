// Offline tests for sending texts, with node:sqlite standing in for D1.
// Run: node --test worker/test/
import test from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import worker, { deriveToken, _resetSchemaFlag, TOUCH_EVERY_MS } from "../src/index.js";
import { normalizeNumber } from "../src/send.js";

const PASS = "correct horse battery staple";
const BASE = "https://texts-mcp.example.workers.dev";

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
    _raw: db,
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

const ctx = { waitUntil: () => {} };
let bridgeToken;
let mcpToken;

async function call(env, method, path, body, token) {
  const headers = { "content-type": "application/json" };
  if (token) headers.authorization = "Bearer " + token;
  const res = await worker.fetch(
    new Request(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }),
    env,
    ctx
  );
  const text = await res.text();
  return { status: res.status, data: text ? JSON.parse(text) : null };
}

async function rpc(env, method, params) {
  return (await call(env, "POST", "/mcp/" + mcpToken, { jsonrpc: "2.0", id: 1, method, params })).data;
}

async function tool(env, name, args = {}) {
  const r = await rpc(env, "tools/call", { name, arguments: args });
  assert.ok(r.result, JSON.stringify(r));
  assert.ok(!r.result.isError, r.result.content?.[0]?.text);
  return JSON.parse(r.result.content[0].text);
}

async function toolError(env, name, args = {}) {
  const r = await rpc(env, "tools/call", { name, arguments: args });
  assert.ok(r.result && r.result.isError, "expected an error, got " + JSON.stringify(r));
  return r.result.content[0].text;
}

const bridge = (env, method, path, body) => call(env, method, "/bridge" + path, body, bridgeToken);

const NOW = Date.now();

async function setup({ canSend = true, sending } = {}) {
  _resetSchemaFlag();
  bridgeToken ??= await deriveToken(PASS, "texts-mcp:bridge:v1");
  mcpToken ??= await deriveToken(PASS, "texts-mcp:mcp:v1");
  const env = { DB: fakeD1(), PASSPHRASE: PASS, TIMEZONE: "America/New_York", RETENTION_DAYS: "30", SEND_WAIT_MS: "0" };
  if (sending !== undefined) env.SENDING_ENABLED = sending;
  await bridge(env, "PUT", "/session", { v: 1, salt: "s", nonce: "n", data: "d", saved_at: 1, pairing_id: "p" });
  await bridge(env, "POST", "/heartbeat", { state: "connected", bridge_version: canSend ? "1.0.9" : "1.0.3", can_send: canSend });
  const me = { id: "me", name: "Me", number: "+17345550000", is_me: true };
  const alex = { id: "a", name: "Alex Rivera", number: "+1 734-555-0101" };
  const sam = { id: "s", name: "Sam Lee", number: "+1 734-555-0202" };
  await bridge(env, "POST", "/ingest", {
    conversations: [
      { id: "c-alex", name: "Alex Rivera", is_group: false, protocol: "rcs", last_message_at_ms: NOW - 60000, participants: [me, alex] },
      { id: "c-team", name: "Robot team", is_group: true, protocol: "rcs", last_message_at_ms: NOW - 120000, participants: [me, alex, sam] },
      { id: "c-sam", name: "Sam Lee", is_group: false, protocol: "sms", last_message_at_ms: NOW - 180000, participants: [me, sam] },
    ],
    messages: [{ id: "m1", conversation_id: "c-alex", ts: NOW - 60000, sender_name: "Alex Rivera", text: "See you at 6?" }],
  });
  return env;
}

test("send tools are listed, and hidden when SENDING_ENABLED is false", async () => {
  let env = await setup();
  let names = (await rpc(env, "tools/list", {})).result.tools.map((t) => t.name);
  for (const n of ["prepare_text", "prepare_new_text", "send_text", "send_status", "cancel_text", "get_recent_texts"]) assert.ok(names.includes(n), n);
  const init = await rpc(env, "initialize", { protocolVersion: "2025-06-18" });
  assert.match(init.result.instructions, /explicit/);

  env = await setup({ sending: "false" });
  names = (await rpc(env, "tools/list", {})).result.tools.map((t) => t.name);
  assert.ok(!names.includes("send_text"));
  const r = await rpc(env, "tools/call", { name: "send_text", arguments: { token: "x", text: "y" } });
  assert.ok(r.error, "send_text must be unknown when sending is off");
  assert.deepEqual((await bridge(env, "GET", "/outbox")).data, { item: null, sending_enabled: false });
});

test("an old bridge that can't send blocks previews", async () => {
  const env = await setup({ canSend: false });
  const msg = await toolError(env, "prepare_text", { conversation: "c-alex", text: "hi" });
  assert.match(msg, /too old/);
});

test("prepare_text needs exactly one existing conversation", async () => {
  const env = await setup();
  // "Alex" matches the one-on-one chat and the group he is in.
  const amb = await tool(env, "prepare_text", { conversation: "Alex", text: "hi" });
  assert.equal(amb.ready, false);
  assert.deepEqual(amb.candidates.map((c) => c.conversation_id).sort(), ["c-alex", "c-team"]);
  assert.equal(amb.token, undefined);

  const none = await tool(env, "prepare_text", { conversation: "+1 999 555 1234", text: "hi" });
  assert.equal(none.ready, false);
  assert.match(none.message, /prepare_new_text/);

  const ok = await tool(env, "prepare_text", { conversation: "c-alex", text: "  On my way  " });
  assert.equal(ok.ready, true);
  assert.equal(ok.preview.to, "Alex Rivera");
  assert.equal(ok.preview.new_conversation, false);
  assert.equal(ok.preview.text, "On my way");
  assert.deepEqual(ok.preview.people, ["Alex Rivera (+1 734-555-0101)"]);
  assert.equal(ok.last_texts_in_conversation[0].text, "See you at 6?");
  assert.match(ok.token, /^tx_[0-9a-f]{32}$/);
});

test("a group chat is sent to as the group", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "Robot team", text: "Meeting moved to 7" });
  assert.equal(p.preview.group_chat, true);
  assert.equal(p.preview.conversation_id, "c-team");
  assert.match(p.preview.note, /whole group/);
  await tool(env, "send_text", { token: p.token, conversation_id: "c-team", text: "Meeting moved to 7" });
  const next = (await bridge(env, "GET", "/outbox")).data;
  assert.equal(next.item.conversation_id, "c-team");
  assert.equal(next.item.new_number, "");
});

test("full send: queue, phone claims once, reports, status follows the phone", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-alex", text: "On my way" });
  const s = await tool(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "On my way" });
  assert.equal(s.state, "queued");
  assert.match(s.send_id, /^s_/);

  // The token works once.
  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "On my way" }), /already used/);

  const first = (await bridge(env, "GET", "/outbox?wait=0")).data;
  assert.deepEqual(first.item, { id: s.send_id, conversation_id: "c-alex", new_number: "", text: "On my way" });
  assert.equal((await bridge(env, "GET", "/outbox?wait=0")).data.item, null, "never handed out twice");
  assert.equal((await tool(env, "send_status", { send_id: s.send_id })).state, "sending");

  // Too late to cancel once the phone has it.
  const c = await tool(env, "cancel_text", { send_id: s.send_id });
  assert.equal(c.cancelled, false);

  assert.deepEqual((await bridge(env, "POST", "/outbox/result", { id: s.send_id, state: "accepted" })).data, { ok: true, updated: true });
  assert.equal((await tool(env, "send_status", { send_id: s.send_id })).state, "accepted");
  await bridge(env, "POST", "/outbox/result", { id: s.send_id, state: "sent", message_id: "m-out", conversation_id: "c-alex" });
  // A late "accepted" can't move it backwards.
  assert.equal((await bridge(env, "POST", "/outbox/result", { id: s.send_id, state: "accepted" })).data.updated, false);
  let st = await tool(env, "send_status", { send_id: s.send_id });
  assert.equal(st.state, "sent");
  assert.equal(st.summary, "Sent.");

  await bridge(env, "POST", "/ingest", {
    messages: [{ id: "m-out", conversation_id: "c-alex", ts: Date.now(), from_me: true, text: "On my way", status: "delivered" }],
  });
  st = await tool(env, "send_status", { send_id: s.send_id });
  assert.equal(st.summary, "Sent and delivered.");
});

test("any change after the preview is refused and voids it", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-alex", text: "On my way" });
  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "On my way!" }), /text differs/);
  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "On my way" }), /already used/);

  const p2 = await tool(env, "prepare_text", { conversation: "c-alex", text: "On my way" });
  assert.match(await toolError(env, "send_text", { token: p2.token, conversation_id: "c-sam", text: "On my way" }), /recipient differs/);

  const p3 = await tool(env, "prepare_text", { conversation: "c-alex", text: "On my way" });
  assert.match(await toolError(env, "send_text", { token: p3.token, number: "+17345550101", text: "On my way" }), /recipient differs/);

  assert.match(await toolError(env, "send_text", { token: "tx_" + "0".repeat(32), conversation_id: "c-alex", text: "x" }), /No preview/);
  assert.equal((await bridge(env, "GET", "/outbox")).data.item, null, "nothing was queued");
});

test("previews expire, and a send the phone doesn't pick up is dropped", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-alex", text: "late" });
  env.DB._raw.prepare("UPDATE outbox SET expires_at = ?").run(Date.now() - 1);
  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "late" }), /expired/);

  const p2 = await tool(env, "prepare_text", { conversation: "c-alex", text: "late too" });
  const s = await tool(env, "send_text", { token: p2.token, conversation_id: "c-alex", text: "late too" });
  env.DB._raw.prepare("UPDATE outbox SET queued_at = ? WHERE id = ?").run(Date.now() - 6 * 60000, s.send_id);
  assert.equal((await bridge(env, "GET", "/outbox")).data.item, null);
  const st = await tool(env, "send_status", { send_id: s.send_id });
  assert.equal(st.state, "expired");
  assert.match(st.summary, /Not sent/);
});

test("a claimed send that never gets an answer becomes unclear, never resent", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-sam", text: "hey" });
  const s = await tool(env, "send_text", { token: p.token, conversation_id: "c-sam", text: "hey" });
  await bridge(env, "GET", "/outbox");
  env.DB._raw.prepare("UPDATE outbox SET claimed_at = ? WHERE id = ?").run(Date.now() - 4 * 60000, s.send_id);
  assert.equal((await bridge(env, "GET", "/outbox")).data.item, null);
  const st = await tool(env, "send_status", { send_id: s.send_id });
  assert.equal(st.state, "unknown");
  assert.match(st.summary, /Check the conversation/);
  // A late answer from the phone still lands.
  await bridge(env, "POST", "/outbox/result", { id: s.send_id, state: "sent" });
  assert.equal((await tool(env, "send_status", { send_id: s.send_id })).state, "sent");
});

test("a queued send can be cancelled before the phone takes it", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-sam", text: "oops" });
  const s = await tool(env, "send_text", { token: p.token, conversation_id: "c-sam", text: "oops" });
  const c = await tool(env, "cancel_text", { send_id: s.send_id });
  assert.equal(c.cancelled, true);
  assert.equal(c.state, "cancelled");
  assert.equal((await bridge(env, "GET", "/outbox")).data.item, null);
});

test("prepare_new_text only for numbers with no conversation, and says it's new", async () => {
  const env = await setup();
  const existing = await tool(env, "prepare_new_text", { number: "(734) 555-0101", text: "hi" });
  assert.equal(existing.ready, false);
  assert.equal(existing.existing_conversation.conversation_id, "c-alex");

  const p = await tool(env, "prepare_new_text", { number: "734 555 0303", text: "Hi, this is Victor" });
  assert.equal(p.ready, true);
  assert.equal(p.preview.new_conversation, true);
  assert.equal(p.preview.number, "+17345550303");
  assert.match(p.preview.note, /NEW conversation/);

  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-alex", text: "Hi, this is Victor" }), /recipient differs/);
  const p2 = await tool(env, "prepare_new_text", { number: "+1 734 555 0303", text: "Hi, this is Victor" });
  const s = await tool(env, "send_text", { token: p2.token, number: "7345550303", text: "Hi, this is Victor" });
  assert.equal(s.new_conversation, true);
  const item = (await bridge(env, "GET", "/outbox")).data.item;
  assert.equal(item.conversation_id, "");
  assert.equal(item.new_number, "+17345550303");
  await bridge(env, "POST", "/outbox/result", { id: s.send_id, state: "sent", conversation_id: "c-new" });
  assert.equal((await tool(env, "send_status", { send_id: s.send_id })).conversation_id, "c-new");
});

test("sends are rate limited", async () => {
  const env = await setup();
  for (let i = 0; i < 5; i++) {
    const p = await tool(env, "prepare_text", { conversation: "c-sam", text: "n" + i });
    await tool(env, "send_text", { token: p.token, conversation_id: "c-sam", text: "n" + i });
  }
  const p = await tool(env, "prepare_text", { conversation: "c-sam", text: "n5" });
  assert.match(await toolError(env, "send_text", { token: p.token, conversation_id: "c-sam", text: "n5" }), /At most 5/);
});

test("phone numbers are normalized", () => {
  assert.equal(normalizeNumber("(734) 555-0101"), "+17345550101");
  assert.equal(normalizeNumber("1-734-555-0101"), "+17345550101");
  assert.equal(normalizeNumber("+44 20 7946 0958"), "+442079460958");
  assert.throws(() => normalizeNumber("555-0101"));
  assert.throws(() => normalizeNumber("call me"));
});

test("bridge outbox endpoints need the bridge token", async () => {
  const env = await setup();
  assert.equal((await call(env, "GET", "/bridge/outbox", undefined, "nope")).status, 401);
  assert.equal((await call(env, "POST", "/bridge/outbox/result", { id: "x", state: "sent" }, "nope")).status, 401);
});

test("the phone's long poll picks up a send queued while it waits", async () => {
  const env = await setup();
  const p = await tool(env, "prepare_text", { conversation: "c-sam", text: "while waiting" });
  const poll = bridge(env, "GET", "/outbox?wait=6");
  await new Promise((r) => setTimeout(r, 500));
  const s = await tool(env, "send_text", { token: p.token, conversation_id: "c-sam", text: "while waiting" });
  const got = (await poll).data;
  assert.equal(got.item && got.item.id, s.send_id);
});

test("outbox polls, send results and uploads count as phone contact", async () => {
  const env = await setup();
  const hbAt = async () => (await env.DB.prepare("SELECT v, updated_at FROM kv WHERE k = 'heartbeat'").first());
  const age = (ms) => env.DB.prepare("UPDATE kv SET updated_at = ? WHERE k = 'heartbeat'").bind(Date.now() - ms).run();
  const before = (await hbAt()).v;

  // An outbox poll moves last contact forward but keeps what the heartbeat said.
  await age(20 * 60 * 1000);
  assert.equal((await tool(env, "texts_status")).link.ok, false);
  await bridge(env, "GET", "/outbox?wait=0");
  let row = await hbAt();
  assert.ok(Date.now() - row.updated_at < 5000);
  assert.equal(row.v, before);
  assert.equal((await tool(env, "texts_status")).link.ok, true);

  // A send result and an upload count too.
  await age(20 * 60 * 1000);
  await bridge(env, "POST", "/outbox/result", { id: "nope", state: "sent" });
  assert.ok(Date.now() - (await hbAt()).updated_at < 5000);
  await age(20 * 60 * 1000);
  await bridge(env, "POST", "/ingest", { conversations: [], messages: [] });
  assert.ok(Date.now() - (await hbAt()).updated_at < 5000);

  // Within TOUCH_EVERY_MS it doesn't rewrite the row.
  await age(TOUCH_EVERY_MS / 2);
  const t = (await hbAt()).updated_at;
  await bridge(env, "GET", "/outbox?wait=0");
  assert.equal((await hbAt()).updated_at, t);

  // A rejected caller doesn't count as the phone.
  await age(20 * 60 * 1000);
  const stale = (await hbAt()).updated_at;
  await call(env, "GET", "/bridge/outbox?wait=0", undefined, "wrong-token");
  assert.equal((await hbAt()).updated_at, stale);
});

test("phone contact without any heartbeat yet creates nothing", async () => {
  _resetSchemaFlag();
  const env = { DB: fakeD1(), PASSPHRASE: PASS, TIMEZONE: "America/New_York", SEND_WAIT_MS: "0" };
  await bridge(env, "GET", "/outbox?wait=0");
  assert.equal(await env.DB.prepare("SELECT v FROM kv WHERE k = 'heartbeat'").first(), null);
});
