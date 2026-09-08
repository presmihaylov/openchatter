// Focused-tab notification regression: a human's involved threads sound on
// every reply, both in the visible channel and elsewhere. Top-level traffic
// keeps its existing focus guard and burst debounce. Works locally or against
// a Cloudflare-gated deployment, and cleans up its temporary workspace.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/threadnotify-check.js
const puppeteer = require('puppeteer-core');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const ACCESS_ID = process.env.ACCESS_ID || '';
const ACCESS_SECRET = process.env.ACCESS_SECRET || '';
const access = ACCESS_ID ? {
  'CF-Access-Client-Id': ACCESS_ID,
  'CF-Access-Client-Secret': ACCESS_SECRET,
} : {};
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
const tag = () => Date.now().toString(36).slice(-7) + Math.floor(Math.random() * 1e6).toString(36);

async function request(path, opts = {}, retries = 3) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, access);
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  if (opts.slug) headers['X-Workspace-Slug'] = opts.slug;
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 429 && retries > 0) {
    await sleep(3000);
    return request(path, opts, retries - 1);
  }
  return { status: resp.status, data };
}

async function must(path, opts = {}, status = 200) {
  const out = await request(path, opts);
  if (out.status !== status) {
    throw new Error(path + ' -> ' + out.status + ' ' + (out.data.error || out.data.code || 'unexpected response'));
  }
  return out.data;
}

(async () => {
  let browser;
  let ownsBrowser = false;
  let humanSession;
  let slug;
  let roomName;
  try {
    const run = tag();
    const humanName = 'notifyhuman-' + run;
    const agentName = 'notifyagent-' + run;
    const registered = await must('/api/v1/auth/password/register', {
      method: 'POST',
      body: { username: 'notify-' + run, password: 'correct horse battery', display_name: humanName },
    }, 201);
    humanSession = registered.token;
    roomName = 'thread notify check ' + run;
    const made = await must('/api/v1/rooms', {
      method: 'POST', token: humanSession,
      body: { name: roomName, slug: 'thread-notify-' + run },
    }, 201);
    slug = made.room.slug;

    const invite = await must('/api/v1/invites', {
      method: 'POST', token: humanSession, slug, body: { bind_owner: true },
    }, 201);
    const joined = await must('/api/v1/rooms/join', {
      method: 'POST', body: { invite: invite.join_url, name: agentName, description: 'notification test agent' },
    }, 201);
    const agentToken = joined.token;
    const channels = (await must('/api/v1/channels', { token: humanSession, slug })).channels;
    const general = channels.find((ch) => ch.name === 'general');
    const plaza = await must('/api/v1/channels', {
      method: 'POST', token: humanSession, slug, body: { name: 'plaza' },
    }, 201);
    await must('/api/v1/channels/' + plaza.id + '/join', { method: 'POST', token: agentToken });

    const post = (channel, token, body, extra = {}) => must('/api/v1/channels/' + channel + '/messages', {
      method: 'POST', token, slug, body: Object.assign({ body }, extra),
    }, 201);
    const agentPost = (channel, body, extra) => post(channel, agentToken, body, extra);

    if (process.env.BROWSER_URL) {
      browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    } else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
      });
      ownsBrowser = true;
    }
    await browser.defaultBrowserContext().overridePermissions(SERVER, ['notifications']);
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 850 });
    await page.setExtraHTTPHeaders(access);
    await page.evaluateOnNewDocument(() => {
      window.__notes = [];
      document.addEventListener('agentchat:notify', (ev) => window.__notes.push(ev.detail));
    });
    await page.goto(SERVER + '/login', { waitUntil: 'networkidle2' });
    await page.evaluate((token) => localStorage.setItem('agentchat:session', token), humanSession);
    await page.goto(SERVER + '/w/' + slug, { waitUntil: 'networkidle2' });
    await page.waitForSelector('#chat-view:not(.hidden)', { timeout: 10000 });
    await page.waitForSelector('#composer-input', { timeout: 10000 });
    await page.bringToFront();
    await page.click('#composer-input');
    await sleep(150);
    assert(await page.evaluate(() => navigator.userActivation.hasBeenActive), 'first pointer interaction did not activate audio');
    assert(await page.evaluate(() => document.hasFocus()), 'test page is not focused');

    const drain = () => page.evaluate(() => window.__notes.splice(0));
    const waitForNotes = (count) => page.waitForFunction((n) => window.__notes.length >= n, { timeout: 10000 }, count);
    const waitForMessage = (body) => page.waitForFunction((text) => (
      [...document.querySelectorAll('#messages .msg')].some((row) => row.textContent.includes(text))
    ), { timeout: 10000 }, body);
    const assertPlayed = (notes, count, label) => {
      assert(notes.length === count, label + ' count: ' + JSON.stringify(notes));
      assert(notes.every((n) => n.sound && n.soundPlayed && n.audioState === 'running'), label + ' audio: ' + JSON.stringify(notes));
    };

    // Existing behavior: top-level traffic in the focused channel is silent.
    await agentPost(general.id, 'focused top level');
    await waitForMessage('focused top level');
    await sleep(500);
    assert((await drain()).length === 0, 'focused top-level post made a notification');

    // The human authored this thread. Every agent reply sounds even though the
    // tab and its parent channel are focused.
    const ownRoot = await post(general.id, humanSession, 'human-owned thread');
    await waitForMessage('human-owned thread');
    await sleep(300);
    assert((await drain()).length === 0, 'own root made a notification');
    await agentPost(general.id, 'focused thread reply 1', { thread_root_id: ownRoot.id });
    await waitForNotes(1);
    let notes = await drain();
    assertPlayed(notes, 1, 'focused involved thread');
    assert(notes[0].key === ownRoot.id && notes[0].why === 'thread', 'wrong focused thread notification: ' + JSON.stringify(notes));

    const threadState = (await must('/api/v1/threads', { token: humanSession, slug })).threads
      .find((thread) => thread.root_id === ownRoot.id);
    assert(threadState && threadState.unread_count >= 1, 'focused parent channel erased thread unread state');
    await page.waitForFunction((title) => [...document.querySelectorAll('.thread-leaf.unread')]
      .some((row) => row.title.includes(title)), { timeout: 10000 }, 'human-owned thread');

    for (let i = 2; i <= 5; i++) {
      await agentPost(general.id, 'focused thread reply ' + i, { thread_root_id: ownRoot.id });
    }
    await waitForNotes(4);
    notes = await drain();
    assertPlayed(notes, 4, 'rapid involved-thread replies');
    assert(notes.every((n) => n.key === ownRoot.id), 'rapid replies used the wrong key: ' + JSON.stringify(notes));

    // The same involved thread sounds while another channel is on screen.
    await page.evaluate(() => [...document.querySelectorAll('#channel-list .chan-name')]
      .find((name) => name.textContent === 'plaza').parentElement.click());
    await page.waitForFunction(() => document.querySelector('#channel-title').textContent.includes('plaza'), { timeout: 10000 });
    await agentPost(general.id, 'other-channel thread reply', { thread_root_id: ownRoot.id });
    await waitForNotes(1);
    notes = await drain();
    assertPlayed(notes, 1, 'other-channel involved thread');

    // A direct mention enrolls the human in an agent-authored thread; the
    // mention and the immediately-following plain reply each sound while its
    // parent channel is focused.
    const mentionedRoot = await agentPost(plaza.id, 'agent-owned thread');
    await waitForMessage('agent-owned thread');
    await sleep(400);
    assert((await drain()).length === 0, 'focused top-level thread root made a notification');
    await agentPost(plaza.id, 'hello @' + humanName, { thread_root_id: mentionedRoot.id });
    await waitForNotes(1);
    notes = await drain();
    assertPlayed(notes, 1, 'thread mention');
    await agentPost(plaza.id, 'plain reply after mention', { thread_root_id: mentionedRoot.id });
    await waitForNotes(1);
    notes = await drain();
    assertPlayed(notes, 1, 'thread reply after mention');

    // Existing behavior: top-level traffic in a different channel still uses
    // the three-second quiet window.
    await agentPost(general.id, 'channel burst 1');
    await agentPost(general.id, 'channel burst 2');
    await waitForNotes(1);
    await sleep(700);
    notes = await drain();
    assertPlayed(notes, 1, 'top-level channel debounce');
    assert(notes[0].why === 'channel', 'top-level burst reason: ' + JSON.stringify(notes));

    console.log('THREADNOTIFY_CHECK_OK');
  } finally {
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (humanSession && slug && roomName) {
      await request('/api/v1/room', { method: 'DELETE', token: humanSession, slug, body: { name: roomName } });
    }
  }
})().catch((err) => {
  console.error('THREADNOTIFY_CHECK_FAIL:', err.message);
  process.exit(1);
});
