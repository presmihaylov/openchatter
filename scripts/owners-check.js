// E2E for agent owners (task 19). In workspace settings an admin sees humans
// only, with each one's agents folded under it; can move an agent to another
// human; removing a human asks a confirm that names their agents and revokes
// them; a legacy ownerless agent shows under "Unowned agents" until an owner
// is picked; and the creator can neither be removed nor leave (409).
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 OUT=<dir> node scripts/owners-check.js
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const puppeteer = require('puppeteer-core');
const { call, createRoom, registerAndLogin, loginPage, openWorkspace, openSettings, uniqUser } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
let step = 'setup';
let failPage = null;
const shot = (page, name) => page.screenshot({ path: path.join(OUT, 'owners-' + name) });
const rows = (page) => page.$$eval('#ws-member-list > li', (els) => els.map((li) => ({
  id: li.dataset.id, name: li.querySelector('.member-head .member-name').textContent, role: (li.querySelector('.member-role') || {}).textContent,
  remove: !!li.querySelector('.member-head > .member-remove'), fold: (li.querySelector('.member-fold') || {}).textContent,
  unowned: li.classList.contains('member-unowned'),
  agents: [...li.querySelectorAll('.member-agent')].map((a) => ({ id: a.dataset.id, name: a.querySelector('.member-name').textContent, owner: a.querySelector('.member-owner').value, remove: !!a.querySelector('.member-remove') })),
})));
const status = async (url, token) => (await fetch(SERVER + url, { headers: { Authorization: 'Bearer ' + token } })).status;
const del = async (url, token, slug) => {
  const r = await fetch(SERVER + url, { method: 'DELETE', headers: { Authorization: 'Bearer ' + token, 'X-Workspace-Slug': slug } });
  return { status: r.status, body: await r.json().catch(() => ({})) };
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
  failPage = page;
  await page.setViewport({ width: 1280, height: 900 });
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
  const dialogs = [];
  page.on('dialog', (d) => { dialogs.push(d.message()); d.accept(); });

  // Cara creates the room (her row stays, so she is the creator); Ada is an
  // admin driving the page; Jill is a plain human with one bound-link agent;
  // crewbot joined with the plain code and so belongs to Cara.
  const caraSession = await registerAndLogin(SERVER, uniqUser(), 'Cara Creator');
  const made = await createRoom(SERVER, caraSession, 'crew');
  const slug = made.room.slug;
  const ws = { 'X-Workspace-Slug': slug };
  const adaSession = await loginPage(page, SERVER, uniqUser(), { displayName: 'Ada Admin' });
  const ada = await call(SERVER, '/api/v1/workspaces/' + slug + '/enter', { method: 'POST', token: adaSession, body: { invite_code: made.invite_code } });
  await call(SERVER, '/api/v1/participants/' + ada.participant.id + '/role', { method: 'POST', token: caraSession, headers: ws, body: { role: 'admin' } });
  const jillSession = await registerAndLogin(SERVER, uniqUser(), 'Jill Human');
  const jill = await call(SERVER, '/api/v1/workspaces/' + slug + '/enter', { method: 'POST', token: jillSession, body: { invite_code: made.invite_code } });
  const jillLink = (await call(SERVER, '/api/v1/invites', { method: 'POST', token: jillSession, headers: ws, body: { bind_owner: true } })).join_url;
  const jillbot = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite: jillLink, name: 'jillbot', description: 'Jill\'s agent' } });
  const crewbot = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite_code: made.invite_code, name: 'crewbot', description: 'joined with the plain code' } });
  const cara = (await call(SERVER, '/api/v1/me', { token: caraSession, headers: ws }));
  assert(jillbot.participant.owner_id === jill.participant.id && crewbot.participant.owner_id === cara.id, 'owners at join: ' + JSON.stringify({ jillbot: jillbot.participant.owner_id, crewbot: crewbot.participant.owner_id }));

  step = '1';
  // 1. humans only on top; the creator has no Remove; agents are folded under their owner
  await openWorkspace(page, SERVER, adaSession, slug);
  await openSettings(page, SERVER, 'workspace');
  await page.waitForSelector('#ws-members:not(.hidden)', { timeout: 8000 });
  await page.waitForFunction(() => document.querySelectorAll('#ws-member-list > li').length === 3, { timeout: 8000 });
  let list = await rows(page);
  const byName = (n) => list.find((r) => r.name === n);
  assert(byName('Cara Creator').role === 'creator' && !byName('Cara Creator').remove && byName('Cara Creator').fold.endsWith('1 agent'), 'creator row: ' + JSON.stringify(byName('Cara Creator')));
  assert(byName('Jill Human').remove && byName('Jill Human').fold.endsWith('1 agent'), 'jill row: ' + JSON.stringify(byName('Jill Human')));
  assert(!byName('Ada Admin').remove && byName('Ada Admin').fold.endsWith('0 agents'), 'own row: ' + JSON.stringify(byName('Ada Admin')));
  assert(!list.some((r) => r.unowned), 'unowned section shown with no unowned agents');
  assert(await page.$eval('#ws-member-list > li[data-id="' + jill.participant.id + '"] .member-agents', (el) => el.classList.contains('hidden')), 'agents not folded');
  await page.click('#ws-member-list > li[data-id="' + jill.participant.id + '"] .member-fold');
  list = await rows(page);
  assert(byName('Jill Human').agents.length === 1 && byName('Jill Human').agents[0].owner === jill.participant.id, 'jill agents: ' + JSON.stringify(byName('Jill Human').agents));
  // 1b. a proper table: every role badge, agents button and Remove sits at one x
  // with one width on every row, the expanded agent row included; the creator's
  // empty Remove cell keeps the column; Remove is the shared subtle .remove-btn
  // (outline, muted, 13px/500, no icon; members-check asserts the same class in
  // the channel modal); "0 agents" looks disabled
  const cols = await page.evaluate(() => {
    const box = (el) => { const r = el.getBoundingClientRect(); return { x: Math.round(r.left), w: Math.round(r.width) }; };
    const all = (sel) => [...document.querySelectorAll(sel)].map(box);
    const rm = document.querySelector('#ws-member-list .member-head > .member-remove');
    const cs = getComputedStyle(rm);
    return {
      roles: all('#ws-member-list .member-head .member-role, #ws-member-list .member-agents:not(.hidden) .member-role'),
      folds: all('#ws-member-list .member-fold'),
      removes: all('#ws-member-list .member-head > .member-remove, #ws-member-list .member-agents:not(.hidden) .member-remove, #ws-member-list .member-remove-gap'),
      gap: document.querySelectorAll('#ws-member-list .member-remove-gap').length,
      gapHidden: [...document.querySelectorAll('#ws-member-list .member-remove-gap')].every((g) => getComputedStyle(g).visibility === 'hidden'),
      remove: { shared: rm.classList.contains('remove-btn'), bg: cs.backgroundColor, border: cs.borderWidth + ' ' + cs.borderStyle, font: cs.fontSize + '/' + cs.fontWeight, icon: !!rm.querySelector('svg'), text: rm.textContent.trim(), rightEdge: Math.round(rm.getBoundingClientRect().right), rowEdge: Math.round(rm.closest('.member-head').getBoundingClientRect().right) },
      zero: [...document.querySelectorAll('#ws-member-list .member-fold')].filter((b) => b.textContent.trim().endsWith('0 agents')).map((b) => b.disabled),
    };
  });
  const oneColumn = (boxes) => boxes.length > 1 && boxes.every((b) => b.x === boxes[0].x && b.w === boxes[0].w);
  assert(cols.roles.length === 4 && oneColumn(cols.roles), 'role badges drift: ' + JSON.stringify(cols.roles));
  assert(cols.folds.length === 3 && oneColumn(cols.folds), 'agents buttons drift: ' + JSON.stringify(cols.folds));
  assert(cols.removes.length === 4 && oneColumn(cols.removes) && cols.gap === 2 && cols.gapHidden, 'remove column drift: ' + JSON.stringify({ removes: cols.removes, gap: cols.gap, hidden: cols.gapHidden }));
  assert(cols.remove.shared && cols.remove.bg === 'rgba(0, 0, 0, 0)' && cols.remove.border === '1px solid' && cols.remove.font === '13px/500' && !cols.remove.icon && cols.remove.text === 'Remove' && cols.remove.rowEdge - cols.remove.rightEdge === 12, 'remove not the shared subtle button: ' + JSON.stringify(cols.remove));
  assert(cols.zero.length === 1 && cols.zero[0] === true, '0 agents not disabled: ' + JSON.stringify(cols.zero));
  await shot(page, 'tree.png');

  step = '2';
  // 2. rebind jillbot to Ada from the select: it moves under Ada, the API agrees
  await page.select('#ws-member-list li[data-id="' + jillbot.participant.id + '"] .member-owner', ada.participant.id);
  await page.waitForFunction((id) => {
    const li = document.querySelector('#ws-member-list > li[data-id="' + id + '"]');
    return li && li.querySelector('.member-fold').textContent.endsWith('1 agent');
  }, { timeout: 8000 }, ada.participant.id);
  const jb = (await call(SERVER, '/api/v1/participants', { token: adaSession, headers: ws })).participants.find((p) => p.id === jillbot.participant.id);
  assert(jb.owner_id === ada.participant.id && jb.owner_name === 'Ada Admin' && jb.owner_user_id, 'rebound agent: ' + JSON.stringify(jb));
  // and back to Jill, so her removal has something to cascade
  await page.click('#ws-member-list > li[data-id="' + ada.participant.id + '"] .member-fold');
  await page.select('#ws-member-list li[data-id="' + jillbot.participant.id + '"] .member-owner', jill.participant.id);
  await page.waitForFunction((id) => {
    const li = document.querySelector('#ws-member-list > li[data-id="' + id + '"]');
    return li && li.querySelector('.member-fold').textContent.endsWith('1 agent');
  }, { timeout: 8000 }, jill.participant.id);
  await shot(page, 'rebound.png');

  step = '3';
  // 3. removing Jill names jillbot in the confirm and takes it with her
  await page.click('#ws-member-list > li[data-id="' + jill.participant.id + '"] .member-head > .member-remove');
  await page.waitForFunction((id) => !document.querySelector('#ws-member-list > li[data-id="' + id + '"]'), { timeout: 8000 }, jill.participant.id);
  assert(dialogs.length === 1 && /Remove Jill Human and 1 agent \(jillbot\)/.test(dialogs[0]), 'confirm text: ' + JSON.stringify(dialogs));
  assert(await status('/api/v1/me', jillbot.token) === 401, 'jillbot token survived its owner');
  assert(await status('/api/v1/me', crewbot.token) === 200, 'crewbot token died with a stranger');
  list = await rows(page);
  assert(list.length === 2 && !list.some((r) => r.agents.some((a) => a.name === 'jillbot')), 'after cascade: ' + JSON.stringify(list));
  const general = await call(SERVER, '/api/v1/channels/general/messages?limit=5', { token: adaSession, headers: ws });
  assert(general.messages.some((m) => /removed Jill Human and 1 agent from the workspace/.test(m.body)), 'no counted #general line: ' + JSON.stringify(general.messages.map((m) => m.body)));
  await shot(page, 'after-remove.png');

  step = '4';
  // 4. a legacy ownerless agent shows under "Unowned agents" until an admin picks one
  execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c', `UPDATE participants SET owner_id = NULL WHERE id = '${crewbot.participant.id}'`], { stdio: ['ignore', 'ignore', 'inherit'] });
  await openSettings(page, SERVER, 'workspace');
  await page.waitForFunction(() => document.querySelector('#ws-member-list > li.member-unowned'), { timeout: 8000 });
  list = await rows(page);
  const un = list.find((r) => r.unowned);
  assert(un.agents.length === 1 && un.agents[0].name === 'crewbot' && un.agents[0].owner === '', 'unowned section: ' + JSON.stringify(un));
  await shot(page, 'unowned.png');
  await page.select('#ws-member-list li[data-id="' + crewbot.participant.id + '"] .member-owner', cara.id);
  await page.waitForFunction(() => !document.querySelector('#ws-member-list > li.member-unowned'), { timeout: 8000 });
  list = await rows(page);
  assert(byName('Cara Creator').fold.endsWith('1 agent'), 'adopted agent: ' + JSON.stringify(list));

  step = '5';
  // 5. the creator can neither be removed nor leave; no token changes
  const r1 = await del('/api/v1/participants/' + cara.id, adaSession, slug);
  assert(r1.status === 409 && r1.body.code === 'owner_protected', 'remove creator: ' + JSON.stringify(r1));
  const r2 = await del('/api/v1/participants/me', caraSession, slug);
  assert(r2.status === 409 && r2.body.code === 'owner_cannot_leave', 'creator leaves: ' + JSON.stringify(r2));
  assert(await status('/api/v1/me', crewbot.token) === 200, 'crewbot token after refused removals');
  await page.click('#ws-member-list > li[data-id="' + cara.id + '"] .member-fold');
  await page.click('#ws-member-list li[data-id="' + crewbot.participant.id + '"] .member-remove');
  await page.waitForFunction((id) => !document.querySelector('#ws-member-list li[data-id="' + id + '"]'), { timeout: 8000 }, crewbot.participant.id);
  assert(await status('/api/v1/me', crewbot.token) === 401, 'crewbot token after its own removal');

  await browser.close();
  if (errors.length) throw new Error('page errors: ' + errors.join(' | '));
  console.log('OWNERS_CHECK_OK');
})().catch(async (e) => {
  console.error('OWNERS_CHECK_FAIL at step ' + step + ': ' + e.message);
  try { if (failPage) { console.error('url=' + failPage.url()); await failPage.screenshot({ path: path.join(OUT, 'owners-fail.png') }); } } catch (_) { /* best effort */ }
  process.exit(1);
});
