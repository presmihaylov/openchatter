// Repainting the Personal-settings avatar must retire the previous protected
// image's object URL. Upload twice and remove once in a real browser, while
// counting browser URL lifecycle calls.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'avatar url lifecycle ' + tag;
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

const chooseAvatar = (page, name) => page.$eval('#avatar-input', (input, filename) => {
  const raw = atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==');
  const bytes = Uint8Array.from(raw, (c) => c.charCodeAt(0));
  const transfer = new DataTransfer();
  transfer.items.add(new File([bytes], filename, { type: 'image/png' }));
  Object.defineProperty(input, 'files', { configurable: true, value: transfer.files });
  input.dispatchEvent(new Event('change', { bubbles: true }));
}, name);

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'avatar-url-' + tag, password: 'correct horse battery', display_name: 'Avatar URL Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'avatar-url-' + tag },
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
    await page.evaluateOnNewDocument(() => {
      window.__urlLife = { created: 0, revoked: 0 };
      const create = URL.createObjectURL.bind(URL);
      const revoke = URL.revokeObjectURL.bind(URL);
      URL.createObjectURL = (value) => { window.__urlLife.created++; return create(value); };
      URL.revokeObjectURL = (value) => { window.__urlLife.revoked++; return revoke(value); };
    });
    const errors = [];
    page.on('pageerror', (error) => errors.push('pageerror: ' + error.message));
    page.on('console', (message) => { if (message.type() === 'error') errors.push('console: ' + message.text()); });
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    const next = encodeURIComponent('/w/' + slug + '/c/general');
    await page.goto(BROWSER_SERVER + '/settings?next=' + next, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#avatar-section:not(.hidden)', { timeout: 10000 });

    await chooseAvatar(page, 'first.png');
    await page.waitForFunction(() => (document.querySelector('#settings-avatar img')?.src || '').startsWith('blob:'), { timeout: 10000 });
    const first = await page.$eval('#settings-avatar img', (img) => img.src);
    await chooseAvatar(page, 'second.png');
    await page.waitForFunction((old) => {
      const src = document.querySelector('#settings-avatar img')?.src || '';
      return src.startsWith('blob:') && src !== old;
    }, { timeout: 10000 }, first);
    const afterSecond = await page.evaluate(() => ({ ...window.__urlLife }));
    await page.click('#avatar-remove');
    await page.waitForFunction(() => document.querySelector('#settings-avatar img')?.getAttribute('src') === '/brand/avatar-default-512.png', { timeout: 10000 });
    const afterRemove = await page.evaluate(() => ({ ...window.__urlLife }));
    console.log(JSON.stringify({ afterSecond, afterRemove }));
    if (afterSecond.created !== 2 || afterSecond.revoked < 1) throw new Error('repaint did not retire the first avatar URL');
    if (afterRemove.revoked < 2) throw new Error('remove did not retire the second avatar URL');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('AVATARURL_CHECK_OK');
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
})().catch((error) => { console.error('AVATARURL_CHECK_FAIL:', error.message); process.exit(1); });
