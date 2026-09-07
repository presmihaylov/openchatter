// E2E for per-workspace badges in the rail (task 13, counts in task 18). A
// belongs to two workspaces and sits in the first; B posts in the second.
// After the focus refresh, A's rail shows a neutral "1" pill on the second
// mark; an @mention turns it red; 100 unreads read "99+"; the tab title and
// the favicon carry the total; opening the second workspace clears its badge
// and it stays clear on the way back.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 OUT=<dir> node scripts/railbadge-check.js
const fs = require('fs');
const path = require('path');
const puppeteer = require('puppeteer-core');
const { call, createRoom, registerAndLogin, loginPage, openWorkspace, uniqUser, switchTo } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
let step = 'setup';
const shot = (page, name) => page.screenshot({ path: path.join(OUT, 'railbadge-' + name) });
const badgeOf = (page, slug) => page.$eval('#rail-list .rail-item[href="/w/' + slug + '"] .rail-badge', (b) => ({ hidden: b.classList.contains('hidden'), count: b.classList.contains('count'), text: b.textContent }));
// the tab regaining focus is the immediate refresh; the 60 s timer is the slow path
const refocus = (page) => page.evaluate(() => window.dispatchEvent(new Event('focus')));
const waitBadge = (page, slug, want) => page.waitForFunction((s, w) => {
  const b = document.querySelector('#rail-list .rail-item[href="/w/' + s + '"] .rail-badge');
  if (!b) return false;
  const st = b.classList.contains('hidden') ? 'none' : b.classList.contains('mention') ? 'mention:' + b.textContent : b.classList.contains('count') ? 'count:' + b.textContent : 'dot';
  return st === w;
}, { timeout: 8000 }, slug, want);

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

  // A owns "home" and "away"; B enters "away" and posts there
  const session = await loginPage(page, SERVER, uniqUser(), { displayName: 'avabadge' });
  const home = await createRoom(SERVER, session, 'home base');
  const away = await createRoom(SERVER, session, 'away team');
  const sessionB = await registerAndLogin(SERVER, uniqUser(), 'Bo Poster');
  await call(SERVER, '/api/v1/workspaces/' + away.room.slug + '/enter', { method: 'POST', token: sessionB, body: { invite: away.invite } });
  const postAway = (body) => call(SERVER, '/api/v1/channels/general/messages', { method: 'POST', token: sessionB, headers: { 'X-Workspace-Slug': away.room.slug }, body: { body } });

  step = '1';
  // 1. clean rail in home; nothing on away
  await openWorkspace(page, SERVER, session, home.room.slug);
  await page.waitForFunction(() => document.querySelectorAll('#rail-list .rail-item').length === 2, { timeout: 8000 });
  assert((await badgeOf(page, away.room.slug)).hidden, 'away badged before any post');
  assert((await badgeOf(page, home.room.slug)).hidden, 'home badged');

  const favicon = () => page.$eval('link[rel="icon"]', (l) => l.getAttribute('href'));
  assert(await page.title() === 'OpenChatter | home base', 'clean title: ' + await page.title());
  assert((await favicon()).startsWith('/brand/'), 'clean favicon: ' + await favicon());

  step = '2';
  // 2. a plain post from B: a neutral "1" pill on away after the focus refresh,
  // the title and the favicon carry the total
  await postAway('news from away');
  await refocus(page);
  await waitBadge(page, away.room.slug, 'count:1');
  const plain = await page.$eval('#rail-list .rail-item[href="/w/' + away.room.slug + '"] .rail-badge', (b) => { const c = getComputedStyle(b); return { bg: c.backgroundColor, text: b.textContent }; });
  assert(plain.bg !== 'rgb(237, 66, 69)', 'plain unread painted red: ' + JSON.stringify(plain));
  await page.waitForFunction(() => document.title === '(1) OpenChatter | home base', { timeout: 4000 });
  assert((await favicon()).startsWith('data:image/png'), 'favicon not drawn: ' + await favicon());
  const labelPlain = await page.$eval('#rail-list .rail-item[href="/w/' + away.room.slug + '"]', (a) => a.getAttribute('aria-label'));
  assert(labelPlain === 'away team, 1 unread', 'aria-label: ' + labelPlain);
  await shot(page, 'dot.png');

  step = '3';
  // 3. an @mention: the pill turns red and shows the mention count; the current mark stays clean
  await postAway('hey @avabadge look at this');
  await refocus(page);
  await waitBadge(page, away.room.slug, 'mention:1');
  assert((await badgeOf(page, home.room.slug)).hidden, 'home badged by away traffic');
  const label = await page.$eval('#rail-list .rail-item[href="/w/' + away.room.slug + '"]', (a) => a.getAttribute('aria-label'));
  assert(label === 'away team, 1 mentions', 'aria-label: ' + label);
  await page.waitForFunction(() => document.title === '(2) OpenChatter | home base', { timeout: 4000 });
  // the count badge is the alert red with a white bold count (task 20, Maya msg 4561407a)
  const paint = await page.$eval('#rail-list .rail-item[href="/w/' + away.room.slug + '"] .rail-badge', (b) => { const c = getComputedStyle(b); return { bg: c.backgroundColor, fg: c.color, weight: c.fontWeight }; });
  assert(paint.bg === 'rgb(237, 66, 69)' && paint.fg === 'rgb(255, 255, 255)' && Number(paint.weight) >= 600, 'badge paint: ' + JSON.stringify(paint));
  await shot(page, 'mention.png');

  step = '3b';
  // 3b. 100 plain unreads: the pill caps at 99+ (mentions still win the colour), the title does not cap
  for (let i = 0; i < 98; i++) await postAway('flood ' + i);
  await refocus(page);
  await waitBadge(page, away.room.slug, 'mention:1');
  await page.waitForFunction(() => document.title === '(100) OpenChatter | home base', { timeout: 8000 }).catch(async () => { throw new Error('title after flood: ' + await page.title() + ' badge ' + JSON.stringify(await badgeOf(page, away.room.slug))); });
  await postAway('@avabadge again');
  await refocus(page);
  await waitBadge(page, away.room.slug, 'mention:2');
  // read it as B would see a plain flood: a fresh account with no mention gets the neutral 99+
  const sessionC = await registerAndLogin(SERVER, uniqUser(), 'Cy Reader');
  await call(SERVER, '/api/v1/workspaces/' + away.room.slug + '/enter', { method: 'POST', token: sessionC, body: { invite: away.invite } });
  await call(SERVER, '/api/v1/workspaces/' + home.room.slug + '/enter', { method: 'POST', token: sessionC, body: { invite: home.invite } });
  for (let i = 0; i < 100; i++) await postAway('for cy ' + i);
  // C gets its own context: localStorage (the session key) is per origin, shared across tabs
  const ctxC = await browser.createBrowserContext();
  const pageC = await ctxC.newPage();
  await pageC.setViewport({ width: 1280, height: 800 });
  await openWorkspace(pageC, SERVER, sessionC, home.room.slug);
  await waitBadge(pageC, away.room.slug, 'count:99+');
  await pageC.waitForFunction(() => document.title === '(100) OpenChatter | home base', { timeout: 8000 });
  await pageC.screenshot({ path: path.join(OUT, 'railbadge-99plus.png') });
  await ctxC.close();

  step = '4';
  // 4. opening away clears it (the channel read marker is written on open)
  await switchTo(page, away.room.slug);
  await page.waitForFunction(() => document.querySelectorAll('#rail-list .rail-item').length === 2, { timeout: 8000 });
  await page.waitForSelector('.msg', { timeout: 8000 });
  await waitBadge(page, away.room.slug, 'none');
  await shot(page, 'opened.png');

  step = '5';
  // 5. back in home, a refresh still shows away clean: the marker stuck
  await openWorkspace(page, SERVER, session, home.room.slug);
  await page.waitForFunction(() => document.querySelectorAll('#rail-list .rail-item').length === 2, { timeout: 8000 });
  await refocus(page);
  await new Promise((r) => setTimeout(r, 800));
  assert((await badgeOf(page, away.room.slug)).hidden, 'away still badged after being read');
  await shot(page, 'clear.png');

  await browser.close();
  if (errors.length) throw new Error('page errors: ' + errors.join(' | '));
  console.log('RAILBADGE_CHECK_OK');
})().catch((e) => { console.error('FAIL at step ' + step + ': ' + e.message + (errors.length ? ' | page errors: ' + errors.join(' | ') : '')); process.exit(1); });
