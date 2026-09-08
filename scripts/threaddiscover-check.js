// A readable deep-linked thread may be outside the viewer's personal thread
// tree. While it is open, it still needs an active leaf under its channel so
// the sidebar represents the view without silently subscribing the viewer.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'thread discovery check ' + tag;
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
      method: 'POST', body: { username: 'thread-discovery-' + tag, password: 'correct horse battery', display_name: 'Thread Discovery Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'thread-discovery-' + tag },
    });
    slug = made.room.slug;
    const author = await api('/api/v1/rooms/join', {
      method: 'POST', body: { invite_code: made.invite_code, name: 'thread-author-' + tag, description: 'thread author' },
    });
    const root = await api('/api/v1/channels/general/messages', {
      method: 'POST', token: author.token, slug, body: { body: 'deep linked discussion ' + tag },
    });
    await api('/api/v1/channels/general/messages', {
      method: 'POST', token: author.token, slug, body: { body: 'a reply the viewer can read', thread_root_id: root.id },
    });
    const personal = await api('/api/v1/threads?include_archived=1', { token: session, slug });
    if ((personal.threads || []).some((thread) => thread.root_id === root.id)) throw new Error('viewer unexpectedly involved before opening');

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
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general/t/' + root.id, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForSelector('#thread-panel:not(.hidden)', { timeout: 10000 });
    await page.waitForFunction((id) => !!document.querySelector(`#messages .msg[data-id="${id}"] .reply-bar`), { timeout: 10000 }, root.id);
    const open = await page.evaluate((id) => ({
      rootInChannel: !!document.querySelector(`#messages .msg[data-id="${id}"]`),
      threadMessages: document.querySelectorAll('#thread-messages .msg').length,
      activeLeaves: document.querySelectorAll('#channel-list .thread-leaf.active').length,
      activeText: document.querySelector('#channel-list .thread-leaf.active .t-snippet')?.textContent || '',
    }), root.id);
    console.log(JSON.stringify(open));
    if (!open.rootInChannel || open.threadMessages !== 2 || open.activeLeaves !== 1 || !open.activeText.includes(tag)) {
      throw new Error('open deep-linked thread is absent from its channel navigation');
    }
    await page.click('#thread-close');
    await page.waitForSelector('#thread-panel.hidden', { timeout: 5000 });
    const leavesAfterClose = await page.$$eval('#channel-list .thread-leaf', (nodes) => nodes.length);
    const after = await api('/api/v1/threads?include_archived=1', { token: session, slug });
    const subscribed = (after.threads || []).some((thread) => thread.root_id === root.id);
    if (leavesAfterClose !== 0 || subscribed) throw new Error('transient leaf changed personal involvement');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('THREADDISCOVER_CHECK_OK');
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
})().catch((error) => { console.error('THREADDISCOVER_CHECK_FAIL:', error.stack); process.exit(1); });
