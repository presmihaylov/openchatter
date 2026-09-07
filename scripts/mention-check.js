require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for dead-mention hardening: the API rejects a handle nobody answers to,
// the UI still lets a human type literal "@text", and mentioning somebody who
// is not in the channel warns the sender instead of vanishing.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/mention-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function call(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  return { status: resp.status, data };
}
async function api(path, opts = {}) {
  const { status, data } = await call(path, opts);
  if (status >= 400) throw new Error(path + ' -> ' + status + ' ' + JSON.stringify(data));
  return data;
}
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };

(async () => {
  const created = await newRoom(SERVER, 'mention check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'mentionbot', description: 't' } });
  const viewer = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'viewer', is_human: true } });
  const abbott = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'abbott', description: 't' } });
  await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'abernathy', description: 't' } });
  await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'abigail', description: 't' } });
  await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'data bot', description: 't' } });
  // joins last on purpose: rank must beat roster order
  await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'abzu', description: 't' } });

  // 1. the roster is fetchable and lists both handles
  const roster = await api('/api/v1/members', { token: bot.token });
  const handles = roster.members.map((m) => m.handle).sort();
  assert(handles.join(',') === 'abbott,abernathy,abigail,abzu,data bot,mentionbot,viewer', 'roster handles: ' + handles);
  assert(roster.members.every((m) => m.dormant === false), 'a fresh member is dormant');

  // 2. an unknown handle is a 422 carrying the roster
  const bad = await call('/api/v1/channels/general/messages', {
    method: 'POST', token: bot.token, body: { body: 'ping @ghost' },
  });
  assert(bad.status === 422, 'unknown mention -> ' + bad.status);
  assert(bad.data.unknown_mentions[0] === 'ghost', 'unknown_mentions: ' + JSON.stringify(bad.data));
  assert(bad.data.members.length === 7, 'the 422 must carry the roster');

  // 3. an out-of-channel mention posts, with a warning
  const priv = await api('/api/v1/channels', { method: 'POST', token: bot.token, body: { name: 'botonly' } });
  const warned = await api('/api/v1/channels/' + priv.id + '/messages', {
    method: 'POST', token: bot.token, body: { body: 'hi @viewer' },
  });
  assert(warned.warnings && /viewer/.test(warned.warnings[0]), 'no warning: ' + JSON.stringify(warned));

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  let alerted = null;
  page.on('dialog', async (d) => { alerted = d.message(); await d.dismiss(); });
    await openAsHuman(page, SERVER, slug, viewer);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });

  // 4. a human typing literal "@text" is never blocked
  await page.type('#composer-input', 'see @ghost-branch for the fix ');
  await page.keyboard.press('Escape');
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => [...document.querySelectorAll('#messages .msg')]
    .some((m) => m.textContent.includes('ghost-branch')), { timeout: 8000 });
  assert(!alerted, 'the UI blocked a literal @text: ' + alerted);

  // 5. the human mentioning an out-of-channel member sees the warning pill
  await api('/api/v1/channels', { method: 'POST', token: viewer.token, body: { name: 'viewer-only' } });
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await page.evaluate(() => [...document.querySelectorAll('#channel-list li')]
    .find((li) => li.textContent.includes('viewer-only')).click());
  await page.waitForFunction(() => document.querySelector('#channel-title').textContent.includes('viewer-only'), { timeout: 8000 });
  // the trailing space dismisses the mention popup, so Enter sends instead of
  // picking a suggestion
  await page.type('#composer-input', 'hey @mentionbot ');
  await page.keyboard.press('Escape');
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => {
    const n = document.getElementById('notice');
    return n && !n.classList.contains('hidden') && /mentionbot/.test(n.textContent);
  }, { timeout: 8000 });

  await page.screenshot({ path: (process.env.OUT || 'tmp') + '/mention-warning.png' });

  // 6. autocomplete ranking: channel members first, then who just talked, and a
  // match on a later word still shows up
  const rank = await api('/api/v1/channels', { method: 'POST', token: viewer.token, body: { name: 'rank-test' } });
  await api('/api/v1/channels/' + rank.id + '/members', { method: 'POST', token: viewer.token, body: { participant: 'abbott' } });
  await api('/api/v1/channels/' + rank.id + '/members', { method: 'POST', token: viewer.token, body: { participant: 'data bot' } });
  await api('/api/v1/channels/' + rank.id + '/members', { method: 'POST', token: viewer.token, body: { participant: 'abzu' } });
  await api('/api/v1/channels/' + rank.id + '/messages', { method: 'POST', token: abbott.token, body: { body: 'abbott speaks here' } });
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await page.evaluate(() => [...document.querySelectorAll('#channel-list li')]
    .find((li) => li.textContent.includes('rank-test')).click());
  await page.waitForFunction(() => document.querySelector('#channel-title').textContent.includes('rank-test'), { timeout: 8000 });
  await page.waitForFunction(() => [...document.querySelectorAll('#messages .msg')]
    .some((m) => m.textContent.includes('abbott speaks here')), { timeout: 8000 });

  const suggest = async (typed) => {
    await page.$eval('#composer-input', (el) => el.__composer.clear());
    await page.focus('#composer-input');
    await page.type('#composer-input', '@' + typed);
    const sel = '.mention-ac:not(.slash-ac):not(.hidden)';
    await page.waitForSelector(sel, { timeout: 5000 });
    return page.$$eval(sel + ' .mention-opt', (ns) => ns.map((n) => ({
      // the row is an avatar image plus the name, so the text is the name alone
      name: n.querySelector('.mention-name').textContent.trim(),
      hint: (n.querySelector('.slash-hint') || {}).textContent || '',
    })));
  };

  const abOpts = await suggest('ab');
  const ab = abOpts.map((o) => o.name);
  await page.screenshot({ path: (process.env.OUT || 'tmp') + '/mention-rank.png' });
  assert(ab[0] === 'abbott', 'the channel member who just spoke is not first: ' + JSON.stringify(ab));
  assert(ab[1] === 'abzu', 'the other channel member is not second: ' + JSON.stringify(ab));
  assert(ab.includes('abernathy') && ab.includes('abigail'), 'non-members vanished: ' + JSON.stringify(ab));
  assert(abOpts[0].hint === '' && /not in channel/.test(abOpts[2].hint),
    'the out-of-channel hint is wrong: ' + JSON.stringify(abOpts));

  // "bot" matches the second word of "data bot"; Slack does the same
  const botMatch = (await suggest('bot')).map((o) => o.name);
  assert(botMatch.includes('data bot'), 'a later-word match was dropped: ' + JSON.stringify(botMatch));
  await page.$eval('#composer-input', (el) => el.__composer.clear());

  // 6. the composer chips a mention as you type, with the same amber/blue split
  //    the feed uses, and leaves an unknown handle as plain text
  await page.$eval('#composer-input', (el) => el.__composer.clear());
  await page.focus('#composer-input');
  await page.type('#composer-input', 'hi @abbott and @viewer and @channel and @ghosty ok');
  await page.keyboard.press('Escape');
  const chips = await page.$$eval('#composer-input .mention',
    (ns) => ns.map((n) => n.textContent + '|' + (n.classList.contains('mention-me') ? 'me' : 'other')));
  assert(chips.length === 3, 'composer chips: ' + JSON.stringify(chips));
  assert(chips[0] === '@abbott|other', 'somebody else must stay blue: ' + JSON.stringify(chips));
  assert(chips[1] === '@viewer|me', 'my own handle must go amber: ' + JSON.stringify(chips));
  assert(chips[2] === '@channel|me', 'a broadcast must go amber: ' + JSON.stringify(chips));
  const typed = await page.$eval('#composer-input', (el) => el.__composer.getPlain
    ? el.__composer.getPlain() : el.textContent);
  assert(/@ghosty ok$/.test(typed), 'decoration changed the text that will be sent: ' + typed);
  await page.$eval('#composer-input', (el) => el.__composer.clear());

  await browser.close();
  console.log('MENTION_CHECK_OK');
})().catch((e) => { console.error('MENTION_CHECK_FAIL:', e.message); process.exit(1); });
