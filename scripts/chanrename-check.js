// E2E for task 20 channel rename: the admin renames #ops from the channel header
// pencil; a member's tab sees the sidebar row, the title and the address bar
// change live and the system entry land; the member has no rename control;
// a taken name is refused with a clear message.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8095 OUT=<dir> node scripts/chanrename-check.js
// two tabs: a background tab gets no animation frames in headless Chrome, so every
// wait polls on a timer instead of rAF
const puppeteer = require('puppeteer-core');
const path = require('path');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';
require('fs').mkdirSync(OUT, { recursive: true });

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}
const chanNames = (page) => page.$$eval('#channel-list li', (lis) =>
  lis.map((li) => ((li.querySelector('.chan-name') || {}).textContent || '').split(' ')[0]).filter(Boolean));
const clickChannel = (page, name) => page.evaluate((n) => {
  const li = [...document.querySelectorAll('#channel-list li')].find((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith(n));
  li.click();
}, name);
const menuItems = (page) => page.$$eval('.context-menu button, .context-menu li, .context-menu div', (els) => els.filter((e) => e.children.length === 0).map((e) => e.textContent.trim()));

let step = 'boot';
let pages = [];
(async () => {
  const created = await newRoom(SERVER, 'rename check');
  const slug = created.room.slug, code = created.invite_code;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'alice', is_human: true } });
  const bob = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'bob', is_human: true } });
  await api('/api/v1/channels', { method: 'POST', body: { name: 'ops' }, token: alice.token });
  await api('/api/v1/channels', { method: 'POST', body: { name: 'taken' }, token: alice.token });
  await api('/api/v1/channels/ops/join', { method: 'POST', token: bob.token });
  await api('/api/v1/channels/ops/messages', { method: 'POST', body: { body: 'before the rename' }, token: alice.token });

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const fail = (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; };
  const admin = await browser.newPage(); admin.on('pageerror', fail);
  const member = await browser.newPage(); member.on('pageerror', fail);
  pages = [admin, member];
  if (process.env.DEBUG) for (const [i, p] of pages.entries()) { p.on('framenavigated', (f) => console.error('nav', i, f.url())); p.on('close', () => console.error('closed', i)); p.on('console', (m) => console.error('console', i, m.type(), m.text())); p.on('error', (e) => console.error('crash', i, e.message)); }
  for (const p of [admin, member]) await p.setViewport({ width: 1100, height: 800 });

  const opsRow = () => [...document.querySelectorAll('#channel-list li')].some((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith('ops'));
  for (const [page, who] of [[member, bob], [admin, alice]]) {
    step = 'open as ' + who.participant.name;
    await openAsHuman(page, SERVER, slug, who);
    step = 'ops row for ' + who.participant.name;
    await page.waitForFunction(opsRow, { polling: 200, timeout: 10000 });
    await clickChannel(page, 'ops');
    step = 'ops feed for ' + who.participant.name;
    await page.waitForFunction(() => /before the rename/.test(document.querySelector('#messages').textContent), { polling: 200, timeout: 10000 });
  }

  step = 'member has no rename control';
  // 1. the member has no rename control: no header pencil, no menu item
  if (!(await member.$eval('#rename-channel', (el) => el.classList.contains('hidden')))) throw new Error('member sees the rename pencil');
  await member.evaluate(() => {
    const li = [...document.querySelectorAll('#channel-list li')].find((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith('ops'));
    li.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, clientX: 60, clientY: 200 }));
  });
  const memberMenu = await member.evaluate(() => [...document.querySelectorAll('div,li,button,span')].filter((e) => e.children.length === 0).map((e) => e.textContent.trim()));
  if (memberMenu.includes('Rename channel')) throw new Error('member menu offers Rename channel');
  await member.keyboard.press('Escape');

  step = 'admin pencil + taken name refused';
  // 2. the admin: pencil visible on header hover; a taken name is refused
  if (await admin.$eval('#rename-channel', (el) => el.classList.contains('hidden'))) throw new Error('admin has no rename pencil');
  step = '2 hover';
  await admin.bringToFront(); // input events need the foreground tab
  await admin.hover('#channel-header');
  step = '2 shot';
  await admin.screenshot({ path: path.join(OUT, 'chanrename-header.png') });
  // window.prompt is stubbed in-page: a native dialog blocks headless Chrome
  step = '2 stub';
  await admin.evaluate(() => {
    window.__answer = 'taken';
    window.prompt = () => window.__answer;
  });
  step = '2 click';
  await admin.click('#rename-channel');
  step = '2 toast';
  await admin.waitForFunction(() => document.querySelector('#toasts .toast.err') !== null, { polling: 200, timeout: 5000 });
  const refused = await admin.evaluate(() => document.querySelector('#toasts .toast.err .toast-text').textContent);
  if (!/already exists/.test(refused)) throw new Error('taken name not refused: ' + refused);

  // 3. rename to Ops-2 (normalized to ops-2): the admin's own tab follows
  await admin.evaluate(() => { window.__answer = 'Ops-2'; });
  await admin.click('#rename-channel');
  await admin.waitForFunction(() => document.querySelector('#channel-title').textContent.trim().endsWith('ops-2'), { polling: 200, timeout: 5000 });
  await admin.waitForFunction(() => location.pathname.endsWith('/c/ops-2'), { polling: 200, timeout: 5000 });
  const adminNames = await chanNames(admin);
  if (!adminNames.includes('ops-2') || adminNames.includes('ops')) throw new Error('admin sidebar: ' + JSON.stringify(adminNames));

  step = 'member follows live';
  await member.bringToFront();
  // 4. the member sees it live: sidebar, title, address bar, system entry
  await member.waitForFunction(() => document.querySelector('#channel-title').textContent.trim().endsWith('ops-2'), { polling: 200, timeout: 5000 });
  await member.waitForFunction(() => location.pathname.endsWith('/c/ops-2'), { polling: 200, timeout: 5000 });
  const memberNames = await chanNames(member);
  if (!memberNames.includes('ops-2') || memberNames.includes('ops')) throw new Error('member sidebar: ' + JSON.stringify(memberNames));
  await member.waitForFunction(() => [...document.querySelectorAll('#messages .msg.system-entry')].some((m) => /alice.*renamed the channel from #ops to #ops-2/.test(m.textContent)), { polling: 200, timeout: 5000 });
  await member.screenshot({ path: path.join(OUT, 'chanrename-member.png') });

  step = 'admin sidebar menu';
  await admin.bringToFront();
  // 5. the sidebar menu offers the admin a rename too
  await admin.evaluate(() => {
    const li = [...document.querySelectorAll('#channel-list li')].find((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith('ops-2'));
    li.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, clientX: 60, clientY: 200 }));
  });
  const adminMenu = await admin.evaluate(() => [...document.querySelectorAll('div,li,button,span')].filter((e) => e.children.length === 0).map((e) => e.textContent.trim()));
  if (!adminMenu.includes('Rename channel')) throw new Error('admin menu lacks Rename channel: ' + JSON.stringify(adminMenu));
  await admin.screenshot({ path: path.join(OUT, 'chanrename-menu.png') });
  await admin.keyboard.press('Escape');

  step = 'general not renamable';
  // 6. #general is never renamable
  await clickChannel(admin, 'general');
  await admin.waitForFunction(() => document.querySelector('#channel-title').textContent.trim().endsWith('general'), { polling: 200, timeout: 5000 });
  if (!(await admin.$eval('#rename-channel', (el) => el.classList.contains('hidden')))) throw new Error('#general shows the rename pencil');

  await browser.close();
  console.log('CHANRENAME_CHECK_OK');
})().catch(async (e) => {
  console.error('CHANRENAME_CHECK_FAIL:', e.message, '(step: ' + step + ')');
  for (const [i, p] of pages.entries()) console.error('page', i, await p.evaluate(() => [location.pathname, document.body.className, document.querySelectorAll('#messages').length, (document.querySelector('#messages') || {}).textContent]).catch(() => 'gone'));
  for (const [i, p] of pages.entries()) await p.screenshot({ path: path.join(OUT, 'chanrename-fail-' + i + '.png') }).catch(() => {});
  process.exit(1);
});
