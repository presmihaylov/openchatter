require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for notifications: relevant traffic still raises a notification event,
// but only a direct tag may request audio. Nothing fires for your own messages
// or top-level traffic in the channel you are viewing; muted channels stay
// quiet and dark except for tags, and settings persist across a reload.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/notify-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman, openSettings, backToRoom } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status >= 400) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const created = await newRoom(SERVER, 'notify check');
  const slug = created.room.slug;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'alice', is_human: true } });
  const bob = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'bob', description: 'bot' } });
  const plaza = await api('/api/v1/channels', { method: 'POST', token: alice.token, body: { name: 'plaza' } });
  await api('/api/v1/channels/' + plaza.id + '/join', { method: 'POST', token: bob.token });
  const say = (channel, body, extra = {}) => api('/api/v1/channels/' + channel + '/messages', {
    method: 'POST', token: bob.token, body: Object.assign({ body }, extra),
  });

  const browser = process.env.BROWSER_URL
    ? await puppeteer.connect({ browserURL: process.env.BROWSER_URL })
    : await puppeteer.launch({
      executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
      headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
    });
  await browser.defaultBrowserContext().overridePermissions(SERVER, ['notifications']);
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  await page.evaluateOnNewDocument(() => {
    window.__notes = [];
    document.addEventListener('agentchat:notify', (ev) => window.__notes.push(ev.detail));
  });
    await openAsHuman(page, SERVER, slug, alice);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await page.waitForFunction(() => document.querySelector('#channel-title').textContent.includes('general'), { timeout: 8000 });

  const notes = () => page.evaluate(() => window.__notes.splice(0));
  // wait for the event stream to deliver, then a beat for the handler
  const settle = async (pred) => {
    await page.waitForFunction(pred, { timeout: 8000 });
    await sleep(400);
  };

  // 1. a top-level post in another channel you are in: one ping
  const root = await say('plaza', 'root in plaza');
  await settle(() => window.__notes.length >= 1);
  let got = await notes();
  assert(got.length === 1 && got[0].why === 'channel' && got[0].sound === false, 'plaza root: ' + JSON.stringify(got));

  // 2. a post in the channel you are looking at: silent
  await say('general', 'you are watching this one');
  await settle(() => [...document.querySelectorAll('#messages .msg')].some((m) => m.textContent.includes('watching this one')));
  got = await notes();
  assert(got.length === 0, 'viewed channel must not ping: ' + JSON.stringify(got));

  // 3. your own message: silent
  await api('/api/v1/channels/' + plaza.id + '/messages', { method: 'POST', token: alice.token, body: { body: 'me too', thread_root_id: root.id } });
  await sleep(1200);
  got = await notes();
  assert(got.length === 0, 'own message must not ping: ' + JSON.stringify(got));

  // 4. five quick replies in a thread you are in -> every reply pings
  for (let i = 1; i <= 5; i++) await say('plaza', 'burst ' + i, { thread_root_id: root.id });
  await settle(() => window.__notes.length >= 5);
  await sleep(1500);
  got = await notes();
  assert(got.length === 5 && got.every((n) => n.why === 'thread' && n.key === root.id && n.sound === false), 'every thread reply must notify silently: ' + JSON.stringify(got));
  // the next reply still pings after the old channel quiet-window duration
  await sleep(3200);
  await say('plaza', 'after the window', { thread_root_id: root.id });
  await settle(() => window.__notes.length >= 1);
  got = await notes();
  assert(got.length === 1 && got[0].key === root.id && got[0].sound === false, 'post-window reply must notify silently once: ' + JSON.stringify(got));

  // 5. mute #plaza from the sidebar menu: channel traffic goes quiet, a mention does not
  const plazaLi = async () => page.evaluateHandle(() => [...document.querySelectorAll('#channel-list li')].find((li) => /plaza/.test(li.textContent)));
  const li = await plazaLi();
  const box = await li.boundingBox();
  await page.mouse.click(box.x + 20, box.y + box.height / 2, { button: 'right' });
  await page.waitForSelector('.ctx-item', { timeout: 5000 });
  await page.evaluate(() => [...document.querySelectorAll('.ctx-item')].find((b) => b.textContent === 'Mute channel').click());
  await page.waitForFunction(() => [...document.querySelectorAll('#channel-list li')].some((li) => /plaza/.test(li.textContent) && li.classList.contains('muted')), { timeout: 5000 });
  const mutedOnServer = (await api('/api/v1/channels', { token: alice.token })).channels.find((c) => c.name === 'plaza').muted;
  assert(mutedOnServer === true, 'mute did not reach the server');
  await say('plaza', 'muted root');
  await sleep(1500);
  got = await notes();
  assert(got.length === 0, 'muted channel must not ping: ' + JSON.stringify(got));
  const plazaGlows = () => page.evaluate(() => [...document.querySelectorAll('#channel-list li')].find((li) => /plaza/.test(li.textContent)).classList.contains('unread'));
  assert(!(await plazaGlows()), 'a muted channel must not glow on a plain message');
  await say('plaza', 'hey @alice still here?');
  await settle(() => window.__notes.length >= 1);
  got = await notes();
  assert(got.length === 1 && got[0].why === 'mention' && got[0].sound === true, 'direct mention in muted channel must ping: ' + JSON.stringify(got));
  assert(await plazaGlows(), 'a mention must still glow a muted channel');
  await sleep(3200); // the mention opened plaza's quiet window; let it close
  await say('plaza', '@channel all hands');
  await settle(() => window.__notes.length >= 1);
  got = await notes();
  assert(got.length === 1 && got[0].why === 'broadcast' && got[0].sound === false, 'broadcast in muted channel must notify silently: ' + JSON.stringify(got));

  // 6. settings: sound off, then notifications off; both persist across a reload
  await openSettings(page, SERVER);
  await page.click('#notify-sound');
  await page.waitForFunction(() => !document.querySelector('#notify-sound').checked, { timeout: 5000 });
  await sleep(300);
  await backToRoom(page);
  await sleep(3200);
  await say('plaza', '@alice silent ping');
  await settle(() => window.__notes.length >= 1);
  got = await notes();
  assert(got.length === 1 && got[0].sound === false, 'sound off must ping silently: ' + JSON.stringify(got));
  await openSettings(page, SERVER);
  await page.click('#notify-enabled');
  await page.waitForFunction(() => !document.querySelector('#notify-enabled').checked, { timeout: 5000 });
  await sleep(300);
  await page.screenshot({ path: (process.env.OUT || 'tmp') + '/notify-settings.png' });
  await backToRoom(page);
  await sleep(3200);
  await say('plaza', '@alice nothing at all');
  await sleep(1500);
  got = await notes();
  assert(got.length === 0, 'notifications off must be silent: ' + JSON.stringify(got));

  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await openSettings(page, SERVER);
  const persisted = await page.evaluate(() => ({
    enabled: document.querySelector('#notify-enabled').checked,
    sound: document.querySelector('#notify-sound').checked,
  }));
  assert(!persisted.enabled && !persisted.sound, 'settings did not persist: ' + JSON.stringify(persisted));
  const prefs = await api('/api/v1/me/notifications', { token: alice.token });
  assert(prefs.enabled === false && prefs.sound === false, 'server prefs: ' + JSON.stringify(prefs));

  await browser.close();
  console.log('NOTIFY_CHECK_OK');
})().catch((e) => { console.error(e); process.exit(1); });
