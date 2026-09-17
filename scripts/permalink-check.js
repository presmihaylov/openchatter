require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for message permalinks: right-click copies a stable /m/<id> URL, opening
// that URL lands on the message and flashes it (paginating back through history
// when the message is old), threaded messages open their thread, and a link to
// a deleted message degrades to the channel view with a note.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/permalink-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
const post = (token, body, root) =>
  api('/api/v1/channels/general/messages', { method: 'POST', token, body: { body, thread_root_id: root } });

(async () => {
  const created = await newRoom(SERVER, 'permalink check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'linkbot', description: 't' } });
  const viewer = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'viewer', is_human: true } });

  // the old message goes first, then enough traffic to push it out of the
  // first page (the client loads 100 at a time)
  const old = await post(bot.token, 'the needle, buried deep in history');
  for (let i = 0; i < 120; i++) await post(bot.token, 'filler ' + i);
  const root = await post(bot.token, 'a thread root');
  const reply = await post(bot.token, 'the reply worth linking', root.id);
  const doomed = await post(bot.token, 'this one gets deleted');
  const recent = await post(bot.token, 'the freshest message');

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  const ctx = browser.defaultBrowserContext();
  await ctx.overridePermissions(SERVER, ['clipboard-read', 'clipboard-write']);
    await openAsHuman(page, SERVER, slug, viewer);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await page.waitForFunction((id) => document.querySelector(`#messages .msg[data-id="${id}"]`) !== null,
    { timeout: 8000 }, recent.id);

  // 1. right-click -> Copy link to message, and the menu offers it first
  await page.evaluate((id) => {
    const el = document.querySelector(`#messages .msg[data-id="${id}"]`);
    const r = el.getBoundingClientRect();
    el.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, clientX: r.left + 60, clientY: r.top + 10 }));
  }, recent.id);
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 5000 });
  const first = await page.$eval('.context-menu .ctx-item', (b) => b.textContent);
  assert(first === 'Copy link to message', 'first menu item is ' + JSON.stringify(first));
  await page.evaluate(() => [...document.querySelectorAll('.context-menu .ctx-item')]
    .find((b) => b.textContent === 'Copy link to message').click());
  await page.waitForFunction(() => document.querySelector('#toasts .toast') !== null, { timeout: 5000 });
  const link = await page.evaluate(() => navigator.clipboard.readText());
  assert(link.endsWith('/r/' + slug + '/c/general/m/' + recent.id), 'copied link is ' + link);

  // 2. pasting it lands on the message and flashes it
  await page.goto(link, { waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => {
    const n = document.querySelector(`#messages .msg[data-id="${id}"]`);
    return n && n.classList.contains('msg-flash');
  }, { timeout: 10000 }, recent.id);

  // 3. an old message above the loaded window: the client pages back to it
  await page.goto(SERVER + '/r/' + slug + '/c/general/m/' + old.id, { waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => {
    const n = document.querySelector(`#messages .msg[data-id="${id}"]`);
    return n && n.classList.contains('msg-flash');
  }, { timeout: 20000 }, old.id);
  const centered = await page.evaluate((id) => {
    const n = document.querySelector(`#messages .msg[data-id="${id}"]`);
    const box = document.getElementById('messages').getBoundingClientRect();
    const r = n.getBoundingClientRect();
    return r.top >= box.top - 2 && r.bottom <= box.bottom + 2;
  }, old.id);
  assert(centered, 'the old message was found but not scrolled into view');
  await page.screenshot({ path: (process.env.OUT || 'tmp') + '/permalink-flash.png' });

  // 4. a threaded message opens its thread
  await page.goto(SERVER + '/r/' + slug + '/c/general/t/' + root.id + '/m/' + reply.id, { waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => {
    const n = document.querySelector(`#thread-messages .msg[data-id="${id}"]`);
    return n && n.classList.contains('msg-flash') && !document.getElementById('thread-panel').classList.contains('hidden');
  }, { timeout: 10000 }, reply.id);

  // 5. a deleted message: channel view survives, with a note
  await api('/api/v1/messages/' + doomed.id, { method: 'DELETE', token: bot.token });
  await page.goto(SERVER + '/r/' + slug + '/c/general/m/' + doomed.id, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => [...document.querySelectorAll('#toasts .toast-text')]
    .some((n) => /unavailable/i.test(n.textContent)), { timeout: 15000 });
  const alive = await page.evaluate(() => document.querySelectorAll('#messages .msg').length > 0
    && !document.getElementById('chat-view').classList.contains('hidden'));
  assert(alive, 'the channel view did not survive a dead permalink');

  await browser.close();
  console.log('PERMALINK_CHECK_OK');
})().catch((e) => { console.error('PERMALINK_CHECK_FAIL:', e.message); process.exit(1); });
