// Browser + API E2E for token-owned agent identities. A plain human sees an X
// only on their own agents; an admin sees it on every agent. Confirming the X
// revokes the token, removes the row, and frees the name for a fresh id while
// old messages keep the old author. Works on local dev or a Cloudflare-gated
// deployment with ACCESS_ID and ACCESS_SECRET.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/agentidentity-check.js
const puppeteer = require('puppeteer-core');

const SERVER = process.env.SERVER || 'http://localhost:8095';
const ACCESS_ID = process.env.ACCESS_ID || '';
const ACCESS_SECRET = process.env.ACCESS_SECRET || '';
const access = ACCESS_ID ? {
  'CF-Access-Client-Id': ACCESS_ID,
  'CF-Access-Client-Secret': ACCESS_SECRET,
} : {};
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

async function request(path, opts = {}) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, access);
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  return { status: resp.status, data };
}

async function must(path, opts = {}, status = 200) {
  const out = await request(path, opts);
  if (out.status !== status) throw new Error(path + ' -> ' + out.status + ' ' + (out.data.error || out.data.code || 'unexpected response'));
  return out.data;
}

const tag = () => Date.now().toString(36).slice(-7) + Math.floor(Math.random() * 1e6).toString(36);
async function register(displayName) {
  const out = await must('/api/v1/auth/password/register', {
    method: 'POST',
    body: { username: 'identity-' + tag(), password: 'correct horse battery', display_name: displayName },
  }, 201);
  return out.token;
}

async function openWorkspace(page, session, slug) {
  await page.setExtraHTTPHeaders(access);
  await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
  await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
  await page.goto(SERVER + '/w/' + slug, { waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
  await page.waitForSelector('#participant-list li', { timeout: 10000 });
}

async function expand(page, name) {
  await page.evaluate((wanted) => {
    const row = [...document.querySelectorAll('#participant-list > li')]
      .find((li) => (li.querySelector('.pname') || {}).textContent === wanted);
    const toggle = row && row.querySelector('.p-toggle');
    if (toggle && toggle.dataset.state === 'collapsed') toggle.click();
  }, name);
  await sleep(150);
}

const canDelete = (page, id) => page.$eval(
  `#participant-list li[data-id="${id}"]`,
  (row) => !!row.querySelector('.agent-delete'),
);

(async () => {
  let browser;
  let adminSession;
  let slug;
  let roomName;
  try {
    adminSession = await register('Admin Owner');
    roomName = 'identity check ' + tag();
    const made = await must('/api/v1/rooms', {
      method: 'POST', token: adminSession, body: { name: roomName, slug: 'identity-' + tag() },
    }, 201);
    slug = made.room.slug;
    const adminID = (await must('/api/v1/me', { token: adminSession, slug })).id;

    const memberSession = await register('Member Owner');
    const entered = await must('/api/v1/workspaces/' + slug + '/enter', {
      method: 'POST', token: memberSession, body: { invite: made.invite },
    });
    const memberID = entered.participant.id;

    const adminLink = (await must('/api/v1/invites', {
      method: 'POST', token: adminSession, slug, body: { bind_owner: true },
    }, 201)).join_url;
    const memberLink = (await must('/api/v1/invites', {
      method: 'POST', token: memberSession, slug, body: { bind_owner: true },
    }, 201)).join_url;
    const adminBot = await must('/api/v1/rooms/join', {
      method: 'POST', body: { invite: adminLink, name: 'adminbot', description: 'admin owned' },
    }, 201);
    const memberBot = await must('/api/v1/rooms/join', {
      method: 'POST', body: { invite: memberLink, name: 'ownedbot', description: 'member owned' },
    }, 201);
    assert(adminBot.participant.owner_id === adminID, 'admin agent has the wrong owner');
    assert(memberBot.participant.owner_id === memberID, 'member agent has the wrong owner');

    const oldMessage = await must('/api/v1/channels/general/messages', {
      method: 'POST', token: memberBot.token, body: { body: 'message from the old identity' },
    }, 201);
    await must('/api/v1/me/offline', { method: 'POST', token: memberBot.token });
    const duplicate = await request('/api/v1/rooms/join', {
      method: 'POST', body: { invite: memberLink, name: 'ownedbot' },
    });
    assert(duplicate.status === 409, 'owner-bound duplicate name returned ' + duplicate.status + ', want 409');

    browser = await puppeteer.launch({
      executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
      headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
    });
    const memberPage = await browser.newPage();
    await memberPage.setViewport({ width: 1280, height: 850 });
    await openWorkspace(memberPage, memberSession, slug);
    await expand(memberPage, 'Member Owner');
    await expand(memberPage, 'Admin Owner');
    assert(await canDelete(memberPage, memberBot.participant.id), 'member cannot see X on their own agent');
    assert(!(await canDelete(memberPage, adminBot.participant.id)), 'member sees X on another human\'s agent');

    let memberConfirm = '';
    memberPage.once('dialog', async (dialog) => { memberConfirm = dialog.message(); await dialog.accept(); });
    await memberPage.click(`#participant-list li[data-id="${memberBot.participant.id}"] .agent-delete`);
    await memberPage.waitForFunction((id) => !document.querySelector(`#participant-list li[data-id="${id}"]`), { timeout: 10000 }, memberBot.participant.id);
    assert(/token will stop working immediately/.test(memberConfirm) && /Past messages stay/.test(memberConfirm), 'delete confirmation does not explain the cost');

    const oldAuth = await request('/api/v1/me', { token: memberBot.token });
    assert(oldAuth.status === 401, 'deleted token returned ' + oldAuth.status + ', want 401');
    const fresh = await must('/api/v1/rooms/join', {
      method: 'POST', body: { invite: memberLink, name: 'ownedbot', description: 'fresh identity' },
    }, 201);
    assert(fresh.participant.id !== memberBot.participant.id, 'recreated name reused the old participant id');
    assert(fresh.token !== memberBot.token, 'recreated name reused the old token');
    const history = await must('/api/v1/messages/' + oldMessage.id, { token: memberSession, slug });
    assert(history.author_id === memberBot.participant.id && history.author_name === 'ownedbot', 'old message lost its old author');

    const adminPage = await browser.newPage();
    await adminPage.setViewport({ width: 1280, height: 850 });
    await openWorkspace(adminPage, adminSession, slug);
    await expand(adminPage, 'Admin Owner');
    await expand(adminPage, 'Member Owner');
    assert(await canDelete(adminPage, adminBot.participant.id), 'admin cannot see X on their own agent');
    assert(await canDelete(adminPage, fresh.participant.id), 'admin cannot see X on another human\'s agent');

    adminPage.once('dialog', async (dialog) => { await dialog.accept(); });
    await adminPage.click(`#participant-list li[data-id="${fresh.participant.id}"] .agent-delete`);
    await adminPage.waitForFunction((id) => !document.querySelector(`#participant-list li[data-id="${id}"]`), { timeout: 10000 }, fresh.participant.id);
    const freshAuth = await request('/api/v1/me', { token: fresh.token });
    assert(freshAuth.status === 401, 'admin-deleted token returned ' + freshAuth.status + ', want 401');

    console.log('AGENTIDENTITY_CHECK_OK');
  } finally {
    if (browser) await browser.close();
    if (adminSession && slug && roomName) {
      await request('/api/v1/room', { method: 'DELETE', token: adminSession, slug, body: { name: roomName } });
    }
  }
})().catch((err) => {
  console.error('AGENTIDENTITY_CHECK_FAIL:', err.message);
  process.exit(1);
});
