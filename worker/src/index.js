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
  // Photos from texts, shrunk on the phone to at most PHOTO_MAX_BYTES and kept
  // as base64 so each fits in one D1 row.
  `CREATE TABLE IF NOT EXISTS photos (
     id TEXT PRIMARY KEY,
     message_id TEXT NOT NULL,
     conversation_id TEXT NOT NULL,
     ts INTEGER NOT NULL,
     mime TEXT NOT NULL,
     width INTEGER,
     height INTEGER,
     bytes INTEGER NOT NULL,
     data TEXT NOT NULL,
     received_at INTEGER NOT NULL)`,
  `CREATE INDEX IF NOT EXISTS photos_message ON photos (message_id)`,
  `CREATE INDEX IF NOT EXISTS photos_ts ON photos (ts)`,
  // Calls from the phone's call log (Termux:API), read-only.
  `CREATE TABLE IF NOT EXISTS calls (
     id TEXT PRIMARY KEY,
     ts INTEGER NOT NULL,
     number TEXT,
     name TEXT,
     direction TEXT NOT NULL,
     duration_sec INTEGER NOT NULL DEFAULT 0,
     received_at INTEGER NOT NULL)`,
  `CREATE INDEX IF NOT EXISTS calls_ts ON calls (ts)`,
  `CREATE INDEX IF NOT EXISTS calls_received ON calls (received_at)`,
  ...OUTBOX_SCHEMA,
];

// Photo limits. One photo stays well under D1's 2 MB row limit, and all photos
// together stay well under the free plan's 500 MB database limit.
export const PHOTO_MAX_BYTES = 1_200_000;
export const PHOTO_BUDGET_BYTES = 250 * 1024 * 1024;
const PHOTO_MIMES = new Set(["image/jpeg", "image/png", "image/gif", "image/webp"]);

let schemaReady = null;
export function ensureSchema(db) {
  if (!db) throw new Error("D1 binding DB is missing");
  if (!schemaReady) {
    schemaReady = db
      .batch(SCHEMA.map((sql) => db.prepare(sql)))
      .then(() => addColumn(db, "ALTER TABLE photos ADD COLUMN thumb INTEGER NOT NULL DEFAULT 0"))
      .catch((err) => {
        schemaReady = null;
        throw err;
      });
  }
  return schemaReady;
}

// addColumn adds a column to a table made by an older version; it is already
// there on every later run.
async function addColumn(db, sql) {
  try {
    await db.prepare(sql).run();
  } catch (err) {
    if (!/duplicate column/i.test(String(err && err.message))) throw err;
  }
}

// Test hook: lets each test start from a fresh database.
export function _resetSchemaFlag() {
  schemaReady = null;
}

export async function kvGet(db, k) {
  const row = await db.prepare("SELECT v, updated_at FROM kv WHERE k = ?").bind(k).first();
  return row ? { value: row.v, updatedAt: row.updated_at } : null;
}

// The phone talks to the Worker more often than it posts a heartbeat: it long-polls
// the outbox about every 25 seconds, reports send results and uploads texts. Each of
// those is fresh proof the phone is reachable, so it moves "last phone contact"
// forward without touching what the last heartbeat said. Writes at most once a
// TOUCH_EVERY_MS so the outbox poll doesn't cost a D1 write each time.
export const TOUCH_EVERY_MS = 30 * 1000;
export async function touchPhoneContact(db, now = Date.now()) {
  await db
    .prepare("UPDATE kv SET updated_at = ? WHERE k = 'heartbeat' AND updated_at < ?")
    .bind(now, now - TOUCH_EVERY_MS)
    .run();
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
        pending_photos: num(b.pending_photos),
        photos_skipped: num(b.photos_skipped),
        photos_failed: num(b.photos_failed),
        photo_problem: str(b.photo_problem, 200),
        photo_previews: num(b.photo_previews),
        full_photos_asked: num(b.full_photos_asked),
        photos_no_media: num(b.photos_no_media),
        can_send: b.can_send === true,
        calls_state: str(b.calls_state, 40),
        calls_detail: str(b.calls_detail, 300),
        calls_checked_at_ms: num(b.calls_checked_at_ms),
      };
      await kvPut(db, "heartbeat", JSON.stringify(hb));
      return json({ ok: true });
    }

    if (sub === "/outbox" && request.method === "GET") {
      await touchPhoneContact(db);
      return json(await bridgeOutboxNext(env, new URL(request.url)));
    }

    if (sub === "/outbox/result" && request.method === "POST") {
      const b = await readJson(request);
      await touchPhoneContact(db);
      return json(await bridgeOutboxResult(env, b || {}));
    }

    if (sub === "/photos/missing" && request.method === "POST") {
      const b = await readJson(request);
      await touchPhoneContact(db);
      return json({ missing: await photosMissing(db, b && b.ids, b && b.full) });
    }

    if (sub === "/photo" && request.method === "POST") {
      const b = await readJson(request);
      await touchPhoneContact(db);
      return json(await storePhoto(db, b || {}, Date.now()));
    }

    if (sub === "/calls" && request.method === "POST") {
      const b = await readJson(request);
      await touchPhoneContact(db);
      return json({ ok: true, ...(await storeCalls(db, b || {}, Date.now())) });
    }

    if (sub === "/ingest" && request.method === "POST") {
      const b = await readJson(request);
      await touchPhoneContact(db);
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
      stmts.push(db.prepare("DELETE FROM photos WHERE conversation_id = ?").bind(id));
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
    stmts.push(db.prepare("DELETE FROM photos WHERE message_id = ?").bind(s));
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
    env.DB.prepare("DELETE FROM photos WHERE ts < ?").bind(cutoff),
    env.DB.prepare("DELETE FROM calls WHERE ts < ?").bind(cutoff),
    env.DB.prepare(
      "DELETE FROM conversations WHERE COALESCE(last_message_at, 0) < ? AND id NOT IN (SELECT DISTINCT conversation_id FROM messages)"
    ).bind(cutoff),
  ]);
  await pruneOutbox(env.DB, cutoff);
}

// ---------- calls ----------

export const CALL_DIRECTIONS = new Set(["incoming", "outgoing", "missed", "rejected", "voicemail", "unknown"]);

// storeCalls saves calls the bridge read from the phone's call log. A call
// that is uploaded again keeps when it first arrived, so it isn't news twice.
export async function storeCalls(db, b, now) {
  const stmts = [];
  for (const c of Array.isArray(b.calls) ? b.calls.slice(0, 500) : []) {
    const id = str(c && c.id, 100);
    const ts = num(c && c.ts);
    if (!id || !ts) continue;
    stmts.push(
      db
        .prepare(
          `INSERT INTO calls (id, ts, number, name, direction, duration_sec, received_at)
           VALUES (?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET ts = excluded.ts, number = excluded.number, name = excluded.name,
             direction = excluded.direction, duration_sec = excluded.duration_sec`
        )
        .bind(
          id,
          ts,
          str(c.number, 60),
          str(c.name, 200),
          CALL_DIRECTIONS.has(c.direction) ? c.direction : "unknown",
          Math.max(0, Math.min(num(c.duration_sec), 7 * 86400)),
          // Calls from the first upload after setup count as already seen, so
          // they don't show up as "arrived late" in the next summary.
          c.backfill ? Math.min(ts, now) : now
        )
    );
  }
  for (let i = 0; i < stmts.length; i += 100) {
    await db.batch(stmts.slice(i, i + 100));
  }
  return { calls: stmts.length };
}

// ---------- photos ----------

// photosMissing returns which of the given photo IDs are not stored yet. An
// ID listed in full also counts while only its preview is stored.
export async function photosMissing(db, ids, full) {
  const list = Array.isArray(ids) ? [...new Set(ids.map((x) => str(x, 200)).filter(Boolean))].slice(0, 100) : [];
  if (!list.length) return [];
  const wantFull = new Set(Array.isArray(full) ? full.map((x) => str(x, 200)) : []);
  const have = new Set();
  for (let i = 0; i < list.length; i += 50) {
    const part = list.slice(i, i + 50);
    const rows = (
      await db.prepare(`SELECT id, thumb FROM photos WHERE id IN (${part.map(() => "?").join(",")})`).bind(...part).all()
    ).results;
    for (const r of rows) if (!(r.thumb && wantFull.has(r.id))) have.add(r.id);
  }
  return list.filter((id) => !have.has(id));
}

const BASE64_RE = /^[A-Za-z0-9+/]*={0,2}$/;

export async function storePhoto(db, b, now) {
  const id = str(b.id, 200);
  const messageId = str(b.message_id, 200);
  const conv = str(b.conversation_id, 200);
  const ts = num(b.ts);
  const mime = str(b.mime, 40);
  const data = typeof b.data === "string" ? b.data : "";
  if (!id || !messageId || !conv || !ts) throw new HttpError(400, "photo needs id, message_id, conversation_id and ts");
  if (!PHOTO_MIMES.has(mime)) throw new HttpError(400, "photo must be JPEG, PNG, GIF or WebP");
  if (!data || data.length % 4 !== 0 || !BASE64_RE.test(data)) throw new HttpError(400, "photo data is not base64");
  const bytes = (data.length / 4) * 3 - (data.endsWith("==") ? 2 : data.endsWith("=") ? 1 : 0);
  if (bytes > PHOTO_MAX_BYTES) throw new HttpError(413, "photo too large");
  await db
    .prepare(
      `INSERT INTO photos (id, message_id, conversation_id, ts, mime, width, height, bytes, data, received_at, thumb)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET message_id = excluded.message_id, conversation_id = excluded.conversation_id,
         ts = excluded.ts, mime = excluded.mime, width = excluded.width, height = excluded.height,
         bytes = excluded.bytes, data = excluded.data, received_at = excluded.received_at, thumb = excluded.thumb
       WHERE excluded.thumb = 0 OR photos.thumb = 1`
    )
    .bind(id, messageId, conv, ts, mime, num(b.width), num(b.height), bytes, data, now, b.thumb === true ? 1 : 0)
    .run();
  await trimPhotos(db);
  return { ok: true, id, bytes };
}

// trimPhotos deletes the oldest photos while all of them together are over budget.
export async function trimPhotos(db, budget = PHOTO_BUDGET_BYTES) {
  for (let i = 0; i < 20; i++) {
    const row = await db.prepare("SELECT COALESCE(SUM(bytes), 0) AS total FROM photos").first();
    if (!row || row.total <= budget) return;
    await db.prepare("DELETE FROM photos WHERE id IN (SELECT id FROM photos ORDER BY ts ASC LIMIT 25)").run();
  }
}

export function json(obj, status = 200, headers = {}) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}
