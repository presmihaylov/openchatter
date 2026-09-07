// Brand check (OpenChatter rename, phase 1): every user-visible surface says
// OpenChatter and shows the new mark. Covers the tab title and the favicon set,
// the boot splash, the app header brand, the login / register / join headings,
// the "no workspace" card, and the served skill and cli.sh. The old name must
// not survive anywhere a user or an agent can read it.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8095 OUT=<dir> node scripts/openchatter-check.js
const puppeteer = require('puppeteer-core');
const path = require('path');
const { newRoom, openAsHuman, call, uniqUser, registerAndLogin } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';
require('fs').mkdirSync(OUT, { recursive: true });
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
const MARK = '/brand/openchatter-logo-mark.png';
const shot = (page, name) => page.screenshot({ path: path.join(OUT, name) });

// an <img> that really decoded, not a broken-image box
const loaded = (page, sel) => page.$eval(sel, (img) => ({ ok: img.complete && img.naturalWidth > 0, src: img.getAttribute('src') }));
const markOK = async (page, sel, where) => {
  const img = await loaded(page, sel);
  assert(img.ok && img.src === MARK, where + ' mark: ' + JSON.stringify(img));
};

(async () => {
  const created = await newRoom(SERVER, 'openchatter check');
  const slug = created.room.slug;
  const alice = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'alice', is_human: true } });

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => { throw new Error('pageerror ' + e.message); });

  // 1. signed out: the login page carries the name, the mark and the favicon set
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await page.waitForSelector('#login-view:not(.hidden)', { timeout: 8000 });
  assert((await page.title()) === 'OpenChatter', 'login title: ' + await page.title());
  await markOK(page, '#login-view h1 img.logo', 'login');
  const icons = await page.$$eval('link[rel="icon"], link[rel="apple-touch-icon"]', (ls) => ls.map((l) => l.getAttribute('href')));
  for (const want of ['/brand/favicon-16.png', '/brand/favicon-32.png', '/brand/apple-touch-icon.png']) {
    assert(icons.includes(want), 'icon link missing ' + want + ': ' + JSON.stringify(icons));
  }
  // the files are really served, at the size they claim
  for (const [href, size] of [['/brand/favicon-16.png', 16], ['/brand/favicon-32.png', 32], ['/brand/favicon-64.png', 64], ['/brand/apple-touch-icon.png', 180], [MARK, 512], ['/favicon.ico', 0]]) {
    const got = await page.evaluate(async (h, s) => {
      const r = await fetch(h);
      if (!r.ok) return { status: r.status };
      if (!s) return { status: 200, w: 0 };
      const bmp = await createImageBitmap(await r.blob());
      return { status: 200, w: bmp.width, h: bmp.height };
    }, href, size);
    assert(got.status === 200, href + ' -> ' + got.status);
    if (size) assert(got.w === size && got.h === size, href + ' is ' + got.w + 'x' + got.h + ', want ' + size);
  }
  await shot(page, 'openchatter-login.png');

  // 2. register and the workspace-less card keep the brand
  await page.goto(SERVER + '/register', { waitUntil: 'networkidle2' });
  await page.waitForSelector('#register-view:not(.hidden)', { timeout: 8000 });
  await markOK(page, '#register-view h1 img.logo', 'register');
  const homeless = await registerAndLogin(SERVER, uniqUser(), 'Nobody');
  await page.evaluate((t) => localStorage.setItem('agentchat:session', t), homeless);
  await page.goto(SERVER + '/', { waitUntil: 'networkidle2' });
  await page.waitForSelector('#no-ws-view:not(.hidden)', { timeout: 8000 });
  await markOK(page, '#no-ws-view h1 img.logo', 'no-workspace');
  assert((await page.title()) === 'OpenChatter', 'no-workspace title: ' + await page.title());
  await shot(page, 'openchatter-nows.png');

  // 3. the splash paints the mark, and the workspace tab title is "OpenChatter | <name>"
  await openAsHuman(page, SERVER, slug, alice);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await markOK(page, '#splash img', 'splash');
  await markOK(page, '#app-brand img.logo', 'header brand');
  assert((await page.$eval('#app-brand', (el) => el.textContent.trim())) === 'OpenChatter', 'header brand text: ' + await page.$eval('#app-brand', (el) => el.textContent));
  await page.waitForFunction(() => document.title === 'OpenChatter | openchatter check', { timeout: 8000 });
  await shot(page, 'openchatter-room.png');

  // 4. the old name is gone from everything a user or an agent reads
  // every document an agent or a user reads, not just the two big ones: a
  // stale name in a harness guide is as wrong as one on the login page
  const served = ['/login', '/cli.sh', '/skill', '/skill/claude-code', '/skill/hermes',
    '/skill/watch.sh', '/skill/bridge.sh', '/skill/inject.sh',
    '/skill/codex', '/skill/opencode', '/skill/pi'];
  for (const p of served) {
    const body = await fetch(SERVER + p).then((r) => r.text());
    assert(!/AgentChat/.test(body), 'the old name survives in ' + p);
    assert(/OpenChatter/.test(body), p + ' never says OpenChatter');
  }

  await browser.close();
  console.log('OPENCHATTER_CHECK_OK');
})().catch((e) => { console.error('OPENCHATTER_CHECK_FAIL:', e.message); process.exit(1); });
