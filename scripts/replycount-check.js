// Repro for the "N replies · Last reply" footer not updating live (task 29).
// A reply that lands while the channel is open must move the root's footer
// without a reload: the count on a root that already had one, and the footer
// appearing at all on a root that had none.
// Run: NODE_PATH=<dir with puppeteer-core> node scripts/replycount-check.js
const puppeteer = require('puppeteer-core');
const { newRoom, enterAs } = require('./lib/login.js');
const SERVER = process.env.SERVER || 'http://localhost:8095';

async function api(path, opts = {}) {
  const resp = await fetch(SERVER + path, {
    method: opts.method || 'GET',
    headers: Object.assign({ 'Content-Type': 'application/json' },
      opts.token ? { Authorization: 'Bearer ' + opts.token } : {},
      opts.ws ? { 'X-Workspace-Slug': opts.ws } : {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(path + ' -> ' + resp.status + ' ' + JSON.stringify(data));
  return data;
}

let step = 'start';
(async () => {
  const created = await newRoom(SERVER, 'replycount check');
  const slug = created.room.slug;
  const bot = await api('/api/v1/rooms/join', { method: 'POST', body: { invite_code: created.invite_code, name: 'countbot', description: 't', avatar: '🤖' } });
  const A = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'root with a thread' } });
  const B = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'root without one' } });

  const browser = await puppeteer.launch({
    executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage();
  const human = await enterAs(page, SERVER, slug, created.invite_code, 'humantester');

  const node = (id) => `[...document.querySelectorAll('#messages .msg')].find(n => n.dataset.id === ${JSON.stringify(id)})`;
  const barText = (id) => `((${node(id)} || {}).querySelector ? (${node(id)}.querySelector('button.reply-bar') || {}).textContent || '' : '')`;

  step = '1-both-roots';
  await page.waitForFunction(`${node(A.id)} && ${node(B.id)}`, { timeout: 8000 });

  // 1. a first reply gives a footer-less root its footer, live
  step = '2-first-reply';
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply one', thread_root_id: A.id } });
  await page.waitForFunction(`${barText(A.id)}.includes('1 reply')`, { timeout: 8000 });

  // 2. open the thread pane, the state Pres was in when he saw the bug
  step = '3-open-thread';
  await page.evaluate(`${node(A.id)}.querySelector('button.reply-bar').click()`);
  await page.waitForFunction(`document.querySelectorAll('#thread-messages .msg').length >= 2`, { timeout: 8000 });

  // 3. a second reply from the other client bumps the count on the root behind it
  step = '4-second-reply';
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply two', thread_root_id: A.id } });
  await page.waitForFunction(`${barText(A.id)}.includes('2 replies')`, { timeout: 8000 });
  if (!(await page.evaluate(`${barText(A.id)}.includes('Last reply')`)))
    throw new Error('no "Last reply" text on the bumped footer');

  // 4. same again for a root whose thread was never opened, thread pane still open
  step = '5-other-root';
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply to B', thread_root_id: B.id } });
  await page.waitForFunction(`${barText(B.id)}.includes('1 reply')`, { timeout: 8000 });

  // 5. the reported case: the reply lands while another channel is on screen,
  // so nothing patches the root's node. Coming back paints the cached page, and
  // a cache that ignored the reply shows no footer at all until a reload.
  step = '6-other-channel';
  const C = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'root read from cache' } });
  await page.waitForFunction(`${node(C.id)}`, { timeout: 8000 });
  await api('/api/v1/channels', { method: 'POST', token: human, ws: slug, body: { name: 'elsewhere' } });
  const clickChan = async (name) => {
    await page.waitForFunction(`[...document.querySelectorAll('#channel-list li')].some((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith(${JSON.stringify(name)}))`, { timeout: 8000 });
    await page.evaluate(`[...document.querySelectorAll('#channel-list li')].find((l) => ((l.querySelector('.chan-name') || {}).textContent || '').startsWith(${JSON.stringify(name)})).click()`);
    await page.waitForFunction(`(document.querySelector('#channel-title') || {}).textContent.includes(${JSON.stringify(name)})`, { timeout: 8000 });
  };
  await clickChan('elsewhere');
  step = '6-reply-offscreen';
  await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply to C', thread_root_id: C.id } });
  await new Promise((r) => setTimeout(r, 1500)); // the event lands with general off screen
  await clickChan('general');
  await page.waitForFunction(`${barText(C.id)}.includes('1 reply')`, { timeout: 8000 });

  // 6. and the footer must survive: no later render may put the stale root back
  step = '7-stays';
  await new Promise((r) => setTimeout(r, 2500));
  if (!(await page.evaluate(`${barText(C.id)}.includes('1 reply')`)))
    throw new Error('footer reverted to the stale count after settling');

  // 7. a re-entry refetches the page, and the feed cursor predates that fetch:
  // a reply the fetch already counted must not be counted a second time
  step = '8-no-double-count';
  await clickChan('elsewhere');
  await clickChan('general');
  await new Promise((r) => setTimeout(r, 1500));
  const cText = await page.evaluate(barText(C.id));
  if (!cText.includes('1 reply') || cText.includes('2 replies'))
    throw new Error('root C counted its reply twice after a refetch: ' + JSON.stringify(cText));
  const aText = await page.evaluate(barText(A.id));
  if (!aText.includes('2 replies'))
    throw new Error('root A lost or gained replies after a refetch: ' + JSON.stringify(aText));

  // 8. and the count moves down as well as up: deleting the only reply takes
  // the footer away live, and no repaint from cache brings it back
  step = '9-delete-reply';
  const doomed = await api('/api/v1/channels/general/messages', { method: 'POST', token: bot.token, body: { body: 'reply to B, doomed', thread_root_id: B.id } });
  await page.waitForFunction(`${barText(B.id)}.includes('2 replies')`, { timeout: 8000 });
  await api('/api/v1/messages/' + doomed.id, { method: 'DELETE', token: bot.token });
  await page.waitForFunction(`${barText(B.id)}.includes('1 reply')`, { timeout: 8000 });
  step = '9-delete-sticks';
  await clickChan('elsewhere');
  await clickChan('general');
  await new Promise((r) => setTimeout(r, 1500));
  const bText = await page.evaluate(barText(B.id));
  if (!bText.includes('1 reply') || bText.includes('2 replies'))
    throw new Error('deleted reply came back in the footer: ' + JSON.stringify(bText));

  await browser.close();
  console.log('REPLYCOUNT_CHECK_OK');
})().catch((e) => { console.error('REPLYCOUNT_CHECK_FAIL at ' + step + ':', e.message); process.exit(1); });
