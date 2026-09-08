// A first visit to an unwarmed channel must clear the previous channel at
// once, retain live events that race the initial snapshot, and land at latest.
// Run with NODE_PATH pointing at puppeteer-core; BROWSER_URL/BROWSER_SERVER
// support a real Chrome reached through a tunnel.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'cold channel check ' + tag;
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
      method: 'POST', body: { username: 'cold-' + tag, password: 'correct horse battery', display_name: 'Cold Channel Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'cold-channel-' + tag },
    });
    slug = made.room.slug;
    const general = made.room.channels
      ? made.room.channels.find((channel) => channel.name === 'general')
      : (await api('/api/v1/channels', { token: session, slug })).channels.find((channel) => channel.name === 'general');
    await api('/api/v1/channels/' + general.id + '/messages', {
      method: 'POST', token: session, slug, body: { body: 'general sentinel must clear' },
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
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForFunction(() => document.querySelector('#messages').textContent.includes('general sentinel must clear'));

    // Created after warmAll took its channel snapshot, so this page is cold.
    const cold = await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'cold-data' } });
    const history = [];
    for (let i = 0; i < 5; i++) history.push(await api('/api/v1/channels/' + cold.id + '/messages', {
      method: 'POST', token: session, slug, body: { body: 'cold history ' + i + ' ' + 'content '.repeat(12) },
    }));
    await page.waitForFunction(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .some((node) => node.textContent === 'cold-data'), { timeout: 10000 });

    await page.setRequestInterception(true);
    let release;
    let readyResolve;
    const ready = new Promise((resolve) => { readyResolve = resolve; });
    page.on('request', (request) => {
      if (!release && request.url().includes('/channels/' + cold.id + '/messages?limit=100')) {
        api('/api/v1/channels/' + cold.id + '/messages?limit=100', { token: session, slug }).then((snapshot) => {
          release = () => request.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(snapshot) });
          readyResolve();
        }, (error) => request.abort().then(() => { throw error; }));
        return;
      }
      request.continue().catch(() => {});
    });
    await page.evaluate(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .find((node) => node.textContent === 'cold-data').parentElement.click());
    await ready;
    const oldChannelVisible = await page.$eval('#messages', (box) => box.textContent.includes('general sentinel must clear'));
    const live = await api('/api/v1/channels/' + cold.id + '/messages', {
      method: 'POST', token: session, slug, body: { body: 'live during first snapshot' },
    });
    await page.waitForFunction((id) => document.querySelector('#messages .msg[data-id="' + id + '"]'), { timeout: 10000 }, live.id);
    await release();
    const apiAfter = await api('/api/v1/channels/' + cold.id + '/messages?limit=100', { token: session, slug });
    await new Promise((resolve) => setTimeout(resolve, 1200));
    const final = await page.$eval('#messages', (box, liveID) => ({
      count: box.querySelectorAll(':scope > .msg').length,
      live: !!box.querySelector('.msg[data-id="' + liveID + '"]'),
      old: box.textContent.includes('general sentinel must clear'),
      bottomGap: box.scrollHeight - box.scrollTop - box.clientHeight,
    }), live.id);
    console.log(JSON.stringify({ apiMessages: apiAfter.messages.length, oldChannelVisible, final }));
    if (oldChannelVisible || final.old) throw new Error('previous channel remained visible during a cold load');
    if (final.count !== history.length + 1 || !final.live) throw new Error('initial snapshot erased a live message');
    if (final.bottomGap > 2) throw new Error('cold channel did not land at the bottom');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('COLDCHANNEL_CHECK_OK');
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
})().catch((error) => { console.error('COLDCHANNEL_CHECK_FAIL:', error.message); process.exit(1); });
