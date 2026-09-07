require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for image-only avatars: an avatar is an image, full stop. A member with
// an uploaded picture, one with none, and a legacy member whose row still
// carries an emoji all render an <img>; no glyph stands in anywhere in the
// chrome, and Remove (DELETE /me/avatar) lands back on the seedling.
// Run: NODE_PATH=<puppeteer dir> SERVER=http://localhost:8095 node scripts/avatar-check.js
const { execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman, openSettings } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const DB_URL = process.env.OPENCHATTER_DB_URL || 'postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable';
const OUT = process.env.OUT || 'tmp';
const SEED_128 = '/brand/avatar-default-128.png';
const SEED_512 = '/brand/avatar-default-512.png';
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
let lastStep = 'start';

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign(opts.body ? { 'Content-Type': 'application/json' } : {}, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

// a solid teal png, distinct from the seedling at a glance
const png = () => {
  const w = 120, h = 120, raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) { for (let x = 0; x < w; x++) raw.set([20, 140, 160], y * (w * 3 + 1) + 1 + x * 3); }
  const crc = (b) => { let c = -1; for (const v of b) { c ^= v; for (let k = 0; k < 8; k++) c = (c >>> 1) ^ (0xEDB88320 & -(c & 1)); } return (c ^ -1) >>> 0; };
  const chunk = (t, d) => { const len = Buffer.alloc(4); len.writeUInt32BE(d.length); const td = Buffer.concat([Buffer.from(t), d]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
};

const uploadAvatar = async (token, bytes) => {
  const fd = new FormData();
  fd.append('file', new Blob([bytes], { type: 'image/png' }), 'face.png');
  const resp = await fetch(SERVER + '/api/v1/me/avatar', { method: 'POST', headers: { Authorization: 'Bearer ' + token }, body: fd });
  if (!resp.ok) throw new Error('avatar upload: ' + resp.status + ' ' + (await resp.text()));
  return resp.json();
};

(async () => {
  const created = await newRoom(SERVER, 'avatar check');
  const slug = created.room.slug, code = created.invite_code;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'alice', is_human: true } });
  // the join still takes an emoji so an old client does not 4xx; it is dropped
  const shot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'shot', avatar: '\u{1F984}', description: 'has a picture' } });
  assert(!('avatar' in shot.participant), 'join handed back an emoji avatar: ' + JSON.stringify(shot.participant));
  const bare = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'bare', description: 'no picture' } });
  const relic = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: code, name: 'relic', description: 'joined back when avatars were emoji' } });

  await uploadAvatar(shot.token, png());
  // a legacy member: the emoji column still holds what they picked years ago
  execFileSync('psql', [DB_URL, '-q', '-v', 'ON_ERROR_STOP=1', '-c',
    `UPDATE participants SET avatar = '\u{1F984}' WHERE room_id = '${created.room.id}' AND name = 'relic'`], { stdio: ['ignore', 'ignore', 'inherit'] });

  for (const who of [shot, bare, relic]) {
    await api('/api/v1/channels/general/messages', { method: 'POST', body: { body: 'hello from ' + who.participant.name }, token: who.token });
  }

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 900 });
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
  await openAsHuman(page, SERVER, slug, alice);
  await page.waitForSelector('#messages .msg', { timeout: 8000 });

  // 1. every message row draws an image: the uploaded one for shot, the shared
  //    seedling for bare and for the legacy emoji row alike
  const rowAvatar = (name) => page.evaluate((n) => {
    const row = [...document.querySelectorAll('#messages .msg')].find((m) => (m.querySelector('.author') || {}).textContent === n);
    if (!row) return { missing: true };
    const box = row.querySelector('.avatar');
    const img = box.querySelector('img');
    return { tag: box.firstElementChild.tagName, src: img ? img.getAttribute('src') : '', text: box.textContent.trim() };
  }, name);
  for (const [name, want] of [['bare', SEED_128], ['relic', SEED_128]]) {
    lastStep = 'row ' + name;
    await page.waitForFunction((n, s) => [...document.querySelectorAll('#messages .msg')]
      .some((m) => (m.querySelector('.author') || {}).textContent === n && (m.querySelector('.avatar img') || {}).getAttribute('src') === s), { timeout: 8000 }, name, want);
    const a = await rowAvatar(name);
    assert(a.tag === 'IMG' && a.src === want && a.text === '', name + ' row avatar: ' + JSON.stringify(a));
  }
  lastStep = 'row shot';
  await page.waitForFunction(() => [...document.querySelectorAll('#messages .msg')]
    .some((m) => (m.querySelector('.author') || {}).textContent === 'shot' && ((m.querySelector('.avatar img') || {}).getAttribute('src') || '').startsWith('blob:')), { timeout: 8000 });
  const up = await rowAvatar('shot');
  assert(up.tag === 'IMG' && up.text === '', 'shot row avatar: ' + JSON.stringify(up));

  // 2. no glyph anywhere in the chrome: not one .avatar-emoji is rendered,
  //    and no avatar box carries text
  const glyphs = () => page.evaluate(() => ({
    emoji: document.querySelectorAll('.avatar-emoji').length,
    texty: [...document.querySelectorAll('.avatar-img, .avatar-msg, .avatar-sm, .avatar-lg, .avatar-me, .avatar-rb')]
      .filter((el) => el.textContent.trim() !== '').length,
  }));
  lastStep = 'glyph sweep, message list';
  let g = await glyphs();
  assert(g.emoji === 0 && g.texty === 0, 'glyphs in the room chrome: ' + JSON.stringify(g));

  // 3. the members panel and a profile card: images there too
  lastStep = 'members panel';
  await page.click('#members-btn');
  await page.waitForSelector('#members-modal:not(.hidden)', { timeout: 8000 });
  g = await glyphs();
  assert(g.emoji === 0 && g.texty === 0, 'glyphs in the members panel: ' + JSON.stringify(g));
  await page.screenshot({ path: path.join(OUT, 'avatar-members.png') });

  // 4. the mention picker names members with their picture, not a stand-in
  lastStep = 'mention picker';
  await page.click('#composer-input');
  await page.type('#composer-input', '@rel');
  await page.waitForSelector('.mention-ac:not(.chan-ac) .mention-opt', { timeout: 8000 });
  const picker = await page.evaluate(() => [...document.querySelectorAll('.mention-ac:not(.chan-ac) .mention-opt')].map((o) => ({
    img: (o.querySelector('.mention-name img') || {}).getAttribute ? o.querySelector('.mention-name img').getAttribute('src') : '',
    text: o.querySelector('.mention-name').textContent.trim(),
  })));
  assert(picker.length > 0 && picker.every((o) => o.img && !/\p{Extended_Pictographic}/u.test(o.text)),
    'mention picker rows: ' + JSON.stringify(picker));
  await page.screenshot({ path: path.join(OUT, 'avatar-mention.png') });
  await page.keyboard.press('Escape');

  // 5. Settings: alice has no picture, so the seedling stands; an upload
  //    replaces it and Remove (DELETE /me/avatar) lands back on the seedling
  lastStep = 'settings';
  await openSettings(page, SERVER, 'personal');
  await page.waitForSelector('#avatar-section:not(.hidden)', { timeout: 8000 });
  const settingsSrc = () => page.$eval('#settings-avatar img', (el) => el.getAttribute('src'));
  assert(await settingsSrc() === SEED_512, 'settings before upload: ' + await settingsSrc());
  assert(await page.$eval('#avatar-remove', (el) => el.classList.contains('hidden')), 'Remove offered with no picture');
  const file = path.join(OUT, 'avatar-upload.png');
  fs.writeFileSync(file, png());
  await (await page.$('#avatar-input')).uploadFile(file);
  lastStep = 'settings after upload';
  await page.waitForFunction(() => ((document.querySelector('#settings-avatar img') || {}).getAttribute('src') || '').startsWith('blob:'), { timeout: 8000 });
  await page.waitForSelector('#avatar-remove:not(.hidden)', { timeout: 8000 });
  await page.screenshot({ path: path.join(OUT, 'avatar-settings-uploaded.png') });
  lastStep = 'settings after remove';
  await page.click('#avatar-remove');
  await page.waitForFunction((s) => (document.querySelector('#settings-avatar img') || {}).getAttribute('src') === s, { timeout: 8000 }, SEED_512);
  assert(await page.$eval('#avatar-remove', (el) => el.classList.contains('hidden')), 'Remove still offered after the remove');
  const me = await api('/api/v1/me', { token: alice.token });
  assert(!('avatar' in me) && !me.avatar_attachment_id, 'GET /me after remove: ' + JSON.stringify(me));
  await page.screenshot({ path: path.join(OUT, 'avatar-settings-seedling.png') });

  // 6. the seedling is a real asset, served at both sizes
  for (const p of [SEED_128, SEED_512]) {
    const resp = await fetch(SERVER + p);
    assert(resp.ok && resp.headers.get('content-type') === 'image/png', p + ': ' + resp.status + ' ' + resp.headers.get('content-type'));
  }

  const real = errors.filter((e) => !e.includes('favicon'));
  assert(!real.length, 'page errors: ' + real.join(' | '));
  console.log('AVATAR_CHECK_OK');
  await browser.close();
  process.exit(0);
})().catch((e) => { console.error('AVATAR_CHECK_FAIL:', e.message, '(after: ' + lastStep + ')'); process.exit(1); });
