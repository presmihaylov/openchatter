// E2E for the agent setup text (the Add an agent modal): the skill line, a
// bound invite link, and nothing about whatever proxy sits in front.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/invite-check.js
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
  const created = await newRoom(SERVER, 'invite check');
  const slug = created.room.slug;

  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await enterAs(page, SERVER, slug, created.invite_code, 'invitetester');
  // expand the own row and open Add an agent
  await page.waitForSelector('#participant-list li', { timeout: 8000 });
  await page.evaluate(() => {
    const li = [...document.querySelectorAll('#participant-list > li')].find((l) => (l.querySelector('.pname') || {}).textContent === 'invitetester');
    const t = li && li.querySelector('.p-toggle');
    if (t && t.dataset.state === 'collapsed') t.click();
  });
  await page.waitForSelector('#addagent-row', { timeout: 5000 });
  await page.evaluate(() => document.getElementById('addagent-row').click());
  await page.waitForFunction(() => document.getElementById('addagent-text').value.includes('/join/inv-'), { timeout: 8000 });
  const text = await page.$eval('#addagent-text', (el) => el.value);

  if (!text.includes('/skill with curl')) throw new Error('invite lost the skill line: ' + text);
  const m = text.match(/(\S+\/join\/inv-\S+)/);
  if (!m || !m[1].startsWith(SERVER + '/join/inv-')) throw new Error('invite lost the link: ' + text);
  if (m[1] === created.invite) throw new Error('agent instructions reuse the workspace link instead of a bound one');
  if (/Invite code:|Join link:/.test(text)) throw new Error('invite still carries the old lines: ' + text);
  if (/CF-Access|Cloudflare/.test(text)) throw new Error('invite carries proxy headers: ' + text);

  await browser.close();
  console.log('INVITE_CHECK_OK');
})().catch((e) => { console.error('INVITE_CHECK_FAIL:', e.message); process.exit(1); });
