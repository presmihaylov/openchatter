// An empty own row exposes Add an agent without a click and stays expanded
// when its first agent joins. Other humans never expose the add row.
const fs = require('fs');
const path = require('path');
const puppeteer = require('puppeteer-core');
const { call, newRoom, enterAs, loginPage, uniqUser } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
let step = 'setup';
const shot = (page, name) => page.screenshot({ path: path.join(OUT, 'addagent-' + name) });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// the tree as [{name, kids:[...]}] plus whether the add row sits right after that human's kids
const tree = (page) => page.$$eval('#participant-list > li', (lis) => {
  const out = []; let cur = null;
  for (const li of lis) {
    if (li.id === 'addagent-row') { if (cur) cur.addRow = true; continue; }
    if (li.classList.contains('offline-toggle') || li.classList.contains('group-label')) continue;
    const name = (li.querySelector('.pname') || {}).textContent || '';
    if (li.classList.contains('participant-leaf')) { if (cur) cur.kids.push(name); continue; }
    cur = { name, kids: [], addRow: false, chevron: (li.querySelector('.p-toggle') || {}).dataset ? li.querySelector('.p-toggle').dataset.state : '' };
    out.push(cur);
  }
  return out;
});
const expand = async (page, name) => {
  await page.evaluate((n) => {
    const li = [...document.querySelectorAll('#participant-list > li')].find((l) => (l.querySelector('.pname') || {}).textContent === n);
    const t = li && li.querySelector('.p-toggle');
    if (t && t.dataset.state === 'collapsed') t.click();
  }, name);
  await sleep(300);
};

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error' && !/status of 403/.test(m.text())) errors.push('console: ' + m.text()); });

  const room = await newRoom(SERVER, 'addagent-check');
  // Alice (admin) owns one agent already, so her row expands on Bob's screen
  // the first human in after newRoom is the admin
  const aliceSession = await enterAs(page, SERVER, room.room.slug, room.invite, 'Alice Admin');
  const aliceLink = await call(SERVER, '/api/v1/invites', { method: 'POST', token: aliceSession, headers: { 'X-Workspace-Slug': room.room.slug }, body: { bind_owner: true } });
  await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite: aliceLink.join_url, name: 'alicebot', description: 'Alice agent' } });
  await sleep(600);

  step = '1';
  // 1. the admin sees the row under herself, after alicebot
  await expand(page, 'Alice Admin');
  let t = await tree(page);
  let alice = t.find((h) => h.name === 'Alice Admin');
  assert(alice && alice.kids.includes('alicebot') && alice.addRow, 'admin has no Add an agent row: ' + JSON.stringify(t));

  step = '2';
  // 2. Bob (plain member) with no agents: his own row needs no click, it shows
  // the add row straight away and has no chevron to hide it behind
  const ctxB = await browser.createBrowserContext();
  const bob = await ctxB.newPage();
  await bob.setViewport({ width: 1280, height: 800 });
  bob.on('pageerror', (e) => errors.push('pageerror B: ' + e.message));
  await enterAs(bob, SERVER, room.room.slug, room.invite, 'Bob Member');
  await bob.waitForSelector('#participant-list li', { timeout: 8000 });
  await sleep(400);
  t = await tree(bob);
  let me = t.find((h) => h.name === 'Bob Member');
  assert(me && me.kids.length === 0 && me.addRow, 'own row without agents hides the add row: ' + JSON.stringify(t));
  assert(me.chevron === '', 'own row without agents has a chevron: ' + JSON.stringify(t));
  assert(await bob.$eval('#addagent-row', (el) => el.getBoundingClientRect().height > 0), 'the add row is not visible');
  // another human's row: agents, no add row
  await expand(bob, 'Alice Admin');
  t = await tree(bob);
  alice = t.find((h) => h.name === 'Alice Admin');
  assert(alice && alice.kids.includes('alicebot') && !alice.addRow, 'another human shows the add row: ' + JSON.stringify(t));
  await shot(bob, 'row.png');

  step = '3';
  // 3. click: the modal mints a link bound to Bob and shows the instructions
  await bob.click('#addagent-row');
  await bob.waitForSelector('#addagent-modal:not(.hidden)', { timeout: 5000 });
  await bob.waitForFunction(() => document.getElementById('addagent-text').value.includes('/join/inv-'), { timeout: 8000 });
  const text = await bob.$eval('#addagent-text', (el) => el.value);
  const link = (text.match(/(\S+\/join\/inv-\S+)/) || [])[1];
  assert(link && link.startsWith(SERVER + '/join/inv-'), 'instructions carry no link: ' + text);
  assert(text.includes(SERVER + '/skill') && text.includes('#general') && text.includes('Bob Member'), 'instructions: ' + text);
  // redesign: no separate link field, wide card, the whole text visible without
  // scrolling, no resize handle, one primary Copy instructions action
  assert(!(await bob.$('#addagent-link')), 'the old link field is back');
  const box = await bob.evaluate(() => {
    const ta = document.getElementById('addagent-text');
    const cs = getComputedStyle(ta);
    return { width: document.getElementById('addagent-card').getBoundingClientRect().width, scroll: ta.scrollHeight, client: ta.clientHeight, resize: cs.resize, mono: cs.fontFamily,
      subtitle: document.querySelector('#addagent-card .hint').textContent, focused: document.activeElement === document.getElementById('addagent-text-copy') };
  });
  assert(box.width >= 640 && box.width <= 720, 'card width: ' + box.width);
  assert(box.scroll <= box.client + 2, 'instructions need scrolling: ' + box.scroll + ' > ' + box.client);
  assert(box.resize === 'none' && /mono/i.test(box.mono), 'textarea chrome: ' + JSON.stringify(box));
  assert(/binds the agent to you/.test(box.subtitle) && /7 days/.test(box.subtitle), 'subtitle: ' + box.subtitle);
  assert(box.focused, 'Copy instructions is not focused on open');
  await shot(bob, 'modal.png');
  await ctxB.overridePermissions(SERVER, ['clipboard-read', 'clipboard-write']);
  await bob.click('#addagent-text-copy');
  await bob.waitForFunction(() => /Copied|copy failed/.test(document.getElementById('addagent-text-copy').textContent), { timeout: 3000 });
  assert(/Copied/.test(await bob.$eval('#addagent-text-copy', (b) => b.textContent)), 'copy did not confirm');
  const clip = await bob.evaluate(() => navigator.clipboard.readText());
  assert(clip.includes(link) && clip === text, 'clipboard lacks the instructions with the link');

  step = '4';
  // 4. an agent joins on the link: it is Bob's, and it nests under him above the
  // add row, in view without a click (the empty row counted as expanded)
  const joined = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite: link, name: 'bobbot', description: 'Bob agent' } });
  const bobId = await bob.evaluate(() => fetch('/api/v1/me', { headers: { Authorization: 'Bearer ' + localStorage.getItem('agentchat:session'), 'X-Workspace-Slug': location.pathname.split('/')[2] } }).then((r) => r.json()).then((m) => m.id));
  assert(joined.participant.owner_id === bobId, 'joined agent owner: ' + JSON.stringify(joined.participant) + ' bob=' + bobId);
  await bob.keyboard.press('Escape');
  await bob.waitForSelector('#addagent-modal.hidden', { timeout: 3000 });
  await bob.waitForFunction(() => [...document.querySelectorAll('#participant-list .pname')].some((e) => e.textContent === 'bobbot'), { timeout: 8000 });
  await sleep(300);
  t = await tree(bob);
  me = t.find((h) => h.name === 'Bob Member');
  assert(me && me.kids.includes('bobbot') && me.addRow && me.chevron === 'open', 'bobbot not under Bob with the add row after: ' + JSON.stringify(t));
  await shot(bob, 'joined.png');
  // a member's unbound mint stays refused
  const bobSession = await bob.evaluate(() => localStorage.getItem('agentchat:session'));
  let refused = false;
  try { await call(SERVER, '/api/v1/invites', { method: 'POST', token: bobSession, headers: { 'X-Workspace-Slug': room.room.slug }, body: {} }); } catch (e) { refused = /403/.test(e.message); }
  assert(refused, 'a member minted an unbound link');

  step = '5';
  // 5. a person who opens the bound link is turned away: it is an agent's key
  const ctxC = await browser.createBrowserContext();
  const carol = await ctxC.newPage();
  await carol.setViewport({ width: 1280, height: 800 });
  await loginPage(carol, SERVER, uniqUser(), { displayName: 'Carol Curious' });
  await carol.goto(link, { waitUntil: 'networkidle2' });
  await carol.waitForSelector('#join-view:not(.hidden)', { timeout: 8000 });
  await carol.waitForFunction(() => /for an agent/.test(document.getElementById('join-msg').textContent), { timeout: 8000 });
  await shot(carol, 'agents-only.png');
  assert(!(await carol.$('#chat-view:not(.hidden)')), 'a human entered on a bound link');

  await browser.close();
  if (errors.length) throw new Error('page errors: ' + errors.join(' | '));
  console.log('ADDAGENT_CHECK_OK');
})().catch((e) => {
  console.log('ADDAGENT_CHECK_FAIL: ' + e.message + ' (step ' + step + ')');
  process.exit(1);
});
