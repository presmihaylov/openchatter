// Concurrent room refreshes must commit in request order. Hold an older room
// snapshot while a channel join paints a newer one, then prove the old response
// cannot erase those channels from the sidebar.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'room response race ' + tag;
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
  let first;
  let third;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'room-race-' + tag, password: 'correct horse battery', display_name: 'Room Race Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'room-race-' + tag },
    });
    slug = made.room.slug;
    const side = await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'side' } });
    await api('/api/v1/channels/' + (side.channel?.id || side.id) + '/leave', { method: 'POST', token: session, slug, body: {} });
    const oldRoom = await api('/api/v1/room', { token: session, slug });

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
    let roomRequests = 0;
    let firstResolve;
    let thirdResolve;
    const firstHeld = new Promise((resolve) => { firstResolve = resolve; });
    const thirdHeld = new Promise((resolve) => { thirdResolve = resolve; });
    page.on('request', (request) => {
      const target = new URL(request.url());
      if (request.method() === 'GET' && target.pathname === '/api/v1/room') {
        roomRequests++;
        if (roomRequests === 1) { first = request; firstResolve(); return; }
        if (roomRequests === 3) { third = request; thirdResolve(); return; }
      }
      request.continue().catch(() => {});
    });

    await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'late' } });
    await firstHeld;
    await page.click('#browse-channels');
    await page.waitForSelector('#browse-modal:not(.hidden)', { timeout: 10000 });
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('#browse-list .browse-row')]
        .find((node) => node.querySelector('.browse-name')?.textContent.includes('side'));
      if (!row) throw new Error('side is missing from Browse');
      row.querySelector('.browse-join').click();
    });
    await page.waitForFunction(() => {
      const names = [...document.querySelectorAll('#channel-list .chan-name')].map((node) => node.textContent);
      return names.includes('side') && names.includes('late');
    }, { timeout: 10000 });
    const fresh = await page.$$eval('#channel-list .chan-name', (nodes) => nodes.map((node) => node.textContent));

    await first.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(oldRoom) });
    await thirdHeld;
    const afterOld = await page.$$eval('#channel-list .chan-name', (nodes) => nodes.map((node) => node.textContent));
    console.log(JSON.stringify({ fresh, afterOld, roomRequests }));
    if (!afterOld.includes('side') || !afterOld.includes('late')) throw new Error('older room response erased newer channel state');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    await third.continue();
    third = null;
    console.log('ROOMRACE_CHECK_OK');
  } finally {
    if (third) await third.continue().catch(() => {});
    if (first) await first.continue().catch(() => {});
    if (page) await page.close().catch(() => {});
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (session && slug) await api('/api/v1/room', {
      method: 'DELETE', token: session, slug, body: { name: roomName },
    }).catch(() => {});
  }
})().catch((error) => { console.error('ROOMRACE_CHECK_FAIL:', error.message); process.exit(1); });
