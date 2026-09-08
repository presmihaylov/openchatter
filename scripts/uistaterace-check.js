// Async modal data must remain owned by the profile/channel that requested it.
// A delayed profile response cannot repaint a newer profile, and a delayed
// member-list response cannot open after channel navigation.
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const access = process.env.ACCESS_ID ? {
  'CF-Access-Client-Id': process.env.ACCESS_ID,
  'CF-Access-Client-Secret': process.env.ACCESS_SECRET,
} : {};
const tag = Date.now().toString(36).slice(-7);
const roomName = 'ui state race check ' + tag;
let session;
let slug;

const api = async (path, opts = {}) => {
  const headers = Object.assign({}, access, opts.body ? { 'Content-Type': 'application/json' } : {});
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const response = await fetch(SERVER + path, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(path + ' -> ' + response.status + ' ' + JSON.stringify(data));
  return data;
};

(async () => {
  let browser;
  let ownsBrowser = false;
  let page;
  try {
    session = (await api('/api/v1/auth/password/register', {
      method: 'POST', body: { username: 'ui-race-' + tag, password: 'correct horse battery', display_name: 'UI Race Check' },
    })).token;
    const made = await api('/api/v1/rooms', {
      method: 'POST', token: session, body: { name: roomName, slug: 'ui-state-race-' + tag },
    });
    slug = made.room.slug;
    const invite = made.invite || made.join_url || made.invite_code;
    const agentA = await api('/api/v1/rooms/join', {
      method: 'POST', status: 201, body: { invite, name: 'delivery-a-' + tag, description: 'first profile' },
    });
    const agentB = await api('/api/v1/rooms/join', {
      method: 'POST', status: 201, body: { invite, name: 'delivery-b-' + tag, description: 'second profile' },
    });
    const channels = await api('/api/v1/channels', { token: session, slug });
    const general = channels.channels.find((channel) => channel.name === 'general');
    const side = await api('/api/v1/channels', {
      method: 'POST', token: session, slug, body: { name: 'side' },
    });

    if (process.env.BROWSER_URL) browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
      });
      ownsBrowser = true;
    }
    page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 720 });
    await page.setExtraHTTPHeaders(access);
    const errors = [];
    page.on('pageerror', (error) => errors.push('pageerror: ' + error.message));
    page.on('console', (message) => { if (message.type() === 'error') errors.push('console: ' + message.text()); });
    await page.goto(BROWSER_SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), session);
    await page.goto(BROWSER_SERVER + '/w/' + slug + '/c/general', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.$$eval('#participant-list .p-toggle[data-state="collapsed"]', (toggles) => toggles.forEach((toggle) => toggle.click()));
    await page.waitForSelector('#participant-list li[data-id="' + agentA.participant.id + '"]', { timeout: 10000 });

    await page.setRequestInterception(true);
    let releaseProfile;
    let profileResolve;
    let releaseMembers;
    let membersResolve;
    const profileReady = new Promise((resolve) => { profileResolve = resolve; });
    const membersReady = new Promise((resolve) => { membersResolve = resolve; });
    page.on('request', (request) => {
      const path = new URL(request.url()).pathname;
      if (!releaseProfile && path === '/api/v1/participants/' + agentA.participant.id + '/delivery') {
        releaseProfile = () => request.respond({
          status: 200, contentType: 'application/json',
          body: JSON.stringify({ acked: 77, delivered: 66, deferred: 55, failed: 44, accepted: 33 }),
        });
        profileResolve();
        return;
      }
      if (!releaseMembers && path === '/api/v1/channels/' + general.id + '/members') {
        api('/api/v1/channels/' + general.id + '/members', { token: session, slug }).then((snapshot) => {
          releaseMembers = () => request.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(snapshot) });
          membersResolve();
        }, (error) => request.abort().then(() => { throw error; }));
        return;
      }
      request.continue().catch(() => {});
    });

    // Profile A's delayed delivery stats must not overwrite profile B.
    await page.evaluate((id) => document.querySelector('#participant-list li[data-id="' + id + '"]').click(), agentA.participant.id);
    await profileReady;
    await page.evaluate((id) => document.querySelector('#participant-list li[data-id="' + id + '"]').click(), agentB.participant.id);
    await page.waitForFunction((name) => document.querySelector('#profile-name').textContent === name, {}, agentB.participant.name);
    await page.waitForSelector('#profile-delivery:not(.hidden)', { timeout: 10000 });
    await releaseProfile();
    await new Promise((resolve) => setTimeout(resolve, 300));
    const profile = await page.evaluate(() => ({
      name: document.querySelector('#profile-name').textContent,
      delivery: document.querySelector('#profile-delivery').textContent,
    }));
    await page.click('#profile-close');

    // A members click belongs to general. If the user moves to side while its
    // request is delayed, the completed request must not open a side modal.
    await page.evaluate(() => document.querySelector('#members-btn').click());
    await membersReady;
    await page.evaluate(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .find((node) => node.textContent === 'side').parentElement.click());
    await page.waitForFunction(() => document.querySelector('#channel-title').textContent.trim() === 'side');
    await releaseMembers();
    await new Promise((resolve) => setTimeout(resolve, 300));
    const members = await page.evaluate(() => ({
      modalVisible: !document.querySelector('#members-modal').classList.contains('hidden'),
      title: document.querySelector('#members-title').textContent,
    }));
    await page.evaluate((id) => document.querySelector('#participant-list li[data-id="' + id + '"]').click(), agentB.participant.id);
    await page.waitForSelector('#profile-modal:not(.hidden)');
    await page.keyboard.press('Escape');
    const profileClosedByEscape = await page.$eval('#profile-modal', (modal) => modal.classList.contains('hidden'));
    console.log(JSON.stringify({ profile, members, profileClosedByEscape }));
    if (profile.name !== agentB.participant.name || profile.delivery.includes('77 acked')) {
      throw new Error('stale delivery stats repainted a newer profile');
    }
    if (members.modalVisible) throw new Error('stale member request opened after channel navigation');
    if (!profileClosedByEscape) throw new Error('Escape did not close the profile');
    if (errors.length) throw new Error('browser errors: ' + errors.join(' | '));
    console.log('UISTATERACE_CHECK_OK');
  } finally {
    if (page) await page.close().catch(() => {});
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (session && slug) await api('/api/v1/room', {
      method: 'DELETE', token: session, slug, body: { name: roomName },
    }).catch(() => {});
  }
})().catch((error) => { console.error('UISTATERACE_CHECK_FAIL:', error.message); process.exit(1); });
