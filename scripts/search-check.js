// E2E for hybrid search (task F). One query, one request to
// /api/v1/search/hybrid, one ranked list: direct (fuzzy text) hits first,
// semantic-only hits fill in below with a small "semantic" tag. Rows read like
// message rows (avatar, name, time, channel, snippet). A long list previews 5
// rows with "... (see more)". A server with no embeddings provider answers
// semantic:false and the panel says so while text hits still render.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/search-check.js
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const { newRoom, openAsHuman } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}
const launch = () => puppeteer.launch({
  executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
});
async function seedLogin(page, slug, joined) {
  await openAsHuman(page, SERVER, slug, joined);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 6000 });
}
const rows = (page) => page.evaluate(() => [...document.querySelectorAll('#search-results .search-hit-row')].map((r) => ({
  snippet: r.querySelector('.sh-snippet').textContent,
  author: r.querySelector('.sh-author').textContent,
  channel: r.querySelector('.sh-channel').textContent,
  avatar: !!r.querySelector('.sh-avatar .avatar-msg'),
  avatarW: r.querySelector('.sh-avatar .avatar-msg') ? Math.round(r.querySelector('.sh-avatar .avatar-msg').getBoundingClientRect().width) : 0,
  via: r.querySelector('.sh-via') ? r.querySelector('.sh-via').textContent : '',
})));
const retype = async (page, q) => { await page.click('#search-input', { clickCount: 3 }); await page.type('#search-input', q); };

(async () => {
  const created = await newRoom(SERVER, 'search check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'searchbot', description: 't', avatar: '🤖' } });
  const human = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'searchhuman', is_human: true } });
  const budget = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'the quarterly budget forecast is due next week' } });
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'please fix the webhook config before deploy' } });
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'lunch options near the office' } });
  // 7 hits for one keyword, to exercise the 5-row preview + see-more expand
  for (let i = 1; i <= 7; i++) {
    await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'sprint retro notes part ' + i } });
  }

  const browser = await launch();
  const page = await browser.newPage();
  await page.setViewport({ width: 1000, height: 800 });
  page.on('pageerror', (e) => { console.error('PAGEERROR', e.message); process.exitCode = 1; });
  await seedLogin(page, slug, human);
  // result rows carry the base .avatar-msg tile. message rows are deliberately
  // 10% larger than it (31 vs 28), so they are no longer the reference here.
  const SEARCH_AVATAR_W = 28;
  const msgAvatarW = await page.evaluate(() => Math.round(document.querySelector('#messages .msg .avatar-msg').getBoundingClientRect().width));
  if (msgAvatarW !== 31) throw new Error('message row avatar is not 31px: ' + msgAvatarW);

  // A) exact text query: the hit renders like a message row, via=text (no tag)
  await page.evaluate(() => document.getElementById('open-search').click());
  await page.waitForSelector('#search-modal:not(.hidden)', { timeout: 3000 });
  await page.type('#search-input', 'budget');
  await page.waitForFunction(() => [...document.querySelectorAll('#search-results .search-hit-row')].some((r) => r.textContent.toLowerCase().includes('budget')), { timeout: 5000 });
  let got = await rows(page);
  const hit = got.find((r) => r.snippet.includes('budget'));
  if (!hit.avatar || hit.avatarW !== SEARCH_AVATAR_W) throw new Error('result row lacks the base-sized avatar: ' + JSON.stringify(hit) + ' want=' + SEARCH_AVATAR_W);
  if (hit.author !== 'searchbot' || hit.channel !== '#general') throw new Error('row meta: ' + JSON.stringify(hit));
  if (hit.via !== '') throw new Error('direct hit must not carry the semantic tag');
  if (!got.every((r) => r.avatar)) throw new Error('every result row needs an avatar');

  // click-through still flashes the target message
  await page.click('#search-results .search-hit-row');
  await page.waitForSelector('#search-modal.hidden', { timeout: 3000 });
  await page.waitForFunction((id) => {
    const n = [...document.querySelectorAll('#messages .msg')].find((x) => x.dataset.id === id);
    return n && n.classList.contains('msg-flash');
  }, { timeout: 3000 }, budget.id);

  // A2) FUZZY: a typo of "webhook" still hits as a direct (untagged) match
  await page.evaluate(() => document.getElementById('open-search').click());
  await page.waitForSelector('#search-modal:not(.hidden)', { timeout: 3000 });
  await retype(page, 'webook');
  await page.waitForFunction(() => [...document.querySelectorAll('#search-results .search-hit-row')]
    .some((r) => r.textContent.toLowerCase().includes('webhook') && !r.querySelector('.sh-via')), { timeout: 6000 })
    .catch(() => { throw new Error('fuzzy text match for webook did not land untagged'); });

  // B) SEMANTIC: pre-warm the embedding, then a query that shares no word with
  // the budget message surfaces it with the "semantic" tag; the direct query
  // shows it once (dedup across legs) and untagged.
  let embedded = false;
  for (let i = 0; i < 25; i++) {
    const s = await api('/api/v1/search/semantic?q=' + encodeURIComponent('company financial planning'), { token: bot.token });
    if ((s.results || []).some((r) => r.id === budget.id)) { embedded = true; break; }
    await new Promise((r) => setTimeout(r, 1000));
  }
  if (!embedded) throw new Error('message never embedded; semantic path cannot be verified');
  await retype(page, 'company financial planning');
  await page.waitForFunction(() => [...document.querySelectorAll('#search-results .search-hit-row')]
    .some((r) => r.textContent.toLowerCase().includes('budget') && r.querySelector('.sh-via')), { timeout: 8000 })
    .catch(() => { throw new Error('semantic-only query did not surface the budget row with a semantic tag'); });
  const note = await page.evaluate(() => document.querySelector('#search-note').classList.contains('hidden'));
  if (!note) throw new Error('off note shown while embeddings are enabled');
  if (process.env.OUT) { fs.mkdirSync(process.env.OUT, { recursive: true }); await page.screenshot({ path: process.env.OUT + '/search-semantic.png' }); }
  await retype(page, 'budget');
  await page.waitForFunction(() => [...document.querySelectorAll('#search-results .search-hit-row')].some((r) => r.textContent.toLowerCase().includes('budget')), { timeout: 8000 });
  await new Promise((r) => setTimeout(r, 1500));
  got = await rows(page);
  const budgetRows = got.filter((r) => r.snippet.includes('budget'));
  if (budgetRows.length !== 1 || budgetRows[0].via !== '') throw new Error('dedup/rank: ' + JSON.stringify(budgetRows));
  if (got[0].snippet !== budgetRows[0].snippet) throw new Error('exact hit must rank first, got ' + JSON.stringify(got[0]));

  // B2) SEE-MORE: 7 direct hits preview as 5 rows + "... (see more)"; the click
  // expands inline to all 7 without a new query.
  await retype(page, 'sprint retro notes');
  await page.waitForFunction(() => {
    const n = [...document.querySelectorAll('#search-results .search-hit-row')].filter((r) => r.textContent.includes('sprint')).length;
    return n === 5 && !!document.querySelector('#search-results .search-more');
  }, { timeout: 8000 }).catch(() => { throw new Error('7-hit list never previewed as 5 rows + see-more'); });
  await page.evaluate(() => document.querySelector('#search-results .search-more').click());
  await page.waitForFunction(() => document.querySelectorAll('#search-results .search-hit-row').length === 7 && !document.querySelector('#search-results .search-more'), { timeout: 4000 })
    .catch(() => { throw new Error('see-more did not expand to 7 rows'); });
  await browser.close();

  // C) DEGRADE: the server answers semantic:false -> the panel says semantic is
  // off; the text hit still renders.
  const b2 = await launch();
  const p2 = await b2.newPage();
  await p2.setRequestInterception(true);
  p2.on('request', (req) => {
    if (!req.url().includes('/api/v1/search/hybrid')) { req.continue(); return; }
    req.respond({ status: 200, contentType: 'application/json', body: JSON.stringify({ semantic: false, results: [Object.assign({}, budget, { author_name: 'searchbot', score: 1, via: 'text' })] }) });
  });
  await seedLogin(p2, slug, human);
  await p2.evaluate(() => document.getElementById('open-search').click());
  await p2.waitForSelector('#search-modal:not(.hidden)', { timeout: 3000 });
  await p2.type('#search-input', 'budget');
  await p2.waitForFunction(() => {
    const note = document.querySelector('#search-note');
    const off = !note.classList.contains('hidden') && note.textContent.toLowerCase().includes('off on this server');
    const textHit = [...document.querySelectorAll('#search-results .search-hit-row')].some((r) => r.textContent.toLowerCase().includes('budget') && !r.querySelector('.sh-via'));
    return off && textHit;
  }, { timeout: 6000 }).catch(() => { throw new Error('semantic:false did not show the off note with the text hit'); });
  await b2.close();

  if (!process.exitCode) console.log('SEARCH_CHECK_OK');
})().catch((e) => { console.error('SEARCH_CHECK_FAIL:', e.message); process.exit(1); });
