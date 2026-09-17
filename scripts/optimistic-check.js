// E2E: optimistic send (Maya FR seq 341). A sent message appears instantly as a
// dimmed placeholder before the backend confirms; when its server echo arrives
// the placeholder settles into the real message with NO duplicate. On a failed
// POST the placeholder rolls back and the draft returns to the input.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/optimistic-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const countWith = (page, text) => page.evaluate((t) => {
  const all = [...document.querySelectorAll('#messages .msg')].filter((m) => m.textContent.includes(t));
  return {
    total: all.length,
    pending: all.filter((m) => m.classList.contains('pending')).length,
    tmpIds: all.filter((m) => (m.dataset.id || '').startsWith('tmp-')).length,
  };
}, text);

(async () => {
  const created = await newRoom(SERVER, 'optimistic check');
  const slug = created.room.slug;
  const human = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'humantester', avatar: '🧑', is_human: true } });

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1000, height: 800 });
  page.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });

    await openAsHuman(page, SERVER, slug, human);
  await page.waitForSelector('#composer-input', { timeout: 6000 });

  // Hold the send POST for ~1.2s so we can observe the placeholder while the
  // server has not yet confirmed. Everything else (the events long-poll) passes
  // through untouched, so the echo still arrives once the POST completes.
  await page.setRequestInterception(true);
  let failNext = false;
  page.on('request', (req) => {
    const isSend = req.method() === 'POST' && /\/channels\/[^/]+\/messages$/.test(req.url());
    if (isSend && failNext) { req.respond({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }); return; }
    if (isSend) { setTimeout(() => req.continue(), 1200); return; }
    req.continue();
  });

  const TEXT = 'optimistic hello ' + slug.slice(0, 6);
  await page.focus('#composer-input');
  await page.type('#composer-input', TEXT);
  await page.keyboard.press('Enter');

  // 1) INSTANT: placeholder is on screen within a moment, dimmed, tmp id; input cleared
  await page.waitForFunction((t) => [...document.querySelectorAll('#messages .msg.pending')].some((m) => m.textContent.includes(t)), { timeout: 600 }, TEXT);
  let s = await countWith(page, TEXT);
  if (s.pending !== 1 || s.tmpIds !== 1) throw new Error('expected one dimmed tmp placeholder, got ' + JSON.stringify(s));
  const cleared = await page.evaluate(() => document.querySelector('#composer-input').value);
  if (cleared !== '') throw new Error('input should clear instantly, got ' + JSON.stringify(cleared));

  // 2) SETTLE: the server echo replaces the placeholder — exactly one real node, no dup
  await page.waitForFunction((t) => {
    const all = [...document.querySelectorAll('#messages .msg')].filter((m) => m.textContent.includes(t));
    return all.length === 1 && !all[0].classList.contains('pending') && !(all[0].dataset.id || '').startsWith('tmp-');
  }, { timeout: 8000 }, TEXT);
  s = await countWith(page, TEXT);
  if (s.total !== 1 || s.pending !== 0 || s.tmpIds !== 0) throw new Error('expected one settled real message, got ' + JSON.stringify(s));

  // 3) FAILURE ROLLBACK: a rejected send drops the placeholder and restores the draft
  failNext = true;
  const FAIL = 'doomed message ' + slug.slice(0, 6);
  await page.focus('#composer-input');
  await page.type('#composer-input', FAIL);
  await page.keyboard.press('Enter');
  await page.waitForFunction((t) => document.querySelector('#composer-input').value === t, { timeout: 6000 }, FAIL);
  const after = await countWith(page, FAIL);
  if (after.total !== 0) throw new Error('failed send should leave no message node, got ' + JSON.stringify(after));
  // the reason sits under the box, not in a dialog, and the next keystroke clears it
  const note = await page.$eval('#composer-error', (el) => ({ hidden: el.classList.contains('hidden'), text: el.textContent }));
  if (note.hidden || !/^Not sent: /.test(note.text)) throw new Error('failed send should explain itself under the composer, got ' + JSON.stringify(note));
  await page.type('#composer-input', '!');
  const noteGone = await page.$eval('#composer-error', (el) => el.classList.contains('hidden'));
  if (!noteGone) throw new Error('typing should clear the send error');

  await browser.close();
  if (!process.exitCode) console.log('OPTIMISTIC_CHECK_OK');
})().catch((e) => { console.error('OPTIMISTIC_CHECK_FAIL:', e.message); process.exit(1); });
