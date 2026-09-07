// Headless check of the workspace switcher (task 05): user A owns two
// workspaces, "/" lands on the last active one, the switcher lists both and a
// switch is a full load of /w/<slug>; A enters B's workspace through
// #enter-view and posts; a fresh user C meets #no-ws-view on "/" and enters
// through its invite form; B revokes A, whose next load shows the removed
// notice and whose switcher drops the workspace; A's sixth create is a 409
// workspace_quota shown inline on /create.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8090 node scripts/switcher-check.js
const puppeteer = require('puppeteer-core');
const path = require('path');
const os = require('os');
const fs = require('fs');
const { call, createRoom, registerAndLogin, loginPage, enterWithCode, openWorkspace, switchTo, uniqUser } = require('./lib/login.js');

const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || os.tmpdir();

let lastStep = 'start';
const shot = async (page, name) => { lastStep = 'shot ' + name; await page.screenshot({ path: path.join(OUT, 'switcher-' + name) }); };
const visible = (page, sel) => { lastStep = 'wait ' + sel; return page.waitForSelector(sel + ':not(.hidden)', { timeout: 8000 }); };
const hiddenNow = (page, sel) => page.$eval(sel, (el) => el.classList.contains('hidden'));
const text = (page, sel) => page.$eval(sel, (el) => el.textContent);
// a browser create derives its slug from a fixed name; rerunning would collide, so
// the test overrides the preview with a unique slug the way a user could
const uniqSlug = (page, sel, base) => page.$eval(sel, (el, v) => { el.value = v; }, base + '-' + uniqUser().slice(-8));
// a room page normalizes its path to /c/<channel> right after boot, so a workspace path matches by prefix
const atPath = (page, p) => { lastStep = 'wait for ' + p; return page.waitForFunction((x) => location.pathname === x || location.pathname.startsWith(x + '/'), { timeout: 8000 }, p); };
const roomApi = (p, session, slug, opts = {}) => call(SERVER, p, Object.assign({ token: session, headers: { 'X-Workspace-Slug': slug } }, opts));
// the switcher mounts after its own GET /api/v1/user, a beat after the chat shows
const openMenu = async (page) => { await visible(page, '#ws-switcher-wrap'); await page.click('#ws-switcher'); await visible(page, '#ws-menu'); };
// the rail lists the workspaces; the header menu carries only actions
const railSlugs = (page) => page.$$eval('#rail-list a.rail-item', (els) => els.map((a) => a.getAttribute('href')).filter((h) => h.startsWith('/w/')).map((h) => h.slice(3)));
const menuSlugs = (page) => page.$$eval('#ws-menu a.ws-item', (els) => els.map((a) => a.getAttribute('href')).filter((h) => h.startsWith('/w/')).map((h) => h.slice(3)));

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const statuses = [];
  const freshPage = async () => {
    const ctx = await browser.createBrowserContext();
    const page = await ctx.newPage();
    await page.setViewport({ width: 1280, height: 800 });
    page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
    page.on('response', (r) => {
      if (!r.url().includes('/api/v1/')) return;
      const entry = [r.url(), r.status(), null];
      statuses.push(entry);
      r.json().then((d) => { entry[2] = d && d.code; }, () => {});
    });
    if (process.env.DEBUG) {
      page.on('framenavigated', (f) => console.error('nav', f.url()));
      page.on('response', (r) => { if (r.url().includes('/api/v1/')) console.error('resp', r.status(), r.url()); });
    }
    return page;
  };
  const lastResponse = (frag) => [...statuses].reverse().find(([u]) => u.includes(frag));

  // A creates "ws zulu" in the browser (lands on /w/<slug>) and "ws alpha" through
  // the API: join order runs against name order, so a name sort fails below
  const pageA = await freshPage();
  const sessionA = await loginPage(pageA, SERVER, uniqUser(), { displayName: 'Ada Switcher', next: '/create' });
  await visible(pageA, '#create-view');
  await pageA.type('#create-room-name', 'ws zulu');
  await uniqSlug(pageA, '#create-room-slug', 'ws-zulu');
  await pageA.click('#create-form button[type=submit]');
  await pageA.waitForFunction(() => location.pathname.startsWith('/w/'), { timeout: 8000 });
  const slug1 = new URL(pageA.url()).pathname.split('/')[2];
  await visible(pageA, '#chat-view');
  await visible(pageA, '#ws-switcher-wrap');
  await pageA.waitForFunction(() => document.querySelector('#ws-current').textContent === 'ws zulu', { timeout: 8000 });
  // the tab title names the app and the workspace (task 20)
  await pageA.waitForFunction(() => document.title === 'OpenChatter | ws zulu', { timeout: 8000 });
  const two = await createRoom(SERVER, sessionA, 'ws alpha');
  const slug2 = two.room.slug;

  // opening ws alpha makes it the last active one; "/" then lands there
  await pageA.goto(SERVER + '/w/' + slug2, { waitUntil: 'networkidle2' });
  await visible(pageA, '#chat-view');
  await pageA.waitForFunction(() => document.querySelector('#ws-current').textContent === 'ws alpha', { timeout: 8000 });
  await pageA.waitForFunction(() => document.title === 'OpenChatter | ws alpha', { timeout: 8000 });
  // a workspace rename reaches the open tab live: header, switcher and title
  await call(SERVER, '/api/v1/room', { method: 'PATCH', token: sessionA, body: { name: 'ws alpha 2' }, headers: { 'X-Workspace-Slug': slug2 } });
  await pageA.waitForFunction(() => document.title === 'OpenChatter | ws alpha 2' && document.querySelector('#ws-current').textContent === 'ws alpha 2', { timeout: 8000 });
  await call(SERVER, '/api/v1/room', { method: 'PATCH', token: sessionA, body: { name: 'ws alpha' }, headers: { 'X-Workspace-Slug': slug2 } });
  await pageA.waitForFunction(() => document.title === 'OpenChatter | ws alpha', { timeout: 8000 });
  const userA = await call(SERVER, '/api/v1/user', { token: sessionA });
  if (userA.last_active_workspace_id !== two.room.id) throw new Error('last_active after opening ws alpha: ' + JSON.stringify(userA));
  if (userA.workspaces.map((w) => w.slug).join(',') !== slug1 + ',' + slug2) throw new Error('workspaces order: ' + JSON.stringify(userA.workspaces));
  await pageA.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await atPath(pageA, '/w/' + slug2);
  await visible(pageA, '#chat-view');
  await pageA.waitForFunction(() => document.querySelector('#ws-current').textContent === 'ws alpha', { timeout: 8000 });
  // the address bar keeps /w/ after the SPA normalizes the channel path
  await pageA.waitForFunction(() => location.pathname.includes('/c/'), { timeout: 8000 });
  if (!pageA.url().startsWith(SERVER + '/w/' + slug2 + '/c/')) throw new Error('normalized path left /w/: ' + pageA.url());

  // the menu: workspace actions only, no workspace rows (the rail is the switcher); Invite member as the creator is admin
  await openMenu(pageA);
  if (await pageA.$eval('#ws-switcher', (el) => el.getAttribute('aria-expanded')) !== 'true') throw new Error('aria-expanded not true when open');
  const items = await pageA.$$eval('#ws-menu .ws-item', (els) => els.map((e) => e.textContent));
  if (items.join('|') !== 'Invite member|Join with invite link|Mute workspace|Settings') throw new Error('menu items: ' + items.join('|'));
  if ((await menuSlugs(pageA)).length) throw new Error('menu still lists workspaces: ' + await menuSlugs(pageA));
  if (!await pageA.$('#ws-menu .ws-sep')) {} else throw new Error('menu still has a divider');
  if ((await railSlugs(pageA)).sort().join(',') !== [slug1, slug2].sort().join(',')) throw new Error('rail links: ' + await railSlugs(pageA));
  await shot(pageA, 'menu.png');
  // Escape closes and returns focus to the button; click outside closes too
  await pageA.keyboard.press('Escape');
  if (!await hiddenNow(pageA, '#ws-menu')) throw new Error('Escape did not close the menu');
  if (await pageA.$eval('#ws-switcher', (el) => el.getAttribute('aria-expanded')) !== 'false') throw new Error('aria-expanded not false when closed');
  if (await pageA.evaluate(() => document.activeElement && document.activeElement.id) !== 'ws-switcher') throw new Error('Escape did not refocus the switcher');
  await openMenu(pageA);
  await pageA.click('#messages');
  if (!await hiddenNow(pageA, '#ws-menu')) throw new Error('outside click did not close the menu');

  // switch to ws zulu: a full load of /w/<slug1> and the header follows
  await switchTo(pageA, slug1);
  if (!pageA.url().startsWith(SERVER + '/w/' + slug1)) throw new Error('switch landed on ' + pageA.url());
  await pageA.waitForFunction(() => document.querySelector('#ws-current').textContent === 'ws zulu', { timeout: 8000 });
  await visible(pageA, '#ws-switcher-wrap');
  if (!await hiddenNow(pageA, '#room-head')) throw new Error('plain room name shown next to the switcher');
  await shot(pageA, 'switched.png');

  // B creates ws three (with a name long enough to overflow the sidebar); A
  // enters it through #enter-view and posts
  const NAME3 = 'ws three with a name long enough to overflow the sidebar menu';
  const sessionB = await registerAndLogin(SERVER, uniqUser(), 'Bea Owner');
  const three = await createRoom(SERVER, sessionB, NAME3);
  const slug3 = three.room.slug;
  await pageA.goto(SERVER + '/w/' + slug3, { waitUntil: 'networkidle2' });
  await visible(pageA, '#enter-view');
  await pageA.waitForFunction((n) => document.querySelector('#enter-room-name').textContent.includes(n), { timeout: 8000 }, NAME3);
  await enterWithCode(pageA, three.invite_code);
  await pageA.waitForFunction((n) => document.querySelector('#ws-current').textContent === n, { timeout: 8000 }, NAME3);
  await pageA.waitForSelector('#composer-input', { timeout: 8000 });
  await pageA.type('#composer-input', 'hello from the switcher');
  await pageA.keyboard.press('Enter');
  await pageA.waitForFunction(() => document.querySelector('#messages').textContent.includes('hello from the switcher'), { timeout: 8000 });
  if ((await railSlugs(pageA)).sort().join(',') !== [slug1, slug2, slug3].sort().join(',')) throw new Error('rail after entering ws three: ' + await railSlugs(pageA));
  await openMenu(pageA);
  // the long name truncates in the header; the menu never widens past the
  // sidebar, which would scroll sideways and clip it
  const fit = await pageA.evaluate(() => {
    const sb = document.querySelector('#sidebar');
    const cur = document.querySelector('#ws-current');
    return { sbScroll: sb.scrollWidth, sbClient: sb.clientWidth, sbRight: sb.getBoundingClientRect().right, menuRight: document.querySelector('#ws-menu').getBoundingClientRect().right, curOverflow: cur.scrollWidth > cur.clientWidth };
  });
  if (fit.sbScroll > fit.sbClient || fit.menuRight > fit.sbRight) throw new Error('menu overflows the sidebar: ' + JSON.stringify(fit));
  if (!fit.curOverflow) throw new Error('long name not truncated in the header: ' + JSON.stringify(fit));
  await pageA.keyboard.press('Escape');
  await shot(pageA, 'entered.png');

  // C has no workspace: "/" shows #no-ws-view, the join form takes an invite link
  const pageC = await freshPage();
  await loginPage(pageC, SERVER, uniqUser(), { displayName: 'Cy Newcomer' });
  await pageC.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await visible(pageC, '#no-ws-view');
  await atPath(pageC, '/');
  if (!await hiddenNow(pageC, '#login-view')) throw new Error('login form shown behind #no-ws-view');
  await shot(pageC, 'no-ws.png');
  await pageC.type('#no-ws-enter-link', 'inv-0000-0000-0000-0000 is not a link');
  await pageC.click('#no-ws-enter-form button[type=submit]');
  await visible(pageC, '#no-ws-enter-error');
  await pageC.waitForFunction(() => /invite link/.test(document.querySelector('#no-ws-enter-error').textContent), { timeout: 8000 });
  await pageC.$eval('#no-ws-enter-link', (el) => { el.value = ''; });
  await pageC.type('#no-ws-enter-link', SERVER + '/join/inv-0000-0000-0000-0000');
  await Promise.all([
    pageC.waitForNavigation({ waitUntil: 'networkidle2', timeout: 8000 }),
    pageC.click('#no-ws-enter-form button[type=submit]'),
  ]);
  await visible(pageC, '#join-view');
  await pageC.waitForFunction(() => /no longer works/.test(document.querySelector('#join-title').textContent), { timeout: 8000 });
  const wrong = lastResponse('/invites/peek');
  if (!wrong || wrong[1] !== 404) throw new Error('wrong link: ' + JSON.stringify(wrong));
  await pageC.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await visible(pageC, '#no-ws-view');
  await pageC.type('#no-ws-enter-link', three.invite);
  await Promise.all([
    pageC.waitForNavigation({ waitUntil: 'networkidle2', timeout: 8000 }),
    pageC.click('#no-ws-enter-form button[type=submit]'),
  ]);
  await pageC.waitForFunction((p) => location.pathname === p || location.pathname.startsWith(p + '/'), { timeout: 8000 }, '/w/' + slug3);
  await atPath(pageC, '/w/' + slug3);
  await visible(pageC, '#chat-view');
  await pageC.waitForFunction(() => document.querySelector('#messages').textContent.includes('hello from the switcher'), { timeout: 8000 });
  await pageC.waitForFunction(() => document.querySelector('#participant-list').textContent.includes('Cy Newcomer'), { timeout: 8000 });
  // C's create form on "/" is the other door: a fresh user D takes it
  const pageD = await freshPage();
  await loginPage(pageD, SERVER, uniqUser(), { displayName: 'Dee Founder' });
  await pageD.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await visible(pageD, '#no-ws-view');
  await pageD.type('#no-ws-create-name', 'ws four');
  await uniqSlug(pageD, '#no-ws-create-slug', 'ws-four');
  await Promise.all([
    pageD.waitForNavigation({ waitUntil: 'networkidle2', timeout: 8000 }),
    pageD.click('#no-ws-create-form button[type=submit]'),
  ]);
  if (!pageD.url().startsWith(SERVER + '/w/')) throw new Error('no-ws create landed on ' + pageD.url());
  await visible(pageD, '#chat-view');
  await pageD.waitForFunction(() => document.querySelector('#ws-current').textContent === 'ws four', { timeout: 8000 });

  // B revokes A from ws three: the removed notice, and the switcher drops it
  const meA3 = await roomApi('/api/v1/me', sessionA, slug3);
  await roomApi('/api/v1/participants/' + meA3.id, sessionB, slug3, { method: 'DELETE' });
  await pageA.goto(SERVER + '/w/' + slug3, { waitUntil: 'networkidle2' });
  await visible(pageA, '#removed-view');
  if (!await hiddenNow(pageA, '#chat-view')) throw new Error('chat view shown to a revoked user');
  await shot(pageA, 'removed.png');
  const userAfter = await call(SERVER, '/api/v1/user', { token: sessionA });
  if (userAfter.workspaces.some((w) => w.slug === slug3)) throw new Error('revoked workspace still listed: ' + JSON.stringify(userAfter.workspaces));
  if (userAfter.last_active_workspace_id) throw new Error('last_active still points somewhere after the revoke: ' + userAfter.last_active_workspace_id);
  // "/" falls back to the first workspace when the last active one is gone
  await pageA.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await atPath(pageA, '/w/' + slug1);
  await visible(pageA, '#chat-view');
  await openMenu(pageA);
  // the rail is the switcher: the revoked workspace is gone, the other two stay; the menu lists no workspaces
  if ((await railSlugs(pageA)).sort().join(',') !== [slug1, slug2].sort().join(',')) throw new Error('rail after the revoke: ' + await railSlugs(pageA));
  if ((await menuSlugs(pageA)).length) throw new Error('menu lists workspaces after the revoke: ' + await menuSlugs(pageA));
  await pageA.keyboard.press('Escape');

  // quota: A owns two, three more through the API make five, the sixth is a 409 on /create
  for (const name of ['ws three of A', 'ws four of A', 'ws five of A']) await createRoom(SERVER, sessionA, name);
  await pageA.goto(SERVER + '/create', { waitUntil: 'networkidle2' });
  await visible(pageA, '#create-view');
  await pageA.type('#create-room-name', 'ws six of A');
  await uniqSlug(pageA, '#create-room-slug', 'ws-six');
  await pageA.click('#create-form button[type=submit]');
  await visible(pageA, '#create-error');
  await pageA.waitForFunction(() => /maximum number of workspaces/.test(document.querySelector('#create-error').textContent), { timeout: 8000 });
  const quota = lastResponse('/api/v1/rooms');
  if (!quota || quota[1] !== 409 || quota[2] !== 'workspace_quota') throw new Error('sixth create: ' + JSON.stringify(quota));
  if (!pageA.url().startsWith(SERVER + '/create')) throw new Error('quota error left /create: ' + pageA.url());
  await shot(pageA, 'quota.png');

  // Sign out lives in the personal menu under the profile row and ends the session
  await openWorkspace(pageA, SERVER, sessionA, slug1);
  await pageA.click('#me-footer');
  await visible(pageA, '#me-menu');
  await Promise.all([
    pageA.waitForNavigation({ waitUntil: 'networkidle2', timeout: 8000 }),
    pageA.click('#me-signout'),
  ]);
  await atPath(pageA, '/login');
  if (await pageA.evaluate(() => localStorage.getItem('agentchat:session')) !== null) throw new Error('session survived sign out');

  // the expected failures (403 non-member and revoked, 400 wrong code, 409
  // quota) log as console errors; nothing else may
  const realErrors = errors.filter((e) => !e.includes('favicon') && !/status of (400|403|404|409)/.test(e));
  if (realErrors.length) throw new Error('page errors: ' + realErrors.join(' | '));
  const okCodes = { 400: ['invite_invalid'], 403: ['workspace_forbidden'], 404: [null, undefined], 409: ['workspace_quota'] };
  const odd = statuses.filter(([, st, c]) => st >= 400 && !(okCodes[st] || []).includes(c));
  if (odd.length) throw new Error('unexpected error responses: ' + JSON.stringify(odd));

  console.log('SWITCHER_CHECK_OK');
  await browser.close();
  process.exit(0);
})().catch((e) => { console.error('SWITCHER_CHECK_FAIL:', e.message, '(after: ' + lastStep + ')'); process.exit(1); });
