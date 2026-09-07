// Shared account helpers for the browser checks. Room creation needs a login
// session since task 03; every check makes its workspace through here.
const { execFileSync } = require('child_process');

const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function call(base, p, opts = {}) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  if (opts.token) headers['Authorization'] = 'Bearer ' + opts.token;
  const resp = await fetch(base + p, {
    method: opts.method || 'GET',
    headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  // register and login share the per-IP limiter (10 burst, 30/min) with
  // every /join the check itself makes
  if (resp.status === 429 && data.code === 'rate_limited') {
    await sleep(3000);
    return call(base, p, opts);
  }
  if (!resp.ok) throw new Error(p + ': ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const uniqUser = () => 'u' + Date.now().toString(36).slice(-6) + Math.floor(Math.random() * 1e6).toString(36);
const PASSWORD = 'correct horse battery';

// registerAndLogin returns a ses_ token for username (a fresh account, or a
// login when the name is already taken by an earlier run)
async function registerAndLogin(base, username = uniqUser(), displayName = '') {
  const body = { username, password: PASSWORD };
  if (displayName) body.display_name = displayName;
  try {
    return (await call(base, '/api/v1/auth/password/register', { method: 'POST', body })).token;
  } catch (e) {
    if (!e.message.includes('username_taken')) throw e;
  }
  return (await call(base, '/api/v1/auth/password/login', { method: 'POST', body: { username, password: PASSWORD } })).token;
}

// createRoom is POST /api/v1/rooms with a session: {room, join_url, invite (link), invite_code (bare token alias)}
// the slug is unique per call: a repeated name would collide on the dev db
// a unique slug per create (the server rejects a taken one), kept under the 60-char cap
const uniqSlug = (name) => {
  const tag = uniqUser();
  const base = name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 59 - tag.length).replace(/-+$/, '');
  return base + '-' + tag;
};
const createRoom = (base, session, name, slug) => call(base, '/api/v1/rooms', { method: 'POST', token: session, body: { name, slug: slug || uniqSlug(name) } });

// newRoom makes a workspace under a throwaway account and then vacates the
// creator's seat, so the first joiner becomes admin and the roster starts
// empty exactly as before task 03 (the Go tests do the same)
async function newRoom(base, name) {
  const session = await registerAndLogin(base);
  const out = await createRoom(base, session, name);
  if (!/^[0-9a-f-]{36}$/.test(out.room.id)) throw new Error('room id: ' + out.room.id);
  execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c',
    `DELETE FROM participants WHERE room_id = '${out.room.id}' AND user_id IS NOT NULL`], { stdio: ['ignore', 'ignore', 'inherit'] });
  return out;
}

// loginPage signs a fresh account into a puppeteer page: register through the
// API, seed the session key on /login, then open opts.next (a room page, say).
// Returns the ses_ token so the caller can drive the API as the same user.
async function loginPage(page, base, username = uniqUser(), opts = {}) {
  const session = await registerAndLogin(base, username, opts.displayName || '');
  await page.goto(base + '/login', { waitUntil: 'networkidle2' });
  await page.evaluate((t) => localStorage.setItem('agentchat:session', t), session);
  if (opts.next) await page.goto(base + opts.next, { waitUntil: 'networkidle2' });
  return session;
}

// enterWithCode submits #enter-form with code and waits for the room to load
async function enterWithCode(page, code) {
  await page.waitForSelector('#enter-view:not(.hidden)', { timeout: 8000 });
  await page.$eval('#enter-code', (el) => { el.value = ''; });
  await page.type('#enter-code', code);
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle2', timeout: 8000 }),
    page.click('#enter-form button[type=submit]'),
  ]);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
}

// enterAs is the swept checks' one-liner: a fresh account named displayName
// signs in, opens the room page, meets #enter-view and gets in with code (an
// invite link or its bare inv- token; the field takes both).
// Returns the ses_ token.
async function enterAs(page, base, slug, code, displayName) {
  const session = await loginPage(page, base, uniqUser(), { displayName, next: '/r/' + slug });
  await enterWithCode(page, code);
  return session;
}

// openWorkspace loads /w/<slug> with the session seeded and waits for the chat
async function openWorkspace(page, base, session, slug) {
  await page.goto(base + '/login', { waitUntil: 'networkidle2' });
  await page.evaluate((t) => localStorage.setItem('agentchat:session', t), session);
  await page.goto(base + '/w/' + slug, { waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
}

// switchTo picks slug in the workspace rail (the header menu no longer lists
// workspaces); since task 23 the switch is in place: the URL and the rail's
// current mark move when the whole pane has swapped
async function switchTo(page, slug) {
  await page.waitForSelector('#rail-list .rail-item[href="/w/' + slug + '"]', { timeout: 8000 });
  await page.click('#rail-list .rail-item[href="/w/' + slug + '"]');
  await page.waitForFunction((s) => location.pathname.startsWith('/w/' + s) && document.querySelector('#rail-list .rail-item[aria-current="true"][data-slug="' + s + '"]'), { timeout: 8000 }, slug);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
}

// openAsHuman boots a page as an existing human participant (a /join with
// is_human). Since 000027 the browser only knows sessions, so the participant
// is linked to a fresh account, as the backfill did, and the page opens
// /r/<slug> on that session. The act_ token keeps driving the API as the same
// identity. Returns the ses_ token. The session key is per browser profile:
// two humans on two pages need two browser contexts.
const humanSessions = new Map();
async function openAsHuman(page, base, slug, joined) {
  const p = joined.participant;
  if (!/^[0-9a-f-]{36}$/.test(p.id)) throw new Error('participant id: ' + p.id);
  let session = humanSessions.get(p.id);
  if (!session) {
    const username = uniqUser();
    session = await registerAndLogin(base, username, p.name);
    execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c',
      `UPDATE participants SET user_id = (SELECT id FROM users WHERE username = '${username}') WHERE id = '${p.id}'`], { stdio: ['ignore', 'ignore', 'inherit'] });
    humanSessions.set(p.id, session);
  }
  await page.goto(base + '/login', { waitUntil: 'networkidle2' });
  await page.evaluate((t) => localStorage.setItem('agentchat:session', t), session);
  await page.goto(base + '/r/' + slug, { waitUntil: 'networkidle2' });
  return session;
}

// the personal settings (notifications, theme, avatar) live on /settings since
// task 09: a check goes there from the room and comes back through the Back link
async function openSettings(page, base, tab = 'personal') {
  const here = await page.evaluate(() => location.pathname + location.search);
  await page.goto(base + '/settings?next=' + encodeURIComponent(here) + '&tab=' + tab, { waitUntil: 'networkidle2' });
  await page.waitForSelector('#settings-view:not(.hidden)', { timeout: 8000 });
  if (tab === 'personal') await page.waitForSelector('#notify-settings:not(.hidden)', { timeout: 8000 });
}
// workspace menu > Invite member: opens the invite modal (admins only)
async function openInviteModal(page) {
  await page.waitForSelector('#ws-switcher-wrap:not(.hidden)', { timeout: 8000 });
  // DOM clicks: the small default viewport clips the sidebar in some checks
  await page.evaluate(() => document.getElementById('ws-switcher').click());
  await page.waitForSelector('#ws-invite-member', { timeout: 5000 });
  await page.evaluate(() => document.getElementById('ws-invite-member').click());
  await page.waitForSelector('#invite-modal:not(.hidden)', { timeout: 5000 });
}

async function backToRoom(page) {
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle2' }), page.click('#settings-back')]);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
}

module.exports = { openSettings, openInviteModal, backToRoom, call, registerAndLogin, createRoom, newRoom, loginPage, enterWithCode, enterAs, openWorkspace, openAsHuman, switchTo, uniqUser, PASSWORD, sleep };
