require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for explicit acknowledgements (task 32): the hover toolbar carries a check
// button, a human ack paints a white check next to the timestamp, the acker
// names live on its tooltip, a second acker over the API lands live, and the
// thread-panel copy of the root shows the same mark.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8095 node scripts/ack-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

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

(async () => {
  const created = await newRoom(SERVER, 'ack check');
  const slug = created.room.slug, code = created.invite_code;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'alice', is_human: true } });
  const bob = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'bob', description: 'bot' } });
  const root = await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'hey @alice can you take this' }, token: bob.token });
  await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'a reply', thread_root_id: root.id }, token: bob.token });

  // the ask is pending for alice until she acks it
  let pending = (await api('/api/v1/me/pending-acks', { token: alice.token })).pending;
  assert(pending.length === 1 && pending[0].message_id === root.id && pending[0].reason === 'mention',
    'pending before ack: ' + JSON.stringify(pending));

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });
  await openAsHuman(page, SERVER, slug, alice);
  const msgSel = `#messages .msg[data-id="${root.id}"]`;
  await page.waitForSelector(msgSel, { timeout: 8000 });

  const mark = (scope = '#messages') => page.$eval(`${scope} .msg[data-id="${root.id}"] .msg-ack`,
    (el) => ({ icon: el.querySelector('svg') ? el.querySelector('svg').dataset.icon : '', title: el.title }));

  // 1. an unacked message carries an empty mark and no tooltip
  let m = await mark();
  assert(m.icon === '' && !m.title, 'fresh message already marked: ' + JSON.stringify(m));

  // 2. the human acks from the hover toolbar: a check appears with her name
  await page.hover(msgSel);
  await page.click(`${msgSel} .msg-actions button[data-act="ack"]`);
  await page.waitForFunction((sel) => !!document.querySelector(`${sel} .msg-ack svg`), { timeout: 4000 }, msgSel);
  m = await mark();
  assert(m.icon === 'check' && m.title === 'acknowledged by alice', 'after alice acks: ' + JSON.stringify(m));
  const onServer = (await api('/api/v1/messages/' + root.id, { token: alice.token })).acked_by;
  assert(onServer.length === 1 && onServer[0].name === 'alice', 'server: ' + JSON.stringify(onServer));

  // acking clears it from her pending list
  pending = (await api('/api/v1/me/pending-acks', { token: alice.token })).pending;
  assert(pending.length === 0, 'pending after ack: ' + JSON.stringify(pending));

  // 3. a second acker over the API lands live and joins the tooltip
  await api('/api/v1/messages/' + root.id + '/ack', { method: 'POST', token: bob.token });
  await page.waitForFunction((sel) => (document.querySelector(`${sel} .msg-ack`) || {}).title === 'acknowledged by alice, bob',
    { timeout: 4000 }, msgSel);

  // 4. acking twice is idempotent: still one row for alice
  await api('/api/v1/messages/' + root.id + '/ack', { method: 'POST', token: alice.token });
  const again = (await api('/api/v1/messages/' + root.id, { token: alice.token })).acked_by;
  assert(again.length === 2, 'ack is not idempotent: ' + JSON.stringify(again));

  // 5. the thread panel's copy of the root shows the same mark
  await page.click(`${msgSel} .reply-bar`);
  await page.waitForSelector(`#thread-panel .msg[data-id="${root.id}"] .msg-ack svg`, { timeout: 4000 });
  m = await mark('#thread-panel');
  assert(m.icon === 'check' && m.title === 'acknowledged by alice, bob', 'thread copy: ' + JSON.stringify(m));
  await page.screenshot({ path: (process.env.OUT || 'tmp') + '/ack.png' });

  await browser.close();
  console.log('ACK_CHECK_OK');
})().catch((e) => { console.error(e); process.exit(1); });
