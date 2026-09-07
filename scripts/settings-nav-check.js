// Headless check that the account pages are not dead ends: the app header
// (brand, username, sign out) on /settings and /create, the Back link and its
// fallbacks (?next=, same-origin referrer, /create; unsafe next ignored), the
// Continue link after a password change, the pw-banner on a room page sending
// the user to /settings with a way back, and signed-in visits to /login and
// /register skipping the form. Needs Postgres for openchatter-passwd.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/settings-nav-check.js
const puppeteer = require('puppeteer-core');
const { execFileSync } = require('child_process');
const path = require('path');
const fs = require('fs');
const { createRoom } = require('./lib/login.js');

const SERVER = process.env.SERVER || 'http://localhost:8095';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const SHOTS = process.env.SHOTS_DIR || '';
const REPO = path.resolve(__dirname, '..');

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  // the per-IP limiter (10 burst, 30/min) is shared with the browser's own calls
  if (resp.status === 429 && data.code !== 'locked_out') {
    await new Promise((r) => setTimeout(r, 3000));
    return api(p, opts);
  }
  if (resp.status >= 400) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const uniq = (prefix) => prefix + Date.now().toString(36).slice(-6) + Math.floor(Math.random() * 1000);
let lastStep = 'start';
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
const shot = async (page, name) => { lastStep = 'shot ' + name; if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name) }); };
const visible = (page, sel) => { lastStep = 'wait ' + sel; return page.waitForSelector(sel + ':not(.hidden)', { timeout: 8000 }); };
const hiddenNow = (page, sel) => page.$eval(sel, (el) => el.classList.contains('hidden'));
const href = (page, sel) => page.$eval(sel, (el) => el.href);
const text = (page, sel) => page.$eval(sel, (el) => el.textContent);
const session = (page) => page.evaluate(() => localStorage.getItem('agentchat:session'));
const setSession = (page, tok) => page.evaluate((t) => localStorage.setItem('agentchat:session', t), tok);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  if (SHOTS) fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  const errors = [];
  const statuses = [];
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
  page.on('response', (r) => { if (r.url().includes('/api/v1/')) statuses.push([r.url(), r.status()]); });
  if (process.env.DEBUG) {
    page.on('framenavigated', (f) => console.error('nav', f.url()));
    page.on('response', (r) => { if (r.url().includes('/api/v1/')) console.error('resp', r.status(), r.url()); });
    page.on('console', (m) => console.error('console', m.type(), m.text()));
  }
  const sawStatus = (frag, st) => statuses.some(([u, s]) => u.includes(frag) && s === st);
  const atPath = (p) => { lastStep = 'wait for ' + p; return page.waitForFunction((x) => location.pathname === x || location.pathname.startsWith(x + '/'), { timeout: 8000 }, p); };
  // the room page rewrites its path to /r/<slug>/c/<channel> once it boots
  const atRoom = () => { lastStep = 'wait for room'; return page.waitForFunction((x) => location.pathname.startsWith(x), { timeout: 8000 }, roomPath); };

  // the page does not retry a per-IP rate_limited 429; the check does
  const submit = async (formSel, apiFrag) => {
    for (let attempt = 0; attempt < 20; attempt++) {
      const start = statuses.length;
      lastStep = 'submit ' + formSel;
      await page.click(formSel + ' button[type=submit]');
      const deadline = Date.now() + 8000;
      let entry = null;
      while (!entry && Date.now() < deadline) {
        entry = statuses.slice(start).find(([u]) => u.includes(apiFrag));
        if (!entry) await sleep(50);
      }
      assert(entry, 'no response to ' + apiFrag + ' after ' + formSel);
      if (entry[1] !== 429) return entry;
      await sleep(3000);
    }
    throw new Error('rate limited for good on ' + apiFrag);
  };

  // an account that owns a room, so the room page boots on the session
  const user = uniq('nav');
  const pw = 'correct horse battery';
  const reg = await api('/api/v1/auth/password/register', { method: 'POST', body: { username: user, password: pw, display_name: 'Nav Tester' } });
  const created = await createRoom(SERVER, reg.token, 'nav check');
  const slug = created.room.slug;
  const roomPath = '/r/' + slug;
  const roomNext = '?next=' + encodeURIComponent(roomPath);
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await setSession(page, reg.token);

  // /settings with a room next: header with username + sign out, Back points at the room
  await page.goto(SERVER + '/settings' + roomNext, { waitUntil: 'networkidle2' });
  await visible(page, '#settings-view');
  await visible(page, '#app-header');
  await visible(page, '#app-user');
  assert(await text(page, '#app-username') === user, 'header username: ' + await text(page, '#app-username'));
  assert(await href(page, '#app-brand') === SERVER + '/', 'brand link: ' + await href(page, '#app-brand'));
  assert(await href(page, '#settings-back') === SERVER + roomPath, 'settings back: ' + await href(page, '#settings-back'));
  assert(await hiddenNow(page, '#pw-ok'), 'pw-ok shown before any change');
  await shot(page, 'settings.png');

  // change the password: the success line stays, Continue goes to the room, header stays
  const pw2 = 'different horse battery';
  await page.type('#pw-current', pw);
  await page.type('#pw-new', pw2);
  await page.type('#pw-confirm', pw2);
  const changed = await submit('#pw-form', '/api/v1/auth/password/change');
  assert(changed[1] === 204, 'change: ' + JSON.stringify(changed));
  await visible(page, '#pw-ok');
  assert(await href(page, '#pw-continue') === SERVER + roomPath, 'continue: ' + await href(page, '#pw-continue'));
  assert(!await hiddenNow(page, '#app-header'), 'header gone after the change');
  assert(await text(page, '#app-username') === user, 'header username after the change');
  await shot(page, 'settings-changed.png');
  lastStep = 'click continue';
  await page.click('#pw-continue');
  await atRoom();
  await visible(page, '#chat-view');

  // from the room page, /settings with no next falls back to the referrer (the room)
  await page.evaluate(() => { location.href = '/settings'; });
  await atPath('/settings');
  await visible(page, '#settings-view');
  assert((await href(page, '#settings-back')).startsWith(SERVER + roomPath), 'referrer back: ' + await href(page, '#settings-back'));

  // no next and no referrer: the landing (/)
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  await visible(page, '#settings-view');
  assert(await href(page, '#settings-back') === SERVER + '/', 'fallback back: ' + await href(page, '#settings-back'));

  // an unsafe next is ignored
  for (const next of ['//evil.example/x', 'javascript:alert(1)', '/login', 'https://evil.example/', '/\t/evil.example/x']) {
    lastStep = 'unsafe next ' + next;
    await page.goto(SERVER + '/settings?next=' + encodeURIComponent(next), { waitUntil: 'networkidle2' });
    await visible(page, '#settings-view');
    const got = await href(page, '#settings-back');
    assert(got === SERVER + '/', 'unsafe next ' + JSON.stringify(next) + ' honoured: ' + got);
  }

  // /create: header and Back
  await page.goto(SERVER + '/create' + roomNext, { waitUntil: 'networkidle2' });
  await visible(page, '#create-view');
  await visible(page, '#app-user');
  assert(await text(page, '#app-username') === user, 'create header username');
  assert(await href(page, '#create-back') === SERVER + roomPath, 'create back: ' + await href(page, '#create-back'));
  await shot(page, 'create.png');
  await page.goto(SERVER + '/create', { waitUntil: 'networkidle2' });
  await visible(page, '#create-view');
  assert(await href(page, '#create-back') === SERVER + '/', 'create fallback back: ' + await href(page, '#create-back'));

  // signed in: /login and /register skip the form and go to next, or land
  // in the user's workspace (/w/<slug>) when there is none
  await page.goto(SERVER + '/login' + roomNext, { waitUntil: 'networkidle2' });
  await atRoom();
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await atPath('/w/' + slug);
  await page.goto(SERVER + '/register' + roomNext, { waitUntil: 'networkidle2' });
  await atRoom();
  await page.goto(SERVER + '/register', { waitUntil: 'networkidle2' });
  await atPath('/w/' + slug);
  // and so does the root
  await page.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await atPath('/w/' + slug);
  assert(await hiddenNow(page, '#register-view'), 'register form shown to a signed-in user');

  // an operator reset: the banner on the room page links to settings with a way back
  const tempPw = 'temporary horse 1';
  execFileSync('go', ['run', './cmd/openchatter-passwd', user], {
    cwd: REPO, input: tempPw, env: Object.assign({}, process.env, { OPENCHATTER_DB_URL: DB_URL }), stdio: ['pipe', 'pipe', 'inherit'],
  });
  const relogin = await api('/api/v1/auth/password/login', { method: 'POST', body: { username: user, password: tempPw } });
  await page.goto(SERVER + roomPath, { waitUntil: 'networkidle2' });
  await setSession(page, relogin.token);
  await page.reload({ waitUntil: 'networkidle2' });
  await visible(page, '#chat-view');
  await visible(page, '#pw-banner');
  assert(await hiddenNow(page, '#app-header'), 'app header shown on a room page');
  // the room page may already have rewritten its path to the open channel
  const bannerHref = await page.$eval('#pw-banner a', (el) => el.getAttribute('href'));
  assert(bannerHref.startsWith('/settings' + roomNext), 'pw-banner link: ' + bannerHref);
  await page.click('#pw-banner a');
  await atPath('/settings');
  await visible(page, '#settings-view');
  await visible(page, '#pw-banner');
  assert((await href(page, '#settings-back')).startsWith(SERVER + roomPath), 'back from the banner: ' + await href(page, '#settings-back'));
  // the banner on an account page keeps the same way back
  assert(await page.$eval('#pw-banner a', (el) => el.getAttribute('href')) === bannerHref, 'pw-banner link on settings');
  // a legacy act_ token with no session is dead since 000027: the room page
  // goes to sign in with the room as next, and the per-slug key is scrubbed
  const legacy = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'Legacy Nav', is_human: true } });
  await page.evaluate(() => localStorage.removeItem('agentchat:session'));
  await page.evaluate((k, t) => localStorage.setItem(k, JSON.stringify({ token: t })), 'agentchat:' + slug, legacy.token);
  await page.goto(SERVER + roomPath, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => location.pathname === '/login', { timeout: 8000 });
  assert(new URL(page.url()).search.startsWith(roomNext), 'legacy token landed on ' + page.url());
  assert(await page.evaluate((k) => localStorage.getItem(k), 'agentchat:' + slug) === null, 'legacy per-slug key kept');
  await setSession(page, relogin.token);

  // sign out from the header: lands on /login, the session key is gone
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  await visible(page, '#settings-view');
  await page.click('#app-signout');
  await atPath('/login');
  await visible(page, '#login-view');
  assert(!await session(page), 'session survived sign out');
  assert(sawStatus('/api/v1/auth/logout', 204), 'logout was not called');
  // the header is there without a session too, minus the user part
  assert(!await hiddenNow(page, '#app-header'), 'header missing on /login');
  assert(await hiddenNow(page, '#app-user'), 'user part shown on /login after sign out');

  // the operator reset revokes the session the page still holds: that 401 is expected
  const realErrors = errors.filter((e) => !e.includes('favicon') && !/status of 401/.test(e));
  assert(!realErrors.length, 'page errors: ' + realErrors.join(' | '));
  console.log('SETTINGS_NAV_CHECK_OK');
  await browser.close();
  process.exit(0);
})().catch((e) => { console.error('SETTINGS_NAV_CHECK_FAIL:', e.message, '(after: ' + lastStep + ')'); process.exit(1); });
