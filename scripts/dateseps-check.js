require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for the Slack-style date separators: one marker before the first message
// of each local calendar day, in the feed and in a thread; sticky at the top of
// the scroll box; the unread divider still lands where it did; a live message
// on an already-open day adds no marker.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/dateseps-check.js
// Node and the page share one pinned zone, far from UTC and without DST, so a
// message at 23:30 local yesterday and one at 00:30 local today fall on the
// same UTC date: an implementation that split days in UTC collapses that pair.
process.env.TZ = 'Pacific/Kiritimati';
const TZ = process.env.TZ;
const puppeteer = require('puppeteer-core');
const path = require('path');
const os = require('os');
const { execFileSync } = require('child_process');
const { newRoom, enterAs, sleep } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const OUT = process.env.OUT || os.tmpdir();

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
const sql = (q) => execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c', q], { stdio: ['ignore', 'ignore', 'inherit'] });
const backdate = (id, d) => sql(`UPDATE messages SET created_at = '${d.toISOString()}' WHERE id = '${id}'`);

// local noon N days back: safe from DST and midnight edge cases
const noonAgo = (n) => { const d = new Date(); d.setHours(12, 0, 0, 0); d.setDate(d.getDate() - n); return d; };
// 23:30 local yesterday and 00:30 local today (or a minute ago, right after midnight)
const lateYesterday = () => { const d = noonAgo(1); d.setHours(23, 30, 0, 0); return d; };
const earlyToday = () => { const d = noonAgo(0); d.setHours(0, 30, 0, 0); return new Date(Math.min(d.getTime(), Date.now() - 60000)); };

// the sequence of divider/message rows in a box, top to bottom
const rows = (sel) => `[...document.querySelector(${JSON.stringify(sel)}).children].map((n) =>
  n.classList.contains('date-divider') ? 'day:' + n.textContent.trim()
  : n.classList.contains('unread-divider') ? 'unread'
  : n.classList.contains('system-entry') ? 'sys' : 'msg:' + n.dataset.id)`;

(async () => {
  const created = await newRoom(SERVER, 'dateseps check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'daybot', description: 't', avatar: '📅' } });

  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('dialog', (d) => d.dismiss());
  await page.emulateTimezone(TZ);
  await page.setViewport({ width: 1200, height: 520 });
  await enterAs(page, SERVER, slug, created.invite_code, 'reader');
  await page.waitForFunction(() => document.querySelector('#channel-title').textContent.includes('general'), { timeout: 8000 });
  await sleep(800); // let the visit mark #general read
  // the viewer leaves, so the bot's posts below stay unread
  await page.goto('about:blank');
  await sleep(500);

  // #days gets a membership entry when the reader is added: a system message
  // that must open the oldest day like any other message
  await api('/api/v1/channels', { method: 'POST', token: bot.token, body: { name: 'days' } });
  await api('/api/v1/channels/days/members', { method: 'POST', token: bot.token, body: { participant: 'reader' } });
  const post = (body, extra = {}) => api('/api/v1/channels/days/messages', { method: 'POST', token: bot.token, body: Object.assign({ body }, extra) });
  const m1 = await post('four hundred days ago');
  const tall = (s) => s + '\n' + Array.from({ length: 30 }, (_, i) => 'line ' + (i + 1)).join('\n');
  const m2 = await post(tall('three days ago')); // tall, so its thread box scrolls
  const m3 = await post('yesterday');
  // tall, so the today group overflows the box and the marker has to stick
  const m4 = await post(tall('today one'));
  const m5 = await post('today two');
  const d400 = noonAgo(400), d3 = noonAgo(3), d1 = lateYesterday(), d0 = earlyToday();
  assert(d1.toISOString().slice(0, 10) === d0.toISOString().slice(0, 10), 'yesterday/today pair must share a UTC date: ' + d1.toISOString() + ' ' + d0.toISOString());
  backdate(m1.id, d400);
  backdate(m2.id, d3);
  backdate(m3.id, d1);
  backdate(m4.id, d0);
  // the reader joined just now, after the backdated 00:30 message; a read
  // marker at 00:29 keeps the unread divider ahead of today's first message
  sql(`INSERT INTO channel_reads (participant_id, channel_id, last_read_at)
    SELECT p.id, c.id, '${new Date(d0.getTime() - 60000).toISOString()}' FROM participants p, channels c
    WHERE p.room_id = '${created.room.id}' AND p.name = 'reader' AND c.room_id = p.room_id AND c.name = 'days'`);
  // the two join entries open the oldest day: system messages count for the boundary
  sql(`UPDATE messages SET created_at = '${new Date(d400.getTime() - 60000).toISOString()}' WHERE room_id = '${created.room.id}' AND kind = 'system'`);

  // expected labels, in the browser's own locale
  const labelOf = (d) => page.evaluate((ms) => {
    const d = new Date(ms); const now = new Date();
    if (d.getFullYear() !== now.getFullYear()) return d.getDate() + ' ' + d.toLocaleDateString([], { month: 'short' }) + ' ' + d.getFullYear();
    return d.toLocaleDateString([], { weekday: 'long' }) + ', ' + d.getDate() + ' ' + d.toLocaleDateString([], { month: 'long' });
  }, d.getTime());

  await page.goto(SERVER + '/r/' + slug + '/c/days', { waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => [...document.querySelectorAll('#messages .msg')].some((n) => n.dataset.id === id), { timeout: 8000 }, m5.id);
  const l400 = await labelOf(d400), l3 = await labelOf(d3);
  const want = ['day:' + l400, 'sys', 'msg:' + m1.id, 'day:' + l3, 'msg:' + m2.id, 'day:Yesterday', 'msg:' + m3.id, 'unread', 'day:Today', 'msg:' + m4.id, 'msg:' + m5.id];
  // one or more membership entries share the oldest day; fold them into one
  const fold = (r) => r.filter((x, i) => x !== 'sys' || r[i - 1] !== 'sys');
  const got = fold(await page.evaluate(rows('#messages')));
  assert(JSON.stringify(got) === JSON.stringify(want), 'feed rows: ' + JSON.stringify(got) + ' want ' + JSON.stringify(want));
  console.log('feed: 4 day markers in order, unread divider before today: OK');

  // sticky: at the bottom the Today marker is pinned to the box top; the
  // older markers, scrolled past, pile up there too but stay hidden
  const stuck = await page.evaluate(() => {
    const box = document.querySelector('#messages');
    const limit = box.getBoundingClientRect().top + parseFloat(getComputedStyle(box).paddingTop);
    const divs = [...document.querySelectorAll('#messages .date-divider')];
    return {
      scrolled: box.scrollTop > 0,
      stuck: divs.filter((d) => d.classList.contains('stuck')).map((d) => d.textContent.trim()),
      gap: Math.abs(divs[3].getBoundingClientRect().top - limit),
      hidden: divs.slice(0, 3).every((d) => getComputedStyle(d).visibility === 'hidden'),
      shown: getComputedStyle(divs[3]).visibility === 'visible',
    };
  });
  assert(stuck.scrolled && stuck.stuck.join() === 'Today' && stuck.gap < 2 && stuck.hidden && stuck.shown, 'sticky marker: ' + JSON.stringify(stuck));
  await page.screenshot({ path: path.join(OUT, 'dateseps-feed-stuck.png') });
  await page.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
  await sleep(200);
  // the separator is subtle: no border, 11px, weight <= 500
  const look = await page.$eval('#messages .date-divider:not(.stuck) span', (el) => { const c = getComputedStyle(el); return { border: c.borderTopWidth, bg: c.backgroundColor, size: c.fontSize, weight: c.fontWeight }; });
  if (!(look.border === '0px' && look.bg === 'rgba(0, 0, 0, 0)' && look.size === '11px' && Number(look.weight) <= 500)) throw new Error('separator look: ' + JSON.stringify(look));
  const atTop = await page.evaluate(() => [...document.querySelectorAll('#messages .date-divider')].map((d) => d.className + ':' + getComputedStyle(d).visibility));
  assert(atTop.every((c) => c === 'date-divider:visible'), 'markers at scrollTop 0: ' + JSON.stringify(atTop));
  await page.screenshot({ path: path.join(OUT, 'dateseps-feed-top.png') });
  console.log('sticky marker pinned while scrolled, free at the top: OK');

  // live append on an open day: no new marker, the unread divider stays
  const m6 = await post('live today');
  await page.waitForFunction((id) => [...document.querySelectorAll('#messages .msg')].some((n) => n.dataset.id === id), { timeout: 8000 }, m6.id);
  const live = fold(await page.evaluate(rows('#messages')));
  assert(JSON.stringify(live) === JSON.stringify(want.concat(['msg:' + m6.id])), 'live rows: ' + JSON.stringify(live));
  console.log('live message today adds no marker: OK');

  // the visit marked the channel read: a reload shows the same markers, no unread divider
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => [...document.querySelectorAll('#messages .msg')].some((n) => n.dataset.id === id), { timeout: 8000 }, m6.id);
  const after = fold(await page.evaluate(rows('#messages')));
  assert(JSON.stringify(after) === JSON.stringify(want.concat(['msg:' + m6.id]).filter((r) => r !== 'unread')), 'reload rows: ' + JSON.stringify(after));
  console.log('unread divider gone after read, day markers kept: OK');

  // thread: root three days ago, no replies yet -> one marker
  await page.goto(SERVER + '/r/' + slug + '/c/days/t/' + m2.id, { waitUntil: 'networkidle2' });
  await page.waitForFunction((id) => [...document.querySelectorAll('#thread-messages .msg')].some((n) => n.dataset.id === id), { timeout: 8000 }, m2.id);
  assert(JSON.stringify(await page.evaluate(rows('#thread-messages'))) === JSON.stringify(['day:' + l3, 'msg:' + m2.id]), 'thread rows before reply');

  // a failed send: the optimistic reply opens Today, the rollback must take
  // that marker away again and leave the scroll handler healthy
  const abortPost = (req) => { if (req.method() === 'POST' && req.url().includes('/messages')) req.abort(); else req.continue(); };
  await page.setRequestInterception(true);
  page.on('request', abortPost);
  await page.click('#thread-input');
  await page.type('#thread-input', 'lost reply');
  await page.keyboard.press('Enter');
  await sleep(1000);
  const afterFail = await page.evaluate(rows('#thread-messages'));
  assert(JSON.stringify(afterFail) === JSON.stringify(['day:' + l3, 'msg:' + m2.id]), 'rows after a failed send: ' + JSON.stringify(afterFail));
  const scrolled = await page.evaluate(() => {
    const box = document.querySelector('#thread-messages');
    box.scrollTop = box.scrollHeight; box.dispatchEvent(new Event('scroll'));
    box.scrollTop = 0; box.dispatchEvent(new Event('scroll'));
    return box.scrollHeight > box.clientHeight;
  });
  await sleep(200);
  assert(scrolled, 'thread box must scroll for the stuck-marker pass to run');
  assert(errors.length === 0, 'page errors after a failed send: ' + JSON.stringify(errors));
  page.off('request', abortPost);
  await page.setRequestInterception(false);
  console.log('failed send leaves no orphan marker, scroll handler clean: OK');

  // reply today -> two markers
  const reply = await post('reply today', { thread_root_id: m2.id });
  await page.waitForFunction((id) => [...document.querySelectorAll('#thread-messages .msg')].some((n) => n.dataset.id === id), { timeout: 8000 }, reply.id);
  const thread = await page.evaluate(rows('#thread-messages'));
  assert(JSON.stringify(thread) === JSON.stringify(['day:' + l3, 'msg:' + m2.id, 'day:Today', 'msg:' + reply.id]), 'thread rows: ' + JSON.stringify(thread));
  await page.screenshot({ path: path.join(OUT, 'dateseps-thread.png') });
  console.log('thread: root day + Today markers: OK');

  assert(errors.length === 0, 'page errors: ' + JSON.stringify(errors));
  await browser.close();
  console.log('DATESEPS_CHECK_OK');
})().catch((e) => { console.error('DATESEPS_CHECK_FAIL:', e.message); process.exit(1); });
