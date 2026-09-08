// Staged attachments and their uploads belong to the channel/thread where
// they started. Navigation must clear them, ignore late completions, and
// release temporary preview object URLs.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'attachment navigation check ' + tag;
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

const clickChannel = async (page, name) => {
  await page.evaluate((wanted) => [...document.querySelectorAll('#channel-list .chan-name')]
    .find((node) => node.textContent === wanted).parentElement.click(), name);
  await page.waitForFunction((wanted) => document.querySelector('#channel-title').textContent.trim() === wanted, {}, name);
};

const chooseFile = (page, filename) => page.$eval('#attach-input', (input, name) => {
  const transfer = new DataTransfer();
  transfer.items.add(new File(['attachment navigation probe'], name, { type: 'image/png' }));
  Object.defineProperty(input, 'files', { configurable: true, value: transfer.files });
  input.dispatchEvent(new Event('change', { bubbles: true }));
}, filename);

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'attachment-' + tag, password: 'correct horse battery', display_name: 'Attachment Navigation Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'attachment-nav-' + tag },
    });
    slug = made.room.slug;
    await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'side' } });

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
    await page.evaluate(() => {
      window.__urlLife = { created: 0, revoked: 0 };
      const create = URL.createObjectURL.bind(URL);
      const revoke = URL.revokeObjectURL.bind(URL);
      URL.createObjectURL = (value) => { window.__urlLife.created++; return create(value); };
      URL.revokeObjectURL = (value) => { window.__urlLife.revoked++; return revoke(value); };
    });

    // A completed staged file must clear when its channel changes.
    await chooseFile(page, 'completed.png');
    await page.waitForSelector('#attach-pending:not(.hidden)', { timeout: 10000 });
    await clickChannel(page, 'side');
    const completedStayed = await page.$eval('#attach-pending', (node) => !node.classList.contains('hidden'));
    await page.$eval('#attach-pending', (node) => node.querySelector('.pending-clear')?.click());
    await clickChannel(page, 'general');

    // A late response must not stage its file in the channel now on screen.
    await page.setRequestInterception(true);
    let release;
    let readyResolve;
    const ready = new Promise((resolve) => { readyResolve = resolve; });
    page.on('request', (request) => {
      if (!release && request.method() === 'POST' && new URL(request.url()).pathname === '/api/v1/attachments') {
        release = () => request.continue();
        readyResolve();
        return;
      }
      request.continue().catch(() => {});
    });
    const responseReady = page.waitForResponse((response) =>
      response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/v1/attachments');
    await chooseFile(page, 'in-flight.png');
    await ready;
    await clickChannel(page, 'side');
    await release();
    await responseReady;
    await new Promise((resolve) => setTimeout(resolve, 300));
    const state = await page.evaluate(() => ({
      inFlightStayed: !document.querySelector('#attach-pending').classList.contains('hidden'),
      lifecycle: window.__urlLife,
    }));
    console.log(JSON.stringify({ completedStayed, ...state }));
    if (completedStayed) throw new Error('completed attachment followed channel navigation');
    if (state.inFlightStayed) throw new Error('late attachment response staged in another channel');
    if (state.lifecycle.created < 1 || state.lifecycle.revoked < 1) throw new Error('preview object URL was not released');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('ATTACHMENTNAV_CHECK_OK');
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
})().catch((error) => { console.error('ATTACHMENTNAV_CHECK_FAIL:', error.message); process.exit(1); });
