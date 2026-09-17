// E2E for the ⋮ "More actions" button on the message hover toolbar. It opens
// the same menu as right-click, gathers every message action, is reachable
// by keyboard (Tab + Enter, menu focuses its first item, Escape closes), and
// copying a thread reply's link points into the thread at that reply.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8095 node scripts/moreactions-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const menuLabels = (page) => page.$$eval('.context-menu .ctx-item', (bs) => bs.map((b) => b.textContent));

(async () => {
  const created = await newRoom(SERVER, 'more actions check');
  const slug = created.room.slug, code = created.invite_code;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'alice', is_human: true } });
  const bob = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'bob', is_human: true } });
  const root = await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'root by bob' }, token: bob.token });
  const reply = await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'reply by alice', thread_root_id: root.id }, token: alice.token });
  const mine = await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'mine by alice' }, token: alice.token });

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });
  await browser.defaultBrowserContext().overridePermissions(SERVER, ['clipboard-read', 'clipboard-write']);
    await openAsHuman(page, SERVER, slug, alice);
  await page.waitForFunction((id) => document.querySelector(`#messages .msg[data-id="${id}"]`) !== null, { timeout: 8000 }, mine.id);

  // 1. ⋮ is the last toolbar item, titled "More actions"; hover reveals it
  const more = (id) => `#messages .msg[data-id="${id}"] .msg-actions button:last-child`;
  const title = await page.$eval(more(root.id), (b) => b.title + '|' + (b.querySelector('svg[data-icon]') || {}).dataset?.icon + '|' + b.textContent.trim());
  assert(title === 'More actions|more-vertical|', 'last toolbar button is ' + title);
  // the other toolbar buttons hold a monochrome SVG, never literal template text
  const icons = await page.$$eval(`#messages .msg[data-id="${root.id}"] .msg-actions button:not(:last-child)`, (bs) => bs.map((b) => b.dataset.act + ':' + ((b.querySelector('svg.lucide') || {}).dataset || {}).icon + ':' + b.textContent.trim()));
  assert(icons.every((i) => /^(react:smile-plus|thread:message-square|edit:pencil|delete:trash-2):$/.test(i)), 'toolbar icons: ' + JSON.stringify(icons));
  // the toolbar never joins a text selection
  assert(await page.$eval(`#messages .msg[data-id="${root.id}"] .msg-actions`, (el) => getComputedStyle(el).userSelect === 'none'), 'toolbar is selectable');
  await page.hover(`#messages .msg[data-id="${root.id}"]`);
  const visible = await page.$eval(more(root.id), (b) => getComputedStyle(b.parentElement).opacity === '1');
  assert(visible, 'toolbar not revealed on hover');

  // 2. click opens the menu with every action for someone else's root
  await page.click(more(root.id));
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 4000 });
  let labels = await menuLabels(page);
  // (alice is the room's first member, so as admin she may also see Delete)
  assert(JSON.stringify(labels.slice(0, 3)) === JSON.stringify(['Copy link to message', 'Reply in thread', 'Subscribe'])
    && !labels.includes('Edit message'), 'menu on another\'s root: ' + JSON.stringify(labels));
  // anchored under the button, not at the cursor origin
  const anchored = await page.evaluate((sel) => {
    const b = document.querySelector(sel).getBoundingClientRect();
    const m = document.querySelector('.context-menu').getBoundingClientRect();
    return m.top >= b.bottom && Math.abs(m.left - b.left) < 200;
  }, more(root.id));
  assert(anchored, 'menu not anchored to the ⋮ button');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('.context-menu'), { timeout: 2000 });

  // 3. own message adds Edit and Delete
  await page.hover(`#messages .msg[data-id="${mine.id}"]`);
  await page.click(more(mine.id));
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 4000 });
  labels = await menuLabels(page);
  assert(labels.includes('Edit message') && labels.includes('Delete message'), 'own message menu: ' + JSON.stringify(labels));
  await page.keyboard.press('Escape');

  // 4. keyboard: focus the button, Enter opens, first item focused, Escape closes
  await page.evaluate((sel) => document.querySelector(sel).focus(), more(root.id));
  const focusedVisible = await page.$eval(more(root.id), (b) => getComputedStyle(b.parentElement).opacity === '1');
  assert(focusedVisible, 'toolbar not revealed on keyboard focus');
  await page.keyboard.press('Enter');
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 4000 });
  const firstFocused = await page.evaluate(() => document.activeElement === document.querySelector('.context-menu .ctx-item'));
  assert(firstFocused, 'first menu item not focused after keyboard open');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('.context-menu'), { timeout: 2000 });

  // 5. Reply in thread from the menu opens the thread; a reply's ⋮ copies a
  //    link that lands in the thread at that reply
  await page.click(more(root.id));
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 4000 });
  await page.evaluate(() => [...document.querySelectorAll('.context-menu .ctx-item')].find((b) => b.textContent === 'Reply in thread').click());
  const inThread = `#thread-messages .msg[data-id="${reply.id}"]`;
  await page.waitForSelector(inThread, { timeout: 6000 });
  await page.hover(inThread);
  await page.click(inThread + ' .msg-actions button:last-child');
  await page.waitForSelector('.context-menu .ctx-item', { timeout: 4000 });
  labels = await menuLabels(page);
  assert(!labels.includes('Reply in thread'), 'a reply must not offer Reply in thread: ' + JSON.stringify(labels));
  await page.evaluate(() => [...document.querySelectorAll('.context-menu .ctx-item')].find((b) => b.textContent === 'Copy link to message').click());
  await page.waitForFunction(() => document.querySelector('#toasts .toast') !== null, { timeout: 5000 });
  const link = await page.evaluate(() => navigator.clipboard.readText());
  assert(link.endsWith(`/r/${slug}/c/general/t/${root.id}/m/${reply.id}`), 'reply link is ' + link);
  await page.goto(link, { waitUntil: 'networkidle2' });
  await page.waitForSelector(inThread, { timeout: 8000 });

  await browser.close();
  console.log('MOREACTIONS_CHECK_OK');
})().catch((e) => { console.error('MOREACTIONS_CHECK_FAIL', e.message); process.exit(1); });
