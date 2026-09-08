// E2E: Slack-style reply bar under messages with replies — avatars, count,
// last-reply time, click-to-open (with /t/ URL), unread glow.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/replybar-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, enterAs } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

(async () => {
  const created = await newRoom(SERVER, 'replybar check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'barbot', description: 't', avatar: '🤖' } });
  const root = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'root message' } });
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply one', thread_root_id: root.id } });
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply two', thread_root_id: root.id } });

  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await enterAs(page, SERVER, slug, created.invite_code, 'humantester');

  // bar renders with avatars + accent count + muted last-reply time
  await page.waitForSelector('button.reply-bar', { timeout: 8000 });
  const bar = await page.evaluate(() => {
    const b = document.querySelector('button.reply-bar');
    return {
      avatars: b.querySelectorAll('.rb-avatars > *').length,
      count: b.querySelector('.rb-count').textContent,
      last: b.querySelector('.rb-last').textContent,
      // Maya FR: the avatars carry no ring at all (it read as a black hairline)
      avatarBorder: getComputedStyle(b.querySelector('.avatar-rb')).borderTopWidth,
    };
  });
  if (bar.avatarBorder !== '0px') throw new Error('replier avatars must have no border, got ' + bar.avatarBorder);
  if (bar.avatars < 1) throw new Error('no replier avatars: ' + JSON.stringify(bar));
  if (bar.count !== '2 replies') throw new Error('count wrong: ' + JSON.stringify(bar));
  if (bar.last !== 'Last reply just now') throw new Error('last-reply text wrong: ' + JSON.stringify(bar));

  // clicking the bar opens the thread and sets the /t/ segment
  await page.click('button.reply-bar');
  await page.waitForFunction(() => !document.querySelector('#thread-panel').classList.contains('hidden'), { timeout: 8000 });
  await page.waitForFunction((want) => location.pathname === want, { timeout: 8000 },
    '/r/' + slug + '/c/general/t/' + root.id);

  // human replies (joins the thread), then a fresh bot reply makes it unread -> glow
  await page.type('#thread-input', 'human reply');
  await page.keyboard.press('Enter');
  await page.waitForFunction(() =>
    document.querySelector('#thread-messages').textContent.includes('human reply'), { timeout: 8000 });
  // Maya FR: a reply that lands while the thread is open is read, so the bar
  // in the channel view must not glow, not now and not after a reload
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply while open', thread_root_id: root.id } });
  await page.waitForFunction(() =>
    document.querySelector('#thread-messages').textContent.includes('reply while open'), { timeout: 8000 });
  await new Promise((r) => setTimeout(r, 1500));
  if (await page.evaluate(() => document.querySelector('button.reply-bar').classList.contains('unread'))) {
    throw new Error('bar glows for a reply read in the open thread');
  }
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('button.reply-bar', { timeout: 8000 });
  await new Promise((r) => setTimeout(r, 1000));
  if (await page.evaluate(() => document.querySelector('button.reply-bar').classList.contains('unread'))) {
    throw new Error('bar glows after reload: the open-thread reply was never marked read server-side');
  }
  await page.waitForFunction(() => !document.querySelector('#thread-panel').classList.contains('hidden'), { timeout: 8000 });
  await page.evaluate(() => document.querySelector('#thread-close').click());
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'late reply', thread_root_id: root.id } });
  await page.waitForFunction(() =>
    document.querySelector('button.reply-bar')?.classList.contains('unread'), { timeout: 10000 });

  await browser.close();
  console.log('REPLYBAR_CHECK_OK');
})().catch((e) => { console.error('REPLYBAR_CHECK_FAIL:', e.message); process.exit(1); });
