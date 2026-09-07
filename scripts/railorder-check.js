// E2E for the rail order and the workspace mute (task 18). A has three
// workspaces: a drag moves the third mark to the top and the order survives a
// reload; B, a member of the same rooms, keeps the join order; Alt+ArrowDown
// moves a focused mark; the mark's context menu mutes the current workspace:
// the pill of a muted workspace turns gray, the tooltip says Muted, an
// @mention in the muted workspace fires no notification, Settings > Personal
// shows the toggle on and turns it off again.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 OUT=<dir> node scripts/railorder-check.js
const fs = require('fs');
const path = require('path');
const puppeteer = require('puppeteer-core');
const { call, createRoom, registerAndLogin, loginPage, openWorkspace, openSettings, uniqUser } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
let step = 'setup';
const shot = (page, name) => page.screenshot({ path: path.join(OUT, 'railorder-' + name) });
const railOrder = (page) => page.$$eval('#rail-list .rail-item', (as) => as.map((a) => a.dataset.slug));
const refocus = (page) => page.evaluate(() => window.dispatchEvent(new Event('focus')));
const waitOrder = (page, want) => page.waitForFunction((w) => [...document.querySelectorAll('#rail-list .rail-item')].map((a) => a.dataset.slug).join(',') === w, { timeout: 8000 }, want.join(','));
const badgeOf = (page, slug) => page.$eval('#rail-list .rail-item[href="/w/' + slug + '"] .rail-badge', (b) => ({ hidden: b.classList.contains('hidden'), count: b.classList.contains('count'), mention: b.classList.contains('mention'), muted: b.classList.contains('muted'), text: b.textContent, bg: getComputedStyle(b).backgroundColor }));
// HTML5 drag and drop through the same events the browser fires: the rail
// handlers read only clientY and the dragged element
const dragTo = (page, fromSlug, toSlug, edge) => page.evaluate((from, to, e) => {
  const src = document.querySelector('#rail-list .rail-item[data-slug="' + from + '"]');
  const dst = document.querySelector('#rail-list .rail-item[data-slug="' + to + '"]');
  const list = document.getElementById('rail-list');
  const dt = new DataTransfer();
  src.dispatchEvent(new DragEvent('dragstart', { bubbles: true, dataTransfer: dt }));
  const r = dst.getBoundingClientRect();
  const y = e === 'top' ? r.top + 2 : r.bottom - 2;
  list.dispatchEvent(new DragEvent('dragover', { bubbles: true, dataTransfer: dt, clientX: r.left + 10, clientY: y }));
  list.dispatchEvent(new DragEvent('drop', { bubbles: true, dataTransfer: dt, clientX: r.left + 10, clientY: y }));
  src.dispatchEvent(new DragEvent('dragend', { bubbles: true, dataTransfer: dt }));
}, fromSlug, toSlug, edge);
const serverOrder = async (session) => (await call(SERVER, '/api/v1/user', { token: session })).workspaces.map((w) => w.slug);

const errors = [];
(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });

  // A owns one, two, three; B enters all three; a poster C too
  const session = await loginPage(page, SERVER, uniqUser(), { displayName: 'avaorder' });
  const one = await createRoom(SERVER, session, 'rail one');
  const two = await createRoom(SERVER, session, 'rail two');
  const three = await createRoom(SERVER, session, 'rail three');
  const rooms = [one, two, three];
  const slugs = rooms.map((r) => r.room.slug);
  const sessionB = await registerAndLogin(SERVER, uniqUser(), 'Bea Order');
  const sessionC = await registerAndLogin(SERVER, uniqUser(), 'Cal Poster');
  for (const r of rooms) {
    await call(SERVER, '/api/v1/workspaces/' + r.room.slug + '/enter', { method: 'POST', token: sessionB, body: { invite: r.invite } });
    await call(SERVER, '/api/v1/workspaces/' + r.room.slug + '/enter', { method: 'POST', token: sessionC, body: { invite: r.invite } });
  }
  const postIn = (slug, body, channel = 'general') => call(SERVER, '/api/v1/channels/' + channel + '/messages', { method: 'POST', token: sessionC, headers: { 'X-Workspace-Slug': slug }, body: { body } });

  step = '1';
  // 1. join order in the rail
  await openWorkspace(page, SERVER, session, slugs[0]);
  await waitOrder(page, slugs);
  await shot(page, 'before.png');

  step = '2';
  // 2. drag three above one: the rail reorders and the server agrees
  await dragTo(page, slugs[2], slugs[0], 'top');
  await waitOrder(page, [slugs[2], slugs[0], slugs[1]]);
  await page.waitForFunction(() => !document.querySelector('#rail-list .rail-item.dragging'), { timeout: 4000 });
  await new Promise((r) => setTimeout(r, 500));
  assert((await serverOrder(session)).join() === [slugs[2], slugs[0], slugs[1]].join(), 'server order after drag');
  await shot(page, 'dragged.png');

  step = '3';
  // 3. a reload keeps it
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await waitOrder(page, [slugs[2], slugs[0], slugs[1]]);

  step = '4';
  // 4. B keeps the join order: the order is per account
  assert((await serverOrder(sessionB)).join() === slugs.join(), 'B order moved: ' + (await serverOrder(sessionB)).join());
  // B gets its own context: localStorage (the session key) is per origin, shared across tabs
  const ctxB = await browser.createBrowserContext();
  const pageB = await ctxB.newPage();
  await pageB.setViewport({ width: 1280, height: 800 });
  await openWorkspace(pageB, SERVER, sessionB, slugs[0]);
  await waitOrder(pageB, slugs);
  await ctxB.close();

  step = '5';
  // 5. keyboard: Alt+ArrowDown on the focused top mark moves it one down
  await page.focus('#rail-list .rail-item[data-slug="' + slugs[2] + '"]');
  await page.keyboard.down('Alt');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.up('Alt');
  await waitOrder(page, [slugs[0], slugs[2], slugs[1]]);
  await new Promise((r) => setTimeout(r, 500));
  assert((await serverOrder(session)).join() === [slugs[0], slugs[2], slugs[1]].join(), 'server order after keyboard move: ' + (await serverOrder(session)).join());

  step = '6';
  // 6. the context menu: Move up / Move down / Mute. Mute the current workspace (one)
  await page.evaluate(() => { window.__notes = []; document.addEventListener('agentchat:notify', (ev) => window.__notes.push(ev.detail)); });
  await page.click('#rail-list .rail-item[data-slug="' + slugs[0] + '"]', { button: 'right' });
  await page.waitForSelector('#rail-ctx:not(.hidden)', { timeout: 4000 });
  const items = await page.$$eval('#rail-ctx button', (bs) => bs.map((b) => b.textContent + (b.disabled ? ' (disabled)' : '')));
  assert(items.join('|') === 'Move up (disabled)|Move down|Mute workspace', 'menu items: ' + items.join('|'));
  await shot(page, 'menu.png');
  await page.click('#rail-ctx-mute');
  await page.waitForFunction((s) => document.querySelector('#rail-list .rail-item[data-slug="' + s + '"]').classList.contains('is-muted'), { timeout: 4000 }, slugs[0]);
  const tip = await page.$eval('#rail-list .rail-item[data-slug="' + slugs[0] + '"]', (a) => a.dataset.tip);
  assert(tip === 'rail one (Muted)', 'tooltip: ' + tip);
  // an @mention in the muted workspace (in a channel that is not open): no notification, the sidebar still counts it
  const side = await call(SERVER, '/api/v1/channels', { method: 'POST', token: sessionC, headers: { 'X-Workspace-Slug': slugs[0] }, body: { name: 'side' } });
  await call(SERVER, '/api/v1/channels/' + side.id + '/join', { method: 'POST', token: session, headers: { 'X-Workspace-Slug': slugs[0] } });
  await page.waitForFunction(() => [...document.querySelectorAll('#channel-list li')].some((li) => li.textContent.includes('side')), { timeout: 8000 });
  await postIn(slugs[0], 'ping @avaorder from side', 'side');
  await new Promise((r) => setTimeout(r, 2500));
  const notes = await page.evaluate(() => window.__notes);
  assert(notes.length === 0, 'a muted workspace notified: ' + JSON.stringify(notes));
  // the title never counts a muted workspace
  assert((await page.title()) === 'OpenChatter | rail one', 'muted title: ' + await page.title());

  step = '7';
  // 7. a muted workspace that is not the open one keeps its count, in gray: mute two, post an @mention there
  await page.click('#rail-list .rail-item[data-slug="' + slugs[1] + '"]', { button: 'right' });
  await page.waitForSelector('#rail-ctx:not(.hidden)', { timeout: 4000 });
  await page.click('#rail-ctx-mute');
  await page.waitForFunction((s) => document.querySelector('#rail-list .rail-item[data-slug="' + s + '"]').classList.contains('is-muted'), { timeout: 4000 }, slugs[1]);
  await postIn(slugs[1], 'hey @avaorder');
  await postIn(slugs[1], 'and more');
  await refocus(page);
  await page.waitForFunction((s) => document.querySelector('#rail-list .rail-item[href="/w/' + s + '"] .rail-badge').textContent === '2', { timeout: 8000 }, slugs[1]);
  const gray = await badgeOf(page, slugs[1]);
  assert(gray.muted && gray.count && !gray.mention && gray.bg === 'rgb(138, 143, 152)', 'muted pill: ' + JSON.stringify(gray));
  const labelMuted = await page.$eval('#rail-list .rail-item[data-slug="' + slugs[1] + '"]', (a) => a.getAttribute('aria-label'));
  assert(labelMuted === 'rail two, muted, 2 unread', 'muted aria-label: ' + labelMuted);
  assert((await page.title()) === 'OpenChatter | rail one', 'title counted a muted workspace: ' + await page.title());
  await shot(page, 'muted.png');
  // an unmuted third with a plain post: neutral pill and the title counts it
  await postIn(slugs[2], 'news in three');
  await refocus(page);
  await page.waitForFunction(() => document.title === '(1) OpenChatter | rail one', { timeout: 8000 });

  step = '8';
  // 8. Personal settings carry no workspace mute any more; the workspace-name
  // dropdown does, and unmuting there restores the colours
  await openSettings(page, SERVER, 'personal');
  await page.waitForSelector('#notify-settings:not(.hidden)', { timeout: 8000 });
  assert(!(await page.$('#ws-mute-section')) && !(await page.$('#ws-mute')), 'the settings mute block must be gone');
  await shot(page, 'settings.png');
  await openWorkspace(page, SERVER, session, slugs[0]);
  await page.waitForSelector('#ws-switcher-wrap:not(.hidden)', { timeout: 8000 });
  await page.click('#ws-switcher');
  await page.waitForSelector('#ws-menu-mute', { timeout: 4000 });
  assert((await page.$eval('#ws-menu-mute', (el) => el.textContent.trim())) === 'Unmute workspace', 'menu should offer Unmute for a muted workspace');
  await page.click('#ws-menu-mute');
  await page.waitForFunction((s) => !document.querySelector('#rail-list .rail-item[data-slug="' + s + '"]').classList.contains('is-muted'), { timeout: 4000 }, slugs[0]);
  await new Promise((r) => setTimeout(r, 500));
  const after = (await call(SERVER, '/api/v1/user', { token: session })).workspaces;
  assert(after.find((w) => w.slug === slugs[0]).muted === false, 'unmute did not persist');
  assert(after.find((w) => w.slug === slugs[1]).muted === true, 'the other mute was lost');
  // back in the room: one's mention in #side now counts in the title
  await openWorkspace(page, SERVER, session, slugs[0]);
  await page.waitForFunction(() => document.title === '(2) OpenChatter | rail one', { timeout: 8000 });
  assert(!(await page.$eval('#rail-list .rail-item[data-slug="' + slugs[0] + '"]', (a) => a.classList.contains('is-muted'))), 'one still muted in the rail');

  await browser.close();
  if (errors.length) throw new Error('page errors: ' + errors.join(' | '));
  console.log('RAILORDER_CHECK_OK');
})().catch((e) => { console.error('FAIL at step ' + step + ': ' + e.message + (errors.length ? ' | page errors: ' + errors.join(' | ') : '')); process.exit(1); });
