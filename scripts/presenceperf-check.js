// Presence is already complete in its event payload. A roster dot changing
// must not refetch the room, sections, Browse list, or channel members.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'presence request check ' + tag;
let session;
let slug;
let step = 'setup';

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

const waitPresence = (page, name, online) => page.waitForFunction((wanted, state) => {
  const row = [...document.querySelectorAll('#participant-list li')]
    .find((node) => node.querySelector('.pname')?.textContent === wanted);
  return !!row && row.querySelector('.dot')?.classList.contains('online') === state;
}, { timeout: 10000 }, name, online);

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'presence-perf-' + tag, password: 'correct horse battery', display_name: 'Presence Request Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'presence-perf-' + tag },
    });
    slug = made.room.slug;
    const agent = await api('/api/v1/rooms/join', {
      method: 'POST', body: { invite_code: made.invite_code, name: 'presence-probe-' + tag, description: 'presence request probe' },
    });

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
    let counts = {};
    page.on('request', (request) => {
      const path = new URL(request.url()).pathname;
      if (path === '/api/v1/room') counts.room = (counts.room || 0) + 1;
      else if (path === '/api/v1/channel-groups') counts.groups = (counts.groups || 0) + 1;
      else if (path === '/api/v1/channels/browse') counts.browse = (counts.browse || 0) + 1;
      else if (/^\/api\/v1\/channels\/[^/]+\/members$/.test(path)) counts.members = (counts.members || 0) + 1;
    });
    step = 'open';
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.evaluate(() => document.querySelector('#participant-list li .p-toggle:not(.spacer)')?.click());
    step = 'initial online';
    try { await waitPresence(page, agent.participant.name, true); }
    catch (error) {
      const rows = await page.$$eval('#participant-list li', (nodes) => nodes.map((node) => ({ text: node.textContent.trim(), html: node.className })));
      throw new Error(error.message + ' rows=' + JSON.stringify(rows) + ' agent=' + JSON.stringify(agent.participant));
    }

    await page.click('#members-btn');
    await page.waitForSelector('#members-modal:not(.hidden)', { timeout: 5000 });
    counts = {};
    step = 'offline';
    await api('/api/v1/me/presence', { method: 'POST', token: agent.token, slug, body: { status: 'offline' } });
    await waitPresence(page, agent.participant.name, false);
    await page.waitForFunction((name) => {
      const row = [...document.querySelectorAll('#members-list .mm-row')]
        .find((node) => node.querySelector('.mm-name')?.textContent === name);
      return !!row && !row.querySelector('.mm-dot')?.classList.contains('on');
    }, { timeout: 5000 }, agent.participant.name);
    await new Promise((resolve) => setTimeout(resolve, 300));
    const offline = { ...counts };
    await page.click('#members-close');
    await page.evaluate((name) => {
      const row = [...document.querySelectorAll('#participant-list li')]
        .find((node) => node.querySelector('.pname')?.textContent === name);
      row.click();
    }, agent.participant.name);
    await page.waitForSelector('#profile-modal:not(.hidden)', { timeout: 5000 });
    if (!/offline/.test(await page.$eval('#profile-meta', (node) => node.textContent))) throw new Error('profile did not open offline');
    counts = {};
    step = 'online';
    await api('/api/v1/me/presence', { method: 'POST', token: agent.token, slug, body: { status: 'online' } });
    await waitPresence(page, agent.participant.name, true);
    await page.waitForFunction(() => /online/.test(document.querySelector('#profile-meta')?.textContent || ''), { timeout: 5000 });
    await new Promise((resolve) => setTimeout(resolve, 300));
    const online = { ...counts };
    console.log(JSON.stringify({ offline, online }));
    const total = (set) => Object.values(set).reduce((sum, n) => sum + n, 0);
    if (total(offline) || total(online)) throw new Error('presence caused structural refetches');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('PRESENCEPERF_CHECK_OK');
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
})().catch((error) => { console.error('PRESENCEPERF_CHECK_FAIL:', error.message, '(step ' + step + ')'); process.exit(1); });
