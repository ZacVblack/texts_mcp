// texts-mcp Worker: stores texts uploaded by texts-bridge on the phone and serves
// them to Claude as an MCP connector. Claude can read texts, and send one only
// through a preview the user approves (see send.js). No dependencies.
//
// Secrets and settings (wrangler.jsonc / dashboard):
//   PASSPHRASE      secret, 16+ characters, same as typed into texts-bridge setup
//   TIMEZONE        IANA zone for times shown to Claude (default America/New_York)
//   RETENTION_DAYS  how long texts are kept (default 30)
//   SENDING_ENABLED "false" turns the send tools off (default on)
//   DB              D1 database binding

import { handleMcp } from "./mcp.js";
import { OUTBOX_SCHEMA, bridgeOutboxNext, bridgeOutboxResult, pruneOutbox } from "./send.js";

const enc = new TextEncoder();
const PURPOSE_BRIDGE = "texts-mcp:bridge:v1";
const PURPOSE_MCP = "texts-mcp:mcp:v1";
const MAX_BODY = 4 * 1024 * 1024;

export default {
  async fetch(request, env, ctx) {
    try {
      return await route(request, env, ctx);
    } catch (err) {
      console.error("Unhandled error", err && err.stack ? err.stack : err);
      return json({ error: "internal error" }, 500);
    }
  },
};

async function route(request, env, ctx) {
  const url = new URL(request.url);
  const path = url.pathname.replace(/\/+$/, "") || "/";

  if (path === "/") {
    return new Response("texts-mcp is running.\n", { headers: { "content-type": "text/plain" } });
  }
  if (!env.PASSPHRASE || String(env.PASSPHRASE).length < 16) {
    return json({ error: "PASSPHRASE secret is missing or shorter than 16 characters" }, 503);
  }

  if (path.startsWith("/mcp/")) {
    const given = path.slice("/mcp/".length);
    const want = await deriveToken(env.PASSPHRASE, PURPOSE_MCP);
    if (!safeEqual(given, want)) return new Response("Not found", { status: 404 });
    await ensureSchema(env.DB);
    return handleMcp(request, env);
  }

  if (path.startsWith("/bridge/")) {
    const auth = request.headers.get("authorization") || "";
    const token = auth.startsWith("Bearer ") ? auth.slice(7).trim() : "";
    const want = await deriveToken(env.PASSPHRASE, PURPOSE_BRIDGE);
    if (!safeEqual(token, want)) return json({ error: "unauthorized" }, 401);
    await ensureSchema(env.DB);
    return handleBridge(request, env, ctx, path.slice("/bridge".length));
  }

  return new Response("Not found", { status: 404 });
}

// ---------- tokens ----------

export async function deriveToken(passphrase, purpose) {
  const key = await crypto.subtle.importKey("raw", enc.encode(passphrase), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, enc.encode(purpose)));
  let hex = "";
  for (const b of sig) hex += b.toString(16).padStart(2, "0");
  return hex.slice(0, 48);
}

function safeEqual(a, b) {
  a = String(a || "");
  b = String(b || "");
  let diff = a.length ^ b.length;
  for (let i = 0; i < b.length; i++) diff |= (a.charCodeAt(i) || 0) ^ b.charCodeAt(i);
  return diff === 0;
}

// ---------- storage ----------

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS messages (
     id TEXT PRIMARY KEY,
     conversation_id TEXT NOT NULL,
     ts INTEGER NOT NULL,
     from_me INTEGER NOT NULL DEFAULT 0,
     sender_name TEXT,
     sender_number TEXT,
     text TEXT NOT NULL,
     kind TEXT,
     status TEXT,
     code_hidden INTEGER NOT NULL DEFAULT 0,
     received_at INTEGER NOT NULL)`,
  `CREATE INDEX IF NOT EXISTS messages_ts ON messages (ts)`,
  `CREATE INDEX IF NOT EXISTS messages_received ON messages (received_at)`,
  `CREATE INDEX IF NOT EXISTS messages_conv_ts ON messages (conversation_id, ts)`,
  `CREATE TABLE IF NOT EXISTS conversations (
     id TEXT PRIMARY KEY,
     name TEXT,
     is_group INTEGER NOT NULL DEFAULT 0,
     protocol TEXT,
     unread INTEGER NOT NULL DEFAULT 0,
     folder TEXT,
     pinned INTEGER NOT NULL DEFAULT 0,
     last_message_at INTEGER,
     participants TEXT,
     updated_at INTEGER NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
  ...OUTBOX_SCHEMA,
];

let schemaReady = null;
export function ensureSchema(db) {
  if (!db) throw new Error("D1 binding DB is missing");
  if (!schemaReady) {
    schemaReady = db.batch(SCHEMA.map((sql) => db.prepare(sql))).catch((err) => {
      schemaReady = null;
      throw err;
    });
  }
  return schemaReady;
}

// Test hook: lets each test start from a fresh database.
export function _resetSchemaFlag() {
  schemaReady = null;
}

export async function kvGet(db, k) {
  const row = await db.prepare("SELECT v, updated_at FROM kv WHERE k = ?").bind(k).first();
  return row ? { value: row.v, updatedAt: row.updated_at } : null;
}

async function kvPut(db, k, v) {
  await db
    .prepare("INSERT INTO kv (k, v, updated_at) VALUES (?, ?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at")
    .bind(k, v, Date.now())
    .run();
}

// ---------- bridge endpoints ----------

async function readJson(request) {
  const len = Number(request.headers.get("content-length") || 0);
  if (len > MAX_BODY) throw new HttpError(413, "body too large");
  const text = await request.text();
  if (text.length > MAX_BODY) throw new HttpError(413, "body too large");
  try {
    return JSON.parse(text);
  } catch {
    throw new HttpError(400, "body is not JSON");
  }
}

class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function handleBridge(request, env, ctx, sub) {
  const db = env.DB;
  try {
    if (sub === "/ping" && request.method === "GET") {
      const [session, hb] = await Promise.all([kvGet(db, "session"), kvGet(db, "heartbeat")]);
      let state = "";
      if (hb) {
        try {
          state = JSON.parse(hb.value).state || "";
        } catch {}
      }
      return json({
        ok: true,
        has_session: !!session,
        state,
        last_heartbeat: hb ? new Date(hb.updatedAt).toISOString() : "",
      });
    }

    if (sub === "/session" && request.method === "GET") {
      const session = await kvGet(db, "session");
      if (!session) return json({ error: "no session" }, 404);
      return new Response(session.value, { headers: { "content-type": "application/json" } });
    }

    if (sub === "/session" && request.method === "PUT") {
      const body = await readJson(request);
      if (!body || body.v !== 1 || typeof body.data !== "string" || typeof body.salt !== "string" ||
          typeof body.nonce !== "string" || typeof body.saved_at !== "number") {
        return json({ error: "not a sealed session" }, 400);
      }
      const clean = {
        v: 1,
        salt: body.salt,
        nonce: body.nonce,
        data: body.data,
        saved_at: body.saved_at,
        pairing_id: typeof body.pairing_id === "string" ? body.pairing_id : "",
      };
      const s = JSON.stringify(clean);
      if (s.length > 256 * 1024) return json({ error: "session too large" }, 413);
      await kvPut(db, "session", s);
      return json({ ok: true });
    }

    if (sub === "/heartbeat" && request.method === "POST") {
      const b = await readJson(request);
      const hb = {
        state: str(b.state, 40),
        detail: str(b.detail, 300),
        phone_responding: b.phone_responding !== false,
        battery_low: !!b.battery_low,
        last_text_at_ms: num(b.last_text_at_ms),
        last_sync_at_ms: num(b.last_sync_at_ms),
        bridge_version: str(b.bridge_version, 40),
        pending_uploads: num(b.pending_uploads),
        can_send: b.can_send === true,
      };
      await kvPut(db, "heartbeat", JSON.stringify(hb));
      return json({ ok: true });
    }

    if (sub === "/outbox" && request.method === "GET") {
      return json(await bridgeOutboxNext(env, new URL(request.url)));
    }

    if (sub === "/outbox/result" && request.method === "POST") {
      const b = await readJson(request);
      return json(await bridgeOutboxResult(env, b || {}));
    }

    if (sub === "/ingest" && request.method === "POST") {
      const b = await readJson(request);
      const result = await ingest(db, b, Date.now());
      ctx && ctx.waitUntil && ctx.waitUntil(prune(env).catch((e) => console.error("prune failed", e)));
      return json({ ok: true, ...result });
    }

    return json({ error: "not found" }, 404);
  } catch (err) {
    if (err instanceof HttpError) return json({ error: err.message }, err.status);
    throw err;
  }
}

function str(v, max) {
  if (typeof v !== "string") return "";
  return v.length > max ? v.slice(0, max) : v;
}

function num(v) {
  return typeof v === "number" && Number.isFinite(v) ? Math.trunc(v) : 0;
}

const KINDS = new Set(["text", "photo", "video", "audio", "file", "contact", "other"]);
const STATUSES = new Set(["sent", "delivered", "read", "failed", "sending", "received"]);

export async function ingest(db, b, now) {
  const stmts = [];
  let texts = 0;
  let conversations = 0;
  let deleted = 0;

  for (const c of Array.isArray(b.conversations) ? b.conversations.slice(0, 500) : []) {
    const id = str(c && c.id, 200);
    if (!id) continue;
    if (c.folder === "hidden") {
      // Spam, blocked or deleted on the phone: remove it here too.
      stmts.push(db.prepare("DELETE FROM messages WHERE conversation_id = ?").bind(id));
      stmts.push(db.prepare("DELETE FROM conversations WHERE id = ?").bind(id));
      deleted++;
      continue;
    }
    const participants = Array.isArray(c.participants)
      ? c.participants.slice(0, 100).map((p) => ({
          id: str(p && p.id, 100),
          name: str(p && p.name, 200),
          number: str(p && p.number, 60),
          is_me: !!(p && p.is_me),
        }))
      : [];
    stmts.push(
      db.prepare(
        `INSERT INTO conversations (id, name, is_group, protocol, unread, folder, pinned, last_message_at, participants, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET name = excluded.name, is_group = excluded.is_group, protocol = excluded.protocol,
           unread = excluded.unread, folder = excluded.folder, pinned = excluded.pinned,
           last_message_at = MAX(COALESCE(conversations.last_message_at, 0), excluded.last_message_at),
           participants = excluded.participants, updated_at = excluded.updated_at`
      ).bind(
        id,
        str(c.name, 300),
        c.is_group ? 1 : 0,
        c.protocol === "rcs" ? "rcs" : "sms",
        c.unread ? 1 : 0,
        c.folder === "archived" ? "archived" : "inbox",
        c.pinned ? 1 : 0,
        num(c.last_message_at_ms),
        JSON.stringify(participants),
        now
      )
    );
    conversations++;
  }

  for (const m of Array.isArray(b.messages) ? b.messages.slice(0, 1000) : []) {
    const id = str(m && m.id, 200);
    const conv = str(m && m.conversation_id, 200);
    const ts = num(m && m.ts);
    if (!id || !conv || !ts) continue;
    stmts.push(
      db.prepare(
        `INSERT INTO messages (id, conversation_id, ts, from_me, sender_name, sender_number, text, kind, status, code_hidden, received_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET conversation_id = excluded.conversation_id, ts = excluded.ts, from_me = excluded.from_me,
           sender_name = excluded.sender_name, sender_number = excluded.sender_number, text = excluded.text,
           kind = excluded.kind, status = excluded.status, code_hidden = excluded.code_hidden`
      ).bind(
        id,
        conv,
        ts,
        m.from_me ? 1 : 0,
        str(m.sender_name, 200),
        str(m.sender_number, 60),
        str(m.text, 20000),
        KINDS.has(m.kind) ? m.kind : "text",
        STATUSES.has(m.status) ? m.status : "received",
        m.code_hidden ? 1 : 0,
        // Texts from the first catch-up after pairing count as already seen, so
        // they don't show up as "arrived late" in the next summary.
        m.backfill ? Math.min(ts, now) : now
      )
    );
    texts++;
  }

  for (const id of Array.isArray(b.deleted_ids) ? b.deleted_ids.slice(0, 1000) : []) {
    const s = str(id, 200);
    if (!s) continue;
    stmts.push(db.prepare("DELETE FROM messages WHERE id = ?").bind(s));
    deleted++;
  }

  // D1 batches run as one transaction; keep each batch a reasonable size.
  for (let i = 0; i < stmts.length; i += 100) {
    await db.batch(stmts.slice(i, i + 100));
  }
  if (texts > 0) await kvPut(db, "last_ingest", String(now));
  return { texts, conversations, deleted };
}

export async function prune(env, now = Date.now()) {
  const days = Math.max(1, Math.min(365, Number(env.RETENTION_DAYS) || 30));
  const cutoff = now - days * 86400000;
  await env.DB.batch([
    env.DB.prepare("DELETE FROM messages WHERE ts < ?").bind(cutoff),
    env.DB.prepare(
      "DELETE FROM conversations WHERE COALESCE(last_message_at, 0) < ? AND id NOT IN (SELECT DISTINCT conversation_id FROM messages)"
    ).bind(cutoff),
  ]);
  await pruneOutbox(env.DB, cutoff);
}

export function json(obj, status = 200, headers = {}) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}
