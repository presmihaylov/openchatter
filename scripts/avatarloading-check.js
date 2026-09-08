// Browser regression for participant avatar loading. A real attachment request
// is held while every avatar surface renders, then released; a second request
// is failed to prove the neutral error state. A fresh avatar built from the
// decoded in-page cache must be settled before its first animation frame.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/avatarloading-check.js
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const OUT = process.env.OUT || 'tmp';
const ACCESS_ID = process.env.ACCESS_ID || '';
const ACCESS_SECRET = process.env.ACCESS_SECRET || '';
const access = ACCESS_ID ? {
  'CF-Access-Client-Id': ACCESS_ID,
  'CF-Access-Client-Secret': ACCESS_SECRET,
} : {};
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (ok, message) => { if (!ok) throw new Error(message); };
const tag = () => Date.now().toString(36).slice(-7) + Math.floor(Math.random() * 1e6).toString(36);

async function request(route, opts = {}, retries = 3) {
  const headers = Object.assign({}, access, opts.body ? { 'Content-Type': 'application/json' } : {});
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + route, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 429 && retries > 0) {
    await sleep(3000);
    return request(route, opts, retries - 1);
  }
  if (resp.status !== (opts.status || 200)) {
    throw new Error(route + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  }
  return data;
}

// A valid solid PNG, kept local to make the check independent of fixture files.
const png = (rgb) => {
  const w = 96, h = 96, raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) raw.set(rgb, y * (w * 3 + 1) + 1 + x * 3);
  }
  const crc = (bytes) => {
    let value = -1;
    for (const byte of bytes) {
      value ^= byte;
      for (let bit = 0; bit < 8; bit++) value = (value >>> 1) ^ (0xEDB88320 & -(value & 1));
    }
    return (value ^ -1) >>> 0;
  };
  const chunk = (type, data) => {
    const length = Buffer.alloc(4); length.writeUInt32BE(data.length);
    const typed = Buffer.concat([Buffer.from(type), data]);
    const checksum = Buffer.alloc(4); checksum.writeUInt32BE(crc(typed));
    return Buffer.concat([length, typed, checksum]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]),
    chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0)),
  ]);
};

async function uploadAvatar(token, bytes) {
  const form = new FormData();
  form.append('file', new Blob([bytes], { type: 'image/png' }), 'avatar.png');
  const resp = await fetch(SERVER + '/api/v1/me/avatar', {
    method: 'POST', headers: Object.assign({}, access, { Authorization: 'Bearer ' + token }), body: form,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status !== 200) throw new Error('avatar upload -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

(async () => {
  let browser;
  let ownsBrowser = false;
  let session;
  let slug;
  let roomName;
  let page;
  try {
    const run = tag();
    const registered = await request('/api/v1/auth/password/register', {
      method: 'POST', status: 201,
      body: { username: 'avatar-load-' + run, password: 'correct horse battery', display_name: 'Avatar Reader' },
    });
    session = registered.token;
    roomName = 'avatar loading check ' + run;
    const made = await request('/api/v1/rooms', {
      method: 'POST', token: session, status: 201,
      body: { name: roomName, slug: 'avatar-loading-' + run },
    });
    slug = made.room.slug;
    const invite = await request('/api/v1/invites', {
      method: 'POST', token: session, slug, status: 201, body: { bind_owner: false },
    });
    const goodName = 'slowface-' + run;
    const badName = 'badface-' + run;
    const good = await request('/api/v1/rooms/join', {
      method: 'POST', status: 201,
      body: { invite: invite.join_url, name: goodName, description: 'avatar loading probe' },
    });
    const bad = await request('/api/v1/rooms/join', {
      method: 'POST', status: 201,
      body: { invite: invite.join_url, name: badName, description: 'avatar failure probe' },
    });
    const goodAvatar = await uploadAvatar(good.token, png([18, 150, 170]));
    const badAvatar = await uploadAvatar(bad.token, png([180, 70, 80]));
    assert(goodAvatar.avatar_attachment_id && badAvatar.avatar_attachment_id,
      'uploads omitted attachment ids');

    const channels = await request('/api/v1/channels', { token: session, slug });
    const general = channels.channels.find((channel) => channel.name === 'general');
    const post = (token, body, extra = {}) => request('/api/v1/channels/' + general.id + '/messages', {
      method: 'POST', token, slug, status: 201, body: Object.assign({ body }, extra),
    });
    const root = await post(good.token, 'slow avatar surface probe ' + run);
    await post(good.token, 'slow avatar thread probe ' + run, { thread_root_id: root.id });
    const broken = await post(bad.token, 'broken avatar surface probe ' + run);

    if (process.env.BROWSER_URL) {
      browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    } else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new',
        userDataDir: process.env.CHROME_PROFILE || undefined,
        dumpio: !!process.env.BROWSER_DUMPIO,
        args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-breakpad', '--disable-crash-reporter'],
      });
      ownsBrowser = true;
    }
    page = await browser.newPage();
    const errors = [];
    const held = [];
    await page.setViewport({ width: 1360, height: 900 });
    await page.setExtraHTTPHeaders(access);
    page.on('pageerror', (error) => errors.push('pageerror: ' + error.message));
    page.on('console', (message) => { if (message.type() === 'error') errors.push('console: ' + message.text()); });

    // Seed the browser session before interception. Only the good attachment
    // remains in flight; the bad attachment takes a genuine network failure.
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.setRequestInterception(true);
    page.on('request', (intercepted) => {
      const url = intercepted.url();
      if (url.includes('/api/v1/attachments/' + goodAvatar.avatar_attachment_id)) {
        held.push(intercepted);
        return;
      }
      if (url.includes('/api/v1/attachments/' + badAvatar.avatar_attachment_id)) {
        intercepted.abort('failed').catch(() => {});
        return;
      }
      intercepted.continue().catch(() => {});
    });
    await page.goto(BROWSER_SERVER + '/w/' + slug, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForSelector(`#messages .msg[data-id="${root.id}"]`, { timeout: 10000 });
    assert(held.length > 0, 'the slow attachment request was not intercepted');

    const stateFor = (selector) => page.$eval(selector, (box) => {
      const img = box.matches('img') ? box : box.querySelector('.avatar-img:not(.owner-badge-av)');
      const rect = img.getBoundingClientRect();
      const style = getComputedStyle(img);
      return {
        tag: img.tagName, state: img.dataset.avatarState, src: img.getAttribute('src'), alt: img.alt,
        text: img.textContent, width: Math.round(rect.width), height: Math.round(rect.height),
        radius: style.borderRadius, background: style.backgroundColor,
      };
    });
    const assertLoading = (where, state) => {
      assert(state.tag === 'IMG' && state.state === 'loading', where + ': ' + JSON.stringify(state));
      assert(state.src === null && state.alt === '' && state.text === '', where + ' leaked a fallback: ' + JSON.stringify(state));
      assert(state.width === state.height && state.width > 0 && state.radius === '50%', where + ' is not a matching circle: ' + JSON.stringify(state));
      assert(state.background !== 'rgba(0, 0, 0, 0)', where + ' has no skeleton fill: ' + JSON.stringify(state));
    };

    assertLoading('message row', await stateFor(`#messages .msg[data-id="${root.id}"] .avatar`));
    await page.waitForFunction((id) => document.querySelector(`#messages .msg[data-id="${id}"] .avatar-img`)?.dataset.avatarState === 'error', {}, broken.id);
    const badState = await stateFor(`#messages .msg[data-id="${broken.id}"] .avatar`);
    assert(badState.state === 'error' && badState.src === null && badState.text === '' && badState.radius === '50%',
      'genuine failure was not neutral: ' + JSON.stringify(badState));

    await page.click('#members-btn');
    await page.waitForSelector(`#members-list .mm-row[data-id="${good.participant.id}"]`, { timeout: 10000 });
    assertLoading('member list', await stateFor(`#members-list .mm-row[data-id="${good.participant.id}"]`));
    await page.click('#members-close');

    await page.click(`#messages .msg[data-id="${root.id}"] .reply-bar`);
    await page.waitForSelector(`#thread-messages .msg[data-id="${root.id}"]`, { timeout: 10000 });
    assertLoading('thread panel', await stateFor(`#thread-messages .msg[data-id="${root.id}"] .avatar`));

    await page.click('#open-search');
    await page.type('#search-input', 'slow avatar surface probe');
    await page.waitForSelector('#search-results .search-hit-row', { timeout: 10000 });
    assertLoading('search result', await stateFor('#search-results .search-hit-row .sh-avatar'));
    await page.click('#search-close');

    await page.click('#composer-input');
    await page.type('#composer-input', '@' + goodName.slice(0, 12));
    await page.waitForSelector('.mention-ac:not(.chan-ac) .mention-opt', { timeout: 10000 });
    assertLoading('mention picker', await stateFor('.mention-ac:not(.chan-ac) .mention-opt .mention-name'));
    const glyphs = await page.evaluate(() => ({
      emojiNodes: document.querySelectorAll('.avatar-emoji').length,
      avatarText: [...document.querySelectorAll('.avatar-img')].filter((img) => img.textContent.trim()).length,
    }));
    assert(glyphs.emojiNodes === 0 && glyphs.avatarText === 0, 'emoji fallback rendered: ' + JSON.stringify(glyphs));
    fs.mkdirSync(OUT, { recursive: true });
    await page.screenshot({ path: path.join(OUT, 'avatar-loading-slow.png') });

    const releasing = held.splice(0);
    await Promise.all(releasing.map((intercepted) => intercepted.continue()));
    await page.waitForFunction((name) => {
      const images = [...document.querySelectorAll('.avatar-img:not(.owner-badge-av)')]
        .filter((img) => img.alt === name);
      return images.length >= 5 && images.every((img) => img.dataset.avatarState === 'loaded' && img.src.startsWith('blob:'));
    }, { timeout: 10000 }, goodName);
    const assertLoaded = (where, state) => assert(state.state === 'loaded' && state.src.startsWith('blob:') && state.alt === goodName,
      where + ' did not swap to the real image: ' + JSON.stringify(state));
    assertLoaded('message row', await stateFor(`#messages .msg[data-id="${root.id}"] .avatar`));
    assertLoaded('member list', await stateFor(`#members-list .mm-row[data-id="${good.participant.id}"]`));
    assertLoaded('thread panel', await stateFor(`#thread-messages .msg[data-id="${root.id}"] .avatar`));
    assertLoaded('search result', await stateFor('#search-results .search-hit-row .sh-avatar'));
    assertLoaded('mention picker', await stateFor('.mention-ac:not(.chan-ac) .mention-opt .mention-name'));

    await page.keyboard.press('Escape');
    await page.click('#composer-input');
    await page.keyboard.down(process.platform === 'darwin' ? 'Meta' : 'Control');
    await page.keyboard.press('A');
    await page.keyboard.up(process.platform === 'darwin' ? 'Meta' : 'Control');
    await page.keyboard.press('Backspace');
    await page.type('#composer-input', '@' + goodName.slice(0, 12));
    await page.waitForSelector('.mention-ac:not(.chan-ac) .mention-opt .avatar-img', { timeout: 10000 });
    const cached = await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => {
      const img = document.querySelector('.mention-ac:not(.chan-ac) .mention-opt .avatar-img');
      resolve({ state: img.dataset.avatarState, loading: img.classList.contains('avatar-loading'), src: img.getAttribute('src') });
    })));
    assert(cached.state === 'loaded' && !cached.loading && cached.src.startsWith('blob:'),
      'decoded cache flashed a skeleton: ' + JSON.stringify(cached));

    const realErrors = errors.filter((error) => !error.includes('favicon') && !error.includes('net::ERR_FAILED'));
    assert(!realErrors.length, 'browser errors: ' + realErrors.join(' | '));
    console.log('AVATARLOADING_CHECK_OK');
  } finally {
    if (page) await page.close().catch(() => {});
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (session && slug && roomName) {
      await request('/api/v1/room', { method: 'DELETE', token: session, slug, body: { name: roomName } }).catch(() => {});
    }
  }
})().catch((error) => {
  console.error('AVATARLOADING_CHECK_FAIL:', error.message);
  process.exit(1);
});
