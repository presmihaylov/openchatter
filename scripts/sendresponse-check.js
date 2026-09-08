// A successful POST response must settle its optimistic row even while the
// live event feed is unavailable. Otherwise the row and pending-send record
// stay forever and can collide with a later identical send.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'send response check ' + tag;
const body = 'response settles me ' + tag;
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
      method: 'POST', body: { username: 'send-response-' + tag, password: 'correct horse battery', display_name: 'Send Response Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'send-response-' + tag },
    });
    slug = made.room.slug;

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
    await page.setRequestInterception(true);
    page.on('request', (request) => {
      const target = new URL(request.url());
      if (target.pathname === '/api/v1/user/events' && target.searchParams.get('wait') === '25') {
        request.abort().catch(() => {});
        return;
      }
      request.continue().catch(() => {});
    });
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });

    const response = page.waitForResponse((res) =>
      res.request().method() === 'POST' && /\/api\/v1\/channels\/[^/]+\/messages$/.test(new URL(res.url()).pathname));
    await page.click('#composer-input');
    await page.type('#composer-input', body);
    await page.keyboard.press('Enter');
    await response;
    await new Promise((resolve) => setTimeout(resolve, 300));
    const state = await page.evaluate((text) => {
      const rows = [...document.querySelectorAll('#messages .msg')]
        .filter((node) => node.querySelector('.content')?.textContent.trim() === text);
      return {
        rows: rows.length,
        pending: rows.filter((node) => node.classList.contains('pending')).length,
        ids: rows.map((node) => node.dataset.id),
      };
    }, body);
    const stored = await api('/api/v1/channels/general/messages?limit=100', { token: session, slug });
    const apiRows = (stored.messages || []).filter((message) => message.body === body).length;
    console.log(JSON.stringify({ ...state, apiRows }));
    if (apiRows !== 1 || state.rows !== 1 || state.pending !== 0 || state.ids.some((id) => id.startsWith('tmp-'))) {
      throw new Error('successful response left an optimistic send unsettled');
    }
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('SENDRESPONSE_CHECK_OK');
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
})().catch((error) => { console.error('SENDRESPONSE_CHECK_FAIL:', error.message); process.exit(1); });
