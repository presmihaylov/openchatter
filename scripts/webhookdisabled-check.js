// E2E: when the room switches an agent's webhook off, its owner gets one error toast and a
// non-owner gets none. psql seeds the hook one failure short of the limit, aimed at loopback.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8090 OPENCHATTER_DB_URL=... node scripts/webhookdisabled-check.js
const puppeteer = require('puppeteer-core');
const { execFileSync } = require('child_process');
const { newRoom, enterAs, call, sleep } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
const toastText = (page) => page.$$eval('.toast .toast-text', (els) => els.map((e) => e.textContent));

(async () => {
  const created = await newRoom(SERVER, 'webhook disabled check');
  const slug = created.room.slug;
  const roomCode = created.invite_code;
  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  try {
    const owner = await browser.newPage();
    const other = await (await browser.createBrowserContext()).newPage();
    for (const p of [owner, other]) p.on('pageerror', (e) => { throw new Error('pageerror ' + e.message); });

    const maya = await enterAs(owner, SERVER, slug, roomCode, 'maya');
    await enterAs(other, SERVER, slug, roomCode, 'dimi');
    const bound = (await call(SERVER, '/api/v1/invites', { method: 'POST', token: maya, headers: { 'X-Workspace-Slug': slug }, body: { bind_owner: true } })).join_url;
    const bot = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite: bound, name: 'hookbot', description: 'agent' } });
    const pinger = await call(SERVER, '/api/v1/rooms/join', { method: 'POST', body: { invite_code: roomCode, name: 'pinger', description: 'agent' } });

    // one failure short of the limit, at an address the guard refuses
    execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c',
      `INSERT INTO participant_webhooks (participant_id, url, secret, consecutive_failures)
       VALUES ('${bot.participant.id}', 'https://127.0.0.1:9/wake', 'seeded-test-secret-0001', 9)`], { stdio: ['ignore', 'ignore', 'inherit'] });
    await call(SERVER, '/api/v1/channels/general/messages', { method: 'POST', token: pinger.token, body: { body: '@hookbot wake up' } });

    await owner.waitForFunction(() => [...document.querySelectorAll('.toast .toast-text')]
      .some((e) => e.textContent.startsWith('Push wakes for hookbot were turned off after 10 failed deliveries')), { timeout: 20000 });
    const seen = await toastText(owner);
    assert(seen.filter((t) => t.startsWith('Push wakes for hookbot')).length === 1, 'the owner should see exactly one toast: ' + JSON.stringify(seen));
    assert(seen.some((t) => t.includes('blocked')), 'the toast should name the last error: ' + JSON.stringify(seen));

    await sleep(2000);
    const otherSeen = await toastText(other);
    assert(!otherSeen.some((t) => t.includes('hookbot')), 'a member who does not own the agent saw the toast: ' + JSON.stringify(otherSeen));

    const hook = await call(SERVER, '/api/v1/me/webhook', { token: bot.token });
    assert(hook.disabled_at && /^blocked/.test(hook.last_error), 'the hook should be disabled: ' + JSON.stringify(hook));
    console.log('WEBHOOKDISABLED_CHECK_OK');
  } finally {
    await browser.close();
  }
})().catch((e) => { console.error('WEBHOOKDISABLED_CHECK_FAIL:', e.message); process.exit(1); });
