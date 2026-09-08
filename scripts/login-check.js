// Headless check of the account pages: register, sign out, sign in, wrong
// password, lockout, the must_change_password banner, changing the password
// on /settings (clears the banner, signs the other tab out), a room page that
// boots on the session (and forgets the legacy per-slug act_ token) and one
// that bounces a legacy act_ token to sign in, since 000027 retired them.
// Needs Postgres for openchatter-passwd (OPENCHATTER_DB_URL, default the dev db).
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
  return { status: resp.status, data };
}

const uniq = (prefix) => prefix + Date.now().toString(36).slice(-6) + Math.floor(Math.random() * 1000);
const shot = async (page, name) => { lastStep = 'shot ' + name; if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name) }); };
let lastStep = 'start';
const visible = (page, sel) => { lastStep = 'wait ' + sel; return page.waitForSelector(sel + ':not(.hidden)', { timeout: 8000 }); };
const hiddenNow = (page, sel) => page.$eval(sel, (el) => el.classList.contains('hidden'));
const session = (page) => page.evaluate(() => localStorage.getItem('agentchat:session'));

// the server's own verdict on a token, from inside the page (default: the page's own session)
const userStatus = (page, tok) => page.evaluate(async (t) => {
  const r = await fetch('/api/v1/user', { headers: { Authorization: 'Bearer ' + (t || localStorage.getItem('agentchat:session')) } });
  return r.status;
}, tok || null);

(async () => {
  if (SHOTS) fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  const errors = [];
  const statuses = []; // [url, status, code] of every API response the page saw
  const allStatuses = []; // the same, never reset
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
  page.on('response', (r) => {
    if (!r.url().includes('/api/v1/')) return;
    const entry = [r.url(), r.status(), null];
    statuses.push(entry);
    allStatuses.push(entry);
    // the body read loses the race against a navigation on success; the code only matters on errors
    r.json().then((d) => { entry[2] = d && d.code; }, () => {});
  });
  if (process.env.DEBUG) {
    page.on('framenavigated', (f) => console.error('nav', f.url()));
    page.on('response', (r) => { if (r.url().includes('/api/v1/')) console.error('resp', r.status(), r.url()); });
    page.on('console', (m) => console.error('console', m.type(), m.text()));
  }
  const sawStatus = (frag, st) => statuses.some(([u, s]) => u.includes(frag) && s === st);
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // submit a form and hand back the API response it caused; the page does not
  // retry a per-IP rate_limited 429 itself, so the check does it here
  const submit = async (formSel, apiFrag) => {
    for (let attempt = 0; attempt < 20; attempt++) {
      const start = statuses.length;
      lastStep = 'submit ' + formSel;
      await page.click(formSel + ' button[type=submit]');
      const deadline = Date.now() + 8000;
      let entry = null;
      while (!entry && Date.now() < deadline) {
        entry = statuses.slice(start).find(([u, st, code]) => u.includes(apiFrag) && (st < 400 || code !== null));
        if (!entry) await sleep(50);
      }
      if (!entry) throw new Error('no response to ' + apiFrag + ' after ' + formSel);
      if (entry[2] !== 'rate_limited') return entry;
      await sleep(3000);
    }
    throw new Error('rate limited for good on ' + apiFrag);
  };

  const user = uniq('login');
  const pw = 'correct horse battery';

  // root goes to the login page
  await page.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  if (!page.url().startsWith(SERVER + '/login')) throw new Error('root did not redirect to /login: ' + page.url());
  await visible(page, '#login-view');
  // the heading carries the app logo (a loaded image), and the tab icon is the brand favicon set
  const logo = await page.$eval('#login-view h1 img.logo', (img) => ({ ok: img.complete && img.naturalWidth > 0, src: img.getAttribute('src') }));
  if (!logo.ok || logo.src !== '/brand/openchatter-logo-mark.png') throw new Error('login logo: ' + JSON.stringify(logo));
  const icons = await page.$$eval('link[rel="icon"]', (ls) => ls.map((l) => l.getAttribute('href')));
  if (!icons.includes('/brand/favicon-32.png')) throw new Error('favicon links: ' + JSON.stringify(icons));
  const splashGone = await page.$eval('#splash', (el) => getComputedStyle(el).display === 'none');
  if (!splashGone) throw new Error('splash still covers the login page');
  await shot(page, 'login.png');

  // /create without a session bounces to login with next
  await page.goto(SERVER + '/create', { waitUntil: 'networkidle2' });
  if (page.url() !== SERVER + '/login?next=%2Fcreate') throw new Error('/create gate: ' + page.url());

  // register through the UI, land on next
  await page.goto(SERVER + '/register?next=%2Fcreate', { waitUntil: 'networkidle2' });
  await visible(page, '#register-view');
  await page.type('#register-username', user);
  await page.type('#register-password', pw);
  await page.type('#register-display', 'Login Tester');
  await shot(page, 'register.png');
  await submit('#register-form', '/api/v1/auth/password/register');
  await page.waitForFunction(() => location.pathname === '/create', { timeout: 8000 });
  await visible(page, '#create-view');
  if (!(await session(page) || '').startsWith('ses_')) throw new Error('no session after register');
  if (!await hiddenNow(page, '#pw-banner')) throw new Error('pw-banner shown for a fresh account');

  // settings: change form + sign out
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  await visible(page, '#settings-view');
  const shown = await page.$eval('#settings-username', (el) => el.textContent);
  if (shown !== user) throw new Error('settings username: ' + shown);
  await shot(page, 'settings.png');
  await page.click('#app-signout');
  await page.waitForFunction(() => location.pathname === '/login', { timeout: 8000 });
  if (await session(page)) throw new Error('session survived sign out');
  if (!sawStatus('/api/v1/auth/logout', 204)) throw new Error('logout was not called');

  // the old session is dead server-side
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  if (!page.url().startsWith(SERVER + '/login?next=%2Fsettings')) throw new Error('settings without session: ' + page.url());

  // the create-account link carries next along
  await visible(page, '#login-view');
  const regHref = await page.$eval('#login-register-link', (el) => el.href);
  if (regHref !== SERVER + '/register?next=%2Fsettings') throw new Error('register link lost next: ' + regHref);

  // wrong password: 401 surfaces as an error line
  await page.type('#login-username', user);
  await page.type('#login-password', 'wrong horse');
  const wrong = await submit('#login-form', '/api/v1/auth/password/login');
  if (wrong[1] !== 401 || wrong[2] !== 'invalid_credentials') throw new Error('wrong password: ' + JSON.stringify(wrong));
  await visible(page, '#login-error');
  const wrongMsg = await page.$eval('#login-error', (el) => el.textContent);
  if (!/invalid username or password/.test(wrongMsg)) throw new Error('wrong password message: ' + wrongMsg);

  // lockout on a throwaway account so the main flow keeps its attempts
  const locked = uniq('lock');
  const reg = await api('/api/v1/auth/password/register', { method: 'POST', body: { username: locked, password: pw } });
  if (reg.status !== 201) throw new Error('lockout register: ' + JSON.stringify(reg));
  for (let i = 0; i < 5; i++) {
    const r = await api('/api/v1/auth/password/login', { method: 'POST', body: { username: locked, password: 'wrong horse' } });
    if (r.status !== 401 || r.data.code !== 'invalid_credentials') throw new Error('lockout attempt ' + i + ': ' + JSON.stringify(r));
  }
  await page.$eval('#login-username', (el) => { el.value = ''; });
  await page.$eval('#login-password', (el) => { el.value = ''; });
  await page.type('#login-username', locked);
  await page.type('#login-password', pw);
  // the right password is refused too, and with the lockout code, not the per-IP one
  const lockedResp = await submit('#login-form', '/api/v1/auth/password/login');
  if (lockedResp[1] !== 429 || lockedResp[2] !== 'locked_out') throw new Error('lockout: ' + JSON.stringify(lockedResp));
  await page.waitForFunction(() => /too many failed attempts/.test(document.querySelector('#login-error').textContent), { timeout: 8000 });

  // a protocol-relative next is ignored
  await page.goto(SERVER + '/login?next=//evil.example/x', { waitUntil: 'networkidle2' });
  await visible(page, '#login-view');
  await page.type('#login-username', user);
  await page.type('#login-password', pw);
  await submit('#login-form', '/api/v1/auth/password/login');
  await page.waitForFunction(() => location.pathname === '/', { timeout: 8000 });
  if (page.url() !== SERVER + '/') throw new Error('unsafe next honoured: ' + page.url());
  await visible(page, '#no-ws-view'); // the landing reads the session at boot; clear it only after that

  // a signed-in visit to /login goes straight to next; a next the URL parser
  // would turn into another host (tab, newline, CR are dropped), or one that
  // points back at the login/register page, falls back to the landing at "/"
  for (const next of ['%2F%09%2Fevil.example%2Fx', '%2F%0A%2Fevil.example%2Fx', '%2F%0D%2Fevil.example%2Fx', '%2Flogin', '%2Flogin%3Fnext%3D%2Flogin', '%2Fregister']) {
    lastStep = 'next ' + next;
    await page.goto(SERVER + '/login?next=' + next, { waitUntil: 'networkidle2' });
    await page.waitForFunction(() => location.pathname === '/', { timeout: 8000 });
    if (page.url() !== SERVER + '/') throw new Error('next ' + next + ' honoured: ' + page.url());
    await visible(page, '#no-ws-view');
  }
  await page.goto(SERVER + '/login?next=%2Fsettings%3Ftab%3D1', { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => location.pathname === '/settings', { timeout: 8000 });
  if (page.url() !== SERVER + '/settings?tab=1') throw new Error('same-origin next lost its query: ' + page.url());
  await visible(page, '#settings-view');

  // sign in with next lands on next
  await page.evaluate(() => localStorage.removeItem('agentchat:session'));
  await page.goto(SERVER + '/login?next=%2Fsettings', { waitUntil: 'networkidle2' });
  await visible(page, '#login-view');
  await page.type('#login-username', user);
  await page.type('#login-password', pw);
  await submit('#login-form', '/api/v1/auth/password/login');
  await page.waitForFunction(() => location.pathname === '/settings', { timeout: 8000 });
  await visible(page, '#settings-view');

  // a landing that cannot list workspaces: the signed-in visit to /login
  // reuses the payload of its session check (one GET /api/v1/user, so a
  // failure of a second one cannot bring back the form), and when the check
  // itself fails the form that comes back still submits through the API
  let userCalls = 0;
  let failFrom = 2;
  const outage = (req) => {
    if (req.method() !== 'GET' || !req.url().endsWith('/api/v1/user')) { req.continue(); return; }
    userCalls++;
    if (userCalls < failFrom) { req.continue(); return; }
    req.respond({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'test outage', code: 'test_outage' }) });
  };
  await page.setRequestInterception(true);
  page.on('request', outage);
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => location.pathname === '/', { timeout: 8000 });
  await visible(page, '#no-ws-view');
  if (userCalls !== 1) throw new Error('signed-in /login made ' + userCalls + ' GET /api/v1/user calls');
  userCalls = 0;
  failFrom = 1;
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await visible(page, '#login-view');
  await page.type('#login-username', user);
  await page.type('#login-password', pw);
  await submit('#login-form', '/api/v1/auth/password/login');
  await visible(page, '#login-view');
  await page.waitForFunction(() => !document.querySelector('#login-form button[type=submit]').disabled, { timeout: 8000 });
  if (page.url() !== SERVER + '/login') throw new Error('form submitted natively: ' + page.url());
  if (!(await session(page) || '').startsWith('ses_')) throw new Error('no session after the login that could not land');
  page.off('request', outage);
  await page.setRequestInterception(false);

  // the user's own workspace, plus a legacy act_ join in it under another
  // name (the creator's row is linked to the account and cannot be adopted by name)
  const created = await createRoom(SERVER, await session(page), 'login check');
  const slug = created.room.slug;
  const joined = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'Legacy Tester', is_human: true } });
  if (!joined.data.token) throw new Error('join room: ' + JSON.stringify(joined));
  const seedLegacy = () => page.evaluate((k, t) => localStorage.setItem(k, JSON.stringify({ token: t })), 'agentchat:' + slug, joined.data.token);
  const legacyKey = () => page.evaluate((k) => localStorage.getItem(k), 'agentchat:' + slug);
  await seedLegacy();
  const openRoom = async () => {
    statuses.length = 0;
    await page.goto(SERVER + '/r/' + slug, { waitUntil: 'networkidle2' });
    await visible(page, '#chat-view');
    await page.waitForFunction(() => document.querySelector('#room-name').textContent === 'login check', { timeout: 8000 });
    if (statuses.some(([, s]) => s === 403)) throw new Error('403 on the room page: ' + JSON.stringify(statuses.filter(([, s]) => s === 403)));
    if (!statuses.some(([u, s]) => u.includes('/api/v1/me') && s === 200)) throw new Error('room did not load');
  };

  // an operator reset flags the account: the banner shows on every page
  const tempPw = 'temporary horse 1';
  execFileSync('go', ['run', './cmd/openchatter-passwd', user], {
    cwd: REPO, input: tempPw, env: Object.assign({}, process.env, { OPENCHATTER_DB_URL: DB_URL }), stdio: ['pipe', 'pipe', 'inherit'],
  });
  // the reset revoked the session: the page bounces to login
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => location.pathname === '/login', { timeout: 8000 });
  await visible(page, '#login-view');
  if (await session(page)) throw new Error('revoked session kept');
  await page.type('#login-username', user);
  await page.type('#login-password', tempPw);
  await submit('#login-form', '/api/v1/auth/password/login');
  await page.waitForFunction(() => location.pathname === '/settings', { timeout: 8000 });
  await visible(page, '#pw-banner');
  await shot(page, 'banner.png');
  lastStep = 'goto /create';
  await page.goto(SERVER + '/create', { waitUntil: 'networkidle2' });
  lastStep = 'on /create ' + page.url();
  await visible(page, '#pw-banner');
  await visible(page, '#create-view');
  // a signed-in visit to /register skips the form like /login does and
  // lands in the user's workspace
  await page.goto(SERVER + '/register', { waitUntil: 'networkidle2' });
  lastStep = 'wait for /w/' + slug;
  await page.waitForFunction((p) => location.pathname.startsWith(p), { timeout: 8000 }, '/w/' + slug);
  await visible(page, '#pw-banner');
  // ...and on a room page, where the session drives the room as the creator
  // and the legacy per-slug token is forgotten
  await openRoom();
  await visible(page, '#pw-banner');
  if (await legacyKey() !== null) throw new Error('legacy per-slug token kept after the session worked');
  const meName = await page.evaluate(async () => (await (await fetch('/api/v1/me', { headers: { Authorization: 'Bearer ' + localStorage.getItem('agentchat:session'), 'X-Workspace-Slug': location.pathname.split('/')[2] } })).json()).name);
  if (meName !== 'Login Tester') throw new Error('room identity: ' + meName);

  // a member with no uploaded picture shows the seedling IMAGE: the workspace
  // creator's row, a joiner's row and a message row all render the same asset.
  // The joiner posts FIRST: the write refreshes its last_seen_at, so it cannot
  // age past the online window and sink into the collapsed offline section
  // while the tree assertion runs.
  const SEEDLING = '/brand/avatar-default-128.png';
  const said = await api('/api/v1/channels/general/messages', { method: 'POST', token: joined.data.token, body: { body: 'first sprout' } });
  if (said.status !== 201) throw new Error('post as the joiner: ' + JSON.stringify(said));
  await page.waitForFunction((s) => [...document.querySelectorAll('.msg')].some((m) => /first sprout/.test(m.textContent) && (m.querySelector('.avatar img') || {}).getAttribute('src') === s), { timeout: 8000 }, SEEDLING);
  const avatarOf = (n) => page.evaluate((name) => {
    const li = [...document.querySelectorAll('#participant-list li')].find((x) => (x.querySelector('.pname') || {}).textContent === name);
    const img = li && li.querySelector('img.avatar-img');
    return img ? img.getAttribute('src') : null;
  }, n);
  await page.waitForFunction(() => [...document.querySelectorAll('#participant-list li .pname')].some((e) => e.textContent === 'Legacy Tester'), { timeout: 8000 });
  for (const n of ['Login Tester', 'Legacy Tester']) {
    const av = await avatarOf(n);
    if (av !== SEEDLING) throw new Error(n + ' default avatar in the tree: ' + av);
  }
  await shot(page, 'seedling-default.png');
  await shot(page, 'room-pw-banner.png');

  // a second tab in its own browser context (own localStorage) holding a second session
  const otherLogin = await api('/api/v1/auth/password/login', { method: 'POST', body: { username: user, password: tempPw } });
  if (otherLogin.status !== 200) throw new Error('second login: ' + JSON.stringify(otherLogin));
  const otherCtx = await browser.createBrowserContext();
  const other = await otherCtx.newPage();
  await other.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await other.evaluate((t) => localStorage.setItem('agentchat:session', t), otherLogin.data.token);
  if (await userStatus(other) !== 200) throw new Error('second tab session not valid before the change');

  // change the password: banner clears here, the other tab is signed out
  await page.bringToFront(); // a background tab stops painting, and the selector polls on rAF
  await page.goto(SERVER + '/settings', { waitUntil: 'networkidle2' });
  await visible(page, '#settings-view');
  await visible(page, '#pw-banner');
  await page.type('#pw-current', tempPw);
  await page.type('#pw-new', pw);
  await page.type('#pw-confirm', pw + 'x');
  await page.click('#pw-form button[type=submit]'); // mismatch is caught client-side, no request
  await visible(page, '#pw-error');
  await page.$eval('#pw-confirm', (el) => { el.value = ''; });
  await page.type('#pw-confirm', pw);
  const changed = await submit('#pw-form', '/api/v1/auth/password/change');
  if (changed[1] !== 204) throw new Error('change: ' + JSON.stringify(changed));
  await visible(page, '#pw-ok');
  await page.waitForFunction(() => document.querySelector('#pw-banner').classList.contains('hidden'), { timeout: 8000 });
  if (!sawStatus('/api/v1/auth/password/change', 204)) throw new Error('change was not a 204');
  if (await userStatus(page) !== 200) throw new Error('changing tab lost its session');
  if (await userStatus(other) !== 401) throw new Error('other tab still signed in');
  await otherCtx.close();
  const after = await api('/api/v1/auth/password/login', { method: 'POST', body: { username: user, password: pw } });
  if (after.status !== 200) throw new Error('new password rejected: ' + JSON.stringify(after));

  // the room page again on the new session: no 403, banner gone
  await openRoom();
  if (!await hiddenNow(page, '#pw-banner')) throw new Error('pw-banner still shown on the room page after the change');

  // the same room with no session: the act_ token is dead since 000027, so the
  // page goes to sign in with the room as next and scrubs the per-slug key
  await page.evaluate(() => localStorage.removeItem('agentchat:session'));
  await seedLegacy();
  await page.goto(SERVER + '/r/' + slug, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => location.pathname === '/login', { timeout: 8000 });
  if (!page.url().startsWith(SERVER + '/login?next=%2Fr%2F' + slug)) throw new Error('legacy token landed on ' + page.url());
  if (await legacyKey() !== null) throw new Error('legacy per-slug token kept on the login bounce');
  await shot(page, 'room-legacy-bounce.png');

  // expected failures (401 wrong password, 429 lockout, 401 revoked session) log as
  // console errors; Chrome's line does not carry the code, so the codes are checked
  // from the responses themselves: every 401 and 429 the page saw must be one of those
  // (a rate_limited 429 is retried by submit above and never the final answer)
  const realErrors = errors.filter((e) => !e.includes('favicon') && !/status of (401|429|500)/.test(e) && !e.startsWith('console: landing'));
  if (realErrors.length) throw new Error('page errors: ' + realErrors.join(' | '));
  // a session_invalid 401 redirects at once, so its body may be lost (null); a 429 never navigates
  const okCodes = { 401: ['invalid_credentials', 'session_invalid', null], 429: ['locked_out', 'rate_limited'], 500: ['test_outage'] };
  const odd = allStatuses.filter(([, st, code]) => st >= 400 && !(okCodes[st] || []).includes(code));
  if (odd.length) throw new Error('unexpected error responses: ' + JSON.stringify(odd));

  console.log('LOGIN_CHECK_OK');
  await browser.close();
  process.exit(0);
})().catch((e) => { console.error('LOGIN_CHECK_FAIL:', e.message, '(after: ' + lastStep + ')'); process.exit(1); });
