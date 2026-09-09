// A resolved participant-avatar object URL must be reused synchronously on
// channel rerenders: no new attachment request and no transient shimmer node.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const traceOnly = process.env.TRACE_ONLY === '1';
const run = Date.now().toString(36).slice(-7);
const roomName = 'avatar cache check ' + run;
let session;
let slug;

const assert = (ok, message) => { if (!ok) throw new Error(message); };
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

const uploadAvatar = async (token, workspace) => {
  const bytes = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==', 'base64');
  const form = new FormData();
  form.append('file', new Blob([bytes], { type: 'image/png' }), 'avatar.png');
  const response = await fetch(SERVER + '/api/v1/me/avatar', {
    method: 'POST',
    headers: Object.assign({}, access, { Authorization: 'Bearer ' + token, 'X-Workspace-Slug': workspace }),
    body: form,
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error('avatar upload -> ' + response.status + ' ' + JSON.stringify(data));
  return data.avatar_attachment_id;
};

const waitAvatar = (page, id) => page.waitForFunction((messageID) => {
  const image = document.querySelector(`#messages .msg[data-id="${messageID}"] .avatar-img`);
  return image && image.dataset.avatarState === 'loaded' && image.complete && image.naturalWidth > 0;
}, { timeout: 10000 }, id);

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'avatar-cache-' + run, password: 'correct horse battery', display_name: 'Avatar Cache Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'avatar-cache-' + run },
    });
    slug = made.room.slug;
    const author = await api('/api/v1/rooms/join', {
      method: 'POST', body: { invite_code: made.invite_code, name: 'avatar-author-' + run, description: 'cache author' },
    });
    const attachmentID = await uploadAvatar(author.token, slug);
    const other = await api('/api/v1/channels', {
      method: 'POST', token: session, slug, body: { name: 'other', topic: 'avatar cache check' },
    });
    await api('/api/v1/channels/' + other.id + '/members', {
      method: 'POST', token: session, slug, body: { participant: author.participant.id },
    });
    const channels = await api('/api/v1/channels', { token: session, slug });
    const general = channels.channels.find((channel) => channel.name === 'general');
    const post = (channel, body) => api('/api/v1/channels/' + channel + '/messages', {
      method: 'POST', token: author.token, slug, body: { body },
    });
    const first = await post(general.id, 'general avatar cache probe');
    const second = await post(other.id, 'other avatar cache probe');

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
    const requests = [];
    let phase = 'initial';
    const target = '/api/v1/attachments/' + attachmentID + '?size=128';
    page.on('request', (request) => {
      if (request.url().includes(target)) requests.push(phase);
    });

    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await waitAvatar(page, first.id);

    // Count a shimmer even if a fulfilled Promise removes it before Chrome's
    // next paint. The frame count separately says whether one was visible.
    await page.evaluate(() => {
      const seen = new WeakSet();
      const original = Node.prototype.appendChild;
      window.__avatarCacheTrace = { insertions: 0, frames: 0, active: true, original };
      Node.prototype.appendChild = function (node) {
        if (window.__avatarCacheTrace.active && node instanceof Element) {
          const images = [node, ...node.querySelectorAll('.avatar-img')]
            .filter((item) => item.matches('.avatar-img.avatar-loading'));
          for (const image of images) {
            if (!seen.has(image)) { seen.add(image); window.__avatarCacheTrace.insertions++; }
          }
        }
        return original.call(this, node);
      };
      const sample = () => {
        if (!window.__avatarCacheTrace.active) return;
        if (document.querySelector('#messages .avatar-img.avatar-loading')) window.__avatarCacheTrace.frames++;
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    });

    phase = 'switch-other';
    await page.evaluate(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .find((node) => node.textContent === 'other').click());
    await waitAvatar(page, second.id);
    phase = 'switch-back';
    await page.evaluate(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .find((node) => node.textContent === 'general').click());
    await waitAvatar(page, first.id);
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const shimmer = await page.evaluate(() => {
      window.__avatarCacheTrace.active = false;
      Node.prototype.appendChild = window.__avatarCacheTrace.original;
      return { insertions: window.__avatarCacheTrace.insertions, frames: window.__avatarCacheTrace.frames };
    });
    const result = {
      initialRequests: requests.filter((item) => item === 'initial').length,
      switchOtherRequests: requests.filter((item) => item === 'switch-other').length,
      switchBackRequests: requests.filter((item) => item === 'switch-back').length,
      cachedShimmerInsertions: shimmer.insertions,
      visibleShimmerFrames: shimmer.frames,
    };
    console.log(JSON.stringify(result));
    assert(result.initialRequests === 1, 'cold load did not make exactly one request');
    assert(result.switchOtherRequests === 0 && result.switchBackRequests === 0, 'channel switch refetched the avatar');
    if (!traceOnly) assert(result.cachedShimmerInsertions === 0 && result.visibleShimmerFrames === 0,
      'cached avatar entered a shimmer state: ' + JSON.stringify(result));
    console.log(traceOnly ? 'AVATARCACHE_TRACE_OK' : 'AVATARCACHE_CHECK_OK');
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
})().catch((error) => { console.error('AVATARCACHE_CHECK_FAIL:', error.stack); process.exit(1); });
