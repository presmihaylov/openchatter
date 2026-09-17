// E2E: every failure class reaches a surface a person can act on.
// 1 a double click on Acknowledge sends one request
// 2 a 5xx on an action: error toast naming the action, no raw body, Retry that works
// 3 a 2xx that is not JSON: the page says it could not read the answer
// 4 a failed send hands the staged attachment back (regression: the chip was lost)
// 5 offline: bar while offline, a send says so inline, bar gone once back
// 6 the live feed dies: bar after the second failed poll, Retry now, the missed
//   message arrives and the workspace is refetched once
// 7 a render crash in one region shows that region's fallback; the rest keeps working
// 8 an expired Cloudflare Access session: persistent bar with Reload, no toast
// 9 a server that cannot boot the page: bar after the second failed attempt, Retry now
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/errors-check.js
const fs = require('fs');
const os = require('os');
const path = require('path');
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman, sleep } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
// SKIP=1,2 leaves cases out, to show one case red on a build without its fix
const skip = new Set((process.env.SKIP || '').split(',').filter(Boolean));
const step = async (n, name, fn) => {
  if (skip.has(String(n))) { console.log(n + ' ' + name + ': skipped'); return; }
  await fn();
  console.log(n + ' ' + name + ': OK');
};

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const json = (status, body) => ({ status, contentType: 'application/json', body: JSON.stringify(body) });
const isAck = (req, id) => req.method() === 'POST' && req.url().endsWith('/messages/' + id + '/ack');
const ackButton = (id) => `#messages .msg[data-id="${id}"] .msg-actions button[data-act="ack"]`;
const waitText = (page, sel, re, timeout = 6000) => page.waitForFunction(
  (s, r) => [...document.querySelectorAll(s)].some((n) => new RegExp(r).test(n.textContent)), { timeout }, sel, re.source);
const clickChannel = async (page, name) => {
  await page.evaluate((wanted) => [...document.querySelectorAll('#channel-list .chan-name')]
    .find((node) => node.textContent === wanted).parentElement.click(), name);
  await page.waitForFunction((wanted) => document.querySelector('#channel-title').textContent.trim() === wanted, { timeout: 6000 }, name);
};
const hasMessage = (page, text, timeout = 8000) => page.waitForFunction(
  (t) => [...document.querySelectorAll('#messages .msg:not(.pending)')].some((m) => m.textContent.includes(t)), { timeout }, text);

(async () => {
  const created = await newRoom(SERVER, 'errors check');
  const slug = created.room.slug, code = created.invite_code;
  const human = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'errhuman', is_human: true } });
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'errbot', description: 'bot' } });
  const say = (body, channel = 'general') => api('/api/v1/channels/' + channel + '/messages', { method: 'POST', body: { body }, token: bot.token });
  const m1 = await say('first ask');
  const m2 = await say('second ask');
  const m3 = await say('third ask');
  const boom = await api('/api/v1/channels', { method: 'POST', body: { name: 'boom' }, token: bot.token });
  await api('/api/v1/channels/' + boom.id + '/join', { method: 'POST', token: human.token });
  await say('boom channel message', boom.id);

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'errors-check-'));
  const png = path.join(dir, 'dot.png');
  fs.writeFileSync(png, Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64'));

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });
  await openAsHuman(page, SERVER, slug, human);
  await page.waitForSelector(`#messages .msg[data-id="${m3.id}"]`, { timeout: 8000 });

  // one interception hook for the whole run; each case swaps the rule
  let rule = null;
  let feedOK = 0;
  page.on('response', (r) => { if (r.url().includes('/api/v1/user/events') && r.status() === 200) feedOK++; });
  const feedHealthy = async () => {
    const mark = feedOK;
    await say('warm'); // ends the poll in flight, or the first one after a backoff
    for (let i = 0; i < 100 && feedOK === mark; i++) await sleep(100);
    assert(feedOK > mark, 'the feed did not come back');
  };
  await page.setRequestInterception(true);
  page.on('request', (req) => { if (rule && rule(req)) return; req.continue(); });

  // ---- 1. double click, one request
  await step(1, 'double click sends once', async () => {
    let ackCalls = 0;
    rule = (req) => { if (!isAck(req, m1.id)) return false; ackCalls++; setTimeout(() => req.continue(), 1000); return true; };
    await page.hover(`#messages .msg[data-id="${m1.id}"]`);
    await page.click(ackButton(m1.id));
    await page.click(ackButton(m1.id));
    await page.waitForFunction((id) => document.querySelector(`.msg[data-id="${id}"] .msg-ack svg`), { timeout: 6000 }, m1.id);
    assert(ackCalls === 1, 'a double click must send one ack, sent ' + ackCalls);
  });

  // ---- 2. a 5xx becomes a toast that names the action, hides the body and retries
  await step(2, '5xx toast with a working Retry', async () => {
    let failOnce = true;
    rule = (req) => { if (!isAck(req, m2.id) || !failOnce) return false; failOnce = false; req.respond(json(503, { error: 'db is down at 10.0.0.3' })); return true; };
    await page.hover(`#messages .msg[data-id="${m2.id}"]`);
    await page.click(ackButton(m2.id));
    await waitText(page, '#toasts .toast.err .toast-text', /^Could not acknowledge the message: /);
    const toastText = await page.$eval('#toasts .toast.err .toast-text', (n) => n.textContent);
    assert(/unavailable/.test(toastText) && !/10\.0\.0\.3|\{/.test(toastText), 'a 5xx body must not leak into the toast: ' + toastText);
    await page.evaluate(() => [...document.querySelectorAll('#toasts .toast.err button')].find((b) => b.textContent === 'Retry').click());
    await page.waitForFunction((id) => document.querySelector(`.msg[data-id="${id}"] .msg-ack svg`), { timeout: 6000 }, m2.id);
  });

  // ---- 3. a 200 that is HTML
  await step(3, 'unreadable answer is named', async () => {
    rule = (req) => { if (!req.url().includes('/api/v1/threads/' + m3.id)) return false; req.respond({ status: 200, contentType: 'text/html', body: '<html><body>maintenance</body></html>' }); return true; };
    await page.hover(`#messages .msg[data-id="${m3.id}"]`);
    await page.click(`#messages .msg[data-id="${m3.id}"] .msg-actions button[data-act="thread"]`);
    await waitText(page, '#toasts .toast.err .toast-text', /could not read/);
    rule = null;
  });

  // ---- 4. a failed send hands the attachment back
  await step(4, 'failed send keeps the attachment', async () => {
    const input = await page.$('#attach-input');
    await input.uploadFile(png);
    await page.waitForFunction(() => !document.getElementById('attach-pending').classList.contains('hidden'), { timeout: 6000 });
    rule = (req) => { if (!(req.method() === 'POST' && /\/channels\/[^/]+\/messages$/.test(req.url()))) return false; req.respond(json(500, { error: 'boom' })); return true; };
    await page.focus('#composer-input');
    await page.type('#composer-input', 'with a file');
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => !document.getElementById('composer-error').classList.contains('hidden'), { timeout: 6000 });
    const chip = await page.$eval('#attach-pending', (el) => ({ hidden: el.classList.contains('hidden'), text: el.textContent }));
    assert(!chip.hidden && /dot\.png/.test(chip.text), 'the failed send must hand the attachment back: ' + JSON.stringify(chip));
    rule = null;
    await page.keyboard.press('Enter');
    await hasMessage(page, 'with a file');
    const sent = (await api('/api/v1/channels/general/messages?limit=50', { token: bot.token })).messages.find((m) => m.body === 'with a file');
    assert(sent && (sent.attachments || []).length === 1, 'the retried send must carry the file: ' + JSON.stringify(sent && sent.attachments));
    assert(await page.$eval('#attach-pending', (el) => el.classList.contains('hidden')), 'the chip leaves with the message');
  });

  // ---- 5. offline
  await step(5, 'offline bar and inline reason', async () => {
    await page.setOfflineMode(true);
    await page.waitForSelector('[data-banner="offline"]', { timeout: 6000 });
    await page.focus('#composer-input');
    await page.type('#composer-input', 'typed offline');
    await page.keyboard.press('Enter');
    await waitText(page, '#composer-error', /^Not sent: You are offline/);
    await page.setOfflineMode(false);
    await page.waitForFunction(() => !document.querySelector('[data-banner="offline"]'), { timeout: 6000 });
    await page.keyboard.press('Enter'); // the draft came back; it goes now
    await hasMessage(page, 'typed offline');
  });

  // ---- 6. the feed dies and comes back
  await step(6, 'feed reconnect bar, replay and one refetch', async () => {
    await feedHealthy(); // the offline spell above left the loop in backoff
    let feedFails = 0;
    rule = (req) => { if (!req.url().includes('/api/v1/user/events')) return false; feedFails++; req.respond(json(503, { error: 'down' })); return true; };
    await say('poke'); // ends the poll in flight; every poll after it fails
    await page.waitForSelector('[data-banner="feed"]', { timeout: 20000 });
    assert(feedFails >= 2, 'the bar waits for a second failed poll, saw ' + feedFails);
    await waitText(page, '[data-banner="feed"]', /Reconnecting/);
    const missed = await say('sent while the feed was down');
    let roomFetches = 0;
    rule = (req) => { if (/\/api\/v1\/room$/.test(req.url())) roomFetches++; return false; };
    await page.evaluate(() => document.querySelector('[data-banner="feed"] button').click());
    await page.waitForFunction(() => !document.querySelector('[data-banner="feed"]'), { timeout: 10000 });
    await page.waitForSelector(`#messages .msg[data-id="${missed.id}"]`, { timeout: 10000 });
    await sleep(500);
    assert(roomFetches === 1, 'one workspace refetch after recovery, got ' + roomFetches);
    rule = null;
  });

  // ---- 7. a render crash stays inside its region
  await step(7, 'region fallback, rest intact, next render heals', async () => {
    await clickChannel(page, 'boom');
    await hasMessage(page, 'boom channel message');
    await page.evaluate(() => {
      // the first message's exact-time tooltip throws once; the sidebar never formats one
      const orig = Date.prototype.toLocaleString;
      Date.prototype.toLocaleString = function () { Date.prototype.toLocaleString = orig; throw new Error('render boom'); };
      [...document.querySelectorAll('#channel-list .chan-name')].find((n) => n.textContent === 'general').parentElement.click();
    });
    await page.waitForSelector('#messages .region-error', { timeout: 6000 });
    const card = await page.$eval('#messages .region-error', (el) => ({ text: el.textContent, reload: !!el.querySelector('button') }));
    assert(/Something went wrong while showing the messages/.test(card.text) && card.reload, 'fallback card: ' + JSON.stringify(card));
    const sidebar = await page.$$eval('#channel-list .chan-name', (n) => n.length);
    assert(sidebar >= 2, 'the sidebar must survive a messages crash, rows: ' + sidebar);
    await clickChannel(page, 'boom');
    await page.waitForFunction(() => document.querySelector('#messages .msg') && !document.querySelector('#messages .region-error'), { timeout: 6000 });
  });

  // ---- 8. Cloudflare Access wants a login
  await step(8, 'access expired bar, no toast', async () => {
    await clickChannel(page, 'general');
    const toastsBefore = await page.$$eval('#toasts .toast', (n) => n.length);
    rule = (req) => { if (!isAck(req, m3.id)) return false; req.respond({ status: 302, headers: { location: 'https://team.cloudflareaccess.com/cdn-cgi/access/login/app' }, body: '' }); return true; };
    await page.hover(`#messages .msg[data-id="${m3.id}"]`);
    await page.click(ackButton(m3.id));
    await page.waitForSelector('[data-banner="access"]', { timeout: 6000 });
    const access = await page.$eval('[data-banner="access"]', (el) => ({ text: el.textContent, action: el.querySelector('button')?.textContent }));
    assert(/Cloudflare Access session has expired/.test(access.text) && access.action === 'Reload', 'access bar: ' + JSON.stringify(access));
    await sleep(300);
    const toastsAfter = await page.$$eval('#toasts .toast', (n) => n.length);
    assert(toastsAfter <= toastsBefore, 'an expired session shows one bar, not a toast per call');
    rule = null;
  });

  // ---- 9. the page cannot boot
  await step(9, 'boot bar with Retry now', async () => {
    const page2 = await browser.newPage();
    page2.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });
    let bootAttempts = 0, blocked = true;
    await page2.setRequestInterception(true);
    page2.on('request', (req) => {
      if (blocked && req.url().includes('/api/v1/')) {
        if (req.url().endsWith('/api/v1/me')) bootAttempts++; // one per enterChat
        req.respond(json(503, { error: 'down' }));
        return;
      }
      req.continue();
    });
    await page2.goto(SERVER + '/r/' + slug, { waitUntil: 'domcontentloaded' });
    await page2.waitForSelector('[data-banner="boot"]', { timeout: 20000 });
    await waitText(page2, '[data-banner="boot"]', /Cannot reach the server/);
    assert(bootAttempts >= 2, 'the boot bar waits for a second failed attempt, saw ' + bootAttempts);
    const visible = await page2.$eval('[data-banner="boot"]', (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; });
    assert(visible, 'the boot bar must show through the splash');
    blocked = false;
    await page2.evaluate(() => document.querySelector('[data-banner="boot"] button').click());
    await page2.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page2.waitForFunction(() => !document.querySelector('[data-banner="boot"]'), { timeout: 6000 });
  });

  await browser.close();
  if (!process.exitCode) console.log('ERRORS_CHECK_OK');
})().catch((e) => { console.error('ERRORS_CHECK_FAIL:', e.message); process.exit(1); });
