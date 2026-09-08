// Channel navigation regression: a cached channel must retain every message
// when revisited and every channel open must land on its newest message.
// Supports a remote real Chrome through BROWSER_URL and a separately tunneled
// app origin through BROWSER_SERVER.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'channel navigation check ' + tag;
let session;
let slug;

const api = async (path, opts = {}) => {
  const headers = Object.assign({}, access, opts.body ? { 'Content-Type': 'application/json' } : {});
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
};

const clickChannel = async (page, name) => {
  await page.waitForFunction((wanted) => [...document.querySelectorAll('#channel-list .chan-name')]
    .some((node) => node.textContent === wanted), {}, name);
  await page.evaluate((wanted) => [...document.querySelectorAll('#channel-list .chan-name')]
    .find((node) => node.textContent === wanted).parentElement.click(), name);
  await page.waitForFunction((wanted) => document.querySelector('#channel-title').textContent.trim() === wanted, {}, name);
};

const state = (page) => page.$eval('#messages', (box) => ({
  count: box.querySelectorAll(':scope > .msg').length,
  ids: [...box.querySelectorAll(':scope > .msg')].map((node) => node.dataset.id),
  top: box.scrollTop,
  height: box.scrollHeight,
  client: box.clientHeight,
  bottomGap: box.scrollHeight - box.scrollTop - box.clientHeight,
}));

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'nav-' + tag, password: 'correct horse battery', display_name: 'Navigation Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'channel-nav-' + tag },
    });
    slug = made.room.slug;
    const data = await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'data' } });
    const elsewhere = await api('/api/v1/channels', { method: 'POST', token: session, slug, body: { name: 'elsewhere' } });
    const posted = [];
    for (let i = 0; i < 24; i++) posted.push(await api('/api/v1/channels/' + data.id + '/messages', {
      method: 'POST', token: session, slug,
      body: { body: 'data row ' + String(i).padStart(2, '0') + ' — ' + 'content '.repeat(18) },
    }));
    await api('/api/v1/channels/' + elsewhere.id + '/messages', {
      method: 'POST', token: session, slug, body: { body: 'elsewhere row' },
    });
    const apiBefore = await api('/api/v1/channels/' + data.id + '/messages?limit=100', { token: session, slug });

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

    await clickChannel(page, 'data');
    await page.waitForFunction((id) => document.querySelector('#messages .msg[data-id="' + id + '"]'), {}, posted[posted.length - 1].id);
    const first = await state(page);
    await page.$eval('#messages', (box) => { box.scrollTop = 0; });
    await clickChannel(page, 'elsewhere');
    await page.waitForFunction(() => document.querySelector('#messages').textContent.includes('elsewhere row'));

    // Hold the cached page's reconciliation at a snapshot taken before the
    // next live message. The message event must remain authoritative when the
    // older response is eventually delivered.
    await page.setRequestInterception(true);
    let releasePageSnapshot;
    let releaseThreadSnapshot;
    let pageSnapshotResolve;
    let threadSnapshotResolve;
    const pageSnapshotReady = new Promise((resolve) => { pageSnapshotResolve = resolve; });
    const threadSnapshotReady = new Promise((resolve) => { threadSnapshotResolve = resolve; });
    page.on('request', (request) => {
      if (!releasePageSnapshot && request.url().includes('/channels/' + data.id + '/messages?limit=100')) {
        api('/api/v1/channels/' + data.id + '/messages?limit=100', { token: session, slug })
          .then((snapshot) => {
            releasePageSnapshot = () => request.respond({
              status: 200, contentType: 'application/json', body: JSON.stringify(snapshot),
            });
            pageSnapshotResolve();
          }, (error) => request.abort().then(() => { throw error; }));
        return;
      }
      if (!releaseThreadSnapshot && new URL(request.url()).pathname === '/api/v1/threads') {
        api('/api/v1/threads', { token: session, slug }).then((snapshot) => {
          releaseThreadSnapshot = () => request.respond({
            status: 200, contentType: 'application/json', body: JSON.stringify(snapshot),
          });
          threadSnapshotResolve();
        }, (error) => request.abort().then(() => { throw error; }));
        return;
      }
      request.continue().catch(() => {});
    });
    await clickChannel(page, 'data');
    await Promise.all([pageSnapshotReady, threadSnapshotReady]);
    const reopened = await state(page);
    const live = await api('/api/v1/channels/' + data.id + '/messages', {
      method: 'POST', token: session, slug, body: { body: 'live during stale reconciliation' },
    });
    await page.waitForFunction((id) => document.querySelector('#messages .msg[data-id="' + id + '"]'), { timeout: 10000 }, live.id);
    const reply = await api('/api/v1/channels/' + data.id + '/messages', {
      method: 'POST', token: session, slug,
      body: { body: 'reply makes the live thread visible', thread_root_id: live.id },
    });
    await page.waitForFunction((snippet) => [...document.querySelectorAll('#channel-list .thread-leaf .t-snippet')]
      .some((node) => node.textContent.startsWith(snippet)), { timeout: 10000 }, 'live during stale');
    const withLive = await state(page);
    await Promise.all([releasePageSnapshot(), releaseThreadSnapshot()]);
    await new Promise((resolve) => setTimeout(resolve, 700));
    const revisited = await state(page);
    const threadLeafSurvived = await page.$$eval('#channel-list .thread-leaf .t-snippet', (nodes) =>
      nodes.some((node) => node.textContent.startsWith('live during stale')));
    const apiAfter = await api('/api/v1/channels/' + data.id + '/messages?limit=100', { token: session, slug });
    const threadAfter = await api('/api/v1/threads/' + live.id, { token: session, slug });
    let threadRendered = [];
    if (threadLeafSurvived) {
      await page.evaluate(() => [...document.querySelectorAll('#channel-list .thread-leaf .t-snippet')]
        .find((node) => node.textContent.startsWith('live during stale')).parentElement.click());
      await page.waitForSelector('#thread-panel:not(.hidden)', { timeout: 10000 });
      await page.waitForFunction((id) => document.querySelector('#thread-messages .msg[data-id="' + id + '"]'), {}, reply.id);
      threadRendered = await page.$$eval('#thread-messages > .msg', (nodes) => nodes.map((node) => node.dataset.id));
    }
    console.log(JSON.stringify({ apiBefore: apiBefore.messages.length, apiAfter: apiAfter.messages.length,
      initialRendered: first.count, reopenedRendered: reopened.count, reopenedBottomGap: reopened.bottomGap,
      liveRendered: withLive.count, finalRendered: revisited.count,
      threadMessages: threadAfter.messages.length, threadRendered: threadRendered.length, threadLeafSurvived }));
    if (first.count !== apiBefore.messages.length) throw new Error('initial render disagrees with API');
    if (reopened.bottomGap > 2) throw new Error('channel revisit did not land at the bottom');
    if (withLive.count !== apiAfter.messages.length || !withLive.ids.includes(live.id)) throw new Error('live event did not reach the page');
    if (revisited.count !== apiAfter.messages.length || !revisited.ids.includes(live.id)) throw new Error('stale reconcile removed a live message');
    if (!threadLeafSurvived) throw new Error('stale thread snapshot removed a live thread');
    if (threadAfter.messages.length !== 2 || threadAfter.messages[1].id !== reply.id) throw new Error('thread API lost messages');
    if (threadRendered.join(',') !== threadAfter.messages.map((message) => message.id).join(',')) throw new Error('thread panel disagrees with API');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('CHANNELNAV_CHECK_OK');
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
})().catch((error) => { console.error('CHANNELNAV_CHECK_FAIL:', error.message); process.exit(1); });
