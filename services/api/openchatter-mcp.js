#!/usr/bin/env node
// OpenChatter MCP server: every outbound room action as an MCP tool, over stdio.
// Run: node openchatter-mcp.js --env ~/.openchatter/<room-slug>.<your-name>.env
// The env file is the identity (SERVER, TOKEN, optional CF_ACCESS_*); nothing secret goes in a harness config.
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const readline = require('readline');

const VERSION = '1.0.0';
const DEFAULT_SERVER = '{{SERVER}}';

// ---------- config ----------
function argValue(name) {
  const i = process.argv.indexOf(name);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  const eq = process.argv.find((a) => a.startsWith(name + '='));
  return eq ? eq.slice(name.length + 1) : '';
}
function expandHome(p) { return p.startsWith('~/') ? path.join(os.homedir(), p.slice(2)) : p; }
function loadEnvFile(file) {
  const out = {};
  for (const raw of fs.readFileSync(file, 'utf8').split('\n')) {
    const line = raw.trim().replace(/^export\s+/, '');
    if (!line || line.startsWith('#') || !line.includes('=')) continue;
    const k = line.slice(0, line.indexOf('=')).trim();
    let v = line.slice(line.indexOf('=') + 1).trim();
    if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) v = v.slice(1, -1);
    out[k] = v;
  }
  return out;
}
// like cli.sh: --env wins, then OPENCHATTER_ENV, then the only ~/.openchatter/*.env
function findEnvFile() {
  const named = argValue('--env') || process.env.OPENCHATTER_ENV || '';
  if (named) return expandHome(named);
  const dir = path.join(os.homedir(), '.openchatter');
  let found = [];
  try { found = fs.readdirSync(dir).filter((f) => f.endsWith('.env')); } catch (_) { found = []; }
  if (found.length === 1) return path.join(dir, found[0]);
  throw new Error(found.length ? found.length + ' env files in ' + dir + ', so pass --env <one of them>' : 'no env file: pass --env <your env file>');
}
let config = null;
let configError = '';
try {
  const envPath = findEnvFile();
  // only the env file: a stray CF_ACCESS_* in the harness env must not ride along
  const file = loadEnvFile(envPath);
  const server = (file.SERVER || (DEFAULT_SERVER.startsWith('{{') ? '' : DEFAULT_SERVER)).replace(/\/$/, '');
  if (!server || !file.TOKEN) throw new Error('no ' + (!server ? 'SERVER' : 'TOKEN') + ' in ' + envPath);
  config = { server, token: file.TOKEN, cfId: file.CF_ACCESS_CLIENT_ID || '', cfSecret: file.CF_ACCESS_CLIENT_SECRET || '' };
} catch (e) {
  configError = 'OpenChatter MCP is not configured: ' + e.message + '. Start it as: node openchatter-mcp.js --env ~/.openchatter/<room-slug>.<your-name>.env';
}

// ---------- room API ----------
class RoomError extends Error {
  constructor(msg, status, data) { super(msg); this.status = status; this.data = data || {}; }
}
async function api(method, route, body) {
  const headers = { Authorization: 'Bearer ' + config.token, 'User-Agent': 'openchatter-mcp/' + VERSION };
  if (config.cfId && config.cfSecret) { headers['CF-Access-Client-Id'] = config.cfId; headers['CF-Access-Client-Secret'] = config.cfSecret; }
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  // a read is safe to send twice; a write never is
  for (let attempt = 1; ; attempt++) {
    let resp;
    try {
      resp = await fetch(config.server + route, {
        method, headers, body: body === undefined ? undefined : JSON.stringify(body),
        redirect: 'manual', signal: AbortSignal.timeout(30000),
      });
    } catch (e) {
      if (method === 'GET' && attempt < 2) continue;
      const maybe = method === 'GET' ? '' : '. It may have gone through: check with read_thread before you retry, or it posts twice';
      throw new RoomError('cannot reach ' + config.server + ' (' + (e.cause && e.cause.code || e.name) + ')' + maybe, 0);
    }
    if (resp.status >= 300 && resp.status < 400) {
      throw new RoomError('the server answered with a redirect (HTTP ' + resp.status + '): something in front of it intercepted the request. If the room sits behind Cloudflare Access, set CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET in the env file.', resp.status);
    }
    const text = await resp.text();
    let data = {};
    try { data = text ? JSON.parse(text) : {}; } catch (_) { data = {}; }
    if (resp.ok) return data;
    if (resp.status >= 500 && method === 'GET' && attempt < 2) continue;
    if (resp.status === 401) throw new RoomError('the server rejected the token in the env file (HTTP 401)', 401, data);
    throw new RoomError(data.error || ('HTTP ' + resp.status), resp.status, data);
  }
}

// ---------- caches and resolution ----------
let me = null;
let channelsCache = null;
async function whoami() { if (!me) me = await api('GET', '/api/v1/me'); return me; }
async function channels(refresh) {
  if (!channelsCache || refresh) channelsCache = (await api('GET', '/api/v1/channels')).channels || [];
  return channelsCache;
}
async function resolveChannel(ref) {
  const want = String(ref || '').trim().replace(/^#/, '');
  if (!want) throw new RoomError('channel is required: a name like "general" or a channel id', 400);
  for (const refresh of [false, true]) {
    const list = await channels(refresh);
    const hit = list.find((c) => c.id === want || c.name === want || c.name === want.toLowerCase());
    if (hit) return hit;
  }
  const names = (await channels()).map((c) => '#' + c.name).join(', ');
  throw new RoomError('no channel "' + want + '" you are a member of. Your channels: ' + names, 404);
}
async function channelName(id) {
  const c = (await channels()).find((x) => x.id === id);
  return c ? '#' + c.name : id;
}
async function messageById(id) {
  const ref = String(id || '').trim();
  if (!/^[0-9a-f-]{36}$/i.test(ref)) throw new RoomError('message_id must be a message uuid like "1a2b3c4d-...", the id shown next to each message', 400);
  return api('GET', '/api/v1/messages/' + ref);
}

// ---------- formatting ----------
function when(ts) { return ts ? String(ts).replace('T', ' ').slice(0, 16) + 'Z' : ''; }
function formatMessage(m, opts = {}) {
  const head = '[' + m.id + '] ' + (m.author_name || '?') + (m.author_kind === 'human' ? ' (human)' : '') + ' · ' + when(m.created_at);
  const extra = [];
  if (opts.showReplies && m.reply_count) extra.push(m.reply_count + ' repl' + (m.reply_count === 1 ? 'y' : 'ies'));
  const reacts = (m.reactions || []).map((r) => r.emoji + (r.count > 1 ? '×' + r.count : '')).join(' ');
  if (reacts) extra.push(reacts);
  if ((m.acked_by || []).length) extra.push('acked by ' + m.acked_by.map((a) => a.name).join(', '));
  if ((m.attachments || []).length) extra.push(m.attachments.length + ' attachment(s)');
  return head + (extra.length ? ' · ' + extra.join(' · ') : '') + '\n' + (m.body || '').replace(/^/gm, '  ');
}
// text only: harnesses that see structuredContent hide the text, and the text carries the guidance
function ok(text) { return { content: [{ type: 'text', text }] }; }
function fail(text) { return { content: [{ type: 'text', text }], isError: true }; }

// mention errors name the way through, since writing ABOUT a handle is legitimate
function closeHandles(bad, handles) {
  const b = bad.toLowerCase();
  return handles.filter((h) => { const l = h.toLowerCase(); return l.includes(b) || b.includes(l) || editDistance(l, b) <= 2; });
}
function editDistance(a, b) {
  let prev = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    const cur = [i];
    for (let j = 1; j <= b.length; j++) cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
    prev = cur;
  }
  return prev[b.length];
}
function mentionHelp(e) {
  const handles = (e.data.members || []).map((m) => m.handle).filter(Boolean);
  const unknown = e.data.unknown_mentions || [];
  const guesses = unknown.map((u) => { const c = closeHandles(u, handles); return c.length ? '@' + u + ' -> did you mean ' + c.map((h) => '@' + h).join(' or ') + '?' : ''; }).filter(Boolean);
  return 'Not posted: ' + unknown.map((u) => '@' + u).join(', ') + ' is not anyone in this room. ' + (guesses.length ? guesses.join(' ') + ' ' : '')
    + 'Current handles: ' + handles.join(', ')
    + '. Fix the handle and call again. To name someone who is not in the room without tagging, write it in `backticks`.';
}

// a bare - or + at line start is a Markdown bullet, so an unfenced diff renders mangled
function looksLikeUnfencedDiff(body) {
  if (body.includes('```')) return false;
  let run = 0, odd = false, starters = new Set();
  for (const line of body.split('\n')) {
    const m = /^\s*([-+])(.*)$/.exec(line);
    if (!m) { run = 0; odd = false; starters = new Set(); continue; }
    run++; starters.add(m[1]);
    if (m[1] === '+' || !m[2].startsWith(' ') || m[2].startsWith('  ')) odd = true;
    if (run >= 2 && (odd || starters.size === 2)) return true;
  }
  return false;
}

// no bypass flags in the schemas: a cold agent sets them up front "to be safe" and the tag silently fails
async function post(channelId, text, rootId) {
  if (looksLikeUnfencedDiff(text)) {
    throw new RoomError('Not posted: this looks like a diff or code outside a ``` fence, and Markdown would eat the leading -/+ as list bullets. Wrap it in ```diff ... ``` and call again. If it really is a list, start each item with "- " (dash, space).', 400);
  }
  const body = { body: text };
  if (rootId) body.thread_root_id = rootId;
  try {
    return await api('POST', '/api/v1/channels/' + channelId + '/messages', body);
  } catch (e) {
    if (e.status === 422) throw new RoomError(mentionHelp(e), 422, e.data);
    throw e;
  }
}
function postedText(verb, m, chName) {
  const tagged = (m.mentions || []).length ? ' Tagged: ' + m.mentions.map((h) => '@' + h).join(', ') + ' (they are woken).' : ' Nobody was tagged, so no agent is woken by it.';
  const warn = (m.warnings || []).length ? '\nNote: ' + m.warnings.join(' ') : '';
  return verb + ' in ' + chName + '. message_id: ' + m.id + ', thread: ' + (m.reply_to || m.thread_root_id || m.id) + '.' + tagged + warn;
}

// ---------- tools ----------
const TEXT_DESC = 'Markdown. Tag an agent or human with @handle to wake them (see list_members); tags inside `code` or ``` fences do not count. Code and diffs go in ``` fences. No shell quoting needed.';
// hints for harnesses that gate writes (Codex "writes" mode): none of these delete anything
const READ = { readOnlyHint: true, openWorldHint: false };
const POST = { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false };
const SET = { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false };
const tools = [
  {
    name: 'reply',
    annotations: POST,
    description: 'Reply in the thread of a message. Use this to answer anyone who tagged you, and for every status, result or follow-up on an existing topic. message_id can be the thread root or any reply in it; the reply always lands in that thread.',
    inputSchema: { type: 'object', properties: {
      message_id: { type: 'string', description: 'Id of the message you are answering (shown as [id] in read_thread / read_channel and in your wake line).' },
      text: { type: 'string', description: TEXT_DESC },
    }, required: ['message_id', 'text'] },
    run: async (a) => {
      const m = await messageById(a.message_id);
      const root = m.thread_root_id || m.id;
      const out = await post(m.channel_id, a.text, root);
      return ok(postedText('Replied', out, await channelName(m.channel_id)));
    },
  },
  {
    name: 'post_message',
    annotations: POST,
    description: 'Start a NEW topic at the top level of a channel. Only for a genuinely new subject: anything about an existing topic is a reply. A root without an @handle wakes no agent.',
    inputSchema: { type: 'object', properties: {
      channel: { type: 'string', description: 'Channel name without # (e.g. "general") or channel id. See list_channels.' },
      text: { type: 'string', description: TEXT_DESC },
    }, required: ['channel', 'text'] },
    run: async (a) => {
      const ch = await resolveChannel(a.channel);
      const out = await post(ch.id, a.text, null);
      return ok(postedText('Posted a new topic', out, '#' + ch.name));
    },
  },
  {
    name: 'read_thread',
    annotations: READ,
    description: 'Read a whole thread in order: the root and every reply. Read it before you answer. message_id can be the root or any reply.',
    inputSchema: { type: 'object', properties: { message_id: { type: 'string', description: 'Any message id in the thread.' } }, required: ['message_id'] },
    run: async (a) => {
      const m = await messageById(a.message_id);
      const root = m.thread_root_id || m.id;
      const t = await api('GET', '/api/v1/threads/' + root);
      const msgs = t.messages || [];
      return ok('Thread ' + root + ' in ' + (await channelName(m.channel_id)) + ', ' + msgs.length + ' message(s). Answer with reply(message_id).\n\n' + msgs.map((x) => formatMessage(x)).join('\n\n'));
    },
  },
  {
    name: 'read_channel',
    annotations: READ,
    description: 'Read the latest messages at the top level of a channel, newest last, with reply counts. Use read_thread to open one.',
    inputSchema: { type: 'object', properties: {
      channel: { type: 'string', description: 'Channel name without # or channel id.' },
      limit: { type: 'integer', minimum: 1, maximum: 100, description: 'How many messages, default 20.' },
    }, required: ['channel'] },
    run: async (a) => {
      const ch = await resolveChannel(a.channel);
      const limit = Math.min(Math.max(parseInt(a.limit || 20, 10) || 20, 1), 100);
      const page = await api('GET', '/api/v1/channels/' + ch.id + '/messages?limit=' + limit);
      const msgs = page.messages || [];
      return ok('#' + ch.name + ', last ' + msgs.length + ' message(s):\n\n' + msgs.map((x) => formatMessage(x, { showReplies: true })).join('\n\n'));
    },
  },
  {
    name: 'react',
    annotations: SET,
    description: 'Add an emoji reaction to a message. 👀 when you pick up an ask (it also acks it), ✅ when it is done, 👍 instead of a "thanks" message. Reactions wake nobody.',
    inputSchema: { type: 'object', properties: {
      message_id: { type: 'string', description: 'The message to react to.' },
      emoji: { type: 'string', description: 'One emoji (👀 ✅ 👍 🎉) or a :shortcode: like :eyes:.' },
    }, required: ['message_id', 'emoji'] },
    run: async (a) => {
      await messageById(a.message_id);
      await api('POST', '/api/v1/messages/' + a.message_id + '/reactions', { emoji: a.emoji });
      return ok('Reacted ' + a.emoji + ' on ' + a.message_id + '.');
    },
  },
  {
    name: 'unreact',
    annotations: SET,
    description: 'Remove one of your emoji reactions from a message (e.g. take 👀 off a task you dropped).',
    inputSchema: { type: 'object', properties: {
      message_id: { type: 'string' }, emoji: { type: 'string', description: 'The emoji to remove.' },
    }, required: ['message_id', 'emoji'] },
    run: async (a) => {
      await api('DELETE', '/api/v1/messages/' + a.message_id + '/reactions/' + encodeURIComponent(a.emoji));
      return ok('Removed ' + a.emoji + ' from ' + a.message_id + '.');
    },
  },
  {
    name: 'ack',
    annotations: SET,
    description: 'Acknowledge an ask addressed to you without an emoji (for a broadcast or an untagged thread reply). Never send "on it" as a message; ack or react instead.',
    inputSchema: { type: 'object', properties: { message_id: { type: 'string' } }, required: ['message_id'] },
    run: async (a) => {
      await api('POST', '/api/v1/messages/' + a.message_id + '/ack');
      return ok('Acked ' + a.message_id + '. The author can see it; nobody was woken.');
    },
  },
  {
    name: 'pending_asks',
    annotations: READ,
    description: 'List the asks addressed to you that you have not acknowledged yet.',
    inputSchema: { type: 'object', properties: {} },
    run: async () => {
      const p = (await api('GET', '/api/v1/me/pending-acks?limit=50')).pending || [];
      if (!p.length) return ok('No pending asks.');
      return ok(p.length + ' pending ask(s):\n' + p.map((x) => '- [' + x.message_id + '] from ' + x.author_name + ' in #' + x.channel_name + ': ' + (x.excerpt || '').replace(/\n/g, ' ').slice(0, 120)).join('\n'));
    },
  },
  {
    name: 'list_members',
    annotations: READ,
    description: 'List everyone in the room with the exact @handle to tag them, whether they are human or agent, and whether they are online.',
    inputSchema: { type: 'object', properties: {} },
    run: async () => {
      const ms = (await api('GET', '/api/v1/members')).members || [];
      return ok(ms.length + ' member(s):\n' + ms.map((m) => '- @' + m.handle + ' · ' + (m.is_human ? 'human' : 'agent') + (m.role === 'admin' ? ' · admin' : '') + ' · ' + (m.online ? 'online' : 'offline')).join('\n'));
    },
  },
  {
    name: 'list_channels',
    annotations: READ,
    description: 'List the channels you are a member of, with ids and unread counts.',
    inputSchema: { type: 'object', properties: {} },
    run: async () => {
      const cs = await channels(true);
      return ok(cs.map((c) => '- #' + c.name + ' (' + c.id + ')' + (c.unread_count ? ' · ' + c.unread_count + ' unread' : '') + (c.topic ? ' · ' + c.topic : '')).join('\n') || 'You are in no channels.');
    },
  },
  {
    name: 'search',
    annotations: READ,
    description: 'Search message history (text and meaning). Returns matches with their ids, so you can read_thread or reply.',
    inputSchema: { type: 'object', properties: {
      query: { type: 'string' },
      channel: { type: 'string', description: 'Optional channel name to search in.' },
      from: { type: 'string', description: 'Optional @handle of the author.' },
      limit: { type: 'integer', minimum: 1, maximum: 50, description: 'Default 10.' },
    }, required: ['query'] },
    run: async (a) => {
      const q = new URLSearchParams({ q: a.query, limit: String(Math.min(Math.max(parseInt(a.limit || 10, 10) || 10, 1), 50)) });
      if (a.channel) q.set('channel', String(a.channel).replace(/^#/, ''));
      if (a.from) q.set('author', String(a.from).replace(/^@/, ''));
      const r = await api('GET', '/api/v1/search/hybrid?' + q);
      const hits = r.results || [];
      if (!hits.length) return ok('No matches for "' + a.query + '".');
      return ok(hits.length + ' match(es):\n\n' + hits.map((x) => formatMessage(x)).join('\n\n'));
    },
  },
  {
    name: 'set_presence',
    annotations: SET,
    description: 'Declare yourself online or offline. Offline: nothing wakes you and asks queue until you come back online.',
    inputSchema: { type: 'object', properties: { status: { type: 'string', enum: ['online', 'offline'] } }, required: ['status'] },
    run: async (a) => {
      const r = await api('POST', '/api/v1/me/presence', { status: a.status });
      const n = (r.events || []).length;
      return ok('You are now ' + a.status + '.' + (a.status === 'online' && n ? ' ' + n + ' event(s) waited while you were offline; check pending_asks.' : ''));
    },
  },
  {
    name: 'whoami',
    annotations: READ,
    description: 'Show which room identity this server acts as (your @handle and role).',
    inputSchema: { type: 'object', properties: {} },
    run: async () => {
      const m = await whoami();
      return ok('You are @' + m.name + ' (' + (m.is_human ? 'human' : 'agent') + ', ' + m.role + ') on ' + config.server + '.');
    },
  },
];

// ---------- MCP over stdio ----------
function send(msg) { process.stdout.write(JSON.stringify(msg) + '\n'); }
async function handle(msg) {
  const { id, method, params } = msg;
  if (id === undefined || id === null) return; // a notification needs no answer
  try {
    if (method === 'initialize') {
      return send({ jsonrpc: '2.0', id, result: {
        protocolVersion: (params && params.protocolVersion) || '2025-06-18',
        capabilities: { tools: {} },
        serverInfo: { name: 'openchatter', version: VERSION },
        instructions: 'Tools for talking in an OpenChatter room as yourself. Answer whoever tagged you with reply (read_thread first). Tag @handles to wake people; untagged messages wake no agent. Ack with react 👀, never with an "on it" message.',
      } });
    }
    if (method === 'ping') return send({ jsonrpc: '2.0', id, result: {} });
    if (method === 'tools/list') {
      return send({ jsonrpc: '2.0', id, result: { tools: tools.map(({ name, description, inputSchema, annotations }) => ({ name, description, inputSchema, annotations })) } });
    }
    if (method === 'tools/call') {
      const tool = tools.find((t) => t.name === (params && params.name));
      if (!tool) return send({ jsonrpc: '2.0', id, result: fail('Unknown tool "' + (params && params.name) + '". Tools: ' + tools.map((t) => t.name).join(', ')) });
      if (!config) return send({ jsonrpc: '2.0', id, result: fail(configError) });
      const args = (params && params.arguments) || {};
      for (const k of tool.inputSchema.required || []) {
        if (args[k] === undefined || args[k] === null || String(args[k]).trim() === '') {
          return send({ jsonrpc: '2.0', id, result: fail('Missing "' + k + '". ' + tool.name + ' needs: ' + tool.inputSchema.required.join(', ') + '.') });
        }
      }
      try {
        return send({ jsonrpc: '2.0', id, result: await tool.run(args) });
      } catch (e) {
        return send({ jsonrpc: '2.0', id, result: fail(e instanceof RoomError ? e.message : 'Unexpected error: ' + e.message) });
      }
    }
    return send({ jsonrpc: '2.0', id, error: { code: -32601, message: 'Method not found: ' + method } });
  } catch (e) {
    return send({ jsonrpc: '2.0', id, error: { code: -32603, message: e.message } });
  }
}
// --check: prove the identity and the route once, for provisioning, then exit
if (process.argv.includes('--check')) {
  (async () => {
    if (!config) { console.error(configError); process.exit(1); }
    try {
      const m = await whoami();
      console.log('openchatter-mcp ' + VERSION + ' ok: @' + m.name + ' on ' + config.server);
    } catch (e) { console.error('openchatter-mcp: ' + e.message); process.exit(1); }
  })();
  return;
}
const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  if (!line.trim()) return;
  let msg;
  try { msg = JSON.parse(line); } catch (_) { return send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }); }
  handle(msg);
});
