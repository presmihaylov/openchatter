// Older invite-list responses must not overwrite a list refreshed after a new
// link is created. This drives the race in a real browser by holding the first
// list request until the create flow and its newer list request have finished.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'invite response race ' + tag;
let session;
let slug;

const api = async (path, opts = {}) => {
  const headers = Object.assign({}, access, opts.body ? { 'Content-Type': 'application/json' } : {});
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const response = await fetch(SERVER + path, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(path + ' -> ' + response.status + ' ' + JSON.stringify(data));
  return data;
};

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'invite-race-' + tag, password: 'correct horse battery', display_name: 'Invite Race Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'invite-race-' + tag },
    });
    slug = made.room.slug;
    const oldList = await api('/api/v1/invites', { token: session, slug });

    if (process.env.BROWSER_URL) browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
      });
      ownsBrowser = true;
    }
    page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 720 });
    await page.setExtraHTTPHeaders(access);
    const errors = [];
    page.on('pageerror', (error) => errors.push('pageerror: ' + error.message));
    page.on('console', (message) => { if (message.type() === 'error') errors.push('console: ' + message.text()); });
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });

    await page.setRequestInterception(true);
    let held;
    let heldResolve;
    const firstHeld = new Promise((resolve) => { heldResolve = resolve; });
    page.on('request', (request) => {
      const target = new URL(request.url());
      if (!held && request.method() === 'GET' && target.pathname === '/api/v1/invites') {
        held = request;
        heldResolve();
        return;
      }
      request.continue().catch(() => {});
    });

    await page.click('#ws-switcher');
    await page.click('#ws-invite-member');
    await firstHeld;
    await page.click('#invite-new-submit');
    await page.waitForFunction(() => document.querySelectorAll('#invite-list .invite-item').length === 2, { timeout: 10000 });
    const beforeRelease = await page.$$eval('#invite-list .invite-item', (rows) => rows.length);
    await held.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(oldList) });
    await new Promise((resolve) => setTimeout(resolve, 300));
    const afterRelease = await page.$$eval('#invite-list .invite-item', (rows) => rows.length);
    console.log(JSON.stringify({ beforeRelease, afterRelease }));
    if (beforeRelease !== 2) throw new Error('newer invite list did not render');
    if (afterRelease !== 2) throw new Error('older invite response overwrote the newer list');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('INVITERACE_CHECK_OK');
  } finally {
    if (page) await page.close().catch(() => {});
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (session && slug) await api('/api/v1/room', {
      method: 'DELETE', token: session, slug, body: { name: roomName },
    }).catch(() => {});
  }
})().catch((error) => { console.error('INVITERACE_CHECK_FAIL:', error.message); process.exit(1); });
