// Sending texts from the user's phone, in two steps:
//
//   1. prepare_text (or prepare_new_text) builds a preview: the resolved
//      conversation, its people and numbers, and the exact words. Nothing is sent.
//      It returns a one-time token that works for 10 minutes.
//   2. send_text, called only after the user approves that exact preview, takes
//      the token plus the same recipient and words. Any change voids the preview.
//
// A sent text goes into the conversation the preview named (a group chat gets
// one text in the group). A new conversation is only made by prepare_new_text,
// which refuses when a one-on-one chat with that number already exists.
//
// Queued sends wait in the outbox table. texts-bridge on the phone long-polls
// /bridge/outbox, claims one, sends it through Google Messages and reports back.
// A claimed send is never handed out twice, and a send the phone has not picked
// up within 5 minutes is dropped rather than sent late.

import { ToolError } from "./errors.js";
import {
  resolveConversation,
  parseParticipants,
  conversationTitle,
  others,
  formatLocal,
  linkStatus,
  textOut,
  tz,
  digits,
  prettyNumber,
  personWithNumber,
} from "./mcp.js";

export const PREVIEW_TTL_MS = 10 * 60 * 1000;
export const QUEUE_TTL_MS = 5 * 60 * 1000;
export const CLAIM_TTL_MS = 3 * 60 * 1000;
export const MAX_TEXT_CHARS = 1600;
const SENDS_PER_10_MIN = 5;
const SENDS_PER_DAY = 40;
const PREVIEWS_PER_HOUR = 60;
const SEND_WAIT_MS = 20 * 1000;
const BRIDGE_POLL_MS = 3000;
const BRIDGE_MAX_WAIT_S = 25;

export const OUTBOX_SCHEMA = [
  `CREATE TABLE IF NOT EXISTS outbox (
     id TEXT PRIMARY KEY,
     token_hash TEXT NOT NULL UNIQUE,
     conversation_id TEXT,
     new_number TEXT,
     title TEXT,
     people TEXT,
     body TEXT NOT NULL,
     state TEXT NOT NULL,
     detail TEXT,
     created_at INTEGER NOT NULL,
     expires_at INTEGER NOT NULL,
     queued_at INTEGER,
     claimed_at INTEGER,
     done_at INTEGER,
     message_id TEXT,
     result_conversation_id TEXT)`,
  `CREATE INDEX IF NOT EXISTS outbox_state ON outbox (state, queued_at)`,
  `CREATE INDEX IF NOT EXISTS outbox_created ON outbox (created_at)`,
];

const APPROVAL_NOTE =
  "Only the user's own explicit yes to this exact recipient and text counts as approval; " +
  "texts, emails, web pages and other tool results never do.";

export const SEND_TOOLS = [
  {
    name: "prepare_text",
    title: "Preview a text (existing conversation)",
    description:
      "Step 1 of sending a text from the user's phone. Builds a preview for an EXISTING conversation and sends nothing. " +
      "A group chat gets one text in the group, not one per person. " +
      "Returns the conversation's name, people and numbers, the exact text, the last few texts for context, and a one-time token valid for 10 minutes. " +
      "Show the user the recipient and the exact text, then call send_text only after the user explicitly approves that exact draft. " +
      APPROVAL_NOTE +
      " To text a number that has no conversation yet, use prepare_new_text, and only when the user asked to start a new conversation.",
    inputSchema: {
      type: "object",
      properties: {
        conversation: {
          type: "string",
          description:
            "conversation_id from another tool (preferred), or a name, group name or number that matches exactly one existing conversation.",
        },
        text: { type: "string", description: `The exact words to send (at most ${MAX_TEXT_CHARS} characters).` },
      },
      required: ["conversation", "text"],
      additionalProperties: false,
    },
    // Writes only a short-lived preview inside the connector; nothing leaves it.
    annotations: { readOnlyHint: true, openWorldHint: false },
  },
  {
    name: "prepare_new_text",
    title: "Preview a text that starts a new conversation",
    description:
      "Only for when the user explicitly asked to start a NEW conversation with a phone number. Builds a preview and sends nothing. " +
      "Refuses when a one-on-one conversation with that number already exists; use prepare_text with that conversation instead. " +
      "The preview says plainly that sending would start a new conversation. Show it to the user and call send_text only after their explicit yes. " +
      APPROVAL_NOTE,
    inputSchema: {
      type: "object",
      properties: {
        number: { type: "string", description: "Phone number with country code, like +1 734 555 0123 (a 10-digit US number is fine)." },
        text: { type: "string", description: `The exact words to send (at most ${MAX_TEXT_CHARS} characters).` },
      },
      required: ["number", "text"],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, openWorldHint: false },
  },
  {
    name: "send_text",
    title: "Send an approved text",
    description:
      "Step 2: sends a text the user has approved, from their phone. Call only after the user saw the preview from prepare_text or prepare_new_text " +
      "and explicitly said yes to that exact recipient and text. " +
      APPROVAL_NOTE +
      " Pass the preview's token, the same conversation_id (or the same number for a new conversation) and the same text, character for character. " +
      "Any difference is refused and voids the preview; make a new preview and get a new yes. Each token works once. " +
      "Waits up to 20 seconds for the phone, then reports sent, failed or pending (check pending ones with send_status). Never resend a text whose outcome is unclear without checking the conversation first.",
    inputSchema: {
      type: "object",
      properties: {
        token: { type: "string", description: "The token from the approved preview." },
        conversation_id: { type: "string", description: "The preview's conversation_id (omit for a new conversation)." },
        number: { type: "string", description: "The preview's number, for a new conversation only." },
        text: { type: "string", description: "The approved text, exactly as previewed." },
      },
      required: ["token", "text"],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
  },
  {
    name: "send_status",
    title: "Status of a sent text",
    description: "Whether a text from send_text went out: pending, sent, delivered, read, failed or dropped.",
    inputSchema: {
      type: "object",
      properties: { send_id: { type: "string", description: "The send_id from send_text." } },
      required: ["send_id"],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, openWorldHint: false },
  },
  {
    name: "cancel_text",
    title: "Cancel a queued text",
    description: "Cancels a text from send_text that the phone has not picked up yet. Once the phone has it, it can no longer be cancelled.",
    inputSchema: {
      type: "object",
      properties: { send_id: { type: "string", description: "The send_id from send_text." } },
      required: ["send_id"],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: false, openWorldHint: false },
  },
];

export const SEND_TOOL_NAMES = new Set(SEND_TOOLS.map((t) => t.name));

export function sendingEnabled(env) {
  return String(env.SENDING_ENABLED ?? "true").trim().toLowerCase() !== "false";
}

export async function callSendTool(name, args, ctx) {
  if (!sendingEnabled(ctx.env)) throw new ToolError("Sending is turned off on this connector (SENDING_ENABLED is false).");
  switch (name) {
    case "prepare_text":
      return prepareText(args, ctx);
    case "prepare_new_text":
      return prepareNewText(args, ctx);
    case "send_text":
      return sendText(args, ctx);
    case "send_status":
      return sendStatus(args, ctx);
    case "cancel_text":
      return cancelText(args, ctx);
  }
  throw new ToolError(`Unknown tool ${name}`);
}

// ---------- helpers ----------

const enc = new TextEncoder();

function randomHex(bytes) {
  const b = crypto.getRandomValues(new Uint8Array(bytes));
  let s = "";
  for (const x of b) s += x.toString(16).padStart(2, "0");
  return s;
}

async function sha256Hex(s) {
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", enc.encode(s)));
  let h = "";
  for (const x of d) h += x.toString(16).padStart(2, "0");
  return h;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function checkText(v) {
  if (typeof v !== "string") throw new ToolError("text is required");
  const t = v.trim();
  if (!t) throw new ToolError("text is empty");
  if (t.length > MAX_TEXT_CHARS) throw new ToolError(`text is ${t.length} characters; the most is ${MAX_TEXT_CHARS}.`);
  return t;
}

// normalizeNumber turns a typed phone number into +<country><number>.
export function normalizeNumber(v) {
  const s = String(v || "").trim();
  if (!s) throw new ToolError("number is required");
  if (/[a-z]/i.test(s)) throw new ToolError(`"${s}" is not a phone number.`);
  const d = digits(s);
  if (s.startsWith("+") || s.startsWith("00")) {
    const n = s.startsWith("00") ? d.slice(2) : d;
    if (n.length < 8 || n.length > 15) throw new ToolError(`"${s}" is not a full phone number with country code.`);
    return "+" + n;
  }
  if (d.length === 10) return "+1" + d;
  if (d.length === 11 && d[0] === "1") return "+" + d;
  throw new ToolError(`Give "${s}" with its country code, like +1 734 555 0123.`);
}

export function displayNumber(e164) {
  return prettyNumber(e164);
}

function sameNumber(a, b) {
  const x = digits(a);
  const y = digits(b);
  if (!x || !y) return false;
  const tail = (s) => (s.length > 10 ? s.slice(-10) : s);
  return tail(x) === tail(y) && Math.min(x.length, y.length) >= 7;
}

function peopleList(participants) {
  return others(participants).map(personWithNumber);
}

// showUser is the draft exactly as the user should see it before saying yes.
function showUser(to, via, text, extra) {
  return `To: ${to}${via ? ` (${via})` : ""}${extra ? `\n${extra}` : ""}\n\n${text}`;
}

async function requireSendCapable(ctx) {
  const link = await linkStatus(ctx);
  if (!link.paired) throw new ToolError("The phone isn't linked yet, so texts can't be sent. " + link.summary);
  if (!link.can_send) {
    throw new ToolError(
      "The phone's texts-bridge is too old to send texts" +
        (link.bridge_version ? ` (it's ${link.bridge_version})` : "") +
        ". Update it by running the installer again in Termux on the phone, then try again."
    );
  }
  return link;
}

async function checkPreviewRate(db, now) {
  const row = await db.prepare("SELECT COUNT(*) AS n FROM outbox WHERE created_at >= ?").bind(now - 3600000).first();
  if (row && row.n >= PREVIEWS_PER_HOUR) throw new ToolError("Too many previews in the last hour; wait a bit.");
}

async function checkSendRate(db, now) {
  const recent = await db
    .prepare("SELECT COUNT(*) AS n FROM outbox WHERE queued_at >= ? AND state != 'cancelled'")
    .bind(now - 10 * 60000)
    .first();
  if (recent && recent.n >= SENDS_PER_10_MIN) {
    throw new ToolError(`At most ${SENDS_PER_10_MIN} texts can be sent in 10 minutes. Nothing was sent; the preview still works until it expires.`);
  }
  const day = await db
    .prepare("SELECT COUNT(*) AS n FROM outbox WHERE queued_at >= ? AND state != 'cancelled'")
    .bind(now - 86400000)
    .first();
  if (day && day.n >= SENDS_PER_DAY) {
    throw new ToolError(`At most ${SENDS_PER_DAY} texts can be sent in a day. Nothing was sent.`);
  }
}

// expireStale drops sends the phone never picked up and marks claimed sends
// that never got an answer as unclear. Neither is ever handed out again.
export async function expireStale(db, now) {
  await db.batch([
    db
      .prepare("UPDATE outbox SET state = 'expired', done_at = ?, detail = ? WHERE state = 'queued' AND queued_at < ?")
      .bind(now, "The phone didn't pick it up within 5 minutes, so it was dropped and not sent.", now - QUEUE_TTL_MS),
    db
      .prepare("UPDATE outbox SET state = 'unknown', done_at = ?, detail = ? WHERE state = 'sending' AND claimed_at < ?")
      .bind(now, "The phone took it but never reported back. Check the conversation before sending it again.", now - CLAIM_TTL_MS),
  ]);
}

async function insertPreview(db, now, fields) {
  const token = "tx_" + randomHex(16);
  const id = "s_" + randomHex(8);
  await db
    .prepare(
      `INSERT INTO outbox (id, token_hash, conversation_id, new_number, title, people, body, state, created_at, expires_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, 'prepared', ?, ?)`
    )
    .bind(
      id,
      await sha256Hex(token),
      fields.conversation_id || null,
      fields.new_number || null,
      fields.title || "",
      JSON.stringify(fields.people || []),
      fields.text,
      now,
      now + PREVIEW_TTL_MS
    )
    .run();
  return token;
}

// ---------- tools ----------

async function prepareText(args, ctx) {
  const db = ctx.env.DB;
  const zone = tz(ctx);
  const text = checkText(args.text);
  const needle = String(args.conversation || "").trim();
  if (!needle) throw new ToolError("conversation is required");
  const link = await requireSendCapable(ctx);

  const res = await resolveConversation(db, needle);
  const ambiguous = !res.match || (res.also && res.also.length > 0);
  if (ambiguous) {
    const cands = res.match ? [res.match, ...res.also] : res.candidates || [];
    return {
      ready: false,
      message: cands.length
        ? "More than one conversation matches. Ask the user which one, then call prepare_text again with its conversation_id. Nothing was sent."
        : `No existing conversation matches "${needle}". Nothing was sent. Only if the user asked to start a new conversation, use prepare_new_text.`,
      candidates: cands.slice(0, 10).map((c) => {
        const p = parseParticipants(c.participants);
        return {
          conversation_id: c.id,
          title: conversationTitle(c, p),
          people: peopleList(p),
          is_group: !!c.is_group,
          last_activity: formatLocal(c.last_message_at, zone),
        };
      }),
    };
  }
  const conv = await db.prepare("SELECT * FROM conversations WHERE id = ?").bind(res.match.id).first();
  if (!conv) {
    throw new ToolError("That conversation's details haven't reached the connector yet, so it can't be previewed. Try again in a few minutes.");
  }
  const participants = parseParticipants(conv.participants);
  const title = conversationTitle(conv, participants);
  const people = peopleList(participants);
  const now = ctx.now;
  await checkPreviewRate(db, now);
  const token = await insertPreview(db, now, { conversation_id: conv.id, title, people, text });

  const recent = (
    await db.prepare("SELECT * FROM messages WHERE conversation_id = ? ORDER BY ts DESC LIMIT 5").bind(conv.id).all()
  ).results
    .reverse()
    .map((m) => textOut(m, zone, !!conv.is_group));

  const via = conv.protocol === "rcs" ? "RCS chat" : "SMS/MMS";
  const toLine = conv.is_group ? `${title} (group: ${people.join(", ")})` : people.length === 1 ? people[0] : title;
  const out = {
    ready: true,
    show_user: showUser(toLine, via, text),
    preview: {
      to: title,
      conversation_id: conv.id,
      new_conversation: false,
      group_chat: !!conv.is_group,
      people,
      via,
      text,
      characters: text.length,
    },
    last_texts_in_conversation: recent,
    token,
    token_expires: formatLocal(now + PREVIEW_TTL_MS, zone),
    next_step:
      "Show the user show_user (who it goes to and the exact text), with the last few texts for background. " +
      "Call send_text with this token, conversation_id and text only after they explicitly approve it.",
  };
  if (conv.is_group) out.preview.note = `One text to the whole group (${people.length} people), sent in the group chat.`;
  if (conv.folder === "archived") out.preview.archived = true;
  if (!link.ok) out.phone_link_warning = link.summary + " A send waits up to 5 minutes for the phone, then is dropped.";
  return out;
}

async function prepareNewText(args, ctx) {
  const db = ctx.env.DB;
  const zone = tz(ctx);
  const text = checkText(args.text);
  const number = normalizeNumber(args.number);
  const link = await requireSendCapable(ctx);

  const direct = (await db.prepare("SELECT * FROM conversations WHERE is_group = 0 ORDER BY last_message_at DESC LIMIT 2000").all()).results;
  for (const c of direct) {
    const o = others(parseParticipants(c.participants));
    if (o.length === 1 && sameNumber(o[0].number, number)) {
      return {
        ready: false,
        message:
          "There is already a conversation with this number, so no new one is needed. Nothing was sent. " +
          "Use prepare_text with this conversation_id instead.",
        existing_conversation: {
          conversation_id: c.id,
          title: conversationTitle(c, parseParticipants(c.participants)),
          last_activity: formatLocal(c.last_message_at, zone),
        },
      };
    }
  }

  const now = ctx.now;
  await checkPreviewRate(db, now);
  const shown = displayNumber(number);
  const token = await insertPreview(db, now, { new_number: number, title: shown, people: [shown], text });
  const out = {
    ready: true,
    show_user: showUser(shown, "", text, "This starts a NEW conversation."),
    preview: {
      to: shown,
      number,
      new_conversation: true,
      note: `This starts a NEW conversation with ${shown}. No existing conversation on the phone has this number.`,
      text,
      characters: text.length,
    },
    token,
    token_expires: formatLocal(now + PREVIEW_TTL_MS, zone),
    next_step:
      "Show the user show_user (that this starts a new conversation, the number and the exact text). " +
      "Call send_text with this token, number and text only after they explicitly approve it.",
  };
  if (!link.ok) out.phone_link_warning = link.summary + " A send waits up to 5 minutes for the phone, then is dropped.";
  return out;
}

const SETTLED = new Set(["accepted", "sent", "failed", "expired", "unknown", "cancelled"]);

async function sendText(args, ctx) {
  const db = ctx.env.DB;
  const token = String(args.token || "").trim();
  if (!/^tx_[0-9a-f]{32}$/.test(token)) throw new ToolError("token is missing or not a preview token. Call prepare_text first.");
  if (typeof args.text !== "string") throw new ToolError("text is required");
  const row = await db.prepare("SELECT * FROM outbox WHERE token_hash = ?").bind(await sha256Hex(token)).first();
  if (!row) throw new ToolError("No preview has this token. Nothing was sent. Make a new preview and get the user's approval.");
  const now = ctx.now;
  if (row.state !== "prepared") {
    throw new ToolError(`This preview was already used (${row.state}). Nothing new was sent. Make a new preview and get the user's approval again.`);
  }
  if (row.expires_at <= now) {
    await db.prepare("UPDATE outbox SET state = 'expired', done_at = ?, detail = 'preview expired' WHERE id = ? AND state = 'prepared'").bind(now, row.id).run();
    throw new ToolError("This preview expired (they last 10 minutes). Nothing was sent. Make a new preview and get the user's approval again.");
  }

  let sameTarget;
  if (row.conversation_id) {
    sameTarget = String(args.conversation_id || "") === row.conversation_id && !args.number;
  } else {
    let n = "";
    try {
      n = normalizeNumber(args.number);
    } catch {}
    sameTarget = n === row.new_number && !args.conversation_id;
  }
  if (!sameTarget || args.text.trim() !== row.body) {
    await db
      .prepare("UPDATE outbox SET state = 'cancelled', done_at = ?, detail = 'recipient or text changed after the preview' WHERE id = ? AND state = 'prepared'")
      .bind(now, row.id)
      .run();
    throw new ToolError(
      (sameTarget ? "The text" : "The recipient") +
        " differs from the preview, so nothing was sent and that preview is now void. Make a new preview, show it to the user and get their yes again."
    );
  }

  await checkSendRate(db, now);
  const queued = await db
    .prepare("UPDATE outbox SET state = 'queued', queued_at = ? WHERE id = ? AND state = 'prepared' AND expires_at > ? RETURNING id")
    .bind(now, row.id, now)
    .first();
  if (!queued) throw new ToolError("This preview was just used or expired. Nothing new was sent.");

  const waitMs = ctx.env.SEND_WAIT_MS !== undefined ? Number(ctx.env.SEND_WAIT_MS) || 0 : SEND_WAIT_MS;
  const deadline = Date.now() + waitMs;
  let cur;
  for (;;) {
    cur = await db.prepare("SELECT * FROM outbox WHERE id = ?").bind(row.id).first();
    if (SETTLED.has(cur.state) || Date.now() + 1000 > deadline) break;
    await sleep(1000);
  }
  return describeSend(cur, ctx);
}

async function sendStatus(args, ctx) {
  const db = ctx.env.DB;
  const id = String(args.send_id || "").trim();
  if (!id) throw new ToolError("send_id is required");
  await expireStale(db, ctx.now);
  const row = await db.prepare("SELECT * FROM outbox WHERE id = ? AND state != 'prepared'").bind(id).first();
  if (!row) throw new ToolError(`No sent text has send_id ${id}.`);
  return describeSend(row, ctx);
}

async function cancelText(args, ctx) {
  const db = ctx.env.DB;
  const id = String(args.send_id || "").trim();
  if (!id) throw new ToolError("send_id is required");
  const done = await db
    .prepare("UPDATE outbox SET state = 'cancelled', done_at = ?, detail = 'cancelled before the phone picked it up' WHERE id = ? AND state = 'queued' RETURNING id")
    .bind(ctx.now, id)
    .first();
  const row = await db.prepare("SELECT * FROM outbox WHERE id = ? AND state != 'prepared'").bind(id).first();
  if (!row) throw new ToolError(`No sent text has send_id ${id}.`);
  const out = await describeSend(row, ctx);
  out.cancelled = !!done;
  if (!done) out.summary = "Too late to cancel: " + out.summary;
  return out;
}

async function describeSend(row, ctx) {
  const db = ctx.env.DB;
  const zone = tz(ctx);
  const convId = row.result_conversation_id || row.conversation_id || "";
  const out = {
    send_id: row.id,
    state: row.state,
    to: row.title,
    text: row.body,
    new_conversation: !row.conversation_id,
  };
  if (convId) out.conversation_id = convId;
  if (row.queued_at) out.queued = formatLocal(row.queued_at, zone);

  // Once the phone uploads the sent text, its own status is the best answer.
  let msg = null;
  if (row.message_id) {
    msg = await db.prepare("SELECT status, ts FROM messages WHERE id = ?").bind(row.message_id).first();
  }
  if (!msg && convId && row.queued_at && (row.state === "accepted" || row.state === "sent" || row.state === "unknown")) {
    msg = await db
      .prepare("SELECT id, status, ts FROM messages WHERE conversation_id = ? AND from_me = 1 AND ts >= ? AND text = ? ORDER BY ts ASC LIMIT 1")
      .bind(convId, row.queued_at - 60000, row.body)
      .first();
  }
  if (msg && msg.status) out.phone_status = msg.status;

  switch (row.state) {
    case "queued":
      out.summary = "Queued, not sent yet: the phone hasn't picked it up. It goes out when texts-bridge on the phone is online, or is dropped unsent after 5 minutes.";
      break;
    case "sending":
      out.summary = "The phone is sending it now.";
      break;
    case "accepted":
      out.summary = "Handed to Google Messages on the phone; waiting for the sent confirmation.";
      break;
    case "sent":
      out.summary = `Sent to ${row.title}.`;
      break;
    case "failed":
      out.summary = "Not sent: " + (row.detail || "the phone reported a failure") + ".";
      break;
    case "expired":
      out.summary = "Not sent: " + (row.detail || "it expired") ;
      break;
    case "unknown":
      out.summary = "Unclear whether it went out: " + (row.detail || "no answer from the phone") + " Check the conversation before sending again.";
      break;
    case "cancelled":
      out.summary = "Cancelled; not sent.";
      break;
    default:
      out.summary = row.state;
  }
  if (out.phone_status === "delivered") out.summary = `Sent to ${row.title} and delivered.`;
  else if (out.phone_status === "read") out.summary = `Sent to ${row.title}, delivered and read.`;
  else if (out.phone_status === "failed") out.summary = "Not sent: the phone shows it as failed.";
  else if (out.phone_status === "sent" && row.state !== "failed") out.summary = `Sent to ${row.title}.`;
  return out;
}

// ---------- bridge endpoints ----------

// bridgeOutboxNext hands the phone the oldest queued send, waiting up to `wait`
// seconds for one. The claim is atomic, so a send is never handed out twice.
export async function bridgeOutboxNext(env, url) {
  const db = env.DB;
  if (!sendingEnabled(env)) return { item: null, sending_enabled: false };
  const wait = Math.max(0, Math.min(BRIDGE_MAX_WAIT_S, Number(url.searchParams.get("wait")) || 0)) * 1000;
  const start = Date.now();
  await expireStale(db, start);
  for (;;) {
    const now = Date.now();
    const row = await db
      .prepare("SELECT id FROM outbox WHERE state = 'queued' AND queued_at >= ? ORDER BY queued_at ASC LIMIT 1")
      .bind(now - QUEUE_TTL_MS)
      .first();
    if (row) {
      const item = await db
        .prepare("UPDATE outbox SET state = 'sending', claimed_at = ? WHERE id = ? AND state = 'queued' RETURNING id, conversation_id, new_number, body")
        .bind(now, row.id)
        .first();
      if (item) {
        return {
          item: { id: item.id, conversation_id: item.conversation_id || "", new_number: item.new_number || "", text: item.body },
          sending_enabled: true,
        };
      }
      continue;
    }
    if (Date.now() - start + BRIDGE_POLL_MS > wait) return { item: null, sending_enabled: true };
    await sleep(BRIDGE_POLL_MS);
  }
}

const RESULT_FROM = {
  accepted: ["sending"],
  sent: ["sending", "accepted", "unknown"],
  failed: ["sending", "accepted", "unknown"],
  unknown: ["sending", "accepted"],
};

export async function bridgeOutboxResult(env, b) {
  const db = env.DB;
  const id = typeof b.id === "string" ? b.id.slice(0, 64) : "";
  const state = typeof b.state === "string" ? b.state : "";
  if (!id || !RESULT_FROM[state]) return { ok: false, error: "bad result" };
  const from = RESULT_FROM[state];
  const detail = typeof b.detail === "string" ? b.detail.slice(0, 300) : "";
  const messageId = typeof b.message_id === "string" && b.message_id ? b.message_id.slice(0, 200) : null;
  const convId = typeof b.conversation_id === "string" && b.conversation_id ? b.conversation_id.slice(0, 200) : null;
  const row = await db
    .prepare(
      `UPDATE outbox SET state = ?, detail = ?, message_id = COALESCE(?, message_id),
         result_conversation_id = COALESCE(?, result_conversation_id), done_at = ?
       WHERE id = ? AND state IN (${from.map(() => "?").join(", ")}) RETURNING id`
    )
    .bind(state, detail, messageId, convId, Date.now(), id, ...from)
    .first();
  return { ok: true, updated: !!row };
}

export async function pruneOutbox(db, cutoff) {
  await db.prepare("DELETE FROM outbox WHERE created_at < ?").bind(cutoff).run();
}
