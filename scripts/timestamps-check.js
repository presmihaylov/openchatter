// Browser E2E for Slack-style relative message timestamps. It backdates a
// small disposable workspace, checks channel/thread/search rendering and the
// exact hover text, then waits through a live minute boundary without a
// message refetch. Node and Chrome use the same fixed viewer timezone.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/timestamps-check.js
process.env.TZ = 'Pacific/Kiritimati';
const { execFileSync } = require('child_process');
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const ACCESS_ID = process.env.ACCESS_ID || '';
const ACCESS_SECRET = process.env.ACCESS_SECRET || '';
const access = ACCESS_ID ? {
  'CF-Access-Client-Id': ACCESS_ID,
  'CF-Access-Client-Secret': ACCESS_SECRET,
} : {};
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
const tag = () => Date.now().toString(36).slice(-7) + Math.floor(Math.random() * 1e6).toString(36);

async function request(path, opts = {}, retries = 3) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, access);
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 429 && retries > 0) {
    await sleep(3000);
    return request(path, opts, retries - 1);
  }
  return { status: resp.status, data };
}

async function must(path, opts = {}, status = 200) {
  const out = await request(path, opts);
  if (out.status !== status) {
    throw new Error(path + ' -> ' + out.status + ' ' + (out.data.error || out.data.code || 'unexpected response'));
  }
  return out.data;
}

const sql = (query) => execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c', query], {
  stdio: ['ignore', 'ignore', 'inherit'],
});
const backdate = (message, date) => {
  assert(/^[0-9a-f-]{36}$/.test(message.id), 'unsafe message id ' + message.id);
  sql(`UPDATE messages SET created_at = '${date.toISOString()}' WHERE id = '${message.id}'`);
};

(async () => {
  let browser;
  let ownsBrowser = false;
  let humanSession;
  let slug;
  let roomName;
  try {
    const run = tag();
    const registered = await must('/api/v1/auth/password/register', {
      method: 'POST',
      body: { username: 'timestamps-' + run, password: 'correct horse battery', display_name: 'Timestamp Reader' },
    }, 201);
    humanSession = registered.token;
    roomName = 'timestamps check ' + run;
    const made = await must('/api/v1/rooms', {
      method: 'POST', token: humanSession,
      body: { name: roomName, slug: 'timestamps-' + run },
    }, 201);
    slug = made.room.slug;
    const invite = await must('/api/v1/invites', {
      method: 'POST', token: humanSession, slug, body: { bind_owner: true },
    }, 201);
    const joined = await must('/api/v1/rooms/join', {
      method: 'POST', body: { invite: invite.join_url, name: 'timebot-' + run, description: 'timestamp test agent' },
    }, 201);
    const agentToken = joined.token;
    const general = (await must('/api/v1/channels', { token: humanSession, slug })).channels
      .find((channel) => channel.name === 'general');
    const post = (body, extra = {}) => must('/api/v1/channels/' + general.id + '/messages', {
      method: 'POST', token: agentToken, slug, body: Object.assign({ body }, extra),
    }, 201);

    const recent = await post('timestamp-probe thirty seconds');
    const minutes = await post('timestamp-probe twenty six minutes');
    const hours = await post('timestamp-probe three hours');
    const yesterday = await post('timestamp-probe yesterday');
    const lastWeek = await post('timestamp-probe last week');
    const lastYear = await post('timestamp-probe last year');
    const threadReply = await post('timestamp-probe thread reply', { thread_root_id: hours.id });

    const now = new Date();
    const at = {
      recent: new Date(now.getTime() - 30 * 1000),
      minutes: new Date(now.getTime() - 26 * 60 * 1000),
      hours: new Date(now.getTime() - 3 * 60 * 60 * 1000),
      yesterday: new Date(now),
      lastWeek: new Date(now),
      lastYear: new Date(now),
      threadReply: new Date(now.getTime() - 26 * 60 * 1000),
    };
    at.yesterday.setDate(now.getDate() - 1);
    at.yesterday.setHours(0, 0, 0, 0);
    at.lastWeek.setDate(now.getDate() - 7);
    at.lastWeek.setHours(10, 32, 0, 0);
    at.lastYear.setFullYear(now.getFullYear() - 1);
    at.lastYear.setHours(10, 32, 0, 0);
    for (const [key, message] of Object.entries({ recent, minutes, hours, yesterday, lastWeek, lastYear, threadReply })) {
      backdate(message, at[key]);
    }

    if (process.env.BROWSER_URL) {
      browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    } else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
      });
      ownsBrowser = true;
    }
    const page = await browser.newPage();
    await page.emulateTimezone(process.env.TZ);
    await page.setViewport({ width: 1280, height: 850 });
    await page.setExtraHTTPHeaders(access);
    await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), humanSession);
    await page.goto(SERVER + '/w/' + slug, { waitUntil: 'networkidle2' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForFunction((id) => document.querySelector(`#messages .msg[data-id="${id}"]`), { timeout: 10000 }, recent.id);

    const shownTime = (id, box = '#messages') => page.$eval(`${box} .msg[data-id="${id}"] time.message-time`, (el) => ({
      text: el.textContent, title: el.title, datetime: el.dateTime,
    }));
    const expectedExact = (date) => page.evaluate((iso) => new Date(iso).toLocaleString([], {
      weekday: 'long', year: 'numeric', month: 'long', day: 'numeric',
      hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short',
    }), date.toISOString());

    const initial = {
      recent: await shownTime(recent.id),
      minutes: await shownTime(minutes.id),
      hours: await shownTime(hours.id),
      yesterday: await shownTime(yesterday.id),
      lastWeek: await shownTime(lastWeek.id),
      lastYear: await shownTime(lastYear.id),
    };
    assert(initial.recent.text === 'just now', '30 seconds: ' + JSON.stringify(initial.recent));
    assert(initial.minutes.text === '26 minutes ago', '26 minutes: ' + JSON.stringify(initial.minutes));
    assert(initial.hours.text === '3 hours ago', '3 hours: ' + JSON.stringify(initial.hours));
    assert(initial.yesterday.text.startsWith('Yesterday at '), 'yesterday: ' + JSON.stringify(initial.yesterday));
    assert(!/ago|Yesterday/.test(initial.lastWeek.text) && initial.lastWeek.text.includes(' at '), 'last week: ' + JSON.stringify(initial.lastWeek));
    assert(initial.lastYear.text.includes(String(at.lastYear.getFullYear())) && initial.lastYear.text.includes(' at '), 'last year: ' + JSON.stringify(initial.lastYear));
    for (const [key, value] of Object.entries(initial)) {
      assert(Math.abs(Date.parse(value.datetime) - at[key].getTime()) < 1000, key + ' datetime: ' + JSON.stringify(value));
      assert(value.title === await expectedExact(at[key]), key + ' hover: ' + JSON.stringify(value));
    }

    // A reply footer and both message headers in the thread use the same rule.
    const footer = await page.$eval(`#messages .msg[data-id="${hours.id}"] .rb-last time`, (el) => el.textContent);
    assert(footer === '26 minutes ago', 'reply footer: ' + footer);
    await page.click(`#messages .msg[data-id="${hours.id}"] .reply-bar`);
    await page.waitForSelector('#thread-panel:not(.hidden)', { timeout: 10000 });
    assert((await shownTime(hours.id, '#thread-messages')).text === '3 hours ago', 'thread root time');
    assert((await shownTime(threadReply.id, '#thread-messages')).text === '26 minutes ago', 'thread reply time');

    // Search rows are message rows too, including their exact hover text.
    await page.click('#open-search');
    await page.type('#search-input', 'timestamp-probe');
    await page.waitForSelector('.search-more', { timeout: 10000 });
    await page.click('.search-more');
    await page.waitForFunction(() => document.querySelectorAll('.search-hit-row').length >= 7, { timeout: 10000 });
    const searchTimes = await page.$$eval('.search-hit-row', (rows) => rows.map((row) => ({
      body: row.querySelector('.sh-snippet').textContent,
      text: row.querySelector('time.sh-time').textContent,
      title: row.querySelector('time.sh-time').title,
    })));
    const searchRecent = searchTimes.find((row) => row.body.includes('thirty seconds'));
    assert(searchRecent && searchRecent.text === 'just now' && searchRecent.title === initial.recent.title,
      'search recent time: ' + JSON.stringify(searchRecent));

    // Wait a real minute. The 30-second message crosses its boundary on the
    // 30-second timer, while the channel message resource count stays fixed.
    const messageFetches = () => page.evaluate(() => performance.getEntriesByType('resource')
      .filter((entry) => /\/channels\/[^/]+\/messages(?:\?|$)/.test(entry.name)).length);
    const before = await messageFetches();
    await sleep(65000);
    const ticked = await shownTime(recent.id);
    assert(ticked.text === '1 minute ago', 'live tick after one minute: ' + JSON.stringify(ticked));
    assert(await messageFetches() === before, 'relative-time tick refetched channel messages');

    console.log('TIMESTAMPS_CHECK_OK');
  } finally {
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (humanSession && slug && roomName) {
      await request('/api/v1/room', { method: 'DELETE', token: humanSession, slug, body: { name: roomName } });
    }
  }
})().catch((err) => {
  console.error('TIMESTAMPS_CHECK_FAIL:', err.message);
  process.exit(1);
});
