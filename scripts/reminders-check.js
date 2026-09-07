// E2E for task 22's profile reminders. An owned agent sets a one-time and a
// recurring reminder; the owner opens its profile and sees both with the next
// fire; a fire (backdated through the db, then the 5s scheduler tick) flips the
// one-time row to "done" live; the owner deletes the recurring one; an ordinary
// member sees no section and gets 403 from the API.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8090 OPENCHATTER_DB_URL=... node scripts/reminders-check.js
const puppeteer = require('puppeteer-core');
const { execFileSync } = require('child_process');
const { newRoom, enterAs, call } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const OUT = process.env.OUT || 'tmp';
require('fs').mkdirSync(OUT, { recursive: true });
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

const openProfile = async (page, name) => {
  await page.evaluate((n) => {
    const shown = [...document.querySelectorAll('#participant-list li .pname')].some((e) => e.textContent === n);
    if (!shown) document.querySelectorAll('#participant-list li .p-toggle').forEach((t) => t.click());
  }, name);
  await page.waitForFunction((n) =>
    [...document.querySelectorAll('#participant-list li .pname')].some((e) => e.textContent === n), { timeout: 4000 }, name);
  await page.evaluate((n) => {
    const li = [...document.querySelectorAll('#participant-list li')].find((x) => (x.querySelector('.pname') || {}).textContent === n);
    li.click();
  }, name);
  await page.waitForSelector('#profile-modal:not(.hidden)', { timeout: 4000 });
};
const closeProfile = async (page) => {
  await page.evaluate(() => document.querySelector('#profile-close')?.click());
  await page.waitForSelector('#profile-modal.hidden', { timeout: 4000 }).catch(() => {});
};
const rows = (page) => page.$$eval('#profile-reminders .rem-row', (els) => els.map((el) => ({
  id: el.dataset.id, text: el.querySelector('.rem-text').textContent, meta: el.querySelector('.rem-meta').textContent,
})));

(async () => {
  const created = await newRoom(SERVER, 'reminders check');
  const slug = created.room.slug;
  const roomCode = created.invite_code;

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  page.on('pageerror', (e) => { throw new Error('pageerror ' + e.message); });
  page.on('dialog', (d) => d.accept());

  const maya = await enterAs(page, SERVER, slug, roomCode, 'maya');
  const ownerCode = (await call(SERVER, '/api/v1/invites', { method: 'POST', token: maya, headers: { 'X-Workspace-Slug': slug }, body: { bind_owner: true } })).join_url;
  const bot = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite: ownerCode, name: 'agentbot', description: 'agent', avatar: '🤖' } });
  const once = await call(SERVER, '/api/v1/me/reminders', { method: 'POST', token: bot.token, body: { text: 'check whether the deploy finished', schedule: 'in 2h' } });
  const daily = await call(SERVER, '/api/v1/me/reminders', { method: 'POST', token: bot.token, body: { text: 'post the standup summary', schedule: 'every day at 09:00', tz: 'Europe/Sofia' } });
  assert(once.kind === 'once' && daily.kind === 'daily', 'reminder kinds: ' + once.kind + ' ' + daily.kind);

  // 1. the owner sees both, with next fire and "last fired never"
  await openProfile(page, 'agentbot');
  await page.waitForFunction(() => document.querySelectorAll('#profile-reminders .rem-row').length === 2, { timeout: 4000 });
  let list = await rows(page);
  console.log('owner sees:', list);
  const onceRow = list.find((r) => r.id === once.id);
  const dailyRow = list.find((r) => r.id === daily.id);
  assert(onceRow && /in 2h · next /.test(onceRow.meta) && /last fired never/.test(onceRow.meta), 'once row: ' + JSON.stringify(onceRow));
  assert(dailyRow && /every day at 09:00 \(Europe\/Sofia\) · next /.test(dailyRow.meta), 'daily row: ' + JSON.stringify(dailyRow));
  await page.screenshot({ path: OUT + '/reminders-owner.png' });

  // 2. the one-time fires (db backdate + scheduler tick); the open profile follows live
  execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c',
    `UPDATE reminders SET next_fire_at = now() WHERE id = '${once.id}'`], { stdio: ['ignore', 'ignore', 'inherit'] });
  await page.waitForFunction((id) => {
    const row = document.querySelector(`#profile-reminders .rem-row[data-id="${id}"]`);
    return row && / · done · last fired /.test(row.querySelector('.rem-meta').textContent);
  }, { timeout: 15000 }, once.id);
  list = await rows(page);
  console.log('after fire:', list.find((r) => r.id === once.id));
  await page.screenshot({ path: OUT + '/reminders-fired.png' });
  // the agent got the event, addressed to it
  const ev = await call(SERVER, '/api/v1/events?after=0&relevant=true', { token: bot.token });
  const hit = (ev.events || []).find((e) => e.type === 'reminder.fired' && e.payload.reminder_id === once.id);
  assert(hit && hit.payload.text === 'check whether the deploy finished' && hit.payload.next_fire_at === null, 'agent poll missing the fire: ' + JSON.stringify(ev.events));
  const inbox = await call(SERVER, '/api/v1/me/inbox?peek=1', { token: bot.token });
  assert((inbox.events || []).some((e) => e.type === 'reminder.fired'), 'inbox missing the fire');

  // 3. the owner deletes the recurring one from the profile
  await page.evaluate((id) => document.querySelector(`#profile-reminders .rem-row[data-id="${id}"] .rem-delete`).click(), daily.id);
  await page.waitForFunction(() => document.querySelectorAll('#profile-reminders .rem-row').length === 1, { timeout: 4000 });
  const left = await call(SERVER, '/api/v1/me/reminders', { token: bot.token });
  assert(left.reminders.length === 1 && left.reminders[0].id === once.id, 'delete did not reach the server: ' + JSON.stringify(left));
  await page.screenshot({ path: OUT + '/reminders-deleted.png' });
  await closeProfile(page);

  // 4. an ordinary member sees no section, and the API says 403
  const page2 = await browser.newPage();
  await enterAs(page2, SERVER, slug, roomCode, 'viewer');
  await openProfile(page2, 'agentbot');
  await new Promise((r) => setTimeout(r, 800));
  const hidden = await page2.$eval('#profile-reminders', (el) => el.classList.contains('hidden'));
  assert(hidden, 'an ordinary member saw the reminders section');
  await page2.screenshot({ path: OUT + '/reminders-viewer.png' });
  const me2 = await page2.evaluate(() => localStorage.getItem('agentchat:session'));
  const resp = await fetch(SERVER + '/api/v1/participants/' + bot.participant.id + '/reminders', { headers: { Authorization: 'Bearer ' + me2, 'X-Workspace-Slug': slug } });
  console.log('stranger reminders status:', resp.status);
  assert(resp.status === 403, 'stranger expected 403, got ' + resp.status);

  await browser.close();
  console.log('REMINDERS_CHECK_OK');
})().catch((e) => { console.error(e); process.exit(1); });
