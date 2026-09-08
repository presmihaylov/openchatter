// An optimistic send must settle even when its server echo arrives after the
// user navigates away. Otherwise stale records consume later identical echoes
// and leave duplicate pending rows (plus unbounded detached-node state).
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'pending navigation check ' + tag;
const body = 'same optimistic body ' + tag;
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

const send = async (page) => {
  await page.click('#composer-input');
  await page.type('#composer-input', body);
  await page.keyboard.press('Enter');
};

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'pending-' + tag, password: 'correct horse battery', display_name: 'Pending Navigation Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'pending-nav-' + tag },
    });
    slug = made.room.slug;
    const channels = await api('/api/v1/channels', { token: session, slug });
    const general = channels.channels.find((channel) => channel.name === 'general');
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

    await page.setRequestInterception(true);
    let releaseFirst;
    let firstResolve;
    const firstReady = new Promise((resolve) => { firstResolve = resolve; });
    page.on('request', (request) => {
      if (!releaseFirst && request.method() === 'POST'
          && new URL(request.url()).pathname === '/api/v1/channels/' + general.id + '/messages') {
        releaseFirst = () => request.continue();
        firstResolve();
        return;
      }
      request.continue().catch(() => {});
    });

    await send(page);
    await firstReady;
    await page.waitForSelector('#messages .msg.pending');
    await clickChannel(page, 'side');
    await releaseFirst();
    await page.waitForFunction(async (base, id, text, headers) => {
      const response = await fetch(base + '/api/v1/channels/' + id + '/messages?limit=100', { headers });
      const out = await response.json();
      return (out.messages || []).some((message) => message.body === text);
    }, { timeout: 10000, polling: 250 }, BROWSER_SERVER, general.id, body,
    Object.assign({}, access, { Authorization: 'Bearer ' + session, 'X-Workspace-Slug': slug }));

    await clickChannel(page, 'general');
    await page.waitForFunction((text) => [...document.querySelectorAll('#messages .content')]
      .some((node) => node.textContent.trim() === text), {}, body);
    await send(page);
    await page.waitForFunction((text) => [...document.querySelectorAll('#messages .msg:not(.pending) .content')]
      .filter((node) => node.textContent.trim() === text).length >= 2, { timeout: 10000 }, body);
    await new Promise((resolve) => setTimeout(resolve, 300));
    const state = await page.evaluate((text) => {
      const rows = [...document.querySelectorAll('#messages .msg')]
        .filter((node) => node.querySelector('.content')?.textContent.trim() === text);
      return { rows: rows.length, pending: rows.filter((node) => node.classList.contains('pending')).length };
    }, body);
    console.log(JSON.stringify(state));
    if (state.rows !== 2 || state.pending !== 0) throw new Error('navigation left a stale optimistic send');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('PENDINGNAV_CHECK_OK');
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
})().catch((error) => { console.error('PENDINGNAV_CHECK_FAIL:', error.message); process.exit(1); });
