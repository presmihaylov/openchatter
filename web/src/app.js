import { ICON } from './icons.js';
import { wsAvatarEl } from './wsavatar.js';
import { avatarImage } from './avatar.js';
/* OpenChatter human web client — vanilla JS, talks to the same REST API as agents. */
import { createComposer } from './composer.js';
import { emojify, searchEmoji, rememberEmoji, shortcodeOf } from './emoji.js';
import { sessionToken, isAccountPage, loginURL, onSessionInvalid, backTarget, fetchWorkspaces, signOut, authApi, noWorkspaceError, inviteErrorText, inviteTokenFrom, wireSlugPreview } from './auth.js';

(() => {
  'use strict';

  // the URL carries only the public slug; joining needs a separate invite link
  const isCreatePage = location.pathname.replace(/\/+$/, '') === '/create';
  // path shape: /r/<slug>[/c/<channel>[/t/<thread-id>]] — channel/thread are
  // restored on load and kept in sync so refresh, back/forward and deep links work.
  // /w/<slug> is the switcher's alias of /r/<slug>; the page keeps whichever it got
  const pathSegs = location.pathname.split('/').filter(Boolean);
  let roomPrefix = pathSegs[0] === 'w' ? '/w/' : '/r/';
  // /join/<token> is an invite link: the page enters the workspace it opens
  const isJoinPage = pathSegs[0] === 'join';
  const joinToken = isJoinPage ? decodeURIComponent(pathSegs[1] || '') : '';
  // the active workspace; an in-app switch (task 23) moves it
  let slug = (isCreatePage || isJoinPage) ? '' : decodeURIComponent(pathSegs[1] || '');
  const storeKey = 'agentchat:' + slug;
  const $ = (id) => document.getElementById(id);

  let me = null;
  let room = null;
  let joinURL = null;
  let isAdmin = false;
  let channels = [];
  let groups = [];           // personal sidebar sections (channel groups)
  let ungrouped = [];        // the default section's channel order (ids)
  let defaultCollapsed = false;
  let participants = [];
  // up here with the rest of the room state on purpose: the composer mounts
  // before the room loads and its live mention highlight reads this on render 1
  let channelMembers = [];
  let current = null;        // current channel object
  let openThreadRoot = null; // message id of the open thread
  let openThreadSummary = null; // transient sidebar row for a deep-linked thread outside my tree
  let railRooms = [];        // the last /api/v1/user workspace list: badges, mutes, order
  let notifyPrefs = { enabled: true, sound: true, archive_after_secs: 3600 };

  // ---------- workspace store (task 23) ----------
  // One entry per member workspace, keyed by slug. The module state above is
  // the active entry adopted by reference: an in-place edit (an unread bump)
  // reaches the entry, a reassignment goes through the entry, then adopt().
  // Nothing half-loaded ever renders: warm() fills an entry off screen first.
  const store = new Map();
  const PAGE_LIMIT = 100;            // messages kept per cached channel page
  const PAGE_TTL = 30 * 60 * 1000;   // a page not opened for this long is dropped
  const entryFor = (s) => {
    let e = store.get(s);
    if (!e) {
      e = { slug: s, warm: false, warming: null, me: null, room: null, joinURL: null, isAdmin: false,
        channels: [], groups: [], ungrouped: [], defaultCollapsed: false,
        participants: [], publicChannels: [], threads: [],
        pages: new Map(), members: new Map(), lastChannelID: null, refreshTimer: 0,
        roomLoadSeq: 0, groupLoadSeq: 0, publicLoadSeq: 0,
        threadLoadSeq: 0, pending: [] };
      store.set(s, e);
    }
    return e;
  };
  const active = () => entryFor(slug);
  const adopt = (e) => {
    me = e.me; room = e.room; joinURL = e.joinURL; isAdmin = e.isAdmin;
    channels = e.channels; groups = e.groups; participants = e.participants;
    ungrouped = e.ungrouped || []; defaultCollapsed = !!e.defaultCollapsed;
    publicChannels = e.publicChannels; threads = e.threads;
  };
  const pageFor = (e, chID) => e.pages.get(chID) || null;
  const setPage = (e, chID, list) => {
    const page = { list: list.slice(-PAGE_LIMIT), openedAt: Date.now(), replies: new Set(), revision: 0, loading: false };
    e.pages.set(chID, page);
    return page;
  };
  // keeps a cached page in step with the room's message events, on screen or
  // not; false means a message.created the page already holds (a replay)
  const pageApply = (e, ev) => {
    const t = ev.type;
    if (t === 'message.created') {
      const m = ev.payload;
      const page = pageFor(e, m.channel_id);
      if (!page) return true;
      // a reply never joins the channel page, but it moves its root's footer:
      // leave the cached root stale and a later repaint from cache drops the
      // "N replies" line the DOM patch had already put there
      if (m.thread_root_id) {
        const root = page.list.find((x) => x.id === m.thread_root_id);
        if (!root) { if (page.loading) page.revision++; return true; }
        if (page.replies.has(m.id)) return false; // a replayed event must not count twice
        // the fetch that filled this page already counted every reply up to the
        // root's last_reply_at, and the feed cursors predate that fetch: without
        // this the first replayed reply after a boot or a warm counts twice
        if (root.last_reply_at && m.created_at <= root.last_reply_at) return false;
        page.replies.add(m.id);
        root.reply_count = (root.reply_count || 0) + 1;
        root.last_reply_at = m.created_at;
        if (!(root.replier_ids || []).includes(m.author_id)) {
          root.replier_ids = (root.replier_ids || []).concat(m.author_id);
        }
        page.revision++;
        return true;
      }
      if (page.list.some((x) => x.id === m.id)) return false;
      page.list.push(m);
      if (page.list.length > PAGE_LIMIT) page.list.splice(0, page.list.length - PAGE_LIMIT);
      page.revision++;
      return true;
    }
    if (t === 'message.edited') {
      const m = ev.payload;
      const page = pageFor(e, m.channel_id);
      if (page) {
        const i = page.list.findIndex((x) => x.id === m.id);
        if (i >= 0) { page.list[i] = m; page.revision++; }
        else if (page.loading) page.revision++;
      }
      return true;
    }
    if (t === 'message.deleted') {
      const rootID = ev.payload.thread_root_id;
      for (const page of e.pages.values()) {
        let changed = page.loading || page.replies.delete(ev.payload.message_id);
        const i = page.list.findIndex((x) => x.id === ev.payload.message_id);
        if (i >= 0) { page.list.splice(i, 1); changed = true; }
        // a deleted reply is not on the page, but it still leaves its root's
        // footer one too high; the exact last_reply_at only a refetch knows
        const root = rootID ? page.list.find((x) => x.id === rootID) : null;
        if (root && root.reply_count) { root.reply_count -= 1; changed = true; }
        if (root && !root.reply_count) root.last_reply_at = null;
        if (changed) page.revision++;
      }
      return true;
    }
    if (t === 'message.reaction') {
      // the cached message must carry the live set, or a later render of the
      // page would put the stale one back into reactionMap
      for (const page of e.pages.values()) {
        const m = page.list.find((x) => x.id === ev.payload.message_id);
        if (m) { m.reactions = ev.payload.reactions || []; page.revision++; }
        else if (page.loading) page.revision++;
      }
    }
    if (t === 'message.ack') {
      for (const page of e.pages.values()) {
        const m = page.list.find((x) => x.id === ev.payload.message_id);
        if (m) { m.acked_by = ev.payload.acked_by || []; page.revision++; }
        else if (page.loading) page.revision++;
      }
    }
    return true;
  };
  const evictPages = () => {
    const cutoff = Date.now() - PAGE_TTL;
    for (const e of store.values()) {
      for (const [chID, page] of e.pages) {
        const onScreen = e === active() && current && current.id === chID;
        if (page.openedAt < cutoff && chID !== e.lastChannelID && !onScreen) e.pages.delete(chID);
      }
    }
  };
  setInterval(evictPages, 5 * 60 * 1000);
  // work that may hit the network, held until a cached render has painted
  const afterPaint = (fn) => requestAnimationFrame(() => setTimeout(fn, 0));
  // One pending attachment per composer. The thread reply shares the upload
  // endpoint but never the slot: a file staged in one composer must not ride
  // out on the other's send.
  const pendingAtt = { main: null, thread: null };
  const pendingAttSeq = { main: 0, thread: 0 };
  const pendingPreviewURL = { main: null, thread: null };
  const attachEls = (which) => (which === 'thread'
    ? { pend: $('thread-attach-pending'), input: $('thread-attach-input') }
    : { pend: $('attach-pending'), input: $('attach-input') });
  const clearPendingAttachment = (which) => {
    pendingAttSeq[which]++;
    pendingAtt[which] = null;
    if (pendingPreviewURL[which]) {
      URL.revokeObjectURL(pendingPreviewURL[which]);
      pendingPreviewURL[which] = null;
    }
    const { pend, input } = attachEls(which);
    if (pend) { pend.replaceChildren(); pend.classList.add('hidden'); }
    if (input) input.value = '';
  };
  const clearThreadAttachment = () => clearPendingAttachment('thread');
  const clearMainAttachment = () => clearPendingAttachment('main');

  // One header builder for every fetch. The login session is the only browser
  // identity; it names its workspace through X-Workspace-Slug on room pages.
  const authHeaders = (extra, wsSlug) => {
    const headers = Object.assign({}, extra);
    const ses = sessionToken();
    if (!ses) return headers;
    headers['Authorization'] = 'Bearer ' + ses;
    if (wsSlug || slug) headers['X-Workspace-Slug'] = wsSlug || slug;
    return headers;
  };
  // a workspace image is served to its members only: the slug must be that workspace's
  const wsHeaders = (wsSlug) => {
    const ses = sessionToken();
    return ses ? { 'Authorization': 'Bearer ' + ses, 'X-Workspace-Slug': wsSlug } : {};
  };
  const paintRoomMark = () => {
    $('room-avatar').replaceChildren(wsAvatarEl(room, 'ws-avatar-sm', wsHeaders(room.slug)));
  };

  // One verdict on an auth failure, shared by api(), the event loop and the
  // boot. True means the failure is handled: the page is leaving, or a view
  // took over. Later calls are no-ops so the callers can simply stop.
  let authHandled = false;
  const routeAuthError = (e) => {
    if (authHandled) return true;
    if (e.status === 401 && e.code === 'session_invalid') {
      authHandled = true;
      onSessionInvalid();
      if (!isAccountPage) location.replace(loginURL());
      return true;
    }
    if (e.status === 404 && e.code === 'workspace_not_found') {
      // the workspace was deleted under this tab: let / pick the next one
      authHandled = true;
      location.replace('/');
      return true;
    }
    if (e.status === 403 && e.code === 'workspace_forbidden') {
      authHandled = true;
      if (e.reason === 'revoked') { showRemoved(); return true; }
      showEnter();
      return true;
    }
    return false;
  };

  // opts.ws names another workspace (a background warm, task 23); its 403/404
  // is the caller's to handle, only a dead session still routes the page
  const api = async (path, opts = {}) => {
    const sent = opts.ws || slug; // a late error for a workspace we left must not route
    const headers = authHeaders(opts.headers, opts.ws);
    if (opts.body && !(opts.body instanceof FormData)) {
      headers['Content-Type'] = 'application/json';
      opts = Object.assign({}, opts, { body: JSON.stringify(opts.body) });
    }
    const resp = await fetch(path, Object.assign({}, opts, { headers }));
    let data = null;
    try { data = await resp.json(); } catch (e) { /* empty body */ }
    if (!resp.ok) {
      const err = new Error((data && data.error) || ('HTTP ' + resp.status));
      err.status = resp.status;
      err.code = data && data.code;
      err.reason = data && data.reason;
      if (sent === slug || err.code === 'session_invalid') routeAuthError(err);
      throw err;
    }
    return data;
  };

  // ---------- rendering ----------

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const escRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  // plain-text fields (topics, profiles): make URLs clickable without ever
  // navigating away; input is esc()aped first so the URL is attribute-safe
  const linkify = (s) => esc(s).replace(/https?:\/\/[^\s<]+[^\s<.,)]/g,
    (u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);

  // links leave the chat: open them in a new tab, without opener access
  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName === 'A' && node.hasAttribute('href')) {
      node.setAttribute('target', '_blank');
      node.setAttribute('rel', 'noopener noreferrer');
    }
  });

  const renderMarkdown = (text) => {
    let html = marked.parse(text, { breaks: true, mangle: false, headerIds: false });
    // :rocket: -> 🚀, but never inside code, where a shortcode is literal text
    html = html.split(/(<pre[\s\S]*?<\/pre>|<code[\s\S]*?<\/code>)/).map((part, i) => (i % 2 ? part : emojify(part))).join('');
    // names may contain spaces/upper case, so match the known names literally
    // (longest first, so "@John Smith" is not eaten by a "@John" match)
    const targets = participants.map((p) => p.name).concat(['channel', 'here', 'everyone'])
      .sort((a, b) => b.length - a.length);
    if (targets.length) {
      const re = new RegExp('@(' + targets.map(escRe).join('|') + ')(?![\\w-])', 'g');
      // anything that pings you — your own name or a @channel/@here/@everyone
      // broadcast — gets the warm amber chip; mentions of others stay blue.
      const meTargets = new Set([me.name, 'channel', 'here', 'everyone']);
      html = html.replace(re, (m, name) =>
        `<strong class="mention${meTargets.has(name) ? ' mention-me' : ''}">${esc(m)}</strong>`);
    }
    // #channel links: only channels you are in. A private channel you are not
    // in stays plain text, so the render leaks nothing about it existing. The
    // wire body stays "#name"; only the view changes.
    const chNames = linkableChannels().map((c) => c.name).sort((a, b) => b.length - a.length);
    if (chNames.length && room) {
      const re = new RegExp('(^|[\\s>(\\[])#(' + chNames.map(escRe).join('|') + ')(?![\\w-])', 'g');
      html = html.replace(re, (m, pre, name) =>
        `${pre}<a class="chanlink" href="/r/${encodeURIComponent(room.slug)}/c/${encodeURIComponent(name)}">#${esc(name)}</a>`);
    }
    // ALLOW_DATA_ATTR:false so markdown can't inject data-act and hijack the msg click handler;
    // SANITIZE_NAMED_PROPS namespaces any user id/name so markdown can't DOM-clobber our elements
    return DOMPurify.sanitize(html, { FORBID_TAGS: ['style', 'form', 'input'], FORBID_ATTR: ['onerror', 'onclick'], ALLOW_DATA_ATTR: false, SANITIZE_NAMED_PROPS: true });
  };

  const fmtTime = (iso) => {
    const d = new Date(iso);
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  };

  const fmtMessageTime = (iso, now = new Date()) => {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    const elapsed = Math.max(0, now.getTime() - d.getTime());
    if (elapsed < 60 * 1000) return 'just now';
    if (elapsed < 60 * 60 * 1000) {
      const n = Math.floor(elapsed / (60 * 1000));
      return `${n} minute${n === 1 ? '' : 's'} ago`;
    }
    if (elapsed < 24 * 60 * 60 * 1000) {
      const n = Math.floor(elapsed / (60 * 60 * 1000));
      return `${n} hour${n === 1 ? '' : 's'} ago`;
    }
    const yd = new Date(now); yd.setDate(now.getDate() - 1);
    if (d.toDateString() === yd.toDateString()) return 'Yesterday at ' + fmtTime(iso);
    const date = d.toLocaleDateString([], {
      month: 'short', day: 'numeric', ...(d.getFullYear() === now.getFullYear() ? {} : { year: 'numeric' }),
    });
    return date + ' at ' + fmtTime(iso);
  };

  const fmtExactMessageTime = (iso) => new Date(iso).toLocaleString([], {
    weekday: 'long', year: 'numeric', month: 'long', day: 'numeric',
    hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short',
  });

  const messageTimeHTML = (iso, className) => `<time class="${className} message-time" datetime="${esc(iso)}" title="${esc(fmtExactMessageTime(iso))}">${esc(fmtMessageTime(iso))}</time>`;

  // Relative labels age in place. This touches only the rendered <time>
  // elements: no channel, thread or search request is made on a clock tick.
  const refreshMessageTimes = () => document.querySelectorAll('time.message-time[datetime]').forEach((el) => {
    const iso = el.getAttribute('datetime');
    el.textContent = fmtMessageTime(iso);
    el.title = fmtExactMessageTime(iso);
  });
  setInterval(refreshMessageTimes, 30000);

  // calendar day in the viewer's local time; the key the date dividers split on
  const dayOf = (iso) => new Date(iso).toDateString();
  const fmtDay = (iso) => {
    const d = new Date(iso);
    const now = new Date();
    if (dayOf(iso) === now.toDateString()) return 'Today';
    const yd = new Date(now); yd.setDate(now.getDate() - 1);
    if (dayOf(iso) === yd.toDateString()) return 'Yesterday';
    if (d.getFullYear() === now.getFullYear()) {
      return d.toLocaleDateString([], { weekday: 'long' }) + ', ' + d.getDate() + ' ' + d.toLocaleDateString([], { month: 'long' });
    }
    return d.getDate() + ' ' + d.toLocaleDateString([], { month: 'short' }) + ' ' + d.getFullYear();
  };
  const dateDividerEl = (iso) => {
    const el = document.createElement('div');
    el.className = 'date-divider';
    el.dataset.day = dayOf(iso);
    el.innerHTML = `<span>${esc(fmtDay(iso))}</span>`;
    return el;
  };
  // Slack-style day markers: one before the first .msg of each local day.
  // Rebuilt from scratch, so a prepended page or a live append never leaves a
  // stale marker mid-day. Sits after the unread divider when both apply.
  const syncDateDividers = (box) => {
    box.querySelectorAll(':scope > .date-divider').forEach((n) => n.remove());
    let last = null;
    [...box.children].filter((n) => n.classList.contains('msg')).forEach((n) => {
      const day = dayOf(n.dataset.at);
      if (day !== last) box.insertBefore(dateDividerEl(n.dataset.at), n);
      last = day;
    });
    syncStuckDividers(box);
  };
  // Sticky markers share one containing block (the box), so every marker
  // scrolled past piles up at the top. Keep only the latest one visible, as a
  // floating pill without side lines; the rest hide until they scroll back.
  const syncStuckDividers = (box) => {
    const divs = [...box.querySelectorAll(':scope > .date-divider')];
    if (!divs.length) return;
    const limit = box.getBoundingClientRect().top + parseFloat(getComputedStyle(box).paddingTop);
    const gap = parseFloat(getComputedStyle(divs[0]).marginBottom);
    // where a marker would sit unpinned: right above its first message
    // (a marker with no message under it is transient; treat it as far below)
    const natural = (d) => (d.nextElementSibling ? d.nextElementSibling.getBoundingClientRect().top - gap - d.offsetHeight : Infinity);
    let shown = -1;
    if (box.scrollTop > 0) divs.forEach((d, i) => { if (natural(d) <= limit + 1) shown = i; });
    divs.forEach((d, i) => {
      const next = divs[i + 1];
      const pushed = next && natural(next) <= limit + d.offsetHeight;
      d.classList.toggle('stuck', i === shown);
      d.classList.toggle('covered', i < shown || (i === shown && !!pushed));
    });
  };

  // attachment images sit behind bearer auth — blob-fetch once, cache the
  // object URL per attachment id (avatars and inline images share this)
  // size 128 or 512 asks for the copy the server resized on upload; avatars
  // draw at 20 to 96px and the originals run to megabytes
  const blobURLs = {};
  const blobEntry = (attID, size) => {
    const key = attID + ':' + (size || 0);
    if (!blobURLs[key]) {
      const entry = { url: null, promise: null };
      blobURLs[key] = entry;
      entry.promise = fetch('/api/v1/attachments/' + attID + (size ? '?size=' + size : ''), { headers: authHeaders() })
        .then((r) => (r.ok ? r.blob() : Promise.reject(new Error('image fetch failed'))))
        .then((b) => {
          entry.url = URL.createObjectURL(b);
          return entry.url;
        })
        .catch(() => {
          if (blobURLs[key] === entry) delete blobURLs[key];
          return null;
        });
    }
    return blobURLs[key];
  };
  // Attachments need a promise every time; avatars can take a resolved object
  // URL synchronously so a full DOM repaint never inserts a cached shimmer.
  const blobURL = (attID, size) => blobEntry(attID, size).promise;
  const avatarBlob = (attID, size) => {
    const entry = blobEntry(attID, size);
    return entry.url || entry.promise;
  };
  // An avatar is an image, always. A member who never uploaded one uses the
  // shared seedling. An uploaded image starts with no src at all: avatarImage
  // owns its loading skeleton and neutral error state, so the seedling never
  // flashes while a protected attachment is fetched.
  const DEFAULT_AVATAR = (size) => '/brand/avatar-default-' + size + '.png';
  const avatarCore = (p, cls) => {
    if (!p) return avatarImage(cls, 'Deleted member', null);
    const size = cls === 'avatar-lg' ? 512 : 128;
    const source = p.avatar_attachment_id
      ? avatarBlob(p.avatar_attachment_id, size)
      : DEFAULT_AVATAR(size);
    return avatarImage(cls, p.name, source);
  };
  // Owner badge: overlay the human owner's avatar bottom-right, Slack-app-badge style.
  // Only for agents with a server-verified owner. Skipped on avatar-rb (reply-bar
  // stack overlaps horizontally, so a corner badge would be occluded).
  const avatarEl = (p, cls) => {
    const core = avatarCore(p, cls);
    const owner = cls !== 'avatar-rb' && p && p.owner_id
      ? participants.find((x) => x.id === p.owner_id) : null;
    if (!owner) return core;
    const wrap = document.createElement('span');
    wrap.className = cls + ' avatar-wrap';
    core.classList.remove(cls);
    const badge = avatarCore(owner, 'owner-badge-av');
    badge.title = `${owner.name}'s agent`;
    wrap.append(core, badge);
    return wrap;
  };

  // Emoji reactions, keyed by message id. Seeded from each message's payload;
  // every message.reaction event carries the full list, so a repaint is a
  // straight replace and copies in the feed and thread panel never disagree.
  const reactionMap = {};
  const QUICK_REACTIONS = ['👀', '✅', '👍', '🎉', '❤️', '🚀', '🙏', '😂'];

  // Acks, keyed by message id. An ack is a receipt, not a reaction: one check
  // for the message however many people acked, and the names live on the title
  // so a glance says "somebody owns this" and a hover says who.
  const ackMap = {};

  function fillAckMark(box, acks) {
    if (!box) return;
    if (!acks || !acks.length) {
      box.innerHTML = '';
      box.removeAttribute('title');
      return;
    }
    box.innerHTML = ICON.check;
    box.title = `acknowledged by ${acks.map((a) => a.name).join(', ')}`;
  }

  function paintAck(msgID, acks) {
    ackMap[msgID] = acks || [];
    document.querySelectorAll(`.msg[data-id="${msgID}"] .msg-ack`).forEach(
      (box) => fillAckMark(box, ackMap[msgID]));
  }

  async function ackMessage(msgID) {
    const out = await api(`/api/v1/messages/${msgID}/ack`, { method: 'POST' });
    paintAck(msgID, out.acked_by);
  }

  // the add-reaction pill: the same smile-plus as the toolbar, tagged for the checks
  const ADD_REACTION_ICON = ICON.smilePlus.replace('class="ico lucide"', 'class="ico lucide rx-add-icon"');

  // "You, Ann and Ben reacted with :eyes:" — Slack's wording, you first
  const reactionTipText = (rx) => {
    const names = rx.participant_ids.map((id, i) => (id === me.id ? 'You' : rx.names[i]));
    if (names.includes('You')) names.splice(0, 0, ...names.splice(names.indexOf('You'), 1));
    const who = names.length <= 1 ? names[0] || '' : names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1];
    return `${who} reacted with ${shortcodeOf(emojify(rx.emoji))}`;
  };

  // one floating tooltip, shown above the hovered or focused pill; instant,
  // themed, and readable, unlike the browser's title delay
  let rxTip = null;
  const hideReactionTip = () => { if (rxTip) rxTip.remove(); rxTip = null; };
  const showReactionTip = (pill, rx) => {
    hideReactionTip();
    rxTip = document.createElement('div');
    rxTip.className = 'rx-tip';
    rxTip.setAttribute('role', 'tooltip');
    rxTip.innerHTML = `<span class="rx-tip-emoji">${esc(emojify(rx.emoji))}</span><span class="rx-tip-text">${esc(reactionTipText(rx))}</span>`;
    document.body.appendChild(rxTip);
    const r = pill.getBoundingClientRect();
    const w = rxTip.offsetWidth, h = rxTip.offsetHeight;
    let left = r.left + r.width / 2 - w / 2;
    left = Math.max(8, Math.min(left, window.innerWidth - w - 8));
    const above = r.top - h - 8;
    rxTip.style.left = left + 'px';
    rxTip.style.top = (above >= 4 ? above : r.bottom + 8) + 'px';
    rxTip.classList.toggle('below', above < 4);
  };
  document.addEventListener('scroll', hideReactionTip, true);
  // scroll events do not bubble, so capture catches both message boxes
  document.addEventListener('scroll', (e) => {
    if (e.target instanceof Element && e.target.matches('#messages, #thread-messages')) syncStuckDividers(e.target);
  }, true);

  const reactionPill = (m, rx) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'reaction' + (rx.participant_ids.includes(me.id) ? ' mine' : '');
    b.dataset.emoji = rx.emoji;
    b.dataset.names = rx.names.join(', ');
    b.setAttribute('aria-label', reactionTipText(rx));
    b.innerHTML = `<span class="rx-emoji">${esc(emojify(rx.emoji))}</span><span class="rx-count">${rx.count}</span>`;
    b.onclick = (ev) => { ev.stopPropagation(); hideReactionTip(); toggleReaction(m, rx.emoji, b.classList.contains('mine')); };
    b.onmouseenter = () => showReactionTip(b, rx);
    b.onmouseleave = hideReactionTip;
    b.onfocus = () => showReactionTip(b, rx);
    b.onblur = hideReactionTip;
    return b;
  };

  const fillReactionBox = (box, m, list) => {
    box.innerHTML = '';
    box.hidden = list.length === 0;
    list.forEach((rx) => box.appendChild(reactionPill(m, rx)));
    if (!list.length) return;
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'reaction add';
    add.title = 'Add reaction';
    add.setAttribute('aria-label', 'Add reaction');
    add.innerHTML = ADD_REACTION_ICON;
    add.onclick = (ev) => { ev.stopPropagation(); const r = add.getBoundingClientRect(); openReactionPicker(r.left, r.bottom + 4, m); };
    box.appendChild(add);
  };

  const renderReactions = (msgId) => {
    hideReactionTip();
    const list = reactionMap[msgId] || [];
    document.querySelectorAll(`.msg[data-id="${msgId}"] .msg-reactions`).forEach((box) => {
      fillReactionBox(box, box._msg, list);
    });
  };

  // toggle is optimistic-free on purpose: the server answers with the full
  // list in a few ms and the event repaints every copy anyway
  const toggleReaction = async (m, emoji, mine) => {
    try {
      const out = mine
        ? await api(`/api/v1/messages/${m.id}/reactions/${encodeURIComponent(emoji)}`, { method: 'DELETE' })
        : await api(`/api/v1/messages/${m.id}/reactions`, { method: 'POST', body: { emoji } });
      reactionMap[m.id] = out.reactions || [];
      renderReactions(m.id);
    } catch (e) { notice(e.message, true); }
  };

  // A small picker: the quick row, then a search box over the whole set.
  // Enter picks the first hit; Esc or click-outside closes.
  let closeReactionPicker = () => {};
  function openReactionPicker(x, y, m) {
    closeReactionPicker();
    closeContextMenu();
    const box = document.createElement('div');
    box.className = 'reaction-picker';
    const quick = document.createElement('div');
    quick.className = 'rp-quick';
    const pick = (emoji, name) => { closeReactionPicker(); if (name) rememberEmoji(name); toggleReaction(m, emoji, false); };
    QUICK_REACTIONS.forEach((e) => {
      const b = document.createElement('button');
      b.type = 'button'; b.textContent = e; b.onclick = () => pick(e);
      quick.appendChild(b);
    });
    const input = document.createElement('input');
    input.type = 'text'; input.placeholder = 'Search emoji…'; input.className = 'rp-search';
    input.setAttribute('aria-label', 'Search emoji');
    const results = document.createElement('div');
    results.className = 'rp-results';
    let hits = [];
    input.oninput = () => {
      hits = searchEmoji(input.value.trim().replace(/^:|:$/g, ''), 16);
      results.innerHTML = '';
      hits.forEach((h) => {
        const b = document.createElement('button');
        b.type = 'button'; b.textContent = h.emoji; b.title = ':' + h.name + ':';
        b.onclick = () => pick(h.emoji, h.name);
        results.appendChild(b);
      });
    };
    input.onkeydown = (e) => {
      if (e.key === 'Enter' && hits.length) { e.preventDefault(); pick(hits[0].emoji, hits[0].name); }
    };
    box.append(quick, input, results);
    document.body.appendChild(box);
    const w = box.offsetWidth, h = box.offsetHeight;
    box.style.left = Math.min(x, window.innerWidth - w - 8) + 'px';
    box.style.top = Math.min(y, window.innerHeight - h - 8) + 'px';
    input.focus();
    const onKey = (e) => { if (e.key === 'Escape') closeReactionPicker(); };
    const onDown = (e) => { if (!box.contains(e.target)) closeReactionPicker(); };
    closeReactionPicker = () => {
      box.remove();
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('mousedown', onDown, true);
      window.removeEventListener('resize', closeReactionPicker);
      closeReactionPicker = () => {};
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('mousedown', onDown, true);
    window.addEventListener('resize', closeReactionPicker);
  }

  // Per-channel interaction recency. Slack ranks the people you are actually
  // talking to above the rest of the room, so remember who spoke here and who
  // got mentioned, and reset it when the channel changes.
  let talkedAt = new Map();
  const noteTalk = (m) => {
    if (!m || m.kind === 'system') return;
    const at = m.created_at || '';
    const bump = (n) => { if (n && (talkedAt.get(n) || '') < at) talkedAt.set(n, at); };
    bump(m.author_name);
    (m.mentions || []).forEach(bump);
  };

  const msgEl = (m, inThread) => {
    noteTalk(m);
    // membership entries: one muted Slack-style line, no avatar, no actions
    if (m.kind === 'system') {
      const el = document.createElement('div');
      el.className = 'msg system-entry';
      el.dataset.id = m.id;
      el.dataset.at = m.created_at;
      el.innerHTML = `<span class="sys-text"><span class="sys-name">${esc(m.author_name)}</span> ${esc(m.body)}</span>${messageTimeHTML(m.created_at, 'sys-time')}`;
      attachMsgMenu(el, m);
      return el;
    }
    const el = document.createElement('div');
    el.className = 'msg';
    el.dataset.id = m.id;
    el.dataset.at = m.created_at;
    if (m.is_broadcast) el.classList.add('broadcast');

    const canEdit = m.author_id === me.id;
    const canDelete = canEdit || me.role === 'admin';
    const actions = [];
    actions.push(`<button data-act="ack" title="Acknowledge" aria-label="Acknowledge">${ICON.check}</button>`);
    actions.push(`<button data-act="react" title="Add reaction" aria-label="Add reaction">${ICON.smilePlus}</button>`);
    if (!inThread && !m.thread_root_id) actions.push(`<button data-act="thread" title="Reply in thread" aria-label="Reply in thread">${ICON.messageSquare}</button>`);
    if (canEdit) actions.push(`<button data-act="edit" title="Edit" aria-label="Edit">${ICON.pencil}</button>`);
    if (canDelete) actions.push(`<button data-act="delete" title="Delete" aria-label="Delete">${ICON.trashTwo}</button>`);
    // last item: every message action in one menu, reachable by keyboard and touch
    actions.push(`<button data-act="more" title="More actions" aria-label="More actions" aria-haspopup="menu">${ICON.moreVertical}</button>`);

    // fetch-with-header + blob keeps the token out of URLs (logs, history, referrers)
    const atts = (m.attachments || []).map((a) =>
      (a.content_type || '').startsWith('image/')
        ? `<img class="inline-img" data-att="${esc(a.id)}" data-name="${esc(a.filename)}" alt="${esc(a.filename)}">`
        : `<button class="attachment" data-att="${esc(a.id)}" data-name="${esc(a.filename)}">${ICON.fileText} ${esc(a.filename)}</button>`).join(' ');

    const replyBar = (!inThread && m.reply_count > 0)
      ? '<button class="reply-bar" data-act="thread"></button>' : '';

    el.innerHTML = `
      <div class="avatar"></div>
      <div class="body">
        <div class="meta"><span class="author">${esc(m.author_name)}</span>${(() => {
          const a = participants.find((x) => x.id === m.author_id);
          return a && a.owner_name ? `<span class="owner-badge" title="server-verified owner">${esc(a.owner_name)}'s agent</span>` : '';
        })()}${messageTimeHTML(m.created_at, 'time')}
          ${m.edited_at ? '<span class="edited"> (edited)</span>' : ''}
          ${m.is_broadcast ? ' <span class="bcast" title="broadcast">' + ICON.megaphone + '</span>' : ''}<span class="msg-ack"></span></div>
        <div class="content">${renderMarkdown(m.body)}</div>
        ${atts}<div class="msg-reactions"></div>${replyBar}
      </div>
      <div class="msg-actions">${actions.join('')}</div>`;
    el.querySelector('.avatar').appendChild(
      avatarEl(participants.find((x) => x.id === m.author_id), 'avatar-msg'));
    ackMap[m.id] = m.acked_by || [];
    fillAckMark(el.querySelector('.msg-ack'), ackMap[m.id]);
    reactionMap[m.id] = m.reactions || [];
    const rxBox = el.querySelector('.msg-reactions');
    rxBox._msg = m; // the pills need the message to toggle against
    fillReactionBox(rxBox, m, reactionMap[m.id]);
    const bar = el.querySelector('.reply-bar');
    if (bar) {
      const avs = document.createElement('span');
      avs.className = 'rb-avatars';
      (m.replier_ids || []).forEach((id) =>
        avs.appendChild(avatarEl(participants.find((x) => x.id === id), 'avatar-rb')));
      const count = document.createElement('span');
      count.className = 'rb-count';
      count.textContent = `${m.reply_count} repl${m.reply_count === 1 ? 'y' : 'ies'}`;
      const last = document.createElement('span');
      last.className = 'rb-last';
      if (m.last_reply_at) last.innerHTML = 'Last reply ' + messageTimeHTML(m.last_reply_at, 'rb-time');
      bar.append(avs, count, last);
      const th = threads.find((x) => x.root_id === m.id);
      if (th && th.unread_count > 0 && !th.muted) bar.classList.add('unread');
    }
    // hljs respects a language-x class from the fence and auto-detects otherwise
    el.querySelectorAll('.content pre code').forEach((c) => {
      try { hljs.highlightElement(c); } catch (e) { /* unknown language tag */ }
    });
    el.querySelectorAll('img.inline-img[data-att]').forEach((im) => {
      // keep the view pinned to the bottom when an image finishes loading late
      im.addEventListener('load', () => {
        const sc = im.closest('#messages, #thread-messages');
        if (sc && sc.scrollHeight - sc.scrollTop - sc.clientHeight < 240) sc.scrollTop = sc.scrollHeight;
      });
      blobURL(im.dataset.att).then((url) => { if (url) im.src = url; });
    });

    el.addEventListener('click', (ev) => {
      // a #channel link navigates in-app, no reload
      const chLink = ev.target.closest('a.chanlink');
      if (chLink && el.contains(chLink)) {
        ev.preventDefault();
        const name = decodeURIComponent(chLink.getAttribute('href').split('/c/')[1] || '');
        goToChannel(name);
        return;
      }
      const inlineImg = ev.target.closest('img.inline-img');
      if (inlineImg && el.contains(inlineImg) && inlineImg.dataset.att) {
        openLightbox(inlineImg.dataset.att, inlineImg.dataset.name); return;
      }
      const attBtn = ev.target.closest('button.attachment');
      if (attBtn && el.contains(attBtn)) {
        if (isTextAttachment(attBtn.dataset.name)) openDoc(attBtn.dataset.att, attBtn.dataset.name);
        else downloadAttachment(attBtn.dataset.att, attBtn.dataset.name);
        return;
      }
      // only real action buttons act — rendered markdown can't fake these
      const btn = ev.target.closest('.msg-actions button, button.reply-bar');
      if (!btn || !el.contains(btn)) return;
      const act = btn.dataset.act;
      if (act === 'react') {
        const r = btn.getBoundingClientRect();
        openReactionPicker(r.left, r.bottom + 4, m);
      }
      if (act === 'ack') ackMessage(m.id);
      if (act === 'thread') openThread(m.thread_root_id || m.id);
      if (act === 'edit') editMessage(m);
      if (act === 'delete') deleteMessage(m);
      if (act === 'more') {
        const r = btn.getBoundingClientRect();
        openContextMenu(r.left, r.bottom + 4, msgMenuItems(m, { inThread, canEdit, canDelete }));
      }
    });
    attachMsgMenu(el, m, { inThread, canEdit, canDelete });
    return el;
  };

  // /r/<room>/c/<channel>/t/<thread>/m/<id> — the thread segment only when the
  // message lives in one, so the link opens the same view the copier saw.
  const permalinkFor = (m) => {
    const ch = channels.find((c) => c.id === m.channel_id) || current;
    let path = '/r/' + encodeURIComponent(room.slug);
    if (ch) path += '/c/' + encodeURIComponent(ch.name);
    if (m.thread_root_id) path += '/t/' + encodeURIComponent(m.thread_root_id);
    return location.origin + path + '/m/' + encodeURIComponent(m.id);
  };

  // The one list of things you can do to a message. Both the right-click menu
  // and the ⋮ toolbar button open it, so nothing hides behind only one gesture.
  const msgMenuItems = (m, opts = {}) => {
    const items = [{
      label: 'Copy link to message',
      run: async () => {
        const ok = await copyText(permalinkFor(m));
        notice(ok ? 'Link copied' : 'Copy failed', !ok);
      },
    }];
    if (m.kind === 'system') return items;
    if (!opts.inThread && !m.thread_root_id) {
      items.push({ label: 'Reply in thread', run: () => openThread(m.id) });
    }
    const root = m.thread_root_id || m.id;
    const th = threads.find((x) => x.root_id === root);
    const subscribed = !!(th && th.subscribed);
    items.push({
      label: subscribed ? 'Unsubscribe' : 'Subscribe',
      run: async () => {
        try {
          await api(`/api/v1/threads/${root}/subscribe`, { method: 'POST', body: { subscribed: !subscribed } });
          loadThreads();
        } catch (e) { alert(e.message); }
      },
    });
    items.push({
      label: 'Add reaction',
      run: () => {
        // anchor the picker to the message's toolbar; the menu itself is gone by now
        const el = document.querySelector(`.msg[data-id="${m.id}"]`);
        const r = el ? el.getBoundingClientRect() : { left: 80, bottom: 80 };
        openReactionPicker(r.left + 60, r.bottom - 8, m);
      },
    });
    if (opts.canEdit) items.push({ label: 'Edit message', run: () => editMessage(m) });
    if (opts.canDelete) items.push({ label: 'Delete message', danger: true, run: () => deleteMessage(m) });
    return items;
  };

  const attachMsgMenu = (el, m, opts) => {
    el.oncontextmenu = (ev) => {
      // right-clicks inside rendered markdown still get the message menu, but
      // text selection keeps the native menu
      if (String(window.getSelection())) return;
      ev.preventDefault();
      openContextMenu(ev.clientX, ev.clientY, msgMenuItems(m, opts));
    };
  };

  // small transient pill at the bottom of the message area: copy confirmations
  // and "this permalink went nowhere" notes, neither of which deserve a dialog
  let noticeTimer = 0;
  const notice = (text, isErr) => {
    let el = document.getElementById('notice');
    if (!el) {
      el = document.createElement('div');
      el.id = 'notice';
      document.body.appendChild(el);
    }
    el.textContent = text;
    el.classList.toggle('err', !!isErr);
    el.classList.remove('hidden');
    clearTimeout(noticeTimer);
    noticeTimer = setTimeout(() => el.classList.add('hidden'), 2600);
  };

  // A single themed context menu, reused for every right-click target. items is
  // [{label, danger?, run}]; dismisses on pick, click-outside, Esc, scroll, resize.
  let closeContextMenu = () => {};
  function openContextMenu(x, y, items) {
    closeContextMenu();
    const menu = document.createElement('div');
    menu.className = 'context-menu';
    items.forEach((it) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'ctx-item' + (it.danger ? ' danger' : '');
      b.textContent = it.label;
      b.onclick = () => { closeContextMenu(); it.run(); };
      menu.appendChild(b);
    });
    document.body.appendChild(menu);
    menu.querySelector('button')?.focus(); // keyboard users land on the first item
    // clamp inside the viewport (menu is measured after it is in the DOM)
    const w = menu.offsetWidth, h = menu.offsetHeight;
    menu.style.left = Math.min(x, window.innerWidth - w - 8) + 'px';
    menu.style.top = Math.min(y, window.innerHeight - h - 8) + 'px';

    const onKey = (e) => { if (e.key === 'Escape') closeContextMenu(); };
    const onDown = (e) => { if (!menu.contains(e.target)) closeContextMenu(); };
    closeContextMenu = () => {
      menu.remove();
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('mousedown', onDown, true);
      window.removeEventListener('scroll', closeContextMenu, true);
      window.removeEventListener('resize', closeContextMenu);
      closeContextMenu = () => {};
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('mousedown', onDown, true);
    window.addEventListener('scroll', closeContextMenu, true);
    window.addEventListener('resize', closeContextMenu);
  }

  // One thread leaf, rendered nested under its parent channel (Discord-style).
  // Same mention-only rule as channels: glow on any unread, a number only for
  // unread @mentions.
  const threadLeafLi = (t) => {
    const li = document.createElement('li');
    li.className = 'thread-leaf';
    const active = t.root_id === openThreadRoot;
    if (active) li.classList.add('active');
    const snippet = t.body.replace(/\s+/g, ' ').slice(0, 30) || '(attachment)';
    // a thread hangs off its channel: the Lucide elbow marks every leaf; a muted
    // thread shows the same bell-off as a muted channel
    li.innerHTML = `<span class="t-icon" aria-hidden="true">${ICON.cornerDownRight}</span>
      <span class="t-snippet">${esc(snippet)}</span>`;
    if (t.muted) { li.classList.add('muted'); li.insertAdjacentHTML('beforeend', '<span class="mute-mark">' + ICON.bellOff + '</span>'); }
    if (t.unread_count > 0 && !t.muted && !active) {
      li.classList.add('unread');
      if (t.unread_mentions > 0) {
        const b = document.createElement('span');
        b.className = 't-count unread-badge';
        b.textContent = t.unread_mentions > 99 ? '99+' : String(t.unread_mentions);
        li.appendChild(b);
      }
    }
    li.title = `${t.author_name}: ${t.body.slice(0, 200)}\n(hover to hide, right-click for actions)`;
    const act = async (path, body) => {
      try {
        await api(`/api/v1/threads/${t.root_id}/${path}`, { method: 'POST', body });
        loadThreads();
      } catch (e) { alert(e.message); }
    };
    // hover-reveal hide (resolve): the thread leaves the sidebar and comes back
    // on its own the next time anyone writes in it or mentions you there
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 't-archive';
    x.innerHTML = ICON.x;
    x.title = 'Hide thread';
    x.setAttribute('aria-label', x.title);
    x.onclick = (ev) => {
      ev.stopPropagation(); // do not open the thread
      if (openThreadRoot === t.root_id) closeThread();
      act('resolve', { resolved: true });
    };
    li.appendChild(x);
    li.onclick = () => openThread(t.root_id);
    li.oncontextmenu = (ev) => {
      ev.preventDefault();
      openContextMenu(ev.clientX, ev.clientY, [
        { label: t.subscribed ? 'Unsubscribe' : 'Subscribe', run: () => act('subscribe', { subscribed: !t.subscribed }) },
        { label: t.muted ? 'Unmute thread' : 'Mute thread', run: () => act('mute', { muted: !t.muted }) },
        { label: 'Hide thread', danger: true, run: () => act('resolve', { resolved: true }) },
      ]);
    };
    return li;
  };

  // ---------- sidebar drag-and-drop ----------
  // The "Move to section…" menu stays as the keyboard path; this is the pointer
  // one. Only channel rows drag; thread leaves and headers are targets at most.
  let dragChannel = null;

  const clearDropMarks = () => document.querySelectorAll('#channel-list li')
    .forEach((n) => n.classList.remove('drop-before', 'drop-after', 'drop-into'));

  const endDrag = () => {
    dragChannel = null;
    $('channel-list').classList.remove('dnd');
    document.querySelectorAll('#channel-list li.dragging').forEach((n) => n.classList.remove('dragging'));
    clearDropMarks();
  };

  // Sidebar layout writes run one at a time. Each ends with a fetchGroups, and a
  // fetch that lands after a newer local change would stomp it back.
  let layoutQueue = Promise.resolve();
  const queueLayout = (fn) => {
    layoutQueue = layoutQueue.catch(() => {}).then(fn);
    return layoutQueue;
  };

  // persist a whole section's order, slot by slot. Every channel in the list
  // gets a row, so a section the user has never dragged stops being row-less.
  const writeOrder = async (ids, groupID) => {
    for (let i = 0; i < ids.length; i += 1) {
      await api('/api/v1/channels/' + ids[i] + '/group', { method: 'PUT', body: { group_id: groupID || null, position: i } });
    }
  };

  // groupID null drops the channel out of every section; index is its slot.
  const dropChannel = async (ch, groupID, index) => {
    const target = groups.find((g) => g.id === groupID);
    const order = target ? (target.channel_ids || []) : defaultMembers().map((c) => c.id);
    const ids = order.filter((id) => id !== ch.id);
    ids.splice(Math.max(0, Math.min(index, ids.length)), 0, ch.id);
    groups.forEach((g) => { g.channel_ids = (g.channel_ids || []).filter((id) => id !== ch.id); });
    // hold the store entry the drag started in: a workspace switch mid-write
    // must not land these positions on the wrong workspace
    const e = active();
    if (target) target.channel_ids = ids;
    else { ungrouped = ids; if (e) e.ungrouped = ids; }
    renderChannels(); // land the row now; the writes only confirm it
    await queueLayout(async () => {
      try {
        await writeOrder(ids, groupID);
      } catch (err) { notice(err.message, true); }
      await fetchGroups(e);
      renderChannels();
    });
  };

  const dropEdge = (li, ev) => {
    const r = li.getBoundingClientRect();
    return ev.clientY > r.top + r.height / 2 ? 1 : 0;
  };

  const makeDragRow = (li, ch, groupID) => {
    li.draggable = true;
    li.dataset.chid = ch.id;
    li.ondragstart = (ev) => {
      dragChannel = ch;
      ev.dataTransfer.effectAllowed = 'move';
      ev.dataTransfer.setData('text/plain', ch.id);
      li.classList.add('dragging');
      $('channel-list').classList.add('dnd');
    };
    li.ondragend = endDrag;
    li.ondragover = (ev) => {
      if (!dragChannel || dragChannel.id === ch.id) return;
      ev.preventDefault();
      clearDropMarks();
      li.classList.add(dropEdge(li, ev) ? 'drop-after' : 'drop-before');
    };
    li.ondrop = (ev) => {
      if (!dragChannel || dragChannel.id === ch.id) return;
      ev.preventDefault();
      ev.stopPropagation();
      const moved = dragChannel;
      const g = groups.find((x) => x.id === groupID);
      const order = g ? (g.channel_ids || []) : defaultMembers().map((c) => c.id);
      const ids = order.filter((id) => id !== moved.id);
      const index = ids.indexOf(ch.id) + dropEdge(li, ev);
      endDrag();
      dropChannel(moved, groupID, index);
    };
  };

  const makeDropZone = (li, groupID, index) => {
    li.ondragover = (ev) => {
      if (!dragChannel) return;
      ev.preventDefault();
      clearDropMarks();
      li.classList.add('drop-into');
    };
    li.ondrop = (ev) => {
      if (!dragChannel) return;
      ev.preventDefault();
      const moved = dragChannel;
      endDrag();
      dropChannel(moved, groupID, index());
    };
  };

  // monochrome sigils: a hash for public, a lock for private, both in the
  // row's own colour so the list carries no emoji colour
  const sigilEl = (priv) => {
    const el = document.createElement('span');
    el.className = 'sigil' + (priv ? ' sigil-lock' : '');
    el.setAttribute('aria-hidden', 'true');
    el.innerHTML = priv ? ICON.lock : ICON.hash;
    return el;
  };
  const setChannelTitle = (ch) => {
    $('channel-title').replaceChildren(sigilEl(ch.private), ch.name);
    // the rename pencil: admins only, never on #general (server rule)
    $('rename-channel').classList.toggle('hidden', !(me && me.role === 'admin' && ch.name !== 'general'));
  };

  const renameChannel = async (ch) => {
    const name = prompt('Rename #' + ch.name + ':', ch.name);
    if (!name || !name.trim() || name.trim() === ch.name) return;
    try {
      await api('/api/v1/channels/' + ch.id, { method: 'PATCH', body: { name: name.trim() } });
    } catch (e) {
      alert(e.code === 'name_taken' ? 'A channel named #' + name.trim().replace(/^#/, '').toLowerCase() + ' already exists.' : e.message);
    }
  };
  $('rename-channel').onclick = () => { if (current) renameChannel(current); };

  // The server's tree is intentionally personal (authored, replied, mentioned
  // or subscribed). A deep link can still open any readable thread; keep that
  // one represented in navigation while it is open without subscribing the
  // viewer or changing which future replies notify them.
  const sidebarThreads = () => openThreadSummary && !threads.some((t) => t.root_id === openThreadSummary.root_id)
    ? [openThreadSummary, ...threads] : threads;

  // One channel row (with its nested thread leaves appended right beneath it).
  const appendChannel = (ul, ch, groupID) => {
    const li = document.createElement('li');
    // sigil first, name in its own span: the checks read .chan-name
    li.append(sigilEl(ch.private), ' ');
    const nameEl = document.createElement('span');
    nameEl.className = 'chan-name';
    nameEl.textContent = ch.name + (ch.archived ? ' (archived)' : '');
    li.appendChild(nameEl);
    if (ch.archived) li.classList.add('archived');
    if (ch.muted) { li.classList.add('muted'); li.insertAdjacentHTML('beforeend', '<span class="mute-mark">' + ICON.bellOff + '</span>'); }
    // while a thread with a sidebar row is open only that row is selected, the
    // parent channel row goes plain; a thread without a row keeps the channel lit
    const tree = sidebarThreads();
    const threadRow = !!openThreadRoot && tree.some((t) => t.root_id === openThreadRoot);
    if (current && ch.id === current.id && !threadRow) li.classList.add('active');
    // Any unread glows the channel name; only @mentions get a numeric badge.
    // A muted channel stays dark unless you are mentioned (or broadcast at).
    const glows = ch.unread_count > 0 && (!ch.muted || ch.unread_mentions > 0);
    if (glows && !(current && ch.id === current.id)) {
      li.classList.add('unread');
      if (ch.unread_mentions > 0) {
        const b = document.createElement('span');
        b.className = 'unread-badge';
        b.textContent = ch.unread_mentions > 99 ? '99+' : String(ch.unread_mentions);
        li.appendChild(b);
      }
    }
    li.onclick = () => selectChannel(ch);
    li.oncontextmenu = (ev) => {
      ev.preventDefault();
      const items = [];
      if (ch.private) items.push({ label: 'Add people', run: () => addPeople(ch) });
      // conversion gate mirrors the server: admins or the creator; #general never
      if (!ch.private && ch.name !== 'general' && (me.role === 'admin' || ch.created_by === me.id)) {
        items.push({ label: 'Make private', run: () => makePrivate(ch) });
      }
      // the server allows this only while the channel is still empty
      if (ch.private && (me.role === 'admin' || ch.created_by === me.id)) {
        items.push({ label: 'Make public', run: () => makePublic(ch) });
      }
      if (me.role === 'admin' && ch.name !== 'general') items.push({ label: 'Rename channel', run: () => renameChannel(ch) });
      items.push({ label: ch.muted ? 'Unmute channel' : 'Mute channel', run: () => muteChannel(ch, !ch.muted) });
      items.push({ label: 'Move to section…', run: () => openMoveMenu(ev.clientX, ev.clientY, ch) });
      // #general is pinned: it can be organized into a section but never left.
      if (ch.name !== 'general') items.push({ label: 'Leave channel', danger: true, run: () => leaveChannel(ch) });
      openContextMenu(ev.clientX, ev.clientY, items);
    };
    makeDragRow(li, ch, groupID);
    ul.appendChild(li);
    const leaves = tree.filter((t) => t.channel_id === ch.id && !isQuiet(t)).map(threadLeafLi);
    leaves.forEach((li) => ul.appendChild(li));
    if (leaves.length) leaves[leaves.length - 1].classList.add('last');
  };

  // A thread you hid by hand never reaches the client (the server drops it
  // until the next message there). A quiet one just leaves the sidebar after
  // archive_after_secs; any new message or mention moves its activity clock,
  // which brings it back on its own. The open thread always stays visible.
  const isQuiet = (t) => {
    if (t.root_id === openThreadRoot) return false;
    const after = Number(notifyPrefs.archive_after_secs) || 0;
    if (after <= 0) return false;
    return Date.now() - Date.parse(t.last_activity_at) > after * 1000;
  };
  // the inactivity clock is client-side; the server holds the truth it reads,
  // and a periodic refetch keeps a second tab's manual hides in step
  setInterval(() => { if (threads.length) renderChannels(); }, 10000);
  setInterval(() => { if (me) loadThreads(); }, 30000);

  const groupOf = () => {
    const map = {};
    groups.forEach((g) => (g.channel_ids || []).forEach((cid) => { map[cid] = g.id; }));
    return map;
  };

  // The default section holds every channel in no named section. Its stored
  // order comes first; a channel created since the last drag has no row yet, so
  // it lands after, in the server's order.
  const defaultMembers = () => {
    const placement = groupOf();
    const loose = channels.filter((ch) => !placement[ch.id]);
    const known = ungrouped.map((id) => loose.find((c) => c.id === id)).filter(Boolean);
    return known.concat(loose.filter((ch) => !ungrouped.includes(ch.id)));
  };

  const renderChannels = () => {
    setTitle(); // the open room's channel state is half of the tab badge
    const ul = $('channel-list');
    ul.innerHTML = '';

    // the default section: a real header like any other, but it cannot be
    // renamed or deleted, and it is also the drop target for leaving a section
    const members0 = defaultMembers();
    const head0 = document.createElement('li');
    head0.className = 'section-header default-section' + (defaultCollapsed ? ' collapsed' : '');
    head0.innerHTML = `<span class="sec-chevron">${defaultCollapsed ? ICON.chevronRight : ICON.chevronDown}</span><span class="sec-name">Channels</span>`;
    const rolled = members0.filter((ch) => ch.unread_count > 0 && (!ch.muted || ch.unread_mentions > 0) && !(current && current.id === ch.id));
    if (defaultCollapsed && rolled.length) {
      head0.classList.add('unread');
      const mentions = members0.reduce((n, ch) => n + (ch.unread_mentions || 0), 0);
      if (mentions > 0) {
        const b = document.createElement('span');
        b.className = 'unread-badge';
        b.textContent = mentions > 99 ? '99+' : String(mentions);
        head0.appendChild(b);
      }
    }
    head0.onclick = () => toggleDefaultSection();
    makeDropZone(head0, null, () => members0.length);
    ul.appendChild(head0);
    if (!defaultCollapsed) members0.forEach((ch) => appendChannel(ul, ch, null));

    // then each personal section, in order
    groups.forEach((g) => {
      const members = (g.channel_ids || []).map((cid) => channels.find((c) => c.id === cid)).filter(Boolean);
      const header = document.createElement('li');
      header.className = 'section-header' + (g.collapsed ? ' collapsed' : '');
      // a collapsed section rolls up its members' attention: glow on any unread,
      // a numeric badge for the total unread @mentions inside it.
      const unread = members.some((c) => c.unread_count > 0 && (!c.muted || c.unread_mentions > 0) && !(current && current.id === c.id));
      const mentions = members.reduce((n, c) => n + (c.unread_mentions || 0), 0);
      if (g.collapsed && unread) header.classList.add('unread');
      header.innerHTML = `<span class="sec-chevron">${g.collapsed ? ICON.chevronRight : ICON.chevronDown}</span><span class="sec-name">${esc(g.name)}</span>`;
      if (g.collapsed && mentions > 0) {
        const b = document.createElement('span');
        b.className = 'unread-badge';
        b.textContent = mentions > 99 ? '99+' : String(mentions);
        header.appendChild(b);
      }
      header.onclick = () => toggleGroup(g);
      // a header drop appends, which is also the only sane slot when collapsed
      makeDropZone(header, g.id, () => (g.channel_ids || []).length);
      header.oncontextmenu = (ev) => {
        ev.preventDefault();
        openContextMenu(ev.clientX, ev.clientY, [
          { label: 'Rename section', run: () => renameGroup(g) },
          { label: 'Delete section', danger: true, run: () => deleteGroup(g) },
        ]);
      };
      ul.appendChild(header);
      if (!g.collapsed) members.forEach((ch) => appendChannel(ul, ch, g.id));
    });
  };

  const fetchGroups = async (e = active()) => {
    const seq = ++e.groupLoadSeq;
    try {
      const layout = await api('/api/v1/channel-groups', { ws: e.slug });
      if (seq !== e.groupLoadSeq) return false;
      e.groups = layout.groups || [];
      e.ungrouped = layout.ungrouped || [];
      e.defaultCollapsed = !!layout.default_collapsed;
    } catch (err) {
      if (seq !== e.groupLoadSeq) return false;
      e.groups = []; e.ungrouped = []; e.defaultCollapsed = false;
    }
    if (e === active()) { groups = e.groups; ungrouped = e.ungrouped; defaultCollapsed = e.defaultCollapsed; }
    return true;
  };

  // Collapse/expand is optimistic: flip locally and re-render, then persist.
  const toggleGroup = async (g) => {
    g.collapsed = !g.collapsed;
    renderChannels();
    try { await api('/api/v1/channel-groups/' + g.id, { method: 'PATCH', body: { collapsed: g.collapsed } }); }
    catch (e) { /* next refresh corrects the flag */ }
  };

  const toggleDefaultSection = async () => {
    defaultCollapsed = !defaultCollapsed;
    const e = active();
    if (e) e.defaultCollapsed = defaultCollapsed;
    renderChannels();
    try { await api('/api/v1/channel-groups/default', { method: 'PATCH', body: { collapsed: defaultCollapsed } }); }
    catch (err) { /* next refresh corrects the flag */ }
  };

  const muteChannel = async (ch, muted) => {
    try {
      await api('/api/v1/channels/' + ch.id + '/mute', { method: 'POST', body: { muted } });
      ch.muted = muted;
      renderChannels();
      notice((muted ? 'Muted #' : 'Unmuted #') + ch.name);
    } catch (e) { alert(e.message); }
  };

  const moveChannel = (ch, groupID) => queueLayout(async () => {
    try {
      await api('/api/v1/channels/' + ch.id + '/group', { method: 'PUT', body: { group_id: groupID } });
      await fetchGroups();
      renderChannels();
    } catch (e) { alert(e.message); }
  });

  const openMoveMenu = (x, y, ch) => {
    const placement = groupOf();
    const items = groups
      .filter((g) => placement[ch.id] !== g.id)
      .map((g) => ({ label: g.name, run: () => moveChannel(ch, g.id) }));
    items.push({ label: 'New section…', run: () => createSectionAndMove(ch) });
    if (placement[ch.id]) items.push({ label: 'Remove from section', danger: true, run: () => moveChannel(ch, null) });
    openContextMenu(x, y, items);
  };

  const createSectionAndMove = async (ch) => {
    const name = prompt('New section name:');
    if (!name || !name.trim()) return;
    try {
      const g = await api('/api/v1/channel-groups', { method: 'POST', body: { name: name.trim() } });
      await moveChannel(ch, g.id);
    } catch (e) { alert(e.message); }
  };

  const renameGroup = async (g) => {
    const name = prompt('Rename section:', g.name);
    if (!name || !name.trim() || name.trim() === g.name) return;
    await queueLayout(async () => {
      try {
        await api('/api/v1/channel-groups/' + g.id, { method: 'PATCH', body: { name: name.trim() } });
        await fetchGroups();
        renderChannels();
      } catch (e) { alert(e.message); }
    });
  };

  // Delete drops the section's channels into the default one. The server appends
  // them after the rows it can see, but a channel never dragged has no row, so
  // only the client knows the full order. Write it back.
  const deleteGroup = (g) => queueLayout(async () => {
    const wanted = defaultMembers().map((c) => c.id).concat((g.channel_ids || []));
    const e = active();
    try {
      await api('/api/v1/channel-groups/' + g.id, { method: 'DELETE' });
      groups = groups.filter((x) => x.id !== g.id);
      ungrouped = wanted;
      if (e) { e.groups = groups; e.ungrouped = wanted; }
      renderChannels();
      await writeOrder(wanted, null);
      await fetchGroups(e);
      renderChannels();
    } catch (err) { alert(err.message); }
  });

  // Reply bars in the open channel view share the thread tree's unread state.
  const syncReplyBars = () => {
    document.querySelectorAll('#messages .reply-bar').forEach((bar) => {
      const id = bar.closest('.msg')?.dataset.id;
      const th = threads.find((x) => x.root_id === id);
      bar.classList.toggle('unread', !!(th && th.unread_count > 0 && !th.muted));
    });
  };

  // the top-level offline section's reveal state.
  // in-memory only — collapses back on reload by design.
  let offlineOpen = false;
  let profileFor = null;
  let profileDeliveryFor = null;

  const showProfile = (p) => {
    profileFor = p;
    const slot = $('profile-avatar');
    slot.innerHTML = '';
    slot.appendChild(avatarEl(p, 'avatar-lg'));
    $('profile-name').textContent = p.name;
    $('profile-meta').textContent =
      `${p.role}${p.is_human ? ' · human' : ' · agent'} · ${p.online ? 'online' : 'offline'}`;
    $('profile-desc').innerHTML = p.description ? linkify(p.description) : 'No description.';
    const tags = (p.tags || []).map((t) => t.tag).join(', ');
    $('profile-tags').textContent = tags ? 'Tags: ' + tags : '';
    showDelivery(p);
    showCapabilities(p);
    showReminders(p);
    $('profile-modal').classList.remove('hidden');
  };

  // Capabilities (task 27): what an agent can be called for. Hidden for
  // humans and for agents without any; offline agents are listed, not callable.
  let profileCapsFor = null; // the agent whose profile is open, for live refresh
  const showCapabilities = (p) => {
    const box = $('profile-caps');
    box.classList.add('hidden');
    box.textContent = '';
    profileCapsFor = p.is_human ? null : p;
    if (p.is_human) return;
    api('/api/v1/participants/' + encodeURIComponent(p.id) + '/capabilities').then((out) => {
      if (profileCapsFor !== p) return; // another profile opened meanwhile
      const caps = out.capabilities || [];
      if (!caps.length) return;
      const h = document.createElement('h4');
      h.textContent = 'Capabilities' + (out.online ? '' : ' · not callable: offline');
      box.appendChild(h);
      caps.forEach((c) => {
        const row = document.createElement('div');
        row.className = 'cap-row';
        const name = document.createElement('code');
        name.textContent = c.name;
        const desc = document.createElement('span');
        desc.textContent = c.description || '';
        const toggle = document.createElement('button');
        toggle.type = 'button';
        toggle.className = 'cap-schema-toggle';
        toggle.textContent = 'schema';
        const pre = document.createElement('pre');
        pre.className = 'cap-schema hidden';
        pre.textContent = JSON.stringify(c.inputSchema, null, 2);
        toggle.onclick = () => pre.classList.toggle('hidden');
        row.append(name, desc, toggle);
        box.append(row, pre);
      });
      box.classList.remove('hidden');
    }).catch(() => {});
  };

  // Reminders (task 22): an agent's owner (or an admin) sees what the agent
  // scheduled for itself and can delete one; everyone else sees nothing.
  let profileRemindersFor = null;
  const fmtFire = (iso) => {
    if (!iso) return 'never';
    const d = new Date(iso);
    return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + fmtTime(iso);
  };
  const showReminders = (p) => {
    const box = $('profile-reminders');
    box.classList.add('hidden');
    box.textContent = '';
    const canSee = me && !p.is_human && (me.role === 'admin' || p.owner_id === me.id);
    profileRemindersFor = canSee ? p : null;
    if (!canSee) return;
    api('/api/v1/participants/' + encodeURIComponent(p.id) + '/reminders').then((out) => {
      if (profileRemindersFor !== p) return;
      const list = out.reminders || [];
      const h = document.createElement('h4');
      h.textContent = 'Reminders' + (list.length ? '' : ' · none');
      box.appendChild(h);
      list.forEach((r) => {
        const row = document.createElement('div');
        row.className = 'rem-row';
        row.dataset.id = r.id;
        const text = document.createElement('div');
        text.className = 'rem-text';
        text.textContent = r.text;
        const meta = document.createElement('div');
        meta.className = 'rem-meta';
        const next = r.next_fire_at ? 'next ' + fmtFire(r.next_fire_at) : 'done';
        meta.textContent = r.schedule + (r.tz && r.tz !== 'UTC' ? ' (' + r.tz + ')' : '') +
          ' · ' + next + ' · last fired ' + fmtFire(r.last_fired_at);
        const del = document.createElement('button');
        del.type = 'button';
        del.className = 'rem-delete';
        del.title = 'Delete this reminder';
        del.innerHTML = ICON.x;
        del.onclick = async () => {
          if (!confirm('Delete this reminder?\n\n' + r.text)) return;
          try {
            await api('/api/v1/participants/' + encodeURIComponent(p.id) + '/reminders/' + encodeURIComponent(r.id), { method: 'DELETE' });
            showReminders(p);
          } catch (e) { alert(e.message); }
        };
        row.append(text, meta, del);
        box.appendChild(row);
      });
      box.classList.remove('hidden');
    }).catch(() => {});
  };

  // Delivery receipts (task 25): an agent's owner, an admin, or the agent
  // itself sees how its addressed events fared; everyone else sees nothing.
  const showDelivery = (p) => {
    const row = $('profile-delivery');
    row.classList.add('hidden');
    row.textContent = '';
    const canSee = me && !p.is_human && (me.role === 'admin' || p.owner_id === me.id || p.id === me.id);
    profileDeliveryFor = canSee ? p : null;
    if (!canSee) return;
    api('/api/v1/participants/' + encodeURIComponent(p.id) + '/delivery').then((st) => {
      if (profileDeliveryFor !== p) return;
      // each row sits in exactly one state, so "awaiting ack" is delivered-but-unacked
      const parts = [st.acked + ' acked', st.delivered + ' awaiting ack', st.deferred + ' deferred', st.failed + ' failed'];
      if (st.accepted) parts.push(st.accepted + ' queued');
      let text = 'Delivery: ' + parts.join(' · ');
      if (st.oldest_unacked_at) text += ' · oldest unacked ' + fmtLastReply(st.oldest_unacked_at);
      row.textContent = text;
      row.title = 'accepted → delivered → acked; deferred while offline; failed once retries ran out or the dead-letter age passed';
      row.classList.remove('hidden');
    }).catch(() => {});
  };

  // leaf=true renders the row as an owned-agent child (indented). Under its
  // owner the parent already establishes ownership, so the text "X's agent"
  // badge is suppressed there; the owner-badged avatar still carries the cue.
  // opts: parents use hasKids/collapsed/kidCount/kidOnline/rollup/onToggle;
  // agent leaves may use canDelete.
  // Non-leaf rows always reserve the toggle column so avatars stay aligned;
  // only a parent with nested agents gets a real chevron.
  const participantLi = (p, leaf, opts) => {
    opts = opts || {};
    const li = document.createElement('li');
    li.dataset.id = p.id;
    if (leaf) li.classList.add('participant-leaf');
    if (!p.online) li.classList.add('offline');
    if (opts.rollup) li.classList.add('rollup'); // a collapsed child's presence, surfaced on the parent
    const tags = (p.tags || []).map((t) => t.tag).join(', ');
    const owner = (!leaf && p.owner_name) ? `<span class="owner-badge" title="server-verified owner">${esc(p.owner_name)}'s agent</span>` : '';
    const toggle = leaf ? '' :
      `<span class="p-toggle${opts.hasKids ? '' : ' spacer'}" data-state="${opts.hasKids ? (opts.collapsed ? 'collapsed' : 'open') : ''}">${opts.hasKids ? (opts.collapsed ? ICON.chevronRight : ICON.chevronDown) : ''}</span>`;
    const count = (opts.hasKids && opts.collapsed) ?
      `<span class="p-agentcount${opts.kidOnline ? '' : ' all-off'}" title="${opts.kidOnline} of ${opts.kidCount} agent${opts.kidCount === 1 ? '' : 's'} online"><span class="on">${opts.kidOnline}</span>/${opts.kidCount}</span>` : '';
    const remove = opts.canDelete ? `<button type="button" class="agent-delete" title="Delete ${esc(p.name)}" aria-label="Delete ${esc(p.name)}">${ICON.x}</button>` : '';
    li.innerHTML = `${toggle}<span class="dot${p.online ? ' online' : ''}"></span>
      <span class="av-slot"></span>
      <span class="pname">${esc(p.name)}</span>${owner}
      <span class="desc-preview">${esc(p.description || (tags ? '[' + tags + ']' : ''))}</span>${remove}${count}`;
    li.querySelector('.av-slot').replaceWith(avatarEl(p, 'avatar-sm'));
    li.title = `${p.name} — ${p.description || ''}${tags ? ' [' + tags + ']' : ''}`;
    li.onclick = () => showProfile(p);
    if (opts.onToggle) {
      const t = li.querySelector('.p-toggle');
      t.onclick = (ev) => { ev.stopPropagation(); opts.onToggle(); }; // chevron toggles, never opens the profile
    }
    if (opts.canDelete) {
      const del = li.querySelector('.agent-delete');
      del.onclick = async (ev) => {
        ev.stopPropagation();
        if (!confirm(`Delete ${p.name}?\n\nIts token will stop working immediately. Past messages stay with this old identity.`)) return;
        del.disabled = true;
        try {
          await api('/api/v1/participants/' + encodeURIComponent(p.id), { method: 'DELETE' });
          const i = participants.findIndex((x) => x.id === p.id);
          if (i >= 0) participants.splice(i, 1);
          renderParticipants();
        } catch (e) {
          del.disabled = false;
          alert(e.message);
        }
      };
    }
    return li;
  };

  // Each human's expand/collapse choice persists per room in localStorage.
  // Default is collapsed, so we store the set of humans the user has EXPANDED;
  // an id absent from the set stays collapsed across reloads.
  const rosterKey = () => 'agentchat:roster:' + (room ? room.slug : '');
  const expandedSet = () => {
    try { return new Set(JSON.parse(localStorage.getItem(rosterKey()) || '[]')); } catch (e) { return new Set(); }
  };
  const toggleHuman = (id) => {
    const set = expandedSet();
    set.has(id) ? set.delete(id) : set.add(id);
    try { localStorage.setItem(rosterKey(), JSON.stringify([...set])); } catch (e) { /* private mode */ }
    renderParticipants();
  };

  // Participants render as a tree: each human is a parent, the agents whose
  // server-verified owner_id points at them nest beneath. The parent's nested
  // agents collapse under a chevron (collapsed by default); a collapsed parent
  // still shows a hidden-agent count and rolls up a glow if any hidden agent is
  // online, so no presence signal is lost by collapsing. Ownerless agents (or
  // ones whose owner is not a visible human) group under "unowned agents".
  const renderParticipants = () => {
    const ul = $('participant-list');
    ul.innerHTML = '';
    const humans = participants.filter((p) => p.is_human);
    const agents = participants.filter((p) => !p.is_human);
    const ownerOf = (a) => (a.owner_id && humans.find((h) => h.id === a.owner_id)) ? a.owner_id : null;
    const canDeleteAgent = (a) => !!(me && me.is_human && (me.role === 'admin' || a.owner_id === me.id));
    const expanded = expandedSet();

    // the one "▸ offline (n)" divider, at the top level: fully-offline humans
    // render BELOW it, and only when it is toggled open.
    const offlineDivider = (count) => {
      const t = document.createElement('li');
      t.className = 'offline-toggle';
      t.innerHTML = `${offlineOpen ? ICON.chevronDown : ICON.chevronRight} offline (${count})`;
      t.onclick = () => {
        offlineOpen = !offlineOpen;
        renderParticipants();
      };
      ul.appendChild(t);
    };

    // renders a parent's kid list as ONE flat list: online kids first, then the
    // offline ones with their grey dot. No sub-header and no second toggle
    // inside a parent: only the top-level offline section collapses.
    const renderKids = (kids) => {
      const on = kids.filter((a) => a.online);
      const off = kids.filter((a) => !a.online);
      [...on, ...off].forEach((a) => ul.appendChild(participantLi(a, true, { canDelete: canDeleteAgent(a) })));
    };

    // an offline human with an online agent stays above the root divider so the
    // agent's presence is never hidden; fully-offline humans sink below it.
    const sunk = (h, kids) => !h.online && !kids.some((a) => a.online);
    const kidsOf = (h) => agents.filter((a) => ownerOf(a) === h.id);

    // your own row always expands: with no agents yet it opens on the
    // "Add an agent" row alone, which is how a human gets their first one
    const mine = (h) => me && me.is_human && h.id === me.id;
    const addAgentRow = () => {
      const li = document.createElement('li');
      li.className = 'addagent-row participant-leaf';
      li.id = 'addagent-row';
      li.innerHTML = '<span class="addagent-plus" aria-hidden="true">' + ICON.plus + '</span>Add an agent';
      li.title = 'Mint a link that badges the joining agent as yours';
      li.onclick = () => openAddAgent();
      ul.appendChild(li);
    };
    const renderHuman = (h) => {
      const kids = kidsOf(h);
      const hasKids = kids.length > 0 || mine(h);
      const collapsed = hasKids && !expanded.has(h.id);
      const rollup = collapsed && kids.some((a) => a.online); // hidden child's green dot, surfaced
      ul.appendChild(participantLi(h, false, {
        hasKids, collapsed, kidCount: kids.length, kidOnline: kids.filter((a) => a.online).length, rollup,
        onToggle: hasKids ? () => toggleHuman(h.id) : null,
      }));
      if (collapsed) return;
      renderKids(kids);
      if (mine(h)) addAgentRow();
    };

    humans.filter((h) => !sunk(h, kidsOf(h))).forEach(renderHuman);

    const ownerless = agents.filter((a) => ownerOf(a) === null);
    if (ownerless.length > 0) {
      const g = document.createElement('li');
      g.className = 'group-label';
      g.textContent = 'unowned agents';
      ul.appendChild(g);
      renderKids(ownerless);
    }

    const sunkHumans = humans.filter((h) => sunk(h, kidsOf(h)));
    if (sunkHumans.length === 0) return;
    offlineDivider(sunkHumans.length);
    if (offlineOpen) sunkHumans.forEach(renderHuman);
  };

  // the unread total behind the tab title and the favicon pill (task 18):
  // every non-muted workspace's unread_count from the rail feed, the open
  // one live from its channel list (a muted channel counts only its
  // mentions, like the sidebar). Muted workspaces never count.
  const roomUnread = () => channels.reduce((n, c) => n + (c.muted ? (c.unread_mentions || 0) : (c.unread_count || 0)), 0);
  const railEntry = (slug) => railRooms.find((w) => w.slug === slug);
  const roomMuted = () => { const w = room && railEntry(room.slug); return !!(w && w.muted); };
  const unreadTotal = () => {
    let n = 0;
    for (const w of railRooms) {
      if (w.muted || (room && w.slug === room.slug)) continue;
      n += wsCounts(w).unread;
    }
    if (room && !roomMuted()) n += roomUnread();
    return n;
  };
  const capCount = (n) => (n > 99 ? '99+' : String(n));
  // the favicon is drawn on a canvas over the shipped 32px mark: a red pill
  // with the count bottom-right, Discord-style; zero puts the plain files back
  const faviconLinks = () => [...document.querySelectorAll('link[rel="icon"]')];
  const faviconOriginal = faviconLinks().map((l) => l.getAttribute('href'));
  let faviconImg = null;
  let faviconShown = 0;
  const paintFavicon = (n) => {
    if (n === faviconShown) return;
    faviconShown = n;
    const links = faviconLinks();
    if (n === 0) { links.forEach((l, i) => l.setAttribute('href', faviconOriginal[i])); return; }
    const draw = () => {
      const cv = document.createElement('canvas');
      cv.width = 32; cv.height = 32;
      const cx = cv.getContext('2d');
      if (faviconImg && faviconImg.complete && faviconImg.naturalWidth) cx.drawImage(faviconImg, 0, 0, 32, 32);
      const text = capCount(n);
      cx.font = `bold ${text.length > 2 ? 11 : 13}px system-ui, sans-serif`;
      const tw = Math.ceil(cx.measureText(text).width);
      const w = Math.max(16, tw + 6);
      const h = 16;
      const x = 32 - w;
      const y = 32 - h;
      cx.fillStyle = '#ed4245';
      cx.beginPath();
      cx.roundRect(x, y, w, h, 8);
      cx.fill();
      cx.fillStyle = '#fff';
      cx.textAlign = 'center';
      cx.textBaseline = 'middle';
      cx.fillText(text, x + w / 2, y + h / 2 + 0.5);
      const url = cv.toDataURL('image/png');
      links.forEach((l) => l.setAttribute('href', url));
    };
    if (!faviconImg) {
      faviconImg = new Image();
      faviconImg.onload = () => { if (faviconShown === n) draw(); };
      faviconImg.src = faviconOriginal[0] || '/brand/favicon-32.png';
    }
    draw();
  };
  const setTitle = () => {
    // "(3) OpenChatter | Acme Team"; plain "OpenChatter" outside a workspace
    const n = unreadTotal();
    document.title = (n > 0 ? `(${n}) ` : '') + 'OpenChatter' + (room ? ' | ' + room.name : '');
    paintFavicon(n);
  };

  // ---------- data flows ----------

  // public channels you are not in: a "#name" for one still links, and
  // clicking it joins. A private one you are not in is not here, so it never
  // links and leaks nothing.
  let publicChannels = [];
  const fetchPublicChannels = async (e = active()) => {
    const seq = ++e.publicLoadSeq;
    try {
      const out = await api('/api/v1/channels/browse', { ws: e.slug });
      if (seq !== e.publicLoadSeq) return false;
      e.publicChannels = (out.channels || []).filter((c) => !c.member);
    } catch {
      if (seq !== e.publicLoadSeq) return false;
      e.publicChannels = [];
    }
    if (e === active()) publicChannels = e.publicChannels;
    return true;
  };
  const linkableChannels = () => channels.filter((c) => !c.archived).concat(publicChannels);

  // the workspace, its sidebar sections and browse list, into the entry; no DOM
  const loadRoomInto = async (e) => {
    const seq = ++e.roomLoadSeq;
    const groupSeq = ++e.groupLoadSeq;
    const publicSeq = ++e.publicLoadSeq;
    const [out, layout, browse] = await Promise.all([
      api('/api/v1/room', { ws: e.slug }),
      api('/api/v1/channel-groups', { ws: e.slug }).catch(() => null),
      api('/api/v1/channels/browse', { ws: e.slug }).catch(() => null),
    ]);
    // A refresh is one snapshot. Once a newer one starts, none of this one's
    // room/sidebar fields may commit, even if its HTTP response arrives last.
    if (seq !== e.roomLoadSeq) return false;
    e.room = out.room;
    e.joinURL = out.join_url;
    e.isAdmin = !!out.admin;
    e.channels = out.channels || [];
    e.participants = out.participants || [];
    if (groupSeq === e.groupLoadSeq) {
      e.groups = layout?.groups || [];
      e.ungrouped = layout?.ungrouped || [];
      e.defaultCollapsed = !!layout?.default_collapsed;
    }
    if (publicSeq === e.publicLoadSeq) e.publicChannels = (browse?.channels || []).filter((c) => !c.member);
    return true;
  };
  // every region of the workspace pane from the active entry, in one pass
  const paintRoom = () => {
    $('room-name').textContent = room.name;
    $('ws-current').textContent = room.name;
    paintRoomMark();
    renderMeFooter();
    renderChannels();
    renderParticipants();
    setTitle();
  };
  const refreshRoom = async () => {
    const e = active();
    if (!await loadRoomInto(e)) return;
    if (e !== active()) return; // switched away meanwhile
    adopt(e);
    paintRoom();
  };

  // ---------- URL <-> view sync ----------

  // A URL apply spans awaits, so a click can land in the middle of one. navSeq
  // lets the stale apply bail instead of undoing the newer navigation, and the
  // fromURL flag (not a shared flag) is what keeps popstate from pushing.
  let navSeq = 0;

  const syncURL = (push) => {
    if (!room) return;
    let path = roomPrefix + encodeURIComponent(room.slug);
    if (current) path += '/c/' + encodeURIComponent(current.name);
    if (openThreadRoot) path += '/t/' + encodeURIComponent(openThreadRoot);
    if (location.pathname === path) return;
    history[push ? 'pushState' : 'replaceState'](null, '', path);
  };

  const applyURL = async () => {
    const seq = ++navSeq;
    const segs = location.pathname.split('/').filter(Boolean);
    const chName = segs[2] === 'c' ? decodeURIComponent(segs[3] || '') : '';
    const rootID = segs[4] === 't' ? decodeURIComponent(segs[5] || '') : '';
    const urlSlug = decodeURIComponent(segs[1] || '');
    if (urlSlug && urlSlug !== slug) {
      await switchTo(urlSlug, { push: false, chName }); // back/forward across workspaces
      if (seq !== navSeq) return;
    }
    // a permalink ends in /m/<id>, with or without the thread segment
    const msgID = segs[4] === 'm' ? decodeURIComponent(segs[5] || '')
      : (segs[6] === 'm' ? decodeURIComponent(segs[7] || '') : '');
    const ch = channels.find((c) => c.name === chName)
      || channels.find((c) => c.name === 'general') || channels[0];
    if (ch && (!current || current.id !== ch.id)) {
      await selectChannel(ch, true);
      if (seq !== navSeq) return; // a click navigated while we were fetching
    }
    if (rootID) {
      try { await openThread(rootID, true); }
      catch (e) { if (seq === navSeq) closeThread(false); }
      if (seq === navSeq && msgID) await revealPermalink(msgID, !!rootID);
      return;
    }
    if (seq === navSeq && openThreadRoot) closeThread(false);
    if (seq === navSeq && msgID) await revealPermalink(msgID, false);
  };

  // A permalink can point at a deleted message, or one in a channel you cannot
  // read. Say so and keep the channel view rather than failing the navigation.
  const revealPermalink = async (msgID, inThread) => {
    if (await revealMessage(msgID, inThread)) return;
    try {
      const m = await api('/api/v1/messages/' + msgID);
      // it exists but did not render here: follow it to its own thread
      if (!inThread && m.thread_root_id) {
        try {
          await openThread(m.thread_root_id, true);
          if (await revealMessage(msgID, true)) return;
        } catch (e) { /* thread gone too */ }
      }
      notice('Message unavailable', true);
    } catch (e) { notice('Message unavailable', true); }
  };

  window.addEventListener('popstate', () => { if (me) applyURL(); });

  const closeThread = (push = true) => {
    if (push) navSeq++;
    const had = openThreadRoot !== null;
    $('thread-panel').classList.add('hidden');
    openThreadRoot = null;
    openThreadSummary = null;
    clearThreadAttachment(); // a file staged here must not follow you elsewhere
    if (had) renderChannels(); // clear the active-thread highlight in the sidebar
    if (had && push) syncURL(true);
  };

  const selectChannel = async (ch, fromURL = false) => {
    if (!fromURL) navSeq++;
    // a thread belongs to its channel; leaving the channel closes it, else a
    // reply would post against a root in another channel (server 400)
    // close without a push; the channel-change push below covers this transition
    if (current && ch.id !== current.id) closeThread(false);
    const changed = !current || current.id !== ch.id;
    if (changed) {
      clearMainAttachment(); // a staged file belongs to the channel where it was chosen
      talkedAt = new Map();
    }
    current = ch;
    syncURL(changed && !fromURL); // refreshes replace, real navigation pushes
    setChannelTitle(ch);
    $('channel-topic').innerHTML = ch.topic ? linkify(ch.topic) : '';
    const e = active();
    e.lastChannelID = ch.id;
    paintHeaderMembers(ch); // the cached count; refreshed after paint
    renderChannels();
    let page = pageFor(e, ch.id);
    const cached = !!page;
    let needsReconcile = false;
    if (!cached) {
      // Clear the previous channel immediately. The empty loading page is also
      // where live events land while the first snapshot is in flight.
      page = setPage(e, ch.id, []);
      page.loading = true;
      renderMessages(page.list, ch);
      const startedPage = page;
      const startedRevision = page.revision;
      let out;
      try {
        out = await api(`/api/v1/channels/${ch.id}/messages?limit=100`);
      } catch (err) {
        if (pageFor(e, ch.id) === startedPage) {
          startedPage.loading = false;
          if (startedPage.revision === 0) e.pages.delete(ch.id);
        }
        throw err;
      }
      const latestPage = pageFor(e, ch.id);
      if (latestPage === startedPage && latestPage.revision === startedRevision) {
        page = setPage(e, ch.id, out.messages || []);
      } else {
        // The response predates a live event (or a newer fetch). Keep that
        // page authoritative and let the guarded reconciliation fill history.
        page = latestPage;
        if (page) page.loading = false;
        needsReconcile = true;
      }
      if (!current || current.id !== ch.id || e !== active()) return; // a newer click won
    }
    page.openedAt = Date.now();
    renderMessages(page.list, ch);
    // a cached page paints with no request in flight; the reads and the
    // reconcile wait for the paint (task 23)
    afterPaint(() => {
      if (!current || current.id !== ch.id || e !== active()) return;
      markRead(ch);
      loadThreads();
      refreshHeaderMembers(ch);
      if (cached || needsReconcile) reconcilePage(e, ch);
    });
  };

  const renderMessages = (list, ch) => {
    const box = $('messages');
    box.innerHTML = '';
    // "new messages" divider goes where unread starts; join time is the
    // baseline for channels never marked read (matches the server's count)
    const cutoff = ch.unread_count > 0 ? (ch.last_read_at || me.created_at) : null;
    let divided = false;
    list.forEach((m) => {
      if (cutoff && !divided && m.author_id !== me.id && m.kind !== 'system' && m.created_at > cutoff) {
        const d = document.createElement('div');
        d.className = 'unread-divider';
        d.innerHTML = '<span>new messages</span>';
        box.appendChild(d);
        divided = true;
      }
      box.appendChild(msgEl(m, false));
    });
    syncDateDividers(box);
    box.scrollTop = box.scrollHeight;
    // Font/image layout may settle after this synchronous paint. A channel
    // open is an explicit jump to latest, so finish that jump after layout too.
    requestAnimationFrame(() => {
      if (current && current.id === ch.id) box.scrollTop = box.scrollHeight;
    });
  };

  // a cached page can have missed an event (a gap while it warmed): fetch the
  // live page after the paint and repaint only when the two differ. The page's
  // revision makes live events authoritative: an older snapshot may never
  // replace a page that changed while its request was in flight.
  const reconcilePage = async (e, ch) => {
    const startedPage = pageFor(e, ch.id);
    if (!startedPage) return;
    const startedRevision = startedPage.revision;
    try {
      const out = await api(`/api/v1/channels/${ch.id}/messages?limit=100`, { ws: e.slug });
      const live = out.messages || [];
      const page = pageFor(e, ch.id);
      if (page !== startedPage || page.revision !== startedRevision) {
        // A newer response or a live create/edit/delete/reaction/ack won this
        // race. Reconcile once more after paint so a pre-existing cache gap is
        // still repaired, but keep the newer page on screen in the meantime.
        if (e === active() && current && current.id === ch.id) {
          afterPaint(() => reconcilePage(e, channels.find((x) => x.id === ch.id) || ch));
        }
        return;
      }
      // reply_count and last_reply_at are part of what a root paints: comparing
      // ids and bodies alone calls a stale footer "same" and skips the repaint
      const same = page && page.list.length === live.length
        && page.list.every((m, i) => m.id === live[i].id && m.body === live[i].body
          && (m.reply_count || 0) === (live[i].reply_count || 0)
          && (m.last_reply_at || '') === (live[i].last_reply_at || ''));
      const fresh = setPage(e, ch.id, live);
      if (same || e !== active() || !current || current.id !== ch.id) return;
      renderMessages(fresh.list, ch);
    } catch (err) { console.error('reconcilePage', err); }
  };

  // Pull one page of history above what is rendered, keeping the reader's
  // anchor still. Returns false at the top of the channel.
  const prependOlderPage = async () => {
    const box = $('messages');
    const anchor = box.querySelector('.msg');
    if (!anchor || !current) return false;
    const out = await api(`/api/v1/channels/${current.id}/messages?limit=100&before_id=${anchor.dataset.id}`);
    const older = out.messages || [];
    if (!older.length) return false;
    const wasAt = anchor.getBoundingClientRect().top;
    const frag = document.createDocumentFragment();
    older.forEach((m) => frag.appendChild(msgEl(m, false)));
    box.insertBefore(frag, box.firstChild);
    syncDateDividers(box); // the old first message may now sit mid-day
    box.scrollTop += anchor.getBoundingClientRect().top - wasAt;
    return true;
  };

  // Scroll a message into view and flash it. A permalink to an old message can
  // point above the loaded window, so page back until it shows up. Threads load
  // whole, so only the channel feed paginates.
  const REVEAL_MAX_PAGES = 20;
  const revealMessage = async (id, inThread) => {
    const container = inThread ? $('thread-messages') : $('messages');
    const find = () => [...container.querySelectorAll('.msg')].find((n) => n.dataset.id === id);
    let node = find();
    for (let i = 0; !node && !inThread && i < REVEAL_MAX_PAGES; i++) {
      if (!(await prependOlderPage())) break;
      node = find();
    }
    if (!node) return false;
    node.scrollIntoView({ block: 'center' });
    node.classList.add('msg-flash');
    setTimeout(() => node.classList.remove('msg-flash'), 1800);
    return true;
  };

  let threads = [];

  // Room-wide: the whole thread tree, tagged with channel_id, so leaves can
  // nest under their parent channel in the sidebar. Several channel opens and
  // reply events can overlap these snapshots; only the newest request may land.
  const loadThreads = async (e = active()) => {
    const seq = ++e.threadLoadSeq;
    try {
      const out = await api('/api/v1/threads', { ws: e.slug });
      if (seq !== e.threadLoadSeq) return;
      e.threads = out.threads || [];
      if (e !== active()) return;
      threads = e.threads;
      // a reply that landed in the open thread is being read right now: the
      // fresh count says 1, but its bar must not glow until the panel closes
      await markThreadRead(openThreadRoot);
      renderChannels();
      syncReplyBars();
    } catch (e) { console.error('loadThreads', e); }
  };

  const markThreadRead = async (rootID) => {
    const t = threads.find((x) => x.root_id === rootID);
    if (!t || t.unread_count === 0) return;
    // optimistic: the thread is on screen, so clear the glow before the round trip
    t.unread_count = 0;
    t.unread_mentions = 0;
    renderChannels();
    syncReplyBars();
    try {
      await api(`/api/v1/threads/${rootID}/read`, { method: 'POST', body: {} });
    } catch (e) { console.error('markThreadRead', e); }
  };

  const markRead = async (ch) => {
    try {
      const out = await api(`/api/v1/channels/${ch.id}/read`, { method: 'POST', body: {} });
      ch.unread_count = 0;
      ch.unread_mentions = 0;
      ch.last_read_at = out.last_read_at;
      renderChannels();
    } catch (e) { console.error('markRead', e); }
  };

  let openThreadSeq = 0;
  const openThread = async (rootID, fromURL = false) => {
    if (!fromURL) navSeq++;
    const seq = ++openThreadSeq;
    const out = await api('/api/v1/threads/' + rootID);
    // the main column must show the thread's channel, whatever opened it
    // (sidebar tree, mention, search, deep link)
    const ch = channels.find((c) => c.id === out.messages[0]?.channel_id);
    if (ch && (!current || current.id !== ch.id)) await selectChannel(ch, true);
    if (seq !== openThreadSeq) return; // a newer open won
    const changed = openThreadRoot !== rootID;
    if (changed) clearThreadAttachment();
    const root = out.messages.find((m) => !m.thread_root_id) || out.messages[0];
    const replies = out.messages.filter((m) => !!m.thread_root_id);
    openThreadSummary = threads.find((t) => t.root_id === rootID) || (root ? {
      root_id: root.id,
      channel_id: root.channel_id,
      body: root.body,
      author_id: root.author_id,
      author_name: root.author_name,
      created_at: root.created_at,
      reply_count: replies.length,
      last_reply_at: replies.at(-1)?.created_at || null,
      last_activity_at: replies.at(-1)?.created_at || root.created_at,
      muted: false,
      unread_count: 0,
      unread_mentions: 0,
      subscribed: false,
    } : null);
    openThreadRoot = rootID;
    if (changed) renderChannels(); // move the active highlight to this thread leaf
    $('thread-panel').classList.remove('hidden');
    syncURL(changed && !fromURL);
    const box = $('thread-messages');
    box.innerHTML = '';
    out.messages.forEach((m) => box.appendChild(msgEl(m, true)));
    syncDateDividers(box);
    box.scrollTop = box.scrollHeight;
    markThreadRead(rootID);
  };

  const downloadAttachment = async (id, name) => {
    try {
      const resp = await fetch('/api/v1/attachments/' + id, { headers: authHeaders() });
      if (!resp.ok) throw new Error('download failed (HTTP ' + resp.status + ')');
      const url = URL.createObjectURL(await resp.blob());
      const a = document.createElement('a');
      a.href = url;
      a.download = name || 'attachment';
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 10000);
    } catch (e) { alert(e.message); }
  };

  // Uploads arrive as application/octet-stream, so the filename decides what
  // can be read in place: markdown renders like a message, other text as-is.
  const TEXT_EXT = /\.(md|markdown|txt|log|csv|json|ya?ml|toml|sh|diff|patch|go|js|ts|py|rb|rs|sql|html|css|xml)$/i;
  const isTextAttachment = (name) => TEXT_EXT.test(name || '');
  const openDoc = async (id, name) => {
    const box = $('doc-body');
    box.innerHTML = '';
    $('doc-name').textContent = name || '';
    $('doc-dl').onclick = () => downloadAttachment(id, name);
    $('doc-modal').classList.remove('hidden');
    try {
      const resp = await fetch('/api/v1/attachments/' + id, { headers: authHeaders() });
      if (!resp.ok) throw new Error('preview failed (HTTP ' + resp.status + ')');
      const text = await resp.text();
      if (/\.(md|markdown)$/i.test(name || '')) {
        box.innerHTML = renderMarkdown(text);
        box.querySelectorAll('pre code').forEach((c) => {
          try { hljs.highlightElement(c); } catch (e) { /* unknown language tag */ }
        });
        return;
      }
      const pre = document.createElement('pre');
      pre.className = 'doc-plain';
      pre.textContent = text;
      box.appendChild(pre);
    } catch (e) { box.textContent = e.message; }
  };
  const closeDoc = () => { $('doc-modal').classList.add('hidden'); $('doc-body').innerHTML = ''; };
  $('doc-close').onclick = closeDoc;
  $('doc-modal').onclick = (ev) => { if (ev.target === $('doc-modal')) closeDoc(); };

  const openLightbox = (id, name) => {
    blobURL(id).then((url) => { if (url) $('lightbox-img').src = url; });
    $('lightbox-name').textContent = name || '';
    $('lightbox-dl').onclick = (ev) => { ev.stopPropagation(); downloadAttachment(id, name); };
    $('lightbox').classList.remove('hidden');
  };

  const editMessage = async (m) => {
    const next = prompt('Edit message:', m.body);
    if (next === null || next.trim() === '' || next === m.body) return;
    try { await api('/api/v1/messages/' + m.id, { method: 'PATCH', body: { body: next } }); }
    catch (e) { alert(e.message); }
  };

  const deleteMessage = async (m) => {
    if (!confirm('Delete this message' + (m.reply_count > 0 ? ' and its thread' : '') + '?')) return;
    try { await api('/api/v1/messages/' + m.id, { method: 'DELETE' }); }
    catch (e) { alert(e.message); }
  };

  // optimistic sends: the placeholder shows instantly, its server echo settles it
  const pendingSends = [];

  const optimisticEl = (body, rootID, att) => {
    const m = {
      id: 'tmp-' + Date.now() + '-' + pendingSends.length,
      author_id: me.id, author_name: me.name,
      body, created_at: new Date().toISOString(),
      thread_root_id: rootID || null,
      reply_count: 0, reactions: [], acked_by: [],
      attachments: att ? [att] : [],
    };
    const el = msgEl(m, !!rootID);
    el.classList.add('pending');
    return el;
  };

  // drop the optimistic placeholder for one of my sends once its real copy lands
  const settleMine = (m, eventSlug = slug) => {
    const i = pendingSends.findIndex((p) => p.workspace === eventSlug && p.channelID === m.channel_id
      && p.authorID === m.author_id && p.rootID === (m.thread_root_id || null) && p.body === m.body);
    if (i < 0) return;
    pendingSends[i].node.remove();
    pendingSends.splice(i, 1);
  };

  // The event feed is normally first, but the successful POST is also an
  // authoritative message. If the feed is reconnecting, settle from this
  // response instead of retaining a pending row and record indefinitely.
  const settleSendResponse = (m, rec) => {
    const i = pendingSends.indexOf(rec);
    if (i < 0) return; // its live event already won
    pendingSends.splice(i, 1);
    const e = store.get(rec.workspace);
    if (e) pageApply(e, { type: 'message.created', payload: m });
    const box = rec.node.parentElement;
    if (box) {
      rec.node.replaceWith(msgEl(m, !!rec.rootID));
      syncDateDividers(box);
    }
    if (rec.rootID && e) {
      loadThreads(e);
      if (e === active() && current && current.id === rec.channelID) refreshRootBar(rec.rootID);
    }
  };

  const post = async (body, threadRootID) => {
    const rootID = threadRootID || null;
    const which = threadRootID ? 'thread' : 'main';
    const workspace = slug;
    const channelID = current.id;
    const authorID = me.id;
    const att = pendingAtt[which];
    // a human typing "@foo" usually means literal text, not a dead mention, so
    // the strict 422 stays for API clients and the UI just posts
    const payload = { body, allow_unknown_mentions: true };
    if (threadRootID) payload.thread_root_id = threadRootID;
    if (att) payload.attachment_ids = [att.id];

    // show the message immediately, before the server round-trip
    const node = optimisticEl(body, rootID, att);
    const rec = { body, rootID, node, workspace, channelID, authorID };
    pendingSends.push(rec);
    if (!rootID && current) {
      const box = $('messages'); box.appendChild(node); syncDateDividers(box); box.scrollTop = box.scrollHeight;
    } else if (rootID && rootID === openThreadRoot) {
      const box = $('thread-messages'); box.appendChild(node); syncDateDividers(box); box.scrollTop = box.scrollHeight;
    }
    if (att) clearPendingAttachment(which);

    try {
      const sent = await api(`/api/v1/channels/${channelID}/messages`, { method: 'POST', body: payload, ws: workspace });
      settleSendResponse(sent, rec);
      // mentioning somebody outside the channel silently reaches nobody
      if (sent && sent.warnings && sent.warnings.length) notice(sent.warnings[0], true);
    } catch (e) {
      const i = pendingSends.indexOf(rec);
      if (i >= 0) pendingSends.splice(i, 1);
      const box = node.parentElement;
      node.remove(); // roll back the placeholder; caller restores the draft
      if (box) syncDateDividers(box); // drop the day marker the placeholder opened
      throw e;
    }
  };

  // ---------- live updates ----------

  const msgNode = (id) =>
    [...document.querySelectorAll('#messages .msg')].find((n) => n.dataset.id === id);

  // replace one rendered channel-message node in place — avoids the full
  // selectChannel refetch that wipes #messages and yanks the reader to the bottom
  const replaceMsgNode = (m) => {
    const old = msgNode(m.id);
    if (old && current && m.channel_id === current.id) old.replaceWith(msgEl(m, false));
  };

  // a thread reply changed a root's reply bar; refetch just that one message
  const refreshRootBar = async (rootID) => {
    try {
      const m = await api('/api/v1/messages/' + rootID);
      replaceMsgNode(m);
    } catch (e) { /* root deleted in the meantime */ }
  };

  // ---------- notifications ----------
  // What notifies follows the agents' relevance rules: a top-level message in a
  // channel you are in, a reply in a thread you are part of, and always a
  // mention or broadcast, even in a muted channel. Nothing for your own
  // messages. A reply in one of your threads is deliberately exempt from the
  // focused-channel guard: seeing the parent channel is not seeing its thread.
  const involvedThread = (m) => {
    if (!m.thread_root_id) return null;
    const known = threads.find((t) => t.root_id === m.thread_root_id);
    if (known) return known;
    // The event carries the server's live participant set. This closes the
    // small gap before a newly-mentioned thread has reached the sidebar list.
    return (m.thread_participants || []).includes(me.name) ? { muted: false } : null;
  };
  const notifyReason = (m) => {
    if (!notifyPrefs.enabled || !me) return null;
    if (m.author_id === me.id || m.kind === 'system') return null;
    if (roomMuted()) return null; // the whole-workspace mute (task 18)
    const ch = channels.find((c) => c.id === m.channel_id);
    if (!ch) return null;
    const th = involvedThread(m);
    if (!th && !document.hidden && document.hasFocus() && current && current.id === ch.id) return null;
    if ((m.mentions || []).includes(me.name)) return 'mention';
    if (m.is_broadcast) return 'broadcast';
    if (ch.muted) return null;
    if (!m.thread_root_id) return 'channel';
    if (!th || th.muted) return null;
    return 'thread';
  };

  // Top-level channel traffic keeps its quiet window. Replies in a thread the
  // viewer is part of sound individually: every reply is actionable even when
  // several arrive together.
  const NOTIFY_WINDOW_MS = 3000;
  const notifyTimers = new Map();
  let audioCtx = null;
  const AudioContextClass = window.AudioContext || window.webkitAudioContext;
  // Browsers only let audio start after a gesture. Create and resume the
  // context inside that gesture, rather than waiting for the first live event.
  const primeAudio = () => {
    if (!AudioContextClass) return;
    try {
      if (!audioCtx) audioCtx = new AudioContextClass();
      if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
    } catch { /* no audio here */ }
  };
  document.addEventListener('pointerdown', primeAudio);
  document.addEventListener('keydown', primeAudio);
  const playPing = async () => {
    if (!audioCtx) return false;
    try {
      if (audioCtx.state === 'suspended') await audioCtx.resume();
      if (audioCtx.state !== 'running') return false;
      const t = audioCtx.currentTime;
      const osc = audioCtx.createOscillator();
      const gain = audioCtx.createGain();
      osc.type = 'sine';
      osc.frequency.setValueAtTime(880, t);
      osc.frequency.setValueAtTime(1175, t + 0.09);
      gain.gain.setValueAtTime(0.0001, t);
      gain.gain.exponentialRampToValueAtTime(0.18, t + 0.01);
      gain.gain.exponentialRampToValueAtTime(0.0001, t + 0.28);
      osc.connect(gain).connect(audioCtx.destination);
      osc.start(t);
      osc.stop(t + 0.3);
      return true;
    } catch {
      return false;
    }
  };

  // a reply in a thread the sidebar does not know yet may still be one you are
  // in (joined from another device, mentioned a moment ago): look once
  const threadChecked = new Map();
  const threadLoads = new Map();
  const ensureThreadKnown = async (rootID) => {
    if (threads.some((t) => t.root_id === rootID)) return;
    const pending = threadLoads.get(rootID);
    if (pending) {
      await pending;
      return;
    }
    const last = threadChecked.get(rootID) || 0;
    if (Date.now() - last < 10000) return;
    threadChecked.set(rootID, Date.now());
    const load = loadThreads().finally(() => threadLoads.delete(rootID));
    threadLoads.set(rootID, load);
    await load;
  };

  const maybeNotify = async (m) => {
    if (m.thread_root_id && notifyPrefs.enabled) await ensureThreadKnown(m.thread_root_id);
    const why = notifyReason(m);
    if (!why) return;
    const key = m.thread_root_id || m.channel_id;
    const everyReply = !!involvedThread(m);
    if (!everyReply) {
      const inWindow = notifyTimers.has(key);
      if (inWindow) clearTimeout(notifyTimers.get(key));
      notifyTimers.set(key, setTimeout(() => notifyTimers.delete(key), NOTIFY_WINDOW_MS));
      if (inWindow) return;
    }
    const ch = channels.find((c) => c.id === m.channel_id);
    const sound = !!notifyPrefs.sound;
    const soundPlayed = sound ? await playPing() : false;
    if (window.Notification && Notification.permission === 'granted' && (document.hidden || !document.hasFocus())) {
      try {
        const n = new Notification(`${m.author_name || 'Someone'} in #${ch ? ch.name : 'channel'}`, {
          body: (m.body || '').slice(0, 140), tag: key,
        });
        n.onclick = () => {
          window.focus();
          if (ch) selectChannel(ch).then(() => { if (m.thread_root_id) openThread(m.thread_root_id); });
          n.close();
        };
      } catch { /* the in-tab badge and sound already happened */ }
    }
    // e2e hook: the observable side of a notification
    document.dispatchEvent(new CustomEvent('agentchat:notify', {
      detail: { key, why, sound, soundPlayed, audioState: audioCtx ? audioCtx.state : '', channel: ch ? ch.name : '' },
    }));
  };

  // the notification, archive and theme controls live on /settings (auth.js);
  // notifyPrefs is loaded once at boot and read by the feed
  const applyPresenceEvent = (e, payload, paint) => {
    const update = (p) => {
      if (!p || p.id !== payload.participant_id) return false;
      p.online = !!payload.online;
      if (p.online) p.last_seen_at = new Date().toISOString();
      return true;
    };
    e.participants.forEach(update);
    for (const list of e.members.values()) list.forEach(update);
    const mine = update(e.me);
    if (!paint || e !== active()) return;
    if (mine) renderMeFooter();
    renderParticipants();
    if (current && e.members.has(current.id)) {
      paintHeaderMembers(current);
      if (!$('members-modal').classList.contains('hidden')) renderMembersModal();
    }
    if (profileFor && profileFor.id === payload.participant_id) {
      profileFor.online = !!payload.online;
      $('profile-meta').textContent =
        `${profileFor.role}${profileFor.is_human ? ' · human' : ' · agent'} · ${profileFor.online ? 'online' : 'offline'}`;
      const capTitle = $('profile-caps').querySelector('h4');
      if (capTitle) capTitle.textContent = 'Capabilities' + (profileFor.online ? '' : ' · not callable: offline');
    }
  };
  const applyEvent = async (ev) => {
    const t = ev.type;
    if (t === 'message.created') settleMine(ev.payload, slug);
    // the feed cursors predate the boot loads, so a message the page already
    // holds can come round again: drop it, the counts already include it
    if (!pageApply(active(), ev)) return;
    if (t === 'message.created') {
      const m = ev.payload;
      maybeNotify(m);
      if (current && m.channel_id === current.id && !m.thread_root_id) {
        const box = $('messages');
        const nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 120;
        box.appendChild(msgEl(m, false));
        syncDateDividers(box); // a first message after midnight opens a new day
        if (nearBottom) box.scrollTop = box.scrollHeight;
        // a hidden tab is not "viewing": marking read here would silently erase
        // the unread count and the new-messages divider for messages never seen
        if (m.author_id !== me.id && !document.hidden && m.kind !== 'system') markRead(current);
      }
      if (!m.thread_root_id && m.author_id !== me.id && m.kind !== 'system' && (!current || m.channel_id !== current.id || document.hidden)) {
        const ch = channels.find((c) => c.id === m.channel_id);
        if (ch) {
          ch.unread_count = (ch.unread_count || 0) + 1;
          if ((m.mentions || []).includes(me.name) || m.is_broadcast) {
            ch.unread_mentions = (ch.unread_mentions || 0) + 1;
          }
          renderChannels();
        }
      }
      if (m.thread_root_id && m.thread_root_id === openThreadRoot) {
        openThread(openThreadRoot);
      }
      if (m.thread_root_id && current && m.channel_id === current.id) {
        await refreshRootBar(m.thread_root_id); // just the root's reply bar, not the whole channel
      }
      // any reply, in any channel, can bring a hidden or quiet thread back
      if (m.thread_root_id) loadThreads(); // tree + reply-bar unread glow
      return;
    }
    if (t === 'message.edited') {
      const m = ev.payload;
      replaceMsgNode(m);
      if (openThreadRoot) {
        try { await openThread(openThreadRoot); }
        catch (e) { closeThread(); }
      }
      return;
    }
    if (t === 'message.deleted') {
      const id = ev.payload.message_id;
      msgNode(id)?.remove();
      syncDateDividers($('messages'));
      // a deleted reply moves its root's footer down, same as a new one moves it up
      if (ev.payload.thread_root_id) await refreshRootBar(ev.payload.thread_root_id);
      if (openThreadRoot === id) closeThread();
      else if (openThreadRoot) {
        try { await openThread(openThreadRoot); } // a reply was deleted
        catch (e) { closeThread(); }
      }
      loadThreads();
      return;
    }
    if (t === 'message.reaction') {
      reactionMap[ev.payload.message_id] = ev.payload.reactions || [];
      renderReactions(ev.payload.message_id);
      return;
    }
    if (t === 'message.ack') {
      paintAck(ev.payload.message_id, ev.payload.acked_by);
      return;
    }
    // a call and its result change no room structure; a refresh per call would
    // hammer every open tab while an MCP client drives the agents
    if (t === 'capability.call' || t === 'capability.result') return;
    // a fired reminder only moves an open profile's next/last fire columns
    if (t === 'reminder.fired') {
      if (profileRemindersFor && ev.payload.participant_id === profileRemindersFor.id) showReminders(profileRemindersFor);
      return;
    }
    if (t === 'capability.registered') {
      // only an open profile of that agent cares; the sidebar does not show capabilities
      if (profileCapsFor && ev.payload.participant_id === profileCapsFor.id) showCapabilities(profileCapsFor);
      return;
    }
    // The event is the complete presence delta. Re-fetching the room, channel
    // sections, Browse and channel roster for one dot caused four requests per
    // transition and amplified agent sleep/wake bursts across every open tab.
    if (t === 'participant.presence_changed') {
      applyPresenceEvent(active(), ev.payload, true);
      return;
    }
    // everything else changes room structure or people — refresh the sidebar
    await refreshRoom();
    // an open Browse list is a snapshot: a channel created or joined elsewhere
    // must show up (or drop out) without closing and reopening it
    if (t.startsWith('channel.') && !$('browse-modal').classList.contains('hidden')) {
      await openBrowse();
    }
    if ((t === 'channel.member_joined' || t === 'channel.member_left') && current) {
      refreshHeaderMembers(current); // keeps the header count, dots, and open modal live
    }
    // my own removal: the channel is gone from my sidebar; leave it if I'm inside
    if (t === 'channel.member_left' && ev.payload.participant_id === me.id
        && current && ev.payload.channel_id === current.id) {
      current = null;
      await selectChannel(channels.find((c) => c.name === 'general') || channels[0]);
      return;
    }
    if (t === 'room.renamed' && room && ev.payload.room_id === room.id) {
      room.name = ev.payload.name;
      $('room-name').textContent = room.name;
      $('ws-current').textContent = room.name;
      setTitle();
      refreshRail();
    }
    if ((t === 'channel.privacy_changed' || t === 'channel.renamed') && current && ev.payload.channel_id === current.id) {
      const ch = channels.find((c) => c.id === current.id);
      if (ch) {
        current = ch;
        setChannelTitle(ch);
        syncURL(false); // the path carries the channel name
      }
    }
    // profile changes (avatar, name) must also repaint already-rendered messages
    if (t === 'participant.updated') {
      if (current) await selectChannel(current);
      if (openThreadRoot) await openThread(openThreadRoot);
    }
    if (t === 'channel.deleted' && current && !channels.some((c) => c.id === current.id)) {
      current = null;
      await selectChannel(channels.find((c) => c.name === 'general') || channels[0]);
    }
  };

  // an event for a workspace that is not on screen keeps its entry current,
  // so a switch back reads the store; its rail badge follows (task 23)
  const applyStoreEvent = (wsSlug, ev) => {
    const e = store.get(wsSlug);
    if (!e) return;
    if (!e.warm) { // the warm replays these into its pages, then refetches the counts
      e.pending.push(ev);
      if (e.pending.length > 200) e.pending.shift();
      return;
    }
    const t = ev.type;
    // The echo owns its optimistic row even if channel/workspace navigation
    // detached that row, or a warmed page already contains the message.
    if (t === 'message.created') settleMine(ev.payload, wsSlug);
    if (t === 'message.reaction') {
      pageApply(e, ev);
      reactionMap[ev.payload.message_id] = ev.payload.reactions || [];
      return;
    }
    if (t === 'message.ack') {
      pageApply(e, ev);
      paintAck(ev.payload.message_id, ev.payload.acked_by);
      return;
    }
    if (t.startsWith('message.')) {
      if (!pageApply(e, ev)) return;
      const m = ev.payload;
      if (t === 'message.created' && !m.thread_root_id && m.author_id !== e.me.id && m.kind !== 'system') {
        const ch = e.channels.find((c) => c.id === m.channel_id);
        if (!ch) return;
        ch.unread_count = (ch.unread_count || 0) + 1;
        if ((m.mentions || []).includes(e.me.name) || m.is_broadcast) ch.unread_mentions = (ch.unread_mentions || 0) + 1;
        paintRailBadges();
      }
      return;
    }
    if (t === 'capability.call' || t === 'capability.result' || t === 'capability.registered' || t === 'reminder.fired') return;
    if (t === 'participant.presence_changed') {
      applyPresenceEvent(e, ev.payload, false);
      return;
    }
    if (t === 'room.renamed') refreshRail(); // the rail tip carries the name
    scheduleRoomRefresh(e); // people or structure changed: one refetch per burst
  };
  const scheduleRoomRefresh = (e) => {
    clearTimeout(e.refreshTimer);
    e.refreshTimer = setTimeout(async () => {
      try {
        if (!await loadRoomInto(e)) return;
        if (e === active()) { adopt(e); paintRoom(); }
        paintRailBadges();
      } catch (err) {
        // removed from it, or it is gone: forget the entry, the rail refetch drops it
        if (err.status === 403 || err.status === 404) { store.delete(e.slug); refreshRail(); }
      }
    }, 800);
  };

  // one long-poll for every member workspace (task 23): the active one goes
  // through applyEvent, the rest into the store. The cursor set is the
  // membership; a change means a workspace was joined or left elsewhere.
  let feedCursors = {};
  const feedLoop = async () => {
    try {
      const qs = Object.entries(feedCursors).map(([s, c]) => encodeURIComponent(s) + ':' + c).join(',');
      const out = await api('/api/v1/user/events?wait=25&cursors=' + qs);
      const before = Object.keys(feedCursors).sort().join(',');
      feedCursors = out.cursors || {};
      const after = Object.keys(feedCursors).sort().join(',');
      if (before && before !== after) {
        for (const s of before.split(',')) if (!(s in feedCursors) && s !== slug) store.delete(s);
        if (slug in feedCursors) refreshRail();
        else refreshRoom().catch(() => {}); // 403 routes to the removed view
      }
      for (const ev of out.events || []) {
        // the cursors are already advanced: one bad event must not eat the rest of the batch
        try {
          if (ev.workspace === slug) await applyEvent(ev);
          else applyStoreEvent(ev.workspace, ev);
        } catch (e) { console.error('applyEvent', ev.type, e); }
      }
    } catch (e) {
      if (routeAuthError(e)) return;
      await new Promise((r) => setTimeout(r, 3000));
    }
    feedLoop();
  };

  // ---------- join / boot ----------

  // the workspace entry for a signed-in non-member: the account supplies the
  // name, only the invite link is asked for
  const showEnter = async () => {
    document.title = 'OpenChatter';
    $('chat-view').classList.add('hidden');
    $('enter-view').classList.remove('hidden');
    try {
      const peek = await api('/api/v1/rooms/peek?slug=' + encodeURIComponent(slug));
      $('enter-room-name').textContent = '“' + peek.name + '”';
      $('enter-room-icon').replaceChildren(wsAvatarEl(peek, 'ws-avatar-md'));
    } catch (e) {
      $('enter-error').textContent = e.status === 404 ? 'This link does not point to a workspace.' : e.message;
      $('enter-error').classList.remove('hidden');
      $('enter-form').querySelector('button[type=submit]').disabled = true;
    }
  };

  const showRemoved = () => {
    $('chat-view').classList.add('hidden');
    $('removed-view').classList.remove('hidden');
  };

  $('enter-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    $('enter-error').classList.add('hidden');
    const btn = $('enter-form').querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      await api('/api/v1/workspaces/' + encodeURIComponent(slug) + '/enter', {
        method: 'POST', body: { invite: $('enter-code').value.trim() },
      });
      location.reload();
    } catch (e) {
      btn.disabled = false;
      $('enter-error').textContent = inviteErrorText(e) || e.message;
      $('enter-error').classList.remove('hidden');
    }
  });

  // the splash stays over the chat view until the session, the workspace list and
  // the current room (channels, messages) are all in: the chat view has to be
  // displayed for the message pane to measure and scroll, but nothing that is not
  // the final layout may ever reach the screen (Maya, msg 62883447)
  const enterChat = async () => {
    document.body.classList.add('booting');
    const e = entryFor(slug);
    const [meOut, prefs, wsOut, cold] = await Promise.all([
      api('/api/v1/me'),
      api('/api/v1/me/notifications').catch(() => null),
      // a dead session or a lost membership fails /me too, and that one routes
      fetchWorkspaces().catch((e) => { if (!e.status || e.status >= 500) console.error('switcher', e); return null; }),
      // the feed cursors come before the loads, so nothing in between is lost
      api('/api/v1/user/events').catch(() => null),
    ]);
    e.me = meOut;
    me = meOut;
    if (prefs) notifyPrefs = prefs;
    if (cold) feedCursors = cold.cursors || {};
    $('enter-view').classList.add('hidden');
    $('chat-view').classList.remove('hidden');
    await Promise.all([loadRoomInto(e), loadThreads(e)]);
    adopt(e);
    e.warm = true;
    paintRoom();
    mountSwitcher(wsOut);
    await applyURL();
    syncURL(false); // normalize the address bar without a history entry
    document.body.classList.remove('booting');
    // the open workspace's counts and page include everything up to now: its
    // cursor starts here, or the feed would count those messages twice
    const late = await api('/api/v1/user/events').catch(() => null);
    if (late && late.cursors && late.cursors[slug] != null) feedCursors[slug] = late.cursors[slug];
    feedLoop();
    afterPaint(warmAll);
    // a warm that failed once is retried: on focus, and on a slow tick
    setInterval(warmAll, 60 * 1000);
  };

  // ---------- switching (task 23) ----------

  const pickChannel = (e, chName) => e.channels.find((c) => c.name === chName)
    || (e.lastChannelID && e.channels.find((c) => c.id === e.lastChannelID))
    || e.channels.find((c) => c.name === 'general') || e.channels[0];
  const warmChannel = async (e, ch) => {
    const [page, members] = await Promise.all([
      pageFor(e, ch.id) ? null : api(`/api/v1/channels/${ch.id}/messages?limit=100`, { ws: e.slug }),
      e.members.has(ch.id) ? null : api('/api/v1/channels/' + ch.id + '/members', { ws: e.slug }).catch(() => null),
    ]);
    if (page) setPage(e, ch.id, page.messages || []);
    if (members) e.members.set(ch.id, members.members || []);
  };
  // fill an entry off screen: me, the workspace, its sections, browse list and
  // threads, then the page and members of the channel a switch would open
  const warm = (e, chName) => {
    if (e.warming) return e.warming;
    e.warming = (async () => {
      try {
        const [meOut] = await Promise.all([api('/api/v1/me', { ws: e.slug }), loadRoomInto(e), loadThreads(e)]);
        e.me = meOut;
        const ch = pickChannel(e, chName);
        if (ch) await warmChannel(e, ch);
        e.warm = true;
        const buf = e.pending;
        e.pending = [];
        if (buf.length) {
          for (const ev of buf) if (ev.type.startsWith('message.')) pageApply(e, ev);
          scheduleRoomRefresh(e);
        }
      } finally { e.warming = null; }
    })();
    return e.warming;
  };
  const ensureReady = async (e, chName) => {
    if (!e.warm) await warm(e, chName); // a shared warm may have picked another channel
    const ch = pickChannel(e, chName);
    if (ch && !pageFor(e, ch.id)) await warmChannel(e, ch);
  };
  // after the first paint: every other member workspace, one at a time, then
  // every channel page with two fetches in flight; a switch to any of them
  // then paints with no request before it
  let warmingAll = null;
  const warmAll = () => {
    if (warmingAll) return warmingAll;
    warmingAll = (async () => {
      for (const ws of railRooms.slice()) {
        const e = entryFor(ws.slug);
        if (e.warm || ws.slug === slug) continue;
        try { await warm(e); } catch (err) { console.error('warm', ws.slug, err); }
      }
      const todo = [];
      for (const e of store.values()) {
        if (!e.warm) continue;
        for (const ch of e.channels) if (!ch.archived && !pageFor(e, ch.id)) todo.push([e, ch]);
      }
      const worker = async () => {
        for (let job = todo.shift(); job; job = todo.shift()) {
          try { await warmChannel(job[0], job[1]); } catch (err) { /* the switch fetches it */ }
        }
      };
      await Promise.all([worker(), worker()]);
    })().finally(() => { warmingAll = null; });
    return warmingAll;
  };

  const setProgress = (on) => $('switch-progress').classList.toggle('hidden', !on);
  // the whole pane from the target entry in one synchronous pass: nothing of
  // the old workspace survives it and nothing of the new one precedes it
  const swapTo = (e, chName, push) => {
    closeThread(false);
    clearMainAttachment();
    closeMembers(); closeBrowse(); closeSearch(); closeInviteModal(); closeAddAgent();
    slug = e.slug;
    roomPrefix = '/w/';
    adopt(e);
    current = null;
    talkedAt = new Map();
    channelMembers = [];
    paintRoom();
    mountMenu();
    paintRailCurrent();
    paintRailBadges();
    const ch = pickChannel(e, chName);
    if (ch) { selectChannel(ch, !push); return; } // cached: paints before any await
    $('messages').innerHTML = '';
    setChannelTitle({ name: '' });
    if (push) syncURL(true);
  };
  let switchSeq = 0;
  // a rail click: the old workspace stays on screen with a thin bar on top
  // while a cold target warms, then the swap is one paint
  const switchTo = async (target, { push = true, chName = '' } = {}) => {
    if (target === slug) return;
    const seq = ++switchSeq;
    const e = entryFor(target);
    try {
      const ch = e.warm && pickChannel(e, chName);
      const ready = e.warm && (!ch || pageFor(e, ch.id));
      if (!ready) {
        setProgress(true);
        await ensureReady(e, chName);
      }
    } catch (err) {
      setProgress(false);
      // not a member any more, or gone: the full page has the enter and removed views
      if (err.status === 403 || err.status === 404) { location.href = '/w/' + encodeURIComponent(target); return; }
      notice('Could not open the workspace', true);
      console.error('switch', err);
      return;
    }
    if (seq !== switchSeq) return; // a later click won
    setProgress(false);
    swapTo(e, chName, push);
  };

  // ---------- workspace switcher (session users only) ----------

  // /settings binds its Workspace tab to the room in ?next=: always the page we
  // are on right now, never a path captured at mount time
  const settingsHref = (tab) => '/settings?' + (tab ? 'tab=' + tab + '&' : '') + 'next=' + encodeURIComponent(location.pathname + location.search);
  const wsMenuItem = (label, opts = {}) => {
    const el = document.createElement(opts.href ? 'a' : 'button');
    el.className = 'ws-item' + (opts.current ? ' current' : '');
    el.setAttribute('role', 'menuitem');
    if (opts.avatar) el.appendChild(opts.avatar);
    const text = document.createElement('span');
    text.className = 'ws-label';
    text.textContent = label;
    el.appendChild(text);
    // a function href resolves on click: the menu mounts before a warm switch
    // pushes its URL, so a baked-in ?next= would name the previous workspace
    if (typeof opts.href === 'function') { el.href = opts.href(); el.dataset.liveHref = '1'; el._resolveHref = () => { el.href = opts.href(); }; }
    if (opts.href && typeof opts.href !== 'function') el.href = opts.href;
    if (!opts.href) el.type = 'button';
    if (opts.onclick) el.onclick = opts.onclick;
    if (opts.id) el.id = opts.id;
    if (opts.icon) {
      const i = document.createElement('span');
      i.className = 'mi-icon';
      i.setAttribute('aria-hidden', 'true');
      i.innerHTML = opts.icon;
      el.prepend(i);
    }
    if (opts.current) el.setAttribute('aria-current', 'true');
    return el;
  };

  // live hrefs (Settings) resolve when the menu opens, so middle-click and
  // "copy link address" see the workspace on screen, not the one at mount
  const resolveLiveHrefs = (menu) => menu.querySelectorAll('[data-live-href]').forEach((a) => a._resolveHref());
  const setMenuOpen = (open) => {
    if (open) resolveLiveHrefs($('ws-menu'));
    $('ws-menu').classList.toggle('hidden', !open);
    $('ws-switcher').setAttribute('aria-expanded', open ? 'true' : 'false');
  };

  // the rail is the switcher; this menu holds only the workspace actions (Maya,
  // msg c61adc39); Create workspace lives on the rail's + alone (Maya, 2026-09-05)
  const mountMenu = () => {
    const menu = $('ws-menu');
    menu.innerHTML = '';
    // only admins get the code from /room, and only they may hand it out
    if (isAdmin) menu.appendChild(wsMenuItem('Invite member', { id: 'ws-invite-member', icon: ICON.mail, onclick: () => { setMenuOpen(false); openInviteModal(); } }));
    menu.appendChild(wsMenuItem('Join with invite link', { id: 'ws-join', icon: ICON.logIn, onclick: () => { setMenuOpen(false); openJoinModal(); } }));
    // the per-user mute sits on the workspace itself (here and the rail's context
    // menu), not in Personal settings (Maya, msg d4c52ff2)
    const ws = room && railEntry(room.slug);
    if (ws) menu.appendChild(wsMenuItem(ws.muted ? 'Unmute workspace' : 'Mute workspace', { id: 'ws-menu-mute', icon: ws.muted ? ICON.bell : ICON.bellOff, onclick: () => { setMenuOpen(false); setWorkspaceMuted(ws, !ws.muted); } }));
    menu.appendChild(wsMenuItem('Settings', { icon: ICON.settings, href: () => settingsHref() }));
    $('ws-current').textContent = room.name;
    paintRoomMark();
  };
  // one GET /api/v1/user per boot fills the rail; from then on the feed keeps it
  const mountSwitcher = (out) => {
    const here = location.pathname + location.search;
    // without the list (fetch failed) the rail still gets the current mark and "+"
    if (!out) { mountRail([], here); return; }
    $('room-head').classList.add('hidden');
    $('ws-switcher-wrap').classList.remove('hidden');
    mountRail(out.workspaces || [], here);
    // after the rail: the menu's mute label reads this workspace's entry
    mountMenu();
  };

  // the profile row at the foot of the sidebar: avatar with a status dot, the
  // name; the hover background says it is clickable, a click opens the personal menu
  const setMeMenuOpen = (open) => {
    if (open) resolveLiveHrefs($('me-menu'));
    $('me-menu').classList.toggle('hidden', !open);
    $('me-footer').setAttribute('aria-expanded', open ? 'true' : 'false');
    // the menu sits above the button in the DOM, so Tab would leave it; land on the first item
    if (open) { const first = $('me-menu').querySelector('.ws-item'); if (first) first.focus(); }
  };
  const renderMeFooter = () => {
    const foot = $('me-footer');
    foot.replaceChildren();
    const wrap = document.createElement('span');
    wrap.className = 'me-avatar';
    wrap.append(avatarEl(me, 'avatar-me'));
    const dot = document.createElement('span');
    dot.className = 'me-dot' + (me.online === false ? '' : ' online');
    dot.title = me.online === false ? 'offline' : 'online';
    wrap.append(dot);
    const name = document.createElement('span');
    name.className = 'me-name';
    name.textContent = me.name; // the role lives in Members and settings
    foot.append(wrap, name);
    const menu = $('me-menu');
    menu.replaceChildren();
    menu.appendChild(wsMenuItem('View profile', { id: 'me-profile', onclick: () => { setMeMenuOpen(false); showProfile(me); } }));
    menu.appendChild(wsMenuItem('Settings', { id: 'me-settings', href: () => settingsHref('personal') }));
    menu.appendChild(wsMenuItem('Sign out', { id: 'me-signout', onclick: () => { setMeMenuOpen(false); signOut(); } }));
  };

  // the rail: one round mark per workspace in the user's own order, the
  // current one marked; a plain click switches in place (task 23), the href
  // still serves a middle click or a new tab. "+" opens
  // create-or-join. Marks drag to reorder (task 18); the right-click menu and
  // Alt+Arrow are the keyboard way, and the menu also holds the mute.
  const mountRail = (workspaces, here) => {
    const list = $('rail-list');
    list.innerHTML = '';
    railRooms = workspaces;
    const rows = workspaces.some((w) => w.slug === room.slug) ? workspaces : [room, ...workspaces];
    for (const ws of rows) {
      const cur = ws.slug === room.slug;
      const a = document.createElement('a');
      a.className = 'rail-item';
      a.href = '/w/' + encodeURIComponent(ws.slug);
      a.dataset.slug = ws.slug;
      a.dataset.id = ws.id;
      a.dataset.tip = ws.name;
      a.setAttribute('aria-label', ws.name);
      if (cur) a.setAttribute('aria-current', 'true');
      // the fallback row (a room missing from the list) has no server id to order
      const listed = workspaces.some((w) => w.id === ws.id);
      a.draggable = listed;
      // the current room's record is fresher than the /user list (an avatar set this session)
      a.appendChild(wsAvatarEl(cur ? room : ws, 'ws-avatar-rail', wsHeaders(ws.slug)));
      const badge = document.createElement('span');
      badge.className = 'rail-badge hidden';
      a.appendChild(badge);
      list.appendChild(a);
      a.addEventListener('click', (ev) => {
        if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
        ev.preventDefault();
        switchTo(ws.slug, { push: true });
      });
      if (!listed) continue;
      a.addEventListener('dragstart', (ev) => railDragStart(ev, a));
      a.addEventListener('dragend', railDragEnd);
      a.addEventListener('contextmenu', (ev) => { ev.preventDefault(); openRailCtx(a, ev); });
      a.addEventListener('keydown', (ev) => {
        if (!ev.altKey || (ev.key !== 'ArrowUp' && ev.key !== 'ArrowDown')) return;
        ev.preventDefault();
        moveRailItem(a, ev.key === 'ArrowUp' ? -1 : 1);
      });
    }
    $('rail-create').href = '/create?next=' + encodeURIComponent(here);
    $('ws-rail').classList.remove('hidden');
    paintRailBadges(workspaces);
  };
  const paintRailCurrent = () => {
    for (const a of railItems()) {
      if (a.dataset.slug === slug) a.setAttribute('aria-current', 'true');
      else a.removeAttribute('aria-current');
    }
    $('rail-create').href = '/create?next=' + encodeURIComponent(location.pathname + location.search);
  };
  // a workspace's counts: from its entry once warm (the store is the live
  // truth, task 23), else the roll-up the /user list came with
  const wsCounts = (ws) => {
    const e = store.get(ws.slug);
    if (!e || !e.warm) return { unread: ws.unread_count || 0, mentions: ws.mentions || 0 };
    const chs = e === active() ? channels : e.channels;
    return {
      unread: chs.reduce((n, c) => n + (c.muted ? (c.unread_mentions || 0) : (c.unread_count || 0)), 0),
      mentions: chs.reduce((n, c) => n + (c.unread_mentions || 0), 0),
    };
  };

  // a count pill on every mark but the current one (the open room's channel
  // badges are the live truth for it): red for @mentions, neutral for plain
  // unreads, gray when the workspace is muted. 99+ caps the number.
  const paintRailBadges = (workspaces) => {
    if (workspaces) railRooms = workspaces;
    for (const ws of railRooms) {
      const a = $('rail-list').querySelector('.rail-item[href="/w/' + encodeURIComponent(ws.slug) + '"]');
      if (!a) continue;
      const badge = a.querySelector('.rail-badge');
      const cur = ws.slug === room.slug;
      const counts = wsCounts(ws);
      const mentions = cur ? 0 : counts.mentions;
      const unread = cur ? 0 : counts.unread;
      const shown = Math.max(unread, mentions);
      badge.classList.toggle('hidden', shown === 0);
      badge.classList.toggle('count', shown > 0);
      badge.classList.toggle('mention', mentions > 0 && !ws.muted);
      badge.classList.toggle('muted', !!ws.muted);
      badge.textContent = shown > 0 ? capCount(mentions > 0 && !ws.muted ? mentions : shown) : '';
      a.classList.toggle('is-muted', !!ws.muted);
      a.dataset.tip = ws.name + (ws.muted ? ' (Muted)' : '');
      const what = ws.muted ? ', muted' + (shown > 0 ? ', ' + shown + ' unread' : '')
        : mentions > 0 ? ', ' + mentions + ' mentions' : unread > 0 ? ', ' + unread + ' unread' : '';
      a.setAttribute('aria-label', ws.name + what);
    }
    setTitle();
  };
  // the membership list again: after a rename, or when the feed's cursor set
  // says a workspace was joined or left elsewhere; the counts come from the store
  const refreshRail = async () => {
    if ($('ws-rail').classList.contains('hidden')) return;
    try {
      mountRail((await fetchWorkspaces()).workspaces || [], location.pathname + location.search);
      warmAll();
    } catch (e) { console.error('rail', e); }
  };

  // ---------- rail order and mute (task 18) ----------
  const railItems = () => [...$('rail-list').querySelectorAll('.rail-item')];
  // the DOM order is the truth the server gets; railRooms follows it so the
  // badge feed and the title agree before the next poll
  // saves run one at a time so the last DOM order is the last one the server sees
  let railSaveChain = Promise.resolve();
  const saveRailOrder = () => {
    const ids = railItems().map((a) => a.dataset.id).filter(Boolean);
    railRooms = ids.map((id) => railRooms.find((w) => w.id === id)).filter(Boolean)
      .concat(railRooms.filter((w) => !ids.includes(w.id)));
    railSaveChain = railSaveChain.then(async () => {
      try {
        const out = await api('/api/v1/user/workspace-order', { method: 'PATCH', body: { order: ids } });
        if (out && out.workspaces) paintRailBadges(out.workspaces);
      } catch (e) { console.error('rail order', e); }
    });
    return railSaveChain;
  };
  const railOrderDirty = () => {
    const ids = railItems().map((a) => a.dataset.id).filter(Boolean);
    return ids.some((id, i) => (railRooms[i] || {}).id !== id);
  };
  const moveRailItem = (a, dir) => {
    const items = railItems();
    const i = items.indexOf(a);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= items.length) return;
    if (dir < 0) items[j].before(a);
    if (dir > 0) items[j].after(a);
    a.focus();
    saveRailOrder();
  };
  let railDragging = null;
  const railDragStart = (ev, a) => {
    railDragging = a;
    a.classList.add('dragging');
    if (ev.dataTransfer) {
      ev.dataTransfer.effectAllowed = 'move';
      try { ev.dataTransfer.setData('text/plain', a.dataset.slug); } catch { /* synthetic event */ }
    }
  };
  // a drop outside the rail fires dragend only, but dragover already moved the
  // mark, so the order on screen is the one to keep
  const railDragEnd = () => {
    if (!railDragging) return;
    railDragging.classList.remove('dragging');
    railDragging = null;
    $('rail-list').classList.remove('drop-target');
    if (railOrderDirty()) saveRailOrder();
  };
  // the dragged mark follows the pointer live: it moves before the mark whose
  // upper half the pointer is over, after the one whose lower half it is over
  $('rail-list').addEventListener('dragover', (ev) => {
    if (!railDragging) return;
    ev.preventDefault();
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'move';
    $('rail-list').classList.add('drop-target');
    for (const a of railItems()) {
      if (a === railDragging) continue;
      const r = a.getBoundingClientRect();
      if (ev.clientY < r.top || ev.clientY > r.bottom) continue;
      if (ev.clientY < r.top + r.height / 2) a.before(railDragging);
      if (ev.clientY >= r.top + r.height / 2) a.after(railDragging);
      return;
    }
  });
  $('rail-list').addEventListener('drop', (ev) => {
    if (!railDragging) return;
    ev.preventDefault();
    railDragEnd();
  });
  // a click on a mark right after a drag would navigate: the drop already
  // consumed it, and a real click never sets railDragging

  const setWorkspaceMuted = async (ws, muted) => {
    try {
      const out = await api('/api/v1/user/workspaces/' + ws.id, { method: 'PATCH', body: { muted } });
      railRooms = railRooms.map((w) => (w.id === ws.id ? { ...w, ...out } : w));
      paintRailBadges(railRooms);
      if (room && ws.slug === room.slug) mountMenu();
      notice((out.muted ? 'Muted ' : 'Unmuted ') + ws.name);
    } catch (e) { console.error('workspace mute', e); }
  };
  // the mark's context menu: move up, move down, mute or unmute
  const closeRailCtx = () => { $('rail-ctx').classList.add('hidden'); };
  const openRailCtx = (a, ev) => {
    const ws = railRooms.find((w) => w.id === a.dataset.id);
    if (!ws) return;
    const menu = $('rail-ctx');
    menu.replaceChildren();
    const items = railItems();
    const i = items.indexOf(a);
    const item = (label, id, onclick, disabled) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'rail-menu-item';
      b.id = id;
      b.setAttribute('role', 'menuitem');
      b.textContent = label;
      b.disabled = !!disabled;
      b.onclick = () => { closeRailCtx(); onclick(); };
      menu.appendChild(b);
    };
    item('Move up', 'rail-ctx-up', () => moveRailItem(a, -1), i <= 0);
    item('Move down', 'rail-ctx-down', () => moveRailItem(a, 1), i >= items.length - 1);
    item(ws.muted ? 'Unmute workspace' : 'Mute workspace', 'rail-ctx-mute', () => setWorkspaceMuted(ws, !ws.muted));
    menu.dataset.slug = ws.slug;
    menu.classList.remove('hidden');
    const r = a.getBoundingClientRect();
    // a keyboard-opened menu (Shift+F10) has no pointer: anchor it to the mark
    const x = ev && ev.clientX ? ev.clientX : r.right + 6;
    const y = ev && ev.clientY ? ev.clientY : r.top;
    menu.style.left = x + 'px';
    menu.style.top = Math.min(y, window.innerHeight - menu.offsetHeight - 8) + 'px';
    menu.querySelector('button:not([disabled])').focus();
  };
  document.addEventListener('click', (ev) => { if (!$('rail-ctx').contains(ev.target)) closeRailCtx(); });
  document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') closeRailCtx(); });

  const setRailMenuOpen = (open) => {
    $('rail-menu').classList.toggle('hidden', !open);
    $('rail-add').setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) $('rail-create').focus();
  };
  $('rail-add').addEventListener('click', (ev) => {
    ev.stopPropagation();
    setRailMenuOpen($('rail-menu').classList.contains('hidden'));
  });
  $('rail-menu').addEventListener('click', (ev) => ev.stopPropagation());
  document.addEventListener('click', () => setRailMenuOpen(false));
  document.addEventListener('keydown', (ev) => {
    if ($('rail-menu').classList.contains('hidden')) return;
    if (ev.key === 'Escape') { setRailMenuOpen(false); $('rail-add').focus(); return; }
    if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
    ev.preventDefault();
    const items = [...$('rail-menu').querySelectorAll('.rail-menu-item')];
    const i = items.indexOf(document.activeElement);
    const step = ev.key === 'ArrowDown' ? 1 : -1;
    items[(i + step + items.length) % items.length].focus();
  });

  // Join with invite link: the #no-ws-view form, in a modal on a room page
  const openJoinModal = () => {
    setRailMenuOpen(false);
    $('join-error').classList.add('hidden');
    $('join-link').value = '';
    $('join-submit').disabled = false;
    $('join-modal').classList.remove('hidden');
    $('join-link').focus();
  };
  const closeJoinModal = () => { $('join-modal').classList.add('hidden'); $('rail-add').focus(); };
  $('rail-join').onclick = openJoinModal;
  $('join-close').onclick = closeJoinModal;
  $('join-modal').onclick = (ev) => { if (ev.target === $('join-modal')) closeJoinModal(); };
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('join-modal').classList.contains('hidden')) closeJoinModal();
  });
  $('join-card').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const err = $('join-error');
    err.classList.add('hidden');
    // the join page does the work: it knows the workspace from the link
    const token = inviteTokenFrom($('join-link').value);
    if (!token) { err.textContent = 'Paste an invite link (…/join/inv-…).'; err.classList.remove('hidden'); return; }
    $('join-submit').disabled = true;
    location.href = '/join/' + encodeURIComponent(token);
  });

  $('ws-switcher').addEventListener('click', (ev) => {
    ev.stopPropagation();
    setMeMenuOpen(false);
    setMenuOpen($('ws-menu').classList.contains('hidden'));
  });
  $('ws-menu').addEventListener('click', (ev) => ev.stopPropagation());
  document.addEventListener('click', () => { setMenuOpen(false); setMeMenuOpen(false); });
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    if (!$('me-menu').classList.contains('hidden')) { setMeMenuOpen(false); $('me-footer').focus(); }
    if ($('ws-menu').classList.contains('hidden')) return;
    setMenuOpen(false);
    $('ws-switcher').focus();
  });
  $('me-footer').addEventListener('click', (ev) => {
    ev.stopPropagation();
    setMenuOpen(false);
    setMeMenuOpen($('me-menu').classList.contains('hidden'));
  });
  $('me-menu').addEventListener('click', (ev) => ev.stopPropagation());

  $('composer').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const text = composerBox.getMarkdown().trim();
    if ((!text && !pendingAtt.main) || !current) return;
    composerBox.clear(); // clear instantly; post shows the placeholder
    try { await post(text); } catch (e) { composerBox.setMarkdown(text); alert(e.message); }
  });

  $('thread-composer').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const text = threadBox.getMarkdown().trim();
    if ((!text && !pendingAtt.thread) || !openThreadRoot) return;
    const root = openThreadRoot;
    threadBox.clear(); // clear instantly; post shows the placeholder
    try { await post(text, root); } catch (e) { threadBox.setMarkdown(text); alert(e.message); }
  });

  // ---------- slash commands: /invite, /join, /leave, /hide ----------
  // Autocomplete mirrors the mention popup; commands run client-side against
  // the existing APIs and never post the raw "/command" text as a message.
  const SLASH_COMMANDS = [
    { name: 'invite', args: '@participant', hint: 'add someone to this channel' },
    { name: 'remove', args: '@participant', hint: 'remove someone from this channel (admin)' },
    { name: 'join', args: '#channel', hint: 'join a public channel' },
    { name: 'leave', args: '', hint: 'leave this channel' },
    { name: 'hide', args: '', hint: 'hide the open thread until someone writes in it' },
  ];

  // the two WYSIWYG composers (Tiptap); the wire format stays markdown
  // Everything the ranker needs: membership decides who can even see the
  // message, recency decides who you are mid-conversation with.
  const DORMANT_MS = 14 * 24 * 3600 * 1000;
  const mentionOptions = () => {
    const inCh = new Set(channelMembers.map((p) => p.id));
    const now = Date.now();
    return participants.map((p) => ({
      name: p.name,
      // the picker draws the member's real avatar image, never a stand-in glyph
      avatar: () => avatarCore(p, 'avatar-sm'),
      inChannel: inCh.has(p.id),
      online: !!p.online,
      dormant: !!p.last_seen_at && now - Date.parse(p.last_seen_at) > DORMANT_MS,
      talkedAt: talkedAt.get(p.name) || '',
    }));
  };

  // what "#" completes to: your channels first, then public ones you could join
  const channelOptions = () => channels.filter((c) => !c.archived)
    .map((c) => ({ name: c.name, private: !!c.private, topic: c.topic }))
    .concat(publicChannels.map((c) => ({ name: c.name, private: false, topic: 'not joined' })));

  // a #channel link: open it, joining first when it is public and you are not in
  const goToChannel = async (name) => {
    const mine = channels.find((c) => c.name === name);
    if (mine) return selectChannel(mine);
    const pub = publicChannels.find((c) => c.name === name);
    if (!pub) return;
    try {
      await api('/api/v1/channels/' + pub.id + '/join', { method: 'POST' });
      await refreshRoom();
      const joined = channels.find((c) => c.id === pub.id);
      if (joined) await selectChannel(joined);
    } catch (e) { alert(e.message); }
  };

  // the send button is muted while the editor is empty (Maya, msg 42e8199f)
  const syncEmpty = (form, box) => () => form.querySelector('.composer-main').classList.toggle('empty', box.isEmpty());
  const composerBox = createComposer({
    mount: $('composer-mount'), id: 'composer-input',
    placeholder: 'Message… (@name to mention, #channel to link, markdown ok)',
    onSubmit: () => $('composer').requestSubmit(),
    onChange: () => syncEmpty($('composer'), composerBox)(),
    getMentionOptions: mentionOptions,
    getMeName: () => (me ? me.name : ''),
    getChannelOptions: channelOptions,
    slashCommands: SLASH_COMMANDS,
    browseChannels: async () => ((await api('/api/v1/channels/browse')).channels || []).filter((c) => !c.member),
    onImageFile: (f) => uploadPending('main', new File([f], f.name || 'pasted-image.png', { type: f.type })),
  });
  const threadBox = createComposer({
    mount: $('thread-mount'), id: 'thread-input',
    placeholder: 'Reply…',
    onSubmit: () => $('thread-composer').requestSubmit(),
    onChange: () => syncEmpty($('thread-composer'), threadBox)(),
    getMentionOptions: mentionOptions,
    getMeName: () => (me ? me.name : ''),
    getChannelOptions: channelOptions,
    slashCommands: SLASH_COMMANDS,
    browseChannels: async () => ((await api('/api/v1/channels/browse')).channels || []).filter((c) => !c.member),
    onImageFile: (f) => uploadPending('thread', new File([f], f.name || 'pasted-image.png', { type: f.type })),
  });
  syncEmpty($('composer'), composerBox)(); syncEmpty($('thread-composer'), threadBox)();

  // inline confirmation / error in the composer area, auto-fades
  const slashStatus = (form, text, isErr) => {
    const host = form;
    let s = host.querySelector('.composer-status');
    if (!s) {
      s = document.createElement('div');
      s.className = 'composer-status';
      host.appendChild(s);
    }
    s.textContent = text;
    s.classList.toggle('err', !!isErr);
    s.classList.remove('hidden');
    clearTimeout(s._fade);
    s._fade = setTimeout(() => s.classList.add('hidden'), 4000);
  };

  const runSlash = async (box, form) => {
    const raw = box.getPlain().trim();
    const m = raw.match(/^\/(\S+)\s*(.*)$/);
    const cmd = m ? m[1].toLowerCase() : '';
    const arg = m ? m[2].trim() : '';
    const done = (msg) => { box.clear(); slashStatus(form, msg); };
    const fail = (msg) => slashStatus(form, msg, true);
    try {
      if (cmd === 'invite') {
        if (!current) return fail('No channel selected.');
        const who = arg.replace(/^@/, '');
        if (!who) return fail('Usage: /invite @participant');
        await api('/api/v1/channels/' + current.id + '/members', { method: 'POST', body: { participant: who } });
        return done('Added ' + who + ' to #' + current.name);
      }
      if (cmd === 'remove') {
        if (!current) return fail('No channel selected.');
        const who = arg.replace(/^@/, '');
        if (!who) return fail('Usage: /remove @participant');
        await api('/api/v1/channels/' + current.id + '/members/' + encodeURIComponent(who), { method: 'DELETE' });
        return done('Removed ' + who + ' from #' + current.name);
      }
      if (cmd === 'join') {
        const name = arg.replace(/^#/, '');
        if (!name) return fail('Usage: /join channel');
        const out = await api('/api/v1/channels/browse');
        const ch = (out.channels || []).find((c) => c.name === name);
        if (!ch) return fail('No public channel "' + name + '" to join.');
        await api('/api/v1/channels/' + ch.id + '/join', { method: 'POST' });
        await refreshRoom();
        await selectChannel(channels.find((c) => c.id === ch.id));
        return done('Joined #' + name);
      }
      if (cmd === 'leave') {
        if (!current) return fail('No channel selected.');
        if (current.name === 'general') return fail('#general cannot be left.');
        const name = current.name;
        await leaveChannel(current);
        return done('Left #' + name);
      }
      if (cmd === 'hide') {
        if (box !== threadBox || !openThreadRoot) return fail('/hide works in an open thread.');
        const root = openThreadRoot;
        await api('/api/v1/threads/' + root + '/resolve', { method: 'POST', body: { resolved: true } });
        closeThread();
        loadThreads();
        return done('Thread hidden.');
      }
      fail('Unknown command /' + cmd);
    } catch (e) { fail('/' + cmd + ' failed: ' + e.message); }
  };

  // capture on document so the slash run wins over the send handlers on the form
  document.addEventListener('submit', (ev) => {
    const box = ev.target.id === 'composer' ? composerBox
      : ev.target.id === 'thread-composer' ? threadBox : null;
    if (!box || !box.getPlain().trimStart().startsWith('/')) return;
    ev.preventDefault();
    ev.stopPropagation();
    runSlash(box, ev.target);
  }, true);


  // ---------- channel members modal ----------

  const paintHeaderMembers = (ch) => {
    const list = active().members.get(ch.id);
    if (!list) return; // the first visit fills it after the paint
    channelMembers = list;
    $('members-count').textContent = channelMembers.length;
    $('members-btn').classList.remove('hidden');
  };
  const refreshHeaderMembers = async (ch) => {
    const e = active();
    try {
      const out = await api('/api/v1/channels/' + ch.id + '/members', { ws: e.slug });
      e.members.set(ch.id, out.members || []);
      if (!current || current.id !== ch.id || e !== active()) return;
      paintHeaderMembers(ch);
      if (!$('members-modal').classList.contains('hidden')) renderMembersModal();
    } catch (err) {
      if (current && current.id === ch.id && e === active()) $('members-btn').classList.add('hidden');
    }
  };

  const memberRow = (p, action) => {
    const row = document.createElement('div');
    row.className = 'mm-row';
    row.dataset.id = p.id;
    row.appendChild(avatarEl(p, 'avatar-rb'));
    const name = document.createElement('span');
    name.className = 'mm-name';
    name.textContent = p.name;
    const dot = document.createElement('span');
    dot.className = 'mm-dot' + (p.online ? ' on' : '');
    dot.title = p.online ? 'online' : 'offline';
    row.append(name, dot);
    if (p.owner_name) {
      const ow = document.createElement('span');
      ow.className = 'mm-owner';
      ow.textContent = p.owner_name + "'s agent";
      row.appendChild(ow);
    }
    if (action) row.appendChild(action);
    return row;
  };

  const renderMembersModal = () => {
    if (!current) return;
    $('members-title').replaceChildren(sigilEl(current.private), current.name + ' · ' + channelMembers.length +
      (channelMembers.length === 1 ? ' member' : ' members'));
    const list = $('members-list');
    list.innerHTML = '';
    // removal is admin-only server-side (public and private alike); #general is pinned
    const canRemove = me.role === 'admin' && current.name !== 'general';
    const groups = [
      ['Humans', channelMembers.filter((p) => p.is_human)],
      ['Agents', channelMembers.filter((p) => !p.is_human)],
    ];
    for (const [label, members] of groups) {
      if (!members.length) continue;
      const h = document.createElement('div');
      h.className = 'mm-group';
      h.textContent = label;
      list.appendChild(h);
      for (const p of members) {
        let btn = null;
        // my own row keeps an invisible placeholder, so Remove sits at one x down the list
        if (canRemove && p.id === me.id) {
          btn = document.createElement('span');
          btn.className = 'remove-gap mm-remove-gap';
          btn.setAttribute('aria-hidden', 'true');
        }
        if (canRemove && p.id !== me.id) {
          btn = document.createElement('button');
          btn.className = 'remove-btn mm-remove';
          btn.textContent = 'Remove';
          btn.onclick = async () => {
            try {
              await api('/api/v1/channels/' + current.id + '/members/' + p.id, { method: 'DELETE' });
              await refreshHeaderMembers(current);
            } catch (e) { alert('Remove failed: ' + e.message); }
          };
        }
        list.appendChild(memberRow(p, btn));
      }
    }
  };

  const renderAddList = () => {
    const box = $('members-addlist');
    box.innerHTML = '';
    const outside = participants.filter((p) => !channelMembers.some((m) => m.id === p.id));
    if (!outside.length) {
      box.textContent = 'Everyone in the room is already here.';
      return;
    }
    for (const p of outside) {
      const btn = document.createElement('button');
      btn.className = 'mm-add';
      btn.textContent = 'Add';
      btn.onclick = async () => {
        try {
          await api('/api/v1/channels/' + current.id + '/members', { method: 'POST', body: { participant: p.name } });
          await refreshHeaderMembers(current);
          renderAddList();
        } catch (e) { alert('Add failed: ' + e.message); }
      };
      box.appendChild(memberRow(p, btn));
    }
  };

  const openMembers = async () => {
    if (!current) return;
    const ch = current;
    const e = active();
    await refreshHeaderMembers(ch);
    if (!current || current.id !== ch.id || e !== active()) return;
    renderMembersModal();
    $('members-addlist').classList.add('hidden');
    $('members-modal').classList.remove('hidden');
  };
  const closeMembers = () => $('members-modal').classList.add('hidden');
  $('members-btn').onclick = openMembers;
  $('members-close').onclick = closeMembers;
  $('members-modal').onclick = (ev) => { if (ev.target.id === 'members-modal') closeMembers(); };
  $('members-invite').onclick = () => { renderAddList(); $('members-addlist').classList.toggle('hidden'); };
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('members-modal').classList.contains('hidden')) closeMembers();
  });

  $('thread-close').onclick = closeThread;

  // Thread panel resize: drag the left divider. Width clamps to 20%-50% of the
  // viewport and persists per browser; the 504px CSS default holds until a drag.
  // Applied as a CSS var so the mobile full-bleed media query still wins.
  const THREAD_W_KEY = 'agentchat:threadWidth';
  const clampThreadW = (px) => Math.max(window.innerWidth * 0.2, Math.min(window.innerWidth * 0.5, px));
  const applyThreadW = (px) => document.documentElement.style.setProperty('--thread-w', px + 'px');
  const storedW = parseFloat(localStorage.getItem(THREAD_W_KEY));
  if (storedW > 0) applyThreadW(clampThreadW(storedW));
  (() => {
    const handle = $('thread-resize'), panel = $('thread-panel');
    let dragging = false;
    handle.addEventListener('mousedown', (e) => {
      e.preventDefault();
      dragging = true;
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
    });
    window.addEventListener('mousemove', (e) => {
      if (!dragging) return;
      // panel hugs the right edge, so its width is everything right of the cursor
      const w = clampThreadW(panel.getBoundingClientRect().right - e.clientX);
      applyThreadW(w);
    });
    window.addEventListener('mouseup', () => {
      if (!dragging) return;
      dragging = false;
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      const w = parseFloat(getComputedStyle(panel).width);
      localStorage.setItem(THREAD_W_KEY, String(Math.round(w)));
    });
    // keep a persisted width legal when the viewport shrinks
    window.addEventListener('resize', () => { if (storedW > 0) applyThreadW(clampThreadW(parseFloat(localStorage.getItem(THREAD_W_KEY)) || storedW)); });
  })();

  $('lightbox').onclick = () => { $('lightbox').classList.add('hidden'); $('lightbox-img').removeAttribute('src'); };

  // ---------- search ----------

  let searchSeq = 0;          // drops stale async results when the query moves on
  let searchTimer = null;
  // filter state: fields AND, repeats within a field OR. Dates are local
  // YYYY-MM-DD; the request turns them into RFC3339 bounds.
  const emptyFilters = () => ({ authors: [], channel: null, after: null, before: null, kinds: [], has: false });
  let searchFilters = emptyFilters();
  let lastSearchURL = ''; // the checks read the params the UI produced

  const openSearch = () => {
    $('search-modal').classList.remove('hidden');
    const input = $('search-input');
    input.focus();
    input.select();
  };
  const closeSearch = () => { closeFilterPop(); $('search-modal').classList.add('hidden'); };

  const scheduleSearch = () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(runSearch, 220);
  };

  const isoDay = (d) => d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
  const daysAgo = (n) => { const d = new Date(); d.setDate(d.getDate() - n); return isoDay(d); };
  const validDay = (s) => /^\d{4}-\d{2}-\d{2}$/.test(s) && !isNaN(Date.parse(s));
  const byName = (list, name) => list.find((x) => x.name.toLowerCase() === String(name).toLowerCase());
  const KIND_LABEL = { message: 'messages', thread: 'threads', attachment: 'attachments' };

  // Slack-style inline tokens: a completed token (trailing space) that
  // resolves lifts into a chip and leaves the box; anything else stays text.
  const liftInlineTokens = () => {
    const input = $('search-input');
    let lifted = false;
    // lookahead keeps the space, so two adjacent tokens both lift in one pass
    const rest = input.value.replace(/(?:^|\s)(from|in|has|after|before|kind):(\S+)(?=\s)/gi, (m, key, val) => {
      const k = key.toLowerCase();
      const v = val.replace(/^[@#]/, '');
      let ok = false;
      if (k === 'from') { const p = byName(participants, v); if (p && !searchFilters.authors.includes(p.id)) searchFilters.authors.push(p.id); ok = !!p; }
      if (k === 'in') { const c = byName(channels, v); if (c) searchFilters.channel = c.id; ok = !!c; }
      if (k === 'has') { ok = /^attachments?$/i.test(v); if (ok) searchFilters.has = true; }
      if (k === 'after' || k === 'before') { ok = validDay(v); if (ok) searchFilters[k] = v; }
      if (k === 'kind') { const kind = v.replace(/s$/, ''); ok = kind in KIND_LABEL; if (ok && !searchFilters.kinds.includes(kind)) searchFilters.kinds.push(kind); }
      if (!ok) return m;
      lifted = true;
      return ' ';
    });
    if (lifted) input.value = rest.replace(/\s{2,}/g, ' ').replace(/^\s+/, '');
    if (lifted) renderChips();
  };

  const searchParams = (q) => {
    const f = searchFilters;
    const p = new URLSearchParams({ q, limit: '50' });
    f.authors.forEach((id) => p.append('author', id));
    if (f.channel) p.set('channel', f.channel);
    if (f.after) p.set('since', new Date(f.after + 'T00:00:00').toISOString());
    if (f.before) p.set('until', new Date(f.before + 'T23:59:59.999').toISOString());
    f.kinds.forEach((k) => p.append('kind', k));
    if (f.has) p.set('has_attachment', 'true');
    return p.toString();
  };

  // One request, one ranked list: the server fuses the text leg and the
  // semantic leg (when it has an embedder) and tags semantic-only rows.
  const runSearch = () => {
    liftInlineTokens();
    const q = $('search-input').value.trim();
    const box = $('search-results');
    const seq = ++searchSeq;
    if (!q) {
      box.innerHTML = '';
      $('search-note').classList.add('hidden');
      if (hasFilters()) box.appendChild(searchNote('search-empty', 'Type something to search within these filters.'));
      return;
    }
    const state = { seq, rows: 'pending', semantic: true, expanded: false };
    paintSearch(state);
    lastSearchURL = '/api/v1/search/hybrid?' + searchParams(q);
    api(lastSearchURL)
      .then((out) => { if (seq === searchSeq) { state.rows = out.results || []; state.semantic = out.semantic !== false; paintSearch(state); } })
      .catch((e) => { if (seq === searchSeq) { state.rows = { err: e.message }; paintSearch(state); } });
  };

  const searchNote = (cls, text) => {
    const d = document.createElement('div');
    d.className = cls;
    d.textContent = text;
    return d;
  };

  // A long list previews a few rows; "(see more)" expands inline for this
  // query only, without a new request.
  const SEARCH_PREVIEW_ROWS = 5;
  const paintSearch = (state) => {
    if (state.seq !== searchSeq) return;
    const box = $('search-results');
    box.innerHTML = '';
    const note = $('search-note');
    note.classList.toggle('hidden', state.semantic);
    if (!state.semantic) note.textContent = 'Semantic search is off on this server (no embeddings provider configured); showing direct matches only.';
    if (state.rows === 'pending') { box.appendChild(searchNote('search-loading', 'Searching…')); return; }
    if (state.rows.err) { box.appendChild(searchNote('search-empty', 'Search failed: ' + state.rows.err)); return; }
    if (!state.rows.length) { box.appendChild(searchNote('search-empty', hasFilters() ? 'No matches with these filters.' : 'No matches.')); return; }
    const shown = state.expanded ? state.rows : state.rows.slice(0, SEARCH_PREVIEW_ROWS);
    shown.forEach((r) => box.appendChild(searchHitRow(r)));
    if (state.rows.length <= shown.length) return;
    const more = document.createElement('div');
    more.className = 'search-more';
    more.textContent = '... (see more)';
    more.onclick = () => { state.expanded = true; paintSearch(state); };
    box.appendChild(more);
  };

  const snippet = (body) => {
    const s = String(body).replace(/\s+/g, ' ').trim();
    return s.length > 180 ? s.slice(0, 180) + '…' : s;
  };
  // a result reads like a message row: avatar, name, time, channel, snippet
  const searchHitRow = (r) => {
    const ch = channels.find((c) => c.id === r.channel_id);
    const row = document.createElement('div');
    row.className = 'search-hit-row';
    const av = document.createElement('div');
    av.className = 'sh-avatar';
    av.appendChild(avatarEl(participants.find((x) => x.id === r.author_id), 'avatar-msg'));
    const body = document.createElement('div');
    body.className = 'sh-body';
    const meta = document.createElement('div');
    meta.className = 'sh-meta';
    meta.innerHTML = `<span class="sh-author">${esc(r.author_name)}</span>` +
      messageTimeHTML(r.created_at, 'sh-time') +
      `<span class="sh-channel">#${esc(ch ? ch.name : 'channel')}</span>` +
      (r.thread_root_id ? '<span class="sh-thread">in thread</span>' : '') +
      (r.via === 'semantic' ? '<span class="sh-via" title="matched by meaning, not by these words">semantic</span>' : '');
    const snip = document.createElement('div');
    snip.className = 'sh-snippet';
    snip.textContent = snippet(r.body); // textContent, never markdown: search hits stay inert
    body.append(meta, snip);
    row.append(av, body);
    row.onclick = () => jumpToMessage(r);
    return row;
  };

  // ----- filter bar: chips + one popover -----
  const hasFilters = () => {
    const f = searchFilters;
    return f.authors.length > 0 || !!f.channel || !!f.after || !!f.before || f.kinds.length > 0 || f.has;
  };
  const chip = (label, text, remove) => {
    const c = document.createElement('span');
    c.className = 'search-chip';
    c.dataset.filter = label;
    c.innerHTML = `<span class="chip-key">${esc(label)}:</span> <span class="chip-val">${esc(text)}</span>`;
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'chip-x';
    x.setAttribute('aria-label', 'remove ' + label + ' filter');
    x.innerHTML = ICON.x;
    x.onclick = () => { remove(); renderChips(); runSearch(); };
    c.appendChild(x);
    return c;
  };
  const renderChips = () => {
    const f = searchFilters;
    const box = $('search-chips');
    box.replaceChildren();
    f.authors.forEach((id) => {
      const p = participants.find((x) => x.id === id);
      box.appendChild(chip('from', p ? p.name : id, () => { f.authors = f.authors.filter((a) => a !== id); }));
    });
    if (f.channel) {
      const c = channels.find((x) => x.id === f.channel);
      box.appendChild(chip('in', '#' + (c ? c.name : f.channel), () => { f.channel = null; }));
    }
    if (f.after) box.appendChild(chip('after', f.after, () => { f.after = null; }));
    if (f.before) box.appendChild(chip('before', f.before, () => { f.before = null; }));
    f.kinds.forEach((k) => box.appendChild(chip('kind', KIND_LABEL[k], () => { f.kinds = f.kinds.filter((x) => x !== k); })));
    if (f.has) box.appendChild(chip('has', 'attachment', () => { f.has = false; }));
    box.classList.toggle('hidden', !hasFilters());
    $('search-clear-filters').classList.toggle('hidden', !hasFilters());
    document.querySelectorAll('#search-filters .sf-btn').forEach((b) => {
      const on = { from: f.authors.length > 0, in: !!f.channel, date: !!(f.after || f.before), kind: f.kinds.length > 0, has: f.has }[b.dataset.sf];
      b.classList.toggle('on', !!on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  };

  let filterPopFor = null;
  const closeFilterPop = () => { $('sf-pop').classList.add('hidden'); $('sf-pop').replaceChildren(); filterPopFor = null; };
  const popOption = (label, checked, onPick, avatarOf) => {
    const o = document.createElement('label');
    o.className = 'sf-opt' + (checked ? ' checked' : '');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = checked;
    cb.onchange = () => { onPick(cb.checked, cb); renderChips(); runSearch(); };
    o.appendChild(cb);
    if (avatarOf) o.appendChild(avatarEl(avatarOf, 'avatar-sm'));
    const t = document.createElement('span');
    t.className = 'sf-opt-label';
    t.textContent = label;
    o.appendChild(t);
    return o;
  };
  const openFilterPop = (which, anchor) => {
    if (filterPopFor === which) { closeFilterPop(); return; }
    closeFilterPop();
    const f = searchFilters;
    const pop = $('sf-pop');
    pop.dataset.for = which;
    if (which === 'from') {
      const people = participants.slice().sort((a, b) => (b.is_human - a.is_human) || a.name.localeCompare(b.name));
      people.forEach((p) => pop.appendChild(popOption(p.name + (p.is_human ? '' : ' · agent'), f.authors.includes(p.id), (on) => {
        f.authors = on ? f.authors.concat(p.id) : f.authors.filter((x) => x !== p.id);
      }, p)));
    }
    if (which === 'in') {
      // one channel at a time: picking one unticks the rest
      channels.slice().sort((a, b) => a.name.localeCompare(b.name)).forEach((c) => pop.appendChild(popOption('#' + c.name, f.channel === c.id, (on, cb) => {
        f.channel = on ? c.id : null;
        pop.querySelectorAll('input').forEach((i) => { if (i !== cb) i.checked = false; });
      })));
    }
    if (which === 'kind') {
      Object.keys(KIND_LABEL).forEach((k) => pop.appendChild(popOption(KIND_LABEL[k], f.kinds.includes(k), (on) => {
        f.kinds = on ? f.kinds.concat(k) : f.kinds.filter((x) => x !== k);
      })));
    }
    if (which === 'date') {
      const presets = document.createElement('div');
      presets.className = 'sf-presets';
      [['Today', 0], ['Last 7 days', 7], ['Last 30 days', 30]].forEach(([label, n]) => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'btn-sm';
        b.dataset.preset = String(n);
        b.textContent = label;
        b.onclick = () => { f.after = daysAgo(n); f.before = null; renderChips(); runSearch(); closeFilterPop(); };
        presets.appendChild(b);
      });
      pop.appendChild(presets);
      const range = document.createElement('div');
      range.className = 'sf-range';
      [['after', 'After'], ['before', 'Before']].forEach(([key, label]) => {
        const l = document.createElement('label');
        l.textContent = label + ' ';
        const i = document.createElement('input');
        i.type = 'date';
        i.id = 'sf-' + key;
        i.value = f[key] || '';
        i.onchange = () => { f[key] = validDay(i.value) ? i.value : null; renderChips(); runSearch(); };
        l.appendChild(i);
        range.appendChild(l);
      });
      pop.appendChild(range);
    }
    const r = anchor.getBoundingClientRect();
    const card = $('search-card').getBoundingClientRect();
    pop.style.left = (r.left - card.left) + 'px';
    pop.classList.remove('hidden');
    filterPopFor = which;
  };
  document.querySelectorAll('#search-filters .sf-btn').forEach((b) => {
    b.onclick = () => {
      if (b.dataset.sf === 'has') { searchFilters.has = !searchFilters.has; renderChips(); runSearch(); return; }
      openFilterPop(b.dataset.sf, b);
    };
  });
  $('search-clear-filters').onclick = () => { searchFilters = emptyFilters(); closeFilterPop(); renderChips(); runSearch(); };
  document.addEventListener('mousedown', (ev) => {
    if (filterPopFor && !ev.target.closest('#sf-pop') && !ev.target.closest('#search-filters')) closeFilterPop();
  });

  // land the reader on the hit: switch channel, open its thread if it lives in
  // one, then scroll to and flash the node. Older top-level hits may sit above
  // the loaded window; we navigate regardless and flash only if it is present.
  const jumpToMessage = async (r) => {
    closeSearch();
    const ch = channels.find((c) => c.id === r.channel_id);
    if (ch && (!current || current.id !== ch.id)) await selectChannel(ch);
    if (r.thread_root_id) { try { await openThread(r.thread_root_id); } catch (e) { /* thread gone */ } }
    await revealMessage(r.id, !!r.thread_root_id);
  };

  // show the platform-correct shortcut in the search field's hint chip
  $('search-kbd').textContent = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent) ? '⌘K' : 'Ctrl K';
  $('open-search').onclick = openSearch;
  $('search-close').onclick = closeSearch;
  $('search-modal').onclick = (ev) => { if (ev.target === $('search-modal')) closeSearch(); };
  $('search-input').addEventListener('input', scheduleSearch);
  $('search-input').addEventListener('keydown', (ev) => {
    // Backspace on an empty box pops the last chip, like Slack
    if (ev.key !== 'Backspace' || $('search-input').value !== '') return;
    const last = $('search-chips').lastElementChild;
    if (last) last.querySelector('.chip-x').click();
  });

  document.addEventListener('keydown', (ev) => {
    if ((ev.metaKey || ev.ctrlKey) && (ev.key === 'k' || ev.key === 'K')) {
      if (!me) return; // only meaningful once in a room
      ev.preventDefault();
      $('search-modal').classList.contains('hidden') ? openSearch() : closeSearch();
    }
    if (ev.key !== 'Escape' || $('search-modal').classList.contains('hidden')) return;
    // first Escape closes an open filter popover, the next one the modal
    if (filterPopFor) { closeFilterPop(); return; }
    closeSearch();
  });

  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('lightbox').classList.contains('hidden')) $('lightbox').click();
    if (ev.key === 'Escape' && !$('doc-modal').classList.contains('hidden')) closeDoc();
    if (ev.key === 'Escape' && !$('browse-modal').classList.contains('hidden')) closeBrowse();
  });

  const closeProfile = () => {
    profileFor = null;
    profileCapsFor = null;
    profileRemindersFor = null;
    profileDeliveryFor = null;
    $('profile-modal').classList.add('hidden');
  };
  $('profile-close').onclick = closeProfile;
  $('profile-modal').onclick = (ev) => {
    if (ev.target === $('profile-modal')) closeProfile();
  };
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('profile-modal').classList.contains('hidden')) closeProfile();
  });

  // navigator.clipboard exists only in a secure context (https or localhost), so
  // it is undefined on plain-HTTP LAN prod. Fall back to a hidden textarea +
  // execCommand and report whether the copy actually landed.
  const copyText = async (text) => {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch (e) { /* fall through to the legacy path */ }
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      return ok;
    } catch (e) { return false; }
  };

  // flip to a confirmed state only when the copy succeeded; otherwise show an
  // error state and hold it a bit longer so the user notices
  const flashCopy = (btn, ok, restore, done = 'copied') => {
    btn.innerHTML = ok ? ICON.check + ' ' + done : ICON.triangleAlert + ' copy failed';
    setTimeout(() => { btn.textContent = restore; }, ok ? 1500 : 2500);
  };

  // ---------- invite modal (workspace menu > Invite member, admins only) ----------
  const showInviteErr = (msg) => { $('invite-error').textContent = msg; $('invite-error').classList.toggle('hidden', !msg); };
  const inviteMeta = (v) => {
    const parts = [v.created_by_name ? 'by ' + v.created_by_name : 'workspace link'];
    parts.push(`${v.uses} ${v.uses === 1 ? 'use' : 'uses'}`);
    if (v.expires_at) parts.push((v.status === 'expired' ? 'expired ' : 'expires ') + new Date(v.expires_at).toLocaleDateString());
    return parts.join(' · ');
  };
  // the row shows a short token; the full url lives in dataset.url and the title
  const shortToken = (url) => { const t = (url.split('/join/')[1] || url); return t.length > 8 ? t.slice(0, 8) + '…' : t; };
  const iconBtn = (cls, icon, label) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = cls + ' icon-btn';
    b.innerHTML = icon;
    b.setAttribute('aria-label', label);
    b.title = label;
    return b;
  };
  let inviteRenderSeq = 0;
  const renderInvites = async () => {
    const seq = ++inviteRenderSeq;
    const workspace = slug;
    let out;
    try { out = await api('/api/v1/invites', { ws: workspace }); }
    catch (e) {
      if (seq === inviteRenderSeq && workspace === slug) showInviteErr(e.message);
      return false;
    }
    if (seq !== inviteRenderSeq || workspace !== slug || $('invite-modal').classList.contains('hidden')) return false;
    const list = $('invite-list');
    list.replaceChildren();
    const invites = (out.invites || []).slice().sort((a, b) => (Date.parse(b.created_at) || 0) - (Date.parse(a.created_at) || 0));
    $('invite-empty').classList.toggle('hidden', invites.length > 0);
    for (const v of invites) {
      const li = document.createElement('li');
      li.className = 'invite-item';
      li.dataset.id = v.id;
      li.dataset.url = v.url;
      const token = document.createElement('code');
      token.className = 'invite-token';
      token.textContent = shortToken(v.url);
      token.title = v.url;
      const info = document.createElement('span');
      info.className = 'invite-info';
      const meta = document.createElement('span');
      meta.className = 'invite-meta';
      meta.textContent = inviteMeta(v);
      info.appendChild(meta);
      if (v.owner_id) {
        const badge = document.createElement('span');
        badge.className = 'invite-badge';
        badge.textContent = 'binds agents to ' + (v.owner_id === (me && me.id) ? 'you' : 'its owner');
        info.appendChild(badge);
      }
      const actions = document.createElement('span');
      actions.className = 'invite-actions';
      const copy = iconBtn('invite-copy', ICON.copy, 'Copy link');
      copy.onclick = async () => {
        const ok = await copyText(v.url);
        copy.innerHTML = ok ? ICON.check : ICON.triangleAlert;
        setTimeout(() => { copy.innerHTML = ICON.copy; }, ok ? 1500 : 2500);
      };
      const revoke = iconBtn('invite-revoke', ICON.trashTwo, 'Revoke');
      revoke.onclick = async () => {
        if (!confirm('Revoke this link? Anyone holding it can no longer join.')) return;
        try { await api('/api/v1/invites/' + encodeURIComponent(v.id), { method: 'DELETE', ws: workspace }); }
        catch (e) { if (workspace === slug) showInviteErr(e.message); return; }
        if (workspace === slug && !$('invite-modal').classList.contains('hidden')) await renderInvites();
      };
      actions.append(copy, revoke);
      li.append(token, info, actions);
      list.appendChild(li);
    }
    return true;
  };
  const openInviteModal = async () => {
    showInviteErr('');
    $('invite-modal').classList.remove('hidden');
    if (await renderInvites()) $('invite-new-submit').focus();
  };
  const closeInviteModal = () => {
    inviteRenderSeq++;
    $('invite-modal').classList.add('hidden');
    $('ws-switcher').focus();
  };
  $('invite-close').onclick = closeInviteModal;
  $('invite-modal').onclick = (ev) => { if (ev.target === $('invite-modal')) closeInviteModal(); };
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('invite-modal').classList.contains('hidden')) closeInviteModal();
  });
  $('invite-new').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const workspace = slug;
    showInviteErr('');
    $('invite-new-submit').disabled = true;
    try {
      const body = { expires_in_seconds: Number($('invite-expiry').value) };
      const out = await api('/api/v1/invites', { method: 'POST', body, ws: workspace });
      if (workspace !== slug || $('invite-modal').classList.contains('hidden')) return;
      await renderInvites();
      const row = $('invite-list').querySelector(`[data-id="${out.invite.id}"]`);
      if (row) { row.scrollIntoView({ block: 'nearest' }); row.querySelector('.invite-copy').focus(); }
    } catch (e) { if (workspace === slug) showInviteErr(e.message); }
    finally { $('invite-new-submit').disabled = false; }
  });

  // "Add an agent": mint a link bound to you and show it with the setup text.
  // Admins and plain members alike: the server allows a member only this
  // bound kind, so the agent always lands under the right human.
  const agentSetupText = (origin, link, access) => {
    const headers = access ?
      `This room sits behind Cloudflare Access, so every curl to ${origin} needs these two headers (treat them like a password, never print or post them):\n` +
      `  -H "CF-Access-Client-Id: ${access.client_id}"\n  -H "CF-Access-Client-Secret: ${access.client_secret}"\n` : '';
    return `You are joining the OpenChatter workspace "${room ? room.name : ''}" as ${me ? me.name : 'your human'}'s agent.\n` +
      `1. Fetch ${origin}/skill with curl and follow it end to end (it installs cli.sh and explains the protocol).\n${headers}` +
      `2. Join with this invite link (it binds you to ${me ? me.name : 'your human'}, server-verified):\n   ${link}\n` +
      `   e.g. curl -s -X POST ${origin}/api/v1/rooms/join -H 'Content-Type: application/json' -d '{"invite":"${link}","name":"<your-name>","description":"<what you do>"}'\n` +
      '3. Read the recent history of #general, then post one short hello there: who you are, what you can help with, and that ' + (me ? me.name : 'your human') + ' owns you.\n' +
      '4. Your token is your identity. Keep it secret and backed up. If you lose it, your human must delete this agent in the UI and add it again; that creates a new identity, while past messages stay with the old one.';
  };
  const showAddAgentErr = (msg) => { const el = $('addagent-error'); el.textContent = msg; el.classList.toggle('hidden', !msg); };
  const openAddAgent = async () => {
    showAddAgentErr('');
    const ta = $('addagent-text');
    ta.value = '';
    ta.style.height = '';
    $('addagent-modal').classList.remove('hidden');
    const origin = new URL(joinURL || location.href).origin;
    try {
      // seven days: a member cannot list or revoke, so their links must not live forever
      const inv = await api('/api/v1/invites', { method: 'POST', body: { bind_owner: true, expires_in_seconds: 7 * 86400 } });
      ta.value = agentSetupText(origin, inv.join_url, inv.access || null);
      // grow to the text so the common case needs no scrolling (css caps the height)
      ta.style.height = 'auto';
      ta.style.height = Math.min(ta.scrollHeight + 2, 560) + 'px';
      $('addagent-text-copy').focus();
    } catch (e) { showAddAgentErr(e.message); }
  };
  const closeAddAgent = () => $('addagent-modal').classList.add('hidden');
  $('addagent-close').onclick = closeAddAgent;
  $('addagent-modal').onclick = (ev) => { if (ev.target === $('addagent-modal')) closeAddAgent(); };
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !$('addagent-modal').classList.contains('hidden')) closeAddAgent();
  });
  $('addagent-text-copy').onclick = async () => flashCopy($('addagent-text-copy'), await copyText($('addagent-text').value), 'Copy instructions', 'Copied');

  // the invite modal points at Add an agent instead of duplicating it
  $('invite-open-addagent').onclick = () => { closeInviteModal(); openAddAgent(); };

  $('new-channel').onclick = async () => {
    const name = prompt('Channel name (lowercase, a-z 0-9 - _):');
    if (!name) return;
    const priv = confirm('Make this channel private? Private channels are invite-only and hidden from browse.');
    try { await api('/api/v1/channels', { method: 'POST', body: { name: name.trim(), private: priv } }); }
    catch (e) { alert(e.message); }
  };

  // Add a participant to a private channel (any member can). The server resolves
  // the name or id and emits channel.member_joined so the new member sees it.
  const addPeople = async (ch) => {
    const who = prompt('Add who? (participant name or id)');
    if (!who) return;
    try {
      await api('/api/v1/channels/' + ch.id + '/members', { method: 'POST', body: { participant: who.trim() } });
    } catch (e) { alert(e.message); }
  };

  // One-way by design: server rejects private -> public, so no undo path here.
  const makePublic = async (ch) => {
    try {
      await api('/api/v1/channels/' + ch.id, { method: 'PATCH', body: { private: false } });
      await refreshRoom();
      if (current && current.id === ch.id) {
        current = channels.find((c) => c.id === ch.id) || current;
        setChannelTitle(current);
      }
    } catch (e) { alert('Make public failed: ' + e.message); }
  };
  const makePrivate = async (ch) => {
    if (!confirm('Make #' + ch.name + ' private? Current members stay, it leaves browse, and joining becomes invite-only. This cannot be undone.')) return;
    try {
      await api('/api/v1/channels/' + ch.id, { method: 'PATCH', body: { private: true } });
      await refreshRoom(); // lock icon in the sidebar right away
      if (current && current.id === ch.id) {
        current = channels.find((c) => c.id === ch.id) || current;
        setChannelTitle(current);
      }
    } catch (e) { alert('Make private failed: ' + e.message); }
  };

  // Browse view: the whole public channel map. Channels you are already in are
  // grayed with a note instead of a Join button, so the list stays a complete
  // picture of the workspace instead of only the gaps.
  const renderBrowse = (list) => {
    const box = $('browse-list');
    box.innerHTML = '';
    if (list.length === 0) {
      box.innerHTML = '<p class="browse-empty">No public channels to browse.</p>';
      return;
    }
    list.forEach((ch) => {
      const row = document.createElement('div');
      row.className = 'browse-row' + (ch.member ? ' member' : '');
      const n = ch.member_count || 0;
      row.innerHTML = `<div class="browse-meta">
          <span class="browse-name">${ch.private ? ICON.lock : ICON.hash}${esc(ch.name)}</span>
          <span class="browse-topic">${esc(ch.topic || '')}</span>
          <span class="browse-count">${n} member${n === 1 ? '' : 's'}</span>
        </div>`;
      // note and button share one fixed-width column so rows never jog
      const action = document.createElement('span');
      action.className = 'browse-action';
      row.appendChild(action);
      if (ch.member) {
        const note = document.createElement('span');
        note.className = 'browse-member-note';
        note.textContent = 'already a member';
        action.appendChild(note);
        box.appendChild(row);
        return;
      }
      const join = document.createElement('button');
      join.type = 'button';
      join.className = 'browse-join btn-sm';
      join.textContent = 'Join';
      join.onclick = async () => {
        join.disabled = true;
        try {
          await api('/api/v1/channels/' + ch.id + '/join', { method: 'POST' });
          await refreshRoom();
          const joined = channels.find((c) => c.id === ch.id);
          if (joined) await selectChannel(joined);
          await openBrowse(); // refresh: the joined channel flips to a member row
        } catch (e) { alert(e.message); join.disabled = false; }
      };
      action.appendChild(join);
      box.appendChild(row);
    });
  };

  const openBrowse = async () => {
    try {
      const out = await api('/api/v1/channels/browse');
      renderBrowse(out.channels || []);
      $('browse-modal').classList.remove('hidden');
    } catch (e) { alert(e.message); }
  };
  const closeBrowse = () => $('browse-modal').classList.add('hidden');

  $('browse-channels').onclick = openBrowse;
  $('browse-close').onclick = closeBrowse;
  $('browse-modal').onclick = (ev) => { if (ev.target.id === 'browse-modal') closeBrowse(); };

  // Leave the current channel via its row context menu. #general is pinned:
  // the server rejects leaving it, so we never offer the action there.
  const leaveChannel = async (ch) => {
    try {
      await api('/api/v1/channels/' + ch.id + '/leave', { method: 'POST' });
      await refreshRoom();
      if (current && current.id === ch.id) {
        await selectChannel(channels.find((c) => c.name === 'general') || channels[0]);
      }
    } catch (e) { alert(e.message); }
  };

  const showPendingAttachment = (which, file) => {
    const pend = attachEls(which).pend;
    if (pendingPreviewURL[which]) URL.revokeObjectURL(pendingPreviewURL[which]);
    pendingPreviewURL[which] = null;
    pend.replaceChildren();
    if (file.type.startsWith('image/')) {
      const im = document.createElement('img');
      im.className = 'pending-thumb';
      pendingPreviewURL[which] = URL.createObjectURL(file);
      im.src = pendingPreviewURL[which];
      pend.appendChild(im);
    }
    pend.insertAdjacentHTML('beforeend', ICON.paperclip + ' ');
    pend.appendChild(document.createTextNode(pendingAtt[which].filename));
    const clear = document.createElement('button');
    clear.type = 'button';
    clear.className = 'pending-clear';
    clear.innerHTML = ICON.x;
    clear.onclick = () => clearPendingAttachment(which);
    pend.appendChild(clear);
    pend.classList.remove('hidden');
  };

  const uploadPending = async (which, file) => {
    clearPendingAttachment(which);
    const seq = pendingAttSeq[which];
    const workspace = slug;
    const channelID = current?.id || null;
    const threadRootID = which === 'thread' ? openThreadRoot : null;
    const stillHere = () => pendingAttSeq[which] === seq && slug === workspace
      && current?.id === channelID && (which === 'main' || openThreadRoot === threadRootID);
    const fd = new FormData();
    fd.append('file', file);
    try {
      const uploaded = await api('/api/v1/attachments', { method: 'POST', body: fd, ws: workspace });
      if (!stillHere()) return;
      pendingAtt[which] = uploaded;
      showPendingAttachment(which, file);
    } catch (e) { if (stillHere()) alert(e.message); }
  };

  for (const which of ['main', 'thread']) {
    const input = attachEls(which).input;
    input.addEventListener('change', async () => {
      const file = input.files[0];
      if (!file) return;
      await uploadPending(which, file);
      input.value = '';
    });
  }

  window.addEventListener('focus', () => {
    // returning to the tab counts as reading what is on screen now
    if (me && current) markRead(current);
    if (me) warmAll();
  });

  // ---------- create workspace (onboarding at /create) ----------

  if (isCreatePage) {
    // the creator lands in the workspace as its admin; no join needed
    if (!sessionToken()) { location.replace(loginURL('/create')); return; }
    $('create-back').href = backTarget();
    $('create-view').classList.remove('hidden');
    wireSlugPreview($('create-room-name'), $('create-room-slug'));
    $('create-form').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const btn = $('create-form').querySelector('button[type=submit]');
      btn.disabled = true;
      try {
        const created = await api('/api/v1/rooms', {
          method: 'POST', body: { name: $('create-room-name').value.trim(), slug: $('create-room-slug').value.trim() },
        });
        location.href = '/w/' + encodeURIComponent(created.room.slug);
      } catch (e) {
        btn.disabled = false;
        $('create-error').textContent = e.message;
        $('create-error').classList.remove('hidden');
      }
    });
    return;
  }

  // the invite link page: peek, then enter the workspace the link opens and
  // land on it; without a session, sign in first (the form names the workspace)
  const showJoin = (title, msg, home) => {
    document.title = 'OpenChatter';
    $('join-title').textContent = title;
    $('join-msg').textContent = msg || '';
    $('join-home').classList.toggle('hidden', !home);
    $('join-view').classList.remove('hidden');
  };
  const joinPage = async () => {
    let peek;
    try { peek = await api('/api/v1/invites/peek?token=' + encodeURIComponent(joinToken)); }
    catch (e) { showJoin('This invite link no longer works', e.status === 404 ? 'Ask whoever invited you for a new one.' : e.message, true); return; }
    $('join-room-icon').replaceChildren(wsAvatarEl(peek, 'ws-avatar-md'));
    if (peek.status !== 'active') {
      showJoin('This invite link no longer works', inviteErrorText({ code: 'invite_' + peek.status }) + ' Ask whoever invited you for a new one.', true);
      return;
    }
    if (!sessionToken()) { location.replace(loginURL()); return; }
    showJoin('Joining “' + peek.name + '”…');
    try {
      await authApi('/api/v1/workspaces/' + encodeURIComponent(peek.slug) + '/enter', { method: 'POST', body: { invite: joinToken } });
      location.replace('/w/' + encodeURIComponent(peek.slug));
    } catch (e) {
      if (e.status === 401) { location.replace(loginURL()); return; }
      if (e.code === 'workspace_forbidden') { showJoin('You were removed from “' + peek.name + '”', 'An admin has to let you back in.', true); return; }
      showJoin('Could not join “' + peek.name + '”', noWorkspaceError(e), true);
    }
  };

  // boot; the account pages belong to auth.js
  if (isAccountPage) return;
  if (isJoinPage) { joinPage(); return; }
  (async () => {
    if (!slug) { document.body.textContent = 'Missing room link.'; return; }
    // a per-slug act_ token from before accounts existed is dead since 000027;
    // scrub it so nothing ever reads it again
    localStorage.removeItem(storeKey);
    if (!sessionToken()) { location.replace(loginURL()); return; }
    for (;;) {
      try { await enterChat(); return; }
      catch (e) {
        // the join page and the removed card show under the splash unless it lifts
        if (routeAuthError(e)) { document.body.classList.remove('booting'); return; }
        if (e.status === 404) { document.body.classList.remove('booting'); showEnter(); return; }
        console.error('boot', e);
        await new Promise((r) => setTimeout(r, 3000));
      }
    }
  })();
})();
