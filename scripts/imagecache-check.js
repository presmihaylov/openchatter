// Browser regression for one protected attachment rendered twice: once in the
// channel and again in its open thread. The first load must not change the
// cache's return type and abort the duplicate render.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/imagecache-check.js
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'image cache check ' + tag;
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

const upload = async (token, workspace) => {
  // A tiny valid PNG is enough: the bug depends on a resolved protected-image
  // fetch, not image dimensions or decoding cost.
  const bytes = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64');
  const form = new FormData();
  form.append('file', new Blob([bytes], { type: 'image/png' }), 'twice.png');
  const response = await fetch(SERVER + '/api/v1/attachments', {
    method: 'POST',
    headers: Object.assign({}, access, { Authorization: 'Bearer ' + token, 'X-Workspace-Slug': workspace }),
    body: form,
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error('attachment upload -> ' + response.status + ' ' + JSON.stringify(data));
  return data;
};

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST',
      body: { username: 'image-cache-' + tag, password: 'correct horse battery', display_name: 'Image Cache Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'image-cache-' + tag },
    });
    slug = made.room.slug;
    const author = await api('/api/v1/rooms/join', {
      method: 'POST', body: { invite_code: made.invite_code, name: 'image-author-' + tag, description: 'image author' },
    });
    const attachment = await upload(author.token, slug);
    const root = await api('/api/v1/channels/general/messages', {
      method: 'POST', token: author.token, slug,
      body: { body: 'the same image belongs in both views', attachment_ids: [attachment.id] },
    });
    await api('/api/v1/channels/general/messages', {
      method: 'POST', token: author.token, slug,
      body: { body: 'thread reply', thread_root_id: root.id },
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
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForFunction((id) => {
      const image = document.querySelector(`#messages .msg[data-id="${id}"] img.inline-img`);
      return image && image.complete && image.naturalWidth > 0;
    }, { timeout: 10000 }, root.id);

    await page.click(`#messages .msg[data-id="${root.id}"] .reply-bar`);
    await page.waitForSelector('#thread-panel:not(.hidden)', { timeout: 10000 });
    await page.waitForFunction((id) => {
      const messages = document.querySelectorAll('#thread-messages .msg');
      const image = document.querySelector(`#thread-messages .msg[data-id="${id}"] img.inline-img`);
      return messages.length === 2 && image && image.complete && image.naturalWidth > 0;
    }, { timeout: 10000 }, root.id);
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('IMAGECACHE_CHECK_OK');
  } finally {
    if (page) await page.close().catch(() => {});
    if (browser) {
      if (ownsBrowser) await browser.close().catch(() => {});
      else browser.disconnect();
    }
    if (session && slug) await api('/api/v1/room', {
      method: 'DELETE', token: session, slug, body: { name: roomName },
    }).catch(() => {});
  }
})().catch((error) => { console.error('IMAGECACHE_CHECK_FAIL:', error.stack); process.exit(1); });
