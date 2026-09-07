// E2E for the one settings place (task 09): /settings has a Workspace tab and
// a Personal tab. An admin renames the workspace and regenerates the invite
// code; a member sees the Workspace tab read-only; Personal changes the
// password, the avatar and a notification pref; the profile modal carries no
// settings any more, and the room page has exactly two ways into /settings
// (the sidebar entry and the workspace menu item).
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 OUT=<dir> node scripts/settings-check.js
const fs = require('fs');
const path = require('path');
const os = require('os');
const puppeteer = require('puppeteer-core');
const { newRoom, enterAs, loginPage, enterWithCode, openSettings, openInviteModal, backToRoom, createRoom, switchTo, call, PASSWORD, sleep } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';
const OUT = process.env.OUT || 'tmp';

const assert = (cond, msg) => { if (!cond) throw new Error(msg); };
async function api(p, opts = {}) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, opts.token ? { Authorization: 'Bearer ' + opts.token } : {});
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + p, { method: opts.method || 'GET', headers, body: opts.body ? JSON.stringify(opts.body) : undefined });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 429) { await sleep(3000); return api(p, opts); }
  if (resp.status >= 400) { const e = new Error(p + ' -> ' + resp.status + ' ' + JSON.stringify(data)); e.status = resp.status; throw e; }
  return data;
}
// a 1x1 png for the avatar upload
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==', 'base64');

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const room = await newRoom(SERVER, 'settings check');
  const slug = room.room.slug;
  const browser = await puppeteer.launch({
    executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  page.on('dialog', (d) => d.accept());
  const adminSession = await enterAs(page, SERVER, slug, room.invite_code, 'Alice');

  // 1. the room page has two doors into settings: the workspace menu item and the
  // personal menu under the profile row; nothing else (the old sidebar button is gone)
  await page.waitForSelector('#ws-switcher-wrap:not(.hidden)', { timeout: 8000 });
  // the hidden password banner also links to /settings; it is not a door
  const doors = await page.$$eval('a[href^="/settings"]', (els) => els.filter((el) => !el.closest('#pw-banner')).map((el) => el.id || el.className));
  assert(doors.length === 2 && doors.includes('ws-item') && doors.includes('me-settings'), 'settings doors: ' + JSON.stringify(doors));
  await page.click('#ws-switcher');
  await page.waitForSelector('#ws-menu:not(.hidden)', { timeout: 4000 });
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle2' }), page.click('#ws-menu a[href^="/settings"]')]);
  await page.waitForSelector('#settings-view:not(.hidden)', { timeout: 8000 });
  assert(await page.$eval('#tab-personal', (el) => el.classList.contains('active')), 'Personal is the default tab');

  // 2. Workspace tab as admin: rename, and the sidebar follows
  await page.click('#tab-workspace');
  await page.waitForSelector('#ws-panel:not(.hidden)', { timeout: 8000 });
  assert(!(await page.$eval('#ws-name', (el) => el.disabled)), 'admin can edit the name');
  // section order mirrors Personal: the logo first, then the text fields (Maya, reply 031f200a)
  const order = await page.$$eval('#ws-panel h2', (hs) => hs.filter((h) => h.offsetParent).map((h) => h.textContent.trim()));
  assert(order.slice(0, 3).join(',') === 'Logo,Name,Link', 'workspace sections start Logo, Name, Link: ' + order.join(','));
  const logoAboveName = await page.evaluate(() => document.getElementById('ws-avatar-row').getBoundingClientRect().top < document.getElementById('ws-name').getBoundingClientRect().top);
  assert(logoAboveName, 'the logo row sits above the name field');
  assert(await page.$eval('#ws-slug', (el) => el.value).then((v) => v.endsWith('/w/' + slug)), 'link shows the slug');
  await page.$eval('#ws-name', (el) => { el.value = ''; });
  await page.type('#ws-name', 'Renamed by settings');
  await page.click('#ws-name-save');
  await page.waitForSelector('#ws-name-ok:not(.hidden)', { timeout: 8000 });
  await page.screenshot({ path: path.join(OUT, 'settings-workspace.png') });
  await backToRoom(page);
  assert((await page.$eval('#ws-current', (el) => el.textContent)) === 'Renamed by settings', 'sidebar did not follow the rename');

  // 3. invite links live in the workspace menu: New link mints one, Revoke
  // kills the original, so the old code is dead and the new link enters
  await openInviteModal(page);
  await page.waitForSelector('#invite-list .invite-item', { timeout: 8000 });
  assert((await page.$$eval('#invite-list .invite-item', (els) => els.map((e) => e.dataset.url))).join() === room.invite, 'first link is not the workspace link');
  await page.click('#invite-new-submit');
  await page.waitForFunction(() => document.querySelectorAll('#invite-list .invite-item').length === 2, { timeout: 8000 });
  const newLink = await page.$$eval('#invite-list .invite-item', (els) => els[0].dataset.url); // newest first
  assert(/\/join\/inv-/.test(newLink) && newLink !== room.invite, 'new link: ' + newLink);
  await page.screenshot({ path: path.join(OUT, 'invite-links.png') });
  // revoke the original workspace link (the list is newest first)
  await page.$$eval('#invite-list .invite-item', (els, u) => els.find((e) => e.dataset.url === u).querySelector('.invite-revoke').click(), room.invite);
  await page.waitForFunction(() => document.querySelectorAll('#invite-list .invite-item').length === 1, { timeout: 8000 });
  await page.keyboard.press('Escape');
  const bobCtx = await browser.createBrowserContext();
  const bob = await bobCtx.newPage();
  await loginPage(bob, SERVER, undefined, { displayName: 'Bob', next: '/r/' + slug });
  await bob.waitForSelector('#enter-view:not(.hidden)', { timeout: 8000 });
  await bob.type('#enter-code', room.invite_code);
  await bob.click('#enter-form button[type=submit]');
  await bob.waitForSelector('#enter-error:not(.hidden)', { timeout: 8000 });
  assert(/revoked/.test(await bob.$eval('#enter-error', (el) => el.textContent)), 'revoked link error text');
  await enterWithCode(bob, newLink);

  // 4. a member sees the Workspace tab read-only
  await openSettings(bob, SERVER, 'workspace');
  await bob.waitForSelector('#ws-panel:not(.hidden)', { timeout: 8000 });
  assert(await bob.$eval('#ws-name', (el) => el.disabled), 'member must not edit the name');
  assert(await bob.$eval('#ws-name-save', (el) => el.classList.contains('hidden')), 'member has no Rename button');
  assert(!(await bob.$('#ws-invite')), 'the settings invite block is gone');
  await bob.screenshot({ path: path.join(OUT, 'settings-workspace-member.png') });

  // 5. Personal: avatar upload and remove, a notification pref, the password
  await openSettings(page, SERVER, 'workspace');
  await page.click('#tab-personal');
  await page.waitForSelector('#avatar-section:not(.hidden)', { timeout: 8000 });
  assert((await page.$eval('#avatar-ws-name', (el) => el.textContent)) === 'Renamed by settings', 'avatar section names the workspace');
  const tmp = path.join(os.tmpdir(), 'settings-check-avatar.png');
  fs.writeFileSync(tmp, PNG);
  const input = await page.$('#avatar-input');
  await input.uploadFile(tmp);
  await page.waitForFunction(() => ((document.querySelector('#settings-avatar img') || {}).getAttribute('src') || '').startsWith('blob:'), { timeout: 8000 });
  await page.waitForSelector('#avatar-remove:not(.hidden)', { timeout: 8000 });
  let me = await api('/api/v1/me', { token: adminSession, slug });
  assert(me.avatar_attachment_id, 'avatar not stored');
  await page.click('#avatar-remove');
  await page.waitForFunction(() => (document.querySelector('#settings-avatar img') || {}).getAttribute('src') === '/brand/avatar-default-512.png', { timeout: 8000 });
  me = await api('/api/v1/me', { token: adminSession, slug });
  assert(!me.avatar_attachment_id, 'avatar not removed');
  await page.click('#notify-sound');
  await page.waitForFunction(() => !document.querySelector('#notify-sound').checked, { timeout: 5000 });
  await sleep(300);
  const prefs = await api('/api/v1/me/notifications', { token: adminSession, slug });
  assert(prefs.sound === false, 'sound pref not saved: ' + JSON.stringify(prefs));
  await page.screenshot({ path: path.join(OUT, 'settings-personal.png') });

  // 5b. shadcn-style controls, drawn inline: the checkboxes are switches with
  // no native look, the selects hide the native arrow behind a Lucide chevron,
  // every section label has one weight, every button one outline style, and
  // the workspace mute block is gone from Personal (Maya, msg d4c52ff2)
  const look = await page.evaluate(() => {
    const cs = (el) => getComputedStyle(el);
    const boxes = [...document.querySelectorAll('#settings-personal input[type=checkbox]')];
    const selects = [...document.querySelectorAll('#settings-personal select')];
    const heads = [...document.querySelectorAll('#settings-personal h2')].filter((h) => h.offsetParent);
    const buttons = [...document.querySelectorAll('#settings-personal button, #settings-personal .upload-btn')].filter((b) => b.offsetParent);
    return {
      muteBlock: !!document.getElementById('ws-mute-section') || !!document.getElementById('ws-mute'),
      boxes: boxes.map((b) => ({ id: b.id, role: b.getAttribute('role'), appearance: cs(b).appearance, w: b.getBoundingClientRect().width, radius: cs(b).borderRadius })),
      selects: selects.map((s) => ({ id: s.id, appearance: cs(s).appearance, border: cs(s).borderWidth, chevron: !!(s.parentElement.querySelector('svg.lucide[data-icon="chevron-down"]')) })),
      headWeights: [...new Set(heads.map((h) => cs(h).fontWeight + '/' + cs(h).fontSize + '/' + cs(h).textTransform))],
      buttons: buttons.map((b) => ({ id: b.id || b.className, border: cs(b).borderWidth + ' ' + cs(b).borderStyle, bg: cs(b).backgroundColor, size: cs(b).fontSize })),
    };
  });
  assert(!look.muteBlock, 'the workspace mute block is still in Personal');
  assert(look.boxes.length === 2 && look.boxes.every((b) => b.role === 'switch' && b.appearance === 'none' && b.w === 36 && b.radius === '999px'), 'switches: ' + JSON.stringify(look.boxes));
  assert(look.selects.length === 2 && look.selects.every((s) => s.appearance === 'none' && s.border === '1px' && s.chevron), 'selects: ' + JSON.stringify(look.selects));
  assert(look.headWeights.length === 1 && /^600\/13px\/uppercase$/.test(look.headWeights[0]), 'section labels differ: ' + JSON.stringify(look.headWeights));
  assert(look.buttons.length >= 3 && look.buttons.every((b) => b.border === '1px solid' && b.bg === 'rgba(0, 0, 0, 0)' && b.size === '13px'), 'buttons: ' + JSON.stringify(look.buttons));
  // the switch is still a real checkbox: space toggles it and the pref saves
  await page.focus('#notify-sound');
  await page.keyboard.press('Space');
  await page.waitForFunction(() => document.querySelector('#notify-sound').checked, { timeout: 5000 });
  await sleep(300);
  assert((await api('/api/v1/me/notifications', { token: adminSession, slug })).sound === true, 'switch did not save');

  await page.type('#pw-current', PASSWORD);
  await page.type('#pw-new', 'a brand new passphrase');
  await page.type('#pw-confirm', 'a brand new passphrase');
  await page.click('#pw-form button[type=submit]');
  await page.waitForSelector('#pw-ok:not(.hidden)', { timeout: 8000 });

  // 6. the profile modal carries no settings any more
  await backToRoom(page);
  // the profile row is a compact footer, ~80% of its old size (Maya, dec00282):
  // 44px row, 32px avatar, 13px name, 9px dot (13px with its border)
  await page.waitForSelector('#me-footer .me-name', { timeout: 8000 });
  const foot = await page.evaluate(() => {
    const b = (s) => { const el = document.querySelector(s); const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return { w: Math.round(r.width), h: Math.round(r.height), font: cs.fontSize, pad: cs.padding, radius: cs.borderRadius }; };
    return { footer: b('#me-footer'), avatar: b('#me-footer .avatar-me'), dot: b('#me-footer .me-dot'), name: b('#me-footer .me-name') };
  });
  assert(foot.footer.h <= 46 && foot.footer.pad === '6px 14px' && foot.avatar.w === 32 && foot.avatar.h === 32 && foot.avatar.radius === '8px' && foot.dot.w === 13 && foot.name.font === '13px', 'profile footer sizes: ' + JSON.stringify(foot));
  await page.click('#me-footer');
  await page.waitForSelector('#me-menu:not(.hidden)', { timeout: 5000 });
  const meItems = await page.$$eval('#me-menu .ws-item', (els) => els.map((e) => e.textContent.trim()));
  assert(meItems.join(',') === 'View profile,Settings,Sign out', 'personal menu items: ' + meItems.join(','));
  await page.click('#me-profile');
  await page.waitForSelector('#profile-modal:not(.hidden)', { timeout: 5000 });
  const leftovers = await page.$$eval('#profile-card #notify-settings, #profile-card #profile-actions, #profile-card select, #profile-card input', (els) => els.length);
  assert(leftovers === 0, 'profile modal still has settings controls: ' + leftovers);

  // 6b. the mute moved to the workspace: the workspace-name dropdown toggles it
  // (the rail's context menu has it too, railorder-check covers that)
  await page.click('#profile-close');
  await page.waitForSelector('#profile-modal.hidden', { timeout: 4000 });
  await page.click('#ws-switcher');
  await page.waitForSelector('#ws-menu-mute', { timeout: 4000 });
  const wsItems = await page.$$eval('#ws-menu .ws-item', (els) => els.map((e) => e.textContent.trim()));
  assert(wsItems.join(',') === 'Invite member,Join with invite link,Mute workspace,Settings', 'workspace menu items: ' + wsItems.join(','));
  await page.click('#ws-menu-mute');
  await page.waitForFunction((s) => document.querySelector('#rail-list .rail-item[data-slug="' + s + '"]').classList.contains('is-muted'), { timeout: 5000 }, slug);
  await sleep(300);
  const wsList = (await api('/api/v1/user', { token: adminSession })).workspaces;
  assert(wsList.find((w) => w.slug === slug).muted === true, 'mute did not persist');
  await page.click('#ws-switcher');
  await page.waitForFunction(() => (document.querySelector('#ws-menu-mute') || {}).textContent === 'Unmute workspace', { timeout: 4000 });
  await page.screenshot({ path: path.join(OUT, 'ws-menu-mute.png') });
  await page.click('#ws-menu-mute');
  await page.waitForFunction((s) => !document.querySelector('#rail-list .rail-item[data-slug="' + s + '"]').classList.contains('is-muted'), { timeout: 5000 }, slug);

  // 6c. REGRESSION (Maya): with two workspaces, Settings > Workspace must show and
  // edit the workspace that is focused in the rail, not the first one. The menu
  // link used to bake ?next= in at mount time, before the switch pushed its URL.
  const second = await createRoom(SERVER, adminSession, 'Second workspace');
  const slug2 = second.room.slug;
  await page.reload({ waitUntil: 'networkidle2' });
  await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 8000 });
  await switchTo(page, slug2);
  await page.click('#ws-switcher');
  await page.waitForSelector('#ws-menu:not(.hidden)', { timeout: 4000 });
  // the href attribute itself must be right once the menu is open (middle-click, copy link address)
  const liveHref = await page.$eval('#ws-menu a[href^="/settings"]', (a) => a.getAttribute('href'));
  assert(decodeURIComponent(liveHref).includes('next=/w/' + slug2), 'open menu holds a stale settings href: ' + liveHref);
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle2' }), page.click('#ws-menu a[href^="/settings"]')]);
  await page.waitForSelector('#settings-view:not(.hidden)', { timeout: 8000 });
  assert(new URL(page.url()).searchParams.get('next').startsWith('/w/' + slug2), 'settings link points at the wrong workspace: ' + page.url());
  await page.click('#tab-workspace');
  await page.waitForSelector('#ws-panel:not(.hidden)', { timeout: 8000 });
  await page.waitForFunction(() => document.getElementById('ws-name').value !== '', { timeout: 8000 });
  assert((await page.$eval('#ws-name', (el) => el.value)) === 'Second workspace', 'Workspace tab shows the wrong workspace: ' + await page.$eval('#ws-name', (el) => el.value));
  assert(await page.$eval('#ws-slug', (el) => el.value).then((v) => v.endsWith('/w/' + slug2)), 'link shows the wrong slug');
  await page.$eval('#ws-name', (el) => { el.value = ''; });
  await page.type('#ws-name', 'Second renamed');
  await page.click('#ws-name-save');
  await page.waitForSelector('#ws-name-ok:not(.hidden)', { timeout: 8000 });
  const wsHeaders = (s) => ({ 'X-Workspace-Slug': s });
  const first = await call(SERVER, '/api/v1/room', { token: adminSession, headers: wsHeaders(slug) });
  const renamed = await call(SERVER, '/api/v1/room', { token: adminSession, headers: wsHeaders(slug2) });
  assert(renamed.room.name === 'Second renamed', 'Save did not rename the second workspace: ' + JSON.stringify(renamed));
  assert(first.room.name === 'Renamed by settings', 'Save touched the first workspace: ' + JSON.stringify(first));
  // the personal menu's Settings door is bound the same way: two warm switches
  // without a reload, so the footer mounted while the URL still named the first
  await backToRoom(page);
  await switchTo(page, slug);
  await switchTo(page, slug2);
  const meHref = await page.evaluate(() => { document.getElementById('me-footer').click(); return document.getElementById('me-settings').getAttribute('href'); });
  assert(decodeURIComponent(meHref).includes('next=/w/' + slug2), 'personal Settings link points at the wrong workspace: ' + meHref);
  await page.keyboard.press('Escape');

  // 7. sign out from Personal lands on /login
  await openSettings(page, SERVER);
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle2' }), page.click('#settings-signout')]);
  assert(new URL(page.url()).pathname === '/login', 'sign out did not land on /login: ' + page.url());

  await browser.close();
  console.log('SETTINGS_CHECK_OK');
})().catch((e) => { console.error(e); process.exit(1); });
