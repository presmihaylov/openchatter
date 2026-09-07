require('fs').mkdirSync(process.env.OUT || 'tmp', { recursive: true });
// E2E for the icon design pass (task 24): every icon in the chrome is an inline
// Lucide SVG from web/src/icons.js (data-icon names it), drawn at one stroke
// width, at 16, 20 or 24px, and no colour emoji or text glyph stands in for an
// icon anywhere in the chrome. Emoji stay where they are content: message
// bodies, reactions, avatars. The walk covers the sidebar, header, composer,
// message toolbar, reactions row, thread panel, every modal and menu, and the
// settings page. Run: NODE_PATH=<dir with puppeteer-core> node scripts/icons-check.js
const fs = require('fs');
const path = require('path');
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman, openSettings, openInviteModal } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';

async function api(p, opts = {}) {
  const resp = await fetch(SERVER + p, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status >= 400) throw new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// the set of names the generator inlined: an icon outside it is not from the set
const iconsSrc = fs.readFileSync(path.join(__dirname, '..', 'web', 'src', 'icons.js'), 'utf8');
const NAMES = JSON.parse(iconsSrc.match(/export const ICON_NAMES = (\[.*\]);/)[1]);

// runs in the page: every visible svg outside content areas must be a set icon
const AUDIT = (names, where) => {
  const CONTENT = '.content, #doc-body, .composer-preview, .ProseMirror, .rx-tip, .emoji-picker, .avatar-img, img';
  const EMOJI_OK = '.content, .msg-reactions, .rx-tip, .avatar-emoji, .avatar, .emoji-picker, .ProseMirror, .composer-preview, .mention-name, .owner-badge, .reply-bar, .rx-quick, .ws-avatar, .rail-item, #settings-avatar, .avatar-lg, .mm-avatar, .profile-avatar, #profile-modal .avatar-emoji';
  const visible = (el) => { const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && !el.closest('.t-archive:not(:hover)'); };
  const bad = [];
  const seen = [];
  for (const svg of document.querySelectorAll('svg')) {
    if (svg.closest(CONTENT)) continue;
    if (!visible(svg)) continue;
    const r = svg.getBoundingClientRect();
    const cs = getComputedStyle(svg);
    const name = svg.dataset.icon;
    const w = Math.round(r.width), h = Math.round(r.height);
    const info = { where, name, w, h, stroke: cs.strokeWidth, cls: svg.getAttribute('class'), in: (svg.parentElement.id || svg.parentElement.className || svg.parentElement.tagName) + '' };
    seen.push(info);
    if (!svg.classList.contains('lucide') || !name) bad.push({ why: 'not a set icon', ...info });
    else if (!names.includes(name)) bad.push({ why: 'name outside the set', ...info });
    // a channel sigil is text-sized: 1em, same weight as the name (stroke follows the font)
    const sigil = !!svg.closest('.sigil, .browse-name');
    if (sigil) {
      const fs = Math.round(parseFloat(getComputedStyle(svg.parentElement).fontSize));
      if (Math.abs(w - fs) > 1 || w !== h) bad.push({ why: 'sigil not text-sized', fs, ...info });
      continue;
    }
    if (cs.strokeWidth !== '2px') bad.push({ why: 'stroke width', ...info });
    if (![16, 20, 24].includes(w) || w !== h) bad.push({ why: 'size', ...info });
  }
  // text glyphs and colour emoji in chrome text nodes
  const GLYPH = /[←-⇿▶-▿☀-➿＋✖✗✓✔\p{Extended_Pictographic}]/u;
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  let n;
  while ((n = walker.nextNode())) {
    const el = n.parentElement;
    if (!el || el.closest(EMOJI_OK) || el.closest('script, style, template')) continue;
    if (!visible(el)) continue;
    const m = n.textContent.match(GLYPH);
    if (m) bad.push({ why: 'glyph in chrome text', where, text: n.textContent.trim().slice(0, 60), glyph: m[0], in: el.id || el.className || el.tagName });
  }
  return { bad, seen };
};

(async () => {
  const created = await newRoom(SERVER, 'icons check');
  const slug = created.room.slug;
  const alice = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'alice', is_human: true } });
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'helper', is_human: false } });
  await api('/api/v1/channels', { method: 'POST', token: alice.token, body: { name: 'vault', private: true } });
  const quiet = await api('/api/v1/channels', { method: 'POST', token: alice.token, body: { name: 'quiet' } });
  await api('/api/v1/channels/' + quiet.id + '/mute', { method: 'POST', token: alice.token, body: { muted: true } });
  const root = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'Root with an emoji in the body 🎉 and a #general link' } });
  await api('/api/v1/channels/general/messages', { method: 'POST', token: alice.token, body: { body: 'a reply', thread_root_id: root.id } });
  await api('/api/v1/messages/' + root.id + '/reactions', { method: 'POST', token: bot.token, body: { emoji: '👀' } });
  const second = await api('/api/v1/channels/general/messages', { method: 'POST', token: alice.token, body: { body: 'second, mine to edit' } });

  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  await openAsHuman(page, SERVER, slug, alice);
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await page.waitForSelector('#channel-list li.thread-leaf', { timeout: 8000 });

  // my own row starts collapsed; the "+ Add an agent" row lives inside it
  await page.evaluate(() => document.querySelector('#participant-list .p-toggle[data-state="collapsed"]').click());
  await page.waitForSelector('#addagent-row', { timeout: 3000 });

  const allBad = [];
  const seenNames = new Set();
  let total = 0;
  const audit = async (where) => {
    const r = await page.evaluate(AUDIT, NAMES, where);
    allBad.push(...r.bad);
    r.seen.forEach((s) => seenNames.add(s.name));
    total += r.seen.length;
    return r.seen.length;
  };
  const shot = (name) => page.screenshot({ path: path.join(OUT, 'icons-' + name + '.png') });
  const esc = () => page.keyboard.press('Escape');
  const dom = (sel) => page.evaluate((s) => { const el = document.querySelector(s); if (!el) throw new Error('missing ' + s); el.click(); }, sel);

  // 1. the room at rest: rail, sidebar (hash, lock, bell-off, thread elbow), header, composer
  const n1 = await audit('room');
  assert(n1 >= 10, 'room chrome shows only ' + n1 + ' icons');
  const sidebar = await page.evaluate(() => ({
    hash: !!document.querySelector('#channel-list li .sigil svg[data-icon="hash"]'),
    lock: !!document.querySelector('#channel-list li .sigil-lock svg[data-icon="lock"]'),
    bellOff: !!document.querySelector('#channel-list li.muted .mute-mark svg[data-icon="bell-off"]'),
    elbow: !!document.querySelector('#channel-list li.thread-leaf .t-icon svg[data-icon="corner-down-right"]'),
    title: !!document.querySelector('#channel-title .sigil svg[data-icon="hash"]'),
    members: !!document.querySelector('#members-btn svg[data-icon="users"]'),
    search: !!document.querySelector('#open-search svg[data-icon="search"]'),
    browse: !!document.querySelector('#browse-channels svg[data-icon="compass"]'),
    plus: !!document.querySelector('#new-channel svg[data-icon="plus"]'),
    railPlus: !!document.querySelector('#rail-add svg[data-icon="plus"]'),
    caret: !!document.querySelector('#ws-switcher .ws-caret svg[data-icon="chevron-down"]'),
    addAgent: !!document.querySelector('#addagent-row svg[data-icon="plus"]'),
    clip: !!document.querySelector('#attach-label svg[data-icon="paperclip"]'),
    send: !!document.querySelector('.composer-send svg[data-icon="arrow-up"]'),
  }));
  const missing = Object.entries(sidebar).filter(([, v]) => !v).map(([k]) => k);
  assert(!missing.length, 'room chrome missing icons: ' + missing.join(', '));
  await shot('room');
  // the hide button on a thread leaf only shows on hover; hovered, it is a 16px x
  await page.hover('#channel-list li.thread-leaf');
  await sleep(200);
  const hideX = await page.$eval('#channel-list li.thread-leaf:hover .t-archive svg', (s) => s.dataset.icon + ':' + Math.round(s.getBoundingClientRect().width));
  assert(hideX === 'x:16', 'thread hide button: ' + hideX);
  await audit('leaf-hover');

  // 2. hover toolbar on a message you own (ack, react, thread, edit, delete, more) + the reactions row
  await page.hover(`#messages .msg[data-id="${second.id}"]`);
  await sleep(150);
  const toolbar = await page.$$eval(`#messages .msg[data-id="${second.id}"] .msg-actions button`, (bs) => bs.map((b) => b.dataset.act + ':' + ((b.querySelector('svg.lucide') || {}).dataset || {}).icon));
  assert(toolbar.join(',') === 'ack:check,react:smile-plus,thread:message-square,edit:pencil,delete:trash-2,more:more-vertical', 'toolbar: ' + toolbar.join(','));
  const rxAdd = await page.$eval(`#messages .msg[data-id="${root.id}"] .msg-reactions .reaction.add svg`, (s) => s.dataset.icon + ':' + s.classList.contains('rx-add-icon'));
  assert(rxAdd === 'smile-plus:true', 'reaction add button: ' + rxAdd);
  await audit('hover');
  await shot('hover');

  // 3. thread panel: header icon, close x, its own composer
  await dom(`#messages .msg[data-id="${root.id}"] .msg-actions button[data-act="thread"]`);
  await page.waitForSelector('#thread-panel:not(.hidden)', { timeout: 5000 });
  await sleep(200);
  const thread = await page.evaluate(() => ({
    head: !!document.querySelector('#thread-panel header svg[data-icon="message-square"]'),
    close: !!document.querySelector('#thread-close svg[data-icon="x"]'),
    send: !!document.querySelector('#thread-panel .composer-send svg[data-icon="arrow-up"]'),
  }));
  assert(thread.head && thread.close && thread.send, 'thread panel icons: ' + JSON.stringify(thread));
  await audit('thread');
  await shot('thread');
  await dom('#thread-close');

  // 4. workspace menu and rail menu
  await dom('#ws-switcher');
  await page.waitForSelector('#ws-menu:not(.hidden)', { timeout: 3000 });
  const wsMenu = await page.$$eval('#ws-menu .ws-item .mi-icon svg', (s) => s.map((x) => x.dataset.icon));
  assert(wsMenu.join(',') === 'mail,log-in,bell-off,settings', 'workspace menu icons: ' + wsMenu.join(','));
  await audit('ws-menu');
  await shot('wsmenu');
  await esc();
  await dom('#rail-add');
  await page.waitForSelector('#rail-menu:not(.hidden)', { timeout: 3000 });
  const railMenu = await page.$$eval('#rail-menu .mi-icon svg', (s) => s.map((x) => x.dataset.icon));
  assert(railMenu.join(',') === 'plus,log-in', 'rail menu icons: ' + railMenu.join(','));
  await audit('rail-menu');
  await shot('railmenu');
  await esc();

  // 5. every modal: search, browse, members, profile, invite, join, add agent
  const modals = [
    ['search', '#open-search', '#search-modal'],
    ['browse', '#browse-channels', '#browse-modal'],
    ['members', '#members-btn', '#members-modal'],
    ['profile', '#participant-list li.participant-leaf:not(.addagent-row)', '#profile-modal'],
    ['addagent', '#addagent-row', '#addagent-modal'],
    ['join', '#rail-add', '#join-modal'],
  ];
  for (const [name, opener, modal] of modals) {
    await page.evaluate((s) => { const el = document.querySelector(s); if (!el) throw new Error('missing ' + s); el.click(); }, opener);
    if (name === 'join') { await page.waitForSelector('#rail-join', { timeout: 3000 }); await dom('#rail-join'); }
    await page.waitForSelector(modal + ':not(.hidden)', { timeout: 5000 });
    await sleep(150);
    const close = await page.$eval(modal, (m) => { const b = m.querySelector('button[id$="-close"] svg'); return b ? b.dataset.icon : null; });
    assert(close === 'x', name + ' modal close is not the x icon: ' + close);
    if (name === 'browse') {
      const rows = await page.$$eval('#browse-list .browse-row .browse-name', (ns) => ns.map((n) => (n.querySelector('svg') || {}).dataset?.icon + ':' + n.textContent));
      assert(rows.some((r) => r === 'hash:general'), 'browse rows: ' + rows.join(' '));
    }
    if (name === 'members') {
      assert(await page.$('#members-invite svg[data-icon="plus"]'), 'Add member lost its plus');
      assert((await page.$eval('#members-invite', (b) => b.textContent.trim())) === 'Add member', 'members button label is not Add member');
    }
    await audit(name);
    await shot(name);
    // the close button is the one thing every modal shares (not all take Escape)
    await page.evaluate((m) => document.querySelector(m + ' button[id$="-close"]').click(), modal);
    await page.waitForSelector(modal + '.hidden', { timeout: 3000 });
  }
  await openInviteModal(page);
  await sleep(150);
  await audit('invite');
  await shot('invite');
  await esc();

  // 6. settings, both tabs: nav icons, h2 icons, back arrow, sign out
  await openSettings(page, SERVER, 'personal');
  await sleep(200);
  const settings = await page.evaluate(() => ({
    back: !!document.querySelector('#settings-back svg[data-icon="arrow-left"]'),
    tabs: [...document.querySelectorAll('#settings-nav [role="tab"] svg')].map((s) => s.dataset.icon).join(','),
    bell: !!document.querySelector('#settings-personal h2 svg[data-icon="bell"]'),
    signout: !!document.querySelector('#settings-signout svg[data-icon="log-out"], #app-signout svg[data-icon="log-out"]'),
  }));
  assert(settings.back && settings.tabs === 'building-2,user' && settings.bell && settings.signout, 'settings icons: ' + JSON.stringify(settings));
  await audit('settings-personal');
  await shot('settings');
  await dom('#tab-workspace');
  await sleep(200);
  await audit('settings-workspace');

  // 7. the verdict
  if (allBad.length) {
    console.error(JSON.stringify(allBad, null, 1));
    throw new Error(allBad.length + ' chrome icon defects (first: ' + allBad[0].why + ' in ' + allBad[0].in + ')');
  }
  console.log('icons audited: ' + total + ' across ' + seenNames.size + ' names');
  await browser.close();
  console.log('ICONS_CHECK_OK');
})().catch((e) => { console.error(e); process.exit(1); });
