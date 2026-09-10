// Real-browser + real served-watcher regression for the agent routing contract.
// A browser human's untagged thread reply wakes the participating test agent;
// an untagged agent reply and the agent's own reply do not. Direct mentions and
// root broadcasts still wake, and OPENCHATTER_HUMAN_THREAD_REPLIES=0 disables
// only the human-thread branch.
// Run: NODE_PATH=<dir with puppeteer-core> SERVER=http://localhost:8095 node scripts/humanthreadwatch-check.js
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');
const puppeteer = require('puppeteer-core');
const { newRoom, openAsHuman, sleep } = require('./lib/login.js');

const SERVER = (process.env.SERVER || 'http://localhost:8095').replace(/\/$/, '');
const BROWSER_SERVER = (process.env.BROWSER_SERVER || SERVER).replace(/\/$/, '');
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

async function api(route, opts = {}) {
  const headers = { 'Content-Type': 'application/json' };
  if (opts.token) headers.Authorization = 'Bearer ' + opts.token;
  const resp = await fetch(SERVER + route, {
    method: opts.method || 'GET', headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(route + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

async function waitFor(predicate, label, timeout = 10000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (await predicate()) return;
    await sleep(100);
  }
  throw new Error('timed out waiting for ' + label);
}

function startWatcher(home, scriptPath) {
  const lines = [];
  let partial = '';
  const child = spawn('sh', [scriptPath], {
    env: Object.assign({}, process.env, { HOME: home }),
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const collect = (chunk) => {
    partial += chunk.toString();
    const split = partial.split('\n');
    partial = split.pop();
    lines.push(...split);
  };
  child.stdout.on('data', collect);
  child.stderr.on('data', collect);
  return { child, lines };
}

async function stopWatcher(run) {
  if (run.child.exitCode !== null) return;
  run.child.kill('SIGTERM');
  await Promise.race([
    new Promise((resolve) => run.child.once('exit', resolve)),
    sleep(3000).then(() => run.child.kill('SIGKILL')),
  ]);
}

(async () => {
  let browser;
  let ownsBrowser = false;
  let watcher;
  let temp;
  try {
    const created = await newRoom(SERVER, 'human thread watcher check');
    const join = (name, isHuman = false) => api('/api/v1/rooms/join', {
      method: 'POST', body: { invite_code: created.invite_code, name, is_human: isHuman },
    });
    const watched = await join('watch-agent');
    const peer = await join('peer-agent');
    const human = await join('browser-human', true);
    const say = (token, body, extra = {}) => api('/api/v1/channels/general/messages', {
      method: 'POST', token, body: Object.assign({ body }, extra),
    });
    const root = await say(watched.token, 'watch-agent thread root');

    temp = fs.mkdtempSync(path.join(os.tmpdir(), 'openchatter-human-thread-'));
    const baseDir = path.join(temp, '.openchatter');
    fs.mkdirSync(baseDir, { mode: 0o700 });
    const base = path.join(baseDir, 'watch-agent');
    let script = await (await fetch(SERVER + '/skill/watch.sh')).text();
    script = script.replace('ME="<your-name>"', 'ME="watch-agent"');
    script = script.replace(
      'BASE="$HOME/.openchatter/<room-slug>.<your-name-with-dashes>"',
      'BASE="$HOME/.openchatter/watch-agent"',
    );
    assert(!script.includes('<your-name>') && !script.includes('<room-slug>'), 'watcher placeholders remain');
    const scriptPath = path.join(temp, 'watch.sh');
    fs.writeFileSync(scriptPath, script, { mode: 0o700 });
    fs.writeFileSync(base + '.env',
      'SERVER=' + SERVER + '\nTOKEN=' + watched.token + '\nOPENCHATTER_ACK_NAG_SECS=0\n',
      { mode: 0o600 });

    if (process.env.BROWSER_URL) {
      browser = await puppeteer.connect({ browserURL: process.env.BROWSER_URL });
    } else {
      browser = await puppeteer.launch({
        executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
        headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
      });
      ownsBrowser = true;
    }
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 850 });
    await openAsHuman(page, BROWSER_SERVER, created.room.slug, human);
    await page.goto(BROWSER_SERVER + '/r/' + created.room.slug + '/c/general/t/' + root.id, { waitUntil: 'networkidle2' });
    await page.waitForSelector('#thread-panel:not(.hidden) #thread-input', { timeout: 10000 });

    watcher = startWatcher(temp, scriptPath);
    await waitFor(() => watcher.lines.some((line) => line.startsWith('WATCHER-ONLINE:')), 'watcher online');
    assert(watcher.lines.some((line) => line.includes('version 2.5.0')), 'watcher version beacon missing');

    const replyLines = () => watcher.lines.filter((line) => line.startsWith('REPLY-TO '));
    const waitForReply = (id) => waitFor(() => replyLines().some((line) => line.includes(id)), 'wake for ' + id);
    const waitThrough = async (id) => {
      const latest = await api('/api/v1/events', { token: watched.token });
      await waitFor(() => {
        try { return Number(fs.readFileSync(base + '.cursor', 'utf8').trim()) >= latest.cursor; } catch (_) { return false; }
      }, 'watcher cursor after ' + id);
      await sleep(250);
    };

    const humanBody = 'browser human untagged reply';
    await page.type('#thread-input', humanBody);
    await page.keyboard.press('Enter');
    await page.waitForFunction((body) => document.querySelector('#thread-messages').textContent.includes(body),
      { timeout: 10000 }, humanBody);
    let humanMessage;
    await waitFor(async () => {
      const thread = await api('/api/v1/threads/' + root.id, { token: watched.token });
      humanMessage = thread.messages.find((message) => message.body === humanBody);
      return Boolean(humanMessage);
    }, 'browser human reply persistence');
    assert(humanMessage && humanMessage.author_kind === 'human', 'browser post lacks human author_kind');
    await waitForReply(humanMessage.id);
    assert(replyLines().filter((line) => line.includes(humanMessage.id)).length === 1, 'human reply double-woke');

    const agentReply = await say(peer.token, 'peer agent untagged reply', { thread_root_id: root.id });
    await waitThrough(agentReply.id);
    assert(!replyLines().some((line) => line.includes(agentReply.id)), 'untagged agent reply woke watcher');

    const ownReply = await say(watched.token, 'watch agent own reply', { thread_root_id: root.id });
    await waitThrough(ownReply.id);
    assert(!replyLines().some((line) => line.includes(ownReply.id)), 'own reply woke watcher');

    const mention = await say(peer.token, '@watch-agent direct wake');
    await waitForReply(mention.id);
    assert(replyLines().filter((line) => line.includes(mention.id)).length === 1, 'direct mention did not wake exactly once');

    const broadcast = await say(peer.token, '@channel root wake');
    await waitForReply(broadcast.id);
    assert(replyLines().filter((line) => line.includes(broadcast.id)).length === 1, 'root broadcast did not wake exactly once');

    await stopWatcher(watcher);
    watcher = undefined;
    fs.writeFileSync(base + '.env',
      'SERVER=' + SERVER + '\nTOKEN=' + watched.token + '\nOPENCHATTER_ACK_NAG_SECS=0\nOPENCHATTER_HUMAN_THREAD_REPLIES=0\n',
      { mode: 0o600 });
    watcher = startWatcher(temp, scriptPath);
    await waitFor(() => watcher.lines.some((line) => line.startsWith('WATCHER-ONLINE:')), 'toggle-off watcher online');
    assert(watcher.lines.some((line) => line.includes('human thread replies off')), 'toggle-off scope beacon missing');

    const disabledBody = 'browser human reply while disabled';
    await page.type('#thread-input', disabledBody);
    await page.keyboard.press('Enter');
    await page.waitForFunction((body) => document.querySelector('#thread-messages').textContent.includes(body),
      { timeout: 10000 }, disabledBody);
    let disabled;
    await waitFor(async () => {
      const after = await api('/api/v1/threads/' + root.id, { token: watched.token });
      disabled = after.messages.find((message) => message.body === disabledBody);
      return Boolean(disabled);
    }, 'disabled browser reply persistence');
    await waitThrough(disabled.id);
    assert(!replyLines().some((line) => line.includes(disabled.id)), 'toggle-off human reply woke watcher');

    console.log('HUMANTHREADWATCH_CHECK_OK');
  } finally {
    if (watcher) await stopWatcher(watcher);
    if (browser) {
      if (ownsBrowser) await browser.close();
      else browser.disconnect();
    }
    if (temp) fs.rmSync(temp, { recursive: true, force: true });
  }
})().catch((err) => {
  console.error('HUMANTHREADWATCH_CHECK_FAIL:', err.message);
  process.exit(1);
});
