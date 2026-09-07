import { ICON } from './icons.js';
import { wsAvatarEl } from './wsavatar.js';
import { isFleetRoom } from './fleet.js';
/* Account pages (/login, /register, /settings) and the password banner.
   /settings is the one settings place: a Workspace tab and a Personal tab.
   The session token is a human's only browser identity; agents keep act_ tokens. */

const SESSION_KEY = 'agentchat:session';
const $ = (id) => document.getElementById(id);
const path = location.pathname.replace(/\/+$/, '') || '/';

export const sessionToken = () => {
  try { return localStorage.getItem(SESSION_KEY) || null; } catch (e) { return null; }
};
const setSession = (tok) => localStorage.setItem(SESSION_KEY, tok);
export const clearSession = () => localStorage.removeItem(SESSION_KEY);

// login and register send the session alone; /settings (wsApi) and room pages (app.js) add X-Workspace-Slug
export const isAccountPage = ['/login', '/register', '/settings', '/create'].includes(path);

// ?next= may only point back into this origin. The string is resolved the
// way the browser will resolve it (the URL parser drops tab/CR/LF, so
// "/\t/evil" is "//evil") and the origin compared; a next that points at
// the login or register page itself would only loop
export const safeNext = (raw, origin = location.origin) => {
  if (typeof raw !== 'string' || raw[0] !== '/') return '/';
  let u;
  try { u = new URL(raw, origin); } catch (e) { return '/'; }
  if (u.origin !== origin) return '/';
  const target = u.pathname.replace(/\/+$/, '') || '/';
  if (target === '/login' || target === '/register') return '/';
  return u.pathname + u.search + u.hash;
};
const rawNext = () => new URLSearchParams(location.search).get('next');
const nextParam = () => safeNext(rawNext());
// same-origin referrer that is not itself an account page, else nothing
const referrerPath = () => {
  let u;
  try { u = new URL(document.referrer); } catch (e) { return null; }
  if (u.origin !== location.origin) return null;
  const target = u.pathname.replace(/\/+$/, '') || '/';
  if (['/login', '/register', '/settings', '/create', '/'].includes(target)) return null;
  return u.pathname + u.search + u.hash;
};
// where the Back link (and Continue after a password change) goes: ?next=
// when given, else the room the user came from, else the signed-in landing
// ("/": the last active workspace, or #no-ws-view)
export const backTarget = () => {
  const raw = rawNext();
  if (raw !== null) return safeNext(raw);
  return referrerPath() || '/';
};
export const loginURL = (next) => '/login?next=' + encodeURIComponent(next || (location.pathname + location.search));
// the sign-in / create-account cross links carry ?next= along
const keepNext = (id) => { $(id).href += location.search; };

// a dead session: forget it, and on the account pages go back to the login form
export const onSessionInvalid = () => {
  clearSession();
  if (path === '/login') return;
  if (isAccountPage) location.replace(loginURL());
};

const request = async (apiPath, opts = {}, extra = {}) => {
  const headers = Object.assign({}, extra);
  const multipart = opts.body instanceof FormData;
  if (!multipart) headers['Content-Type'] = 'application/json';
  const tok = sessionToken();
  if (tok) headers['Authorization'] = 'Bearer ' + tok;
  const resp = await fetch(apiPath, {
    method: opts.method || 'GET',
    headers,
    body: multipart ? opts.body : (opts.body ? JSON.stringify(opts.body) : undefined),
  });
  let data = null;
  try { data = await resp.json(); } catch (e) { /* 204 */ }
  if (resp.ok) return data;
  const err = new Error((data && data.error) || ('HTTP ' + resp.status));
  err.status = resp.status;
  err.code = data && data.code;
  if (resp.status === 401 && err.code === 'session_invalid') onSessionInvalid();
  throw err;
};
export const authApi = (apiPath, opts) => request(apiPath, opts);
// the session names a workspace through X-Workspace-Slug: how /settings reaches
// the participant-scoped endpoints (room, me, avatar, notifications)
const wsApi = (slug, apiPath, opts) => request(apiPath, opts, { 'X-Workspace-Slug': slug });

// the account pages get the app bar; the username and sign out wait for a valid session
const showHeader = (user) => {
  if (!isAccountPage) return;
  $('app-header').classList.remove('hidden');
  document.body.classList.add('has-header');
  $('app-user').classList.toggle('hidden', !user);
  if (!user) return;
  $('app-username').textContent = user.username;
  $('app-signout').onclick = signOut;
};

export const signOut = async () => {
  try { await authApi('/api/v1/auth/logout', { method: 'POST' }); } catch (e) { /* already gone */ }
  clearSession();
  location.href = '/login';
};

// fetchWorkspaces is the switcher's one call per boot: {user, workspaces,
// last_active_workspace_id}; the server only names a workspace the user is
// still a live member of
export const fetchWorkspaces = () => authApi('/api/v1/user');

// the slug out of a pasted workspace link (/r/<slug> or /w/<slug>), or the
// bare slug the user typed
// slugify mirrors pkg/slug on the server: ASCII-folded, lowercase, hyphens
export const slugify = (name) => String(name || '').normalize('NFD').replace(/[\u0300-\u036f]/g, '').toLowerCase()
  .replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60).replace(/-+$/, '');

// wireSlugPreview keeps the slug input following the name until the user
// edits the slug by hand; clearing it hands control back to the name
export const wireSlugPreview = (nameEl, slugEl) => {
  let manual = false;
  nameEl.addEventListener('input', () => { if (!manual) slugEl.value = slugify(nameEl.value); });
  slugEl.addEventListener('input', () => { manual = slugEl.value !== ''; });
};

export const slugFromLink = (raw) => {
  const text = String(raw || '').trim();
  if (!text) return '';
  if (!/[/\s]/.test(text)) return text;
  let u;
  try { u = new URL(text, location.origin); } catch (e) { return ''; }
  const segs = u.pathname.split('/').filter(Boolean);
  if (segs.length < 2 || !['r', 'w'].includes(segs[0])) return '';
  try { return decodeURIComponent(segs[1]); } catch (e) { return ''; }
};

// inviteTokenFrom pulls the token out of a pasted invite link, or accepts the
// bare token; anything else is ''
export const inviteTokenFrom = (raw) => {
  let text = String(raw || '').trim();
  const i = text.lastIndexOf('/join/');
  if (i >= 0) text = text.slice(i + 6);
  text = text.replace(/[?#].*$/, '').replace(/^\/+|\/+$/g, '');
  return /^inv-[a-z0-9-]+$/i.test(text) ? text : '';
};

export const inviteErrorText = (e) => {
  if (e.code === 'invite_invalid') return 'That invite link does not open this workspace.';
  if (e.code === 'invite_expired') return 'This invite link has expired.';
  if (e.code === 'invite_revoked') return 'This invite link was revoked.';
  if (e.code === 'invite_agents_only') return 'This invite link is for an agent, not a person. Ask for a workspace link.';
  return null;
};

export const noWorkspaceError = (e) => {
  if (e.code === 'workspace_quota') return 'You already own the maximum number of workspaces.';
  const inv = inviteErrorText(e);
  if (inv) return inv;
  if (e.status === 404) return 'This link does not point to a workspace.';
  return e.message;
};

// #no-ws-view: the signed-in landing for a user with no live membership
const showNoWorkspace = () => {
  document.querySelectorAll('.auth-view').forEach((el) => el.classList.add('hidden'));
  $('no-ws-view').classList.remove('hidden');
  const wire = (formID, errID, submit) => {
    $(formID).addEventListener('submit', async (ev) => {
      ev.preventDefault();
      hideErr(errID);
      const btn = $(formID).querySelector('button[type=submit]');
      btn.disabled = true;
      try {
        await submit();
      } catch (e) {
        btn.disabled = false;
        showErr(errID, noWorkspaceError(e));
      }
    });
  };
  wireSlugPreview($('no-ws-create-name'), $('no-ws-create-slug'));
  wire('no-ws-create-form', 'no-ws-create-error', async () => {
    const out = await authApi('/api/v1/rooms', { method: 'POST', body: { name: $('no-ws-create-name').value.trim(), slug: $('no-ws-create-slug').value.trim() } });
    location.href = '/w/' + encodeURIComponent(out.room.slug);
  });
  wire('no-ws-enter-form', 'no-ws-enter-error', async () => {
    // the join page does the work: it knows the workspace from the link
    const token = inviteTokenFrom($('no-ws-enter-link').value);
    if (!token) throw new Error('Paste an invite link (…/join/inv-…).');
    location.href = '/join/' + encodeURIComponent(token);
  });
};

// landing is what "/" means for a signed-in user: the last active workspace,
// else the first one joined, else #no-ws-view. The server sends "/" to
// /login, so the login page runs this when the session is still good.
// A page that already fetched the payload passes it in, so the landing has
// no second request to lose.
export const landing = async (prefetched) => {
  let out = prefetched;
  if (!out) {
    try { out = await fetchWorkspaces(); } catch (e) {
      if (e.status !== 401) console.error('landing', e);
      // a dead session on the login page: the form comes back
      if (path === '/login') $('login-view').classList.remove('hidden');
      return;
    }
  }
  const ws = out.workspaces || [];
  const target = ws.find((w) => w.id === out.last_active_workspace_id) || ws[0];
  if (target) { location.replace('/w/' + encodeURIComponent(target.slug)); return; }
  if (location.pathname !== '/') history.replaceState(null, '', '/');
  showNoWorkspace();
};

// go follows a resolved ?next=: "/" is the landing, everything else a page
const go = (next, prefetched) => {
  if (next === '/') return landing(prefetched);
  location.href = next;
  return null;
};

const setBanner = (id, on) => {
  $(id).classList.toggle('hidden', !on);
  document.body.classList.toggle('has-banner', !$('pw-banner').classList.contains('hidden'));
};

const showErr = (id, msg) => { $(id).textContent = msg; $(id).classList.remove('hidden'); };
const hideErr = (id) => $(id).classList.add('hidden');

const loginErrorText = (e) => {
  if (e.status === 429) return e.message + ' (try again in a minute)';
  return e.message;
};

const providers = () => authApi('/api/v1/auth/providers').catch(() => ({ providers: ['password'], registration_enabled: true }));

// the pw-banner rides on every page that holds a session; it also refreshes
// after a successful change (must_change_password flips to false)
// lastUserPayload is the whole /api/v1/user answer of the latest refresh, so
// a page that lands right after it can reuse the workspace list
let lastUserPayload = null;
const refreshUser = async () => {
  if (!sessionToken()) { setBanner('pw-banner', false); return null; }
  try {
    const out = await authApi('/api/v1/user');
    lastUserPayload = out;
    setBanner('pw-banner', !!(out.user && out.user.must_change_password));
    // the settings link brings the user back here (a room page) afterwards
    $('pw-banner').querySelector('a').href = '/settings?next=' + encodeURIComponent(isAccountPage ? backTarget() : location.pathname + location.search);
    showHeader(out.user);
    return out.user;
  } catch (e) {
    setBanner('pw-banner', false);
    return null;
  }
};

const loginPage = async () => {
  $('login-view').classList.remove('hidden');
  const provs = await providers();
  if (!provs.registration_enabled) $('login-register-hint').classList.add('hidden');
  keepNext('login-register-link');
  inviteHint('login-invite');
  const list = provs.providers || [];
  if (!list.includes('password')) $('login-form').querySelectorAll('label, button[type=submit]').forEach((el) => el.classList.add('hidden'));
  // an extra provider (Clerk) gets its redirect flow with its own task; the
  // button is the placeholder the design names
  for (const name of list.filter((n) => n !== 'password')) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'secondary';
    b.disabled = true;
    b.textContent = 'Sign in with ' + name;
    $('login-providers').appendChild(b);
  }
  // the handler goes on first: whenever the form is (re)shown it must submit
  // through the API, never natively with the credentials in the URL
  $('login-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    hideErr('login-error');
    const btn = $('login-form').querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const out = await authApi('/api/v1/auth/password/login', {
        method: 'POST',
        body: { username: $('login-username').value.trim().toLowerCase(), password: $('login-password').value },
      });
      setSession(out.token);
      $('login-view').classList.add('hidden');
      await go(nextParam());
    } catch (e) {
      showErr('login-error', loginErrorText(e));
    }
    // a landing that failed brought the form back; it must be usable again
    btn.disabled = false;
  });
  // an already valid session skips the form
  if (sessionToken() && await refreshUser()) { $('login-view').classList.add('hidden'); await go(nextParam(), lastUserPayload); }
};

// a ?next= into /join/<token> means the visitor followed an invite link:
// say which workspace it opens, so the form makes sense
const inviteHint = async (id) => {
  const m = /^\/join\/([^/?#]+)/.exec(nextParam());
  if (!m) return;
  try {
    const peek = await authApi('/api/v1/invites/peek?token=' + encodeURIComponent(m[1]));
    if (peek.status !== 'active') return;
    $(id).textContent = 'You were invited to “' + peek.name + '”. ' + (id === 'login-invite' ? 'Sign in to join.' : 'Create an account to join.');
    $(id).classList.remove('hidden');
  } catch (e) { /* the join page says why */ }
};

const registerPage = async () => {
  // an already valid session skips the form
  if (sessionToken() && await refreshUser()) { await go(nextParam(), lastUserPayload); return; }
  const provs = await providers();
  if (!provs.registration_enabled || !(provs.providers || []).includes('password')) {
    $('register-closed').classList.remove('hidden');
    return;
  }
  $('register-view').classList.remove('hidden');
  keepNext('register-login-link');
  inviteHint('register-invite');
  $('register-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    hideErr('register-error');
    const btn = $('register-form').querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const out = await authApi('/api/v1/auth/password/register', {
        method: 'POST',
        body: {
          username: $('register-username').value.trim().toLowerCase(),
          password: $('register-password').value,
          display_name: $('register-display').value.trim(),
        },
      });
      setSession(out.token);
      $('register-view').classList.add('hidden');
      await go(nextParam());
    } catch (e) {
      btn.disabled = false;
      showErr('register-error', e.message);
    }
  });
};

// the workspace /settings talks about: the room in ?next= (the page the user
// came from), else the last active one
const settingsSlug = () => {
  const fromNext = slugFromLink(backTarget());
  if (fromNext) return fromNext;
  const out = lastUserPayload || {};
  const ws = (out.workspaces || []).find((w) => w.id === out.last_active_workspace_id) || (out.workspaces || [])[0];
  return ws ? ws.slug : '';
};

const flash = (btn, text, done = 'Copied') => {
  const was = text || btn.textContent;
  btn.textContent = done;
  setTimeout(() => { btn.textContent = was; }, 1200);
};
const copyText = async (btn, text) => {
  try { await navigator.clipboard.writeText(text); flash(btn, null, 'Copied'); } catch (e) { flash(btn, null, 'Copy failed'); }
};

// The URL only changes on a click: the first paint keeps whatever query the
// login redirect carried here (login-check asserts it survives verbatim).
const showTab = (name, remember = true) => {
  for (const b of document.querySelectorAll('#settings-nav [role=tab]')) {
    const on = b.dataset.tab === name;
    b.setAttribute('aria-selected', on ? 'true' : 'false');
    b.classList.toggle('active', on);
  }
  $('settings-workspace').classList.toggle('hidden', name !== 'workspace');
  $('settings-personal').classList.toggle('hidden', name !== 'personal');
  if (!remember) return;
  const u = new URL(location.href);
  u.searchParams.set('tab', name);
  history.replaceState(null, '', u.pathname + u.search);
};

// Workspace tab: name, link, members. Admins edit; members read. Invite
// links live in the workspace menu, not here.
const workspaceTab = async (slug) => {
  let out;
  try { out = await wsApi(slug, '/api/v1/room'); } catch (e) {
    // a slug we cannot open (removed from the workspace, or the server is down): say why
    $('ws-none').textContent = 'Cannot open the settings of ' + slug + ': ' + e.message;
    $('ws-none').classList.remove('hidden');
    return null;
  }
  const admin = !!out.admin;
  $('ws-panel').classList.remove('hidden');
  let room = out.room;
  const wsHeaders = { 'Authorization': 'Bearer ' + sessionToken(), 'X-Workspace-Slug': slug };
  const paintWsAvatar = () => {
    $('ws-avatar-slot').replaceChildren(wsAvatarEl(room, 'ws-avatar-lg', wsHeaders));
    $('ws-avatar-remove').classList.toggle('hidden', !room.avatar_attachment_id);
  };
  paintWsAvatar();
  $('ws-avatar-actions').classList.toggle('hidden', !admin);
  $('ws-avatar-input').addEventListener('change', async () => {
    const file = $('ws-avatar-input').files[0];
    if (!file) return;
    const fd = new FormData();
    fd.append('file', file);
    try { room = await wsApi(slug, '/api/v1/room/avatar', { method: 'POST', body: fd }); paintWsAvatar(); } catch (e) { alert(e.message); }
    $('ws-avatar-input').value = '';
  });
  $('ws-avatar-remove').onclick = async () => {
    try { room = await wsApi(slug, '/api/v1/room/avatar', { method: 'DELETE' }); paintWsAvatar(); } catch (e) { alert(e.message); }
  };
  $('ws-name').value = out.room.name;
  $('ws-name').disabled = !admin;
  $('ws-name-save').classList.toggle('hidden', !admin);
  $('ws-slug').value = location.origin + '/w/' + out.room.slug;
  $('ws-slug-copy').onclick = () => copyText($('ws-slug-copy'), $('ws-slug').value);
  $('ws-mcp').value = location.origin + '/api/v1/w/' + out.room.slug + '/mcp';
  $('ws-mcp-copy').onclick = () => copyText($('ws-mcp-copy'), $('ws-mcp').value);
  $('ws-name-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    hideErr('ws-name-error');
    $('ws-name-ok').classList.add('hidden');
    const name = $('ws-name').value.trim();
    if (!name) { showErr('ws-name-error', 'the name cannot be empty'); return; }
    $('ws-name-save').disabled = true;
    try {
      room = await wsApi(slug, '/api/v1/room', { method: 'PATCH', body: { name } });
      $('ws-name').value = room.name;
      $('avatar-ws-name').textContent = room.name;
      $('ws-name-ok').classList.remove('hidden');
    } catch (e) { showErr('ws-name-error', e.message); }
    $('ws-name-save').disabled = false;
  });
  if (!admin) return out;
  membersSection(slug, () => room);
  dangerZone(slug, () => room);
  return out;
};

const timeAgo = (iso) => {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  return Math.floor(s / 86400) + ' d ago';
};

// Members (task 19): humans only, each with its agents folded under it. An
// admin can rebind an agent to another human, remove one agent, or remove a
// human together with every agent they own. The creator and the last admin
// are never removable: the server says so with a 409, shown as-is.
const membersSection = async (slug, getRoom) => {
  $('ws-members').classList.remove('hidden');
  const ul = $('ws-member-list');
  let me;
  try { me = await wsApi(slug, '/api/v1/me'); } catch (e) { showErr('ws-members-error', 'Cannot load the members: ' + e.message); return; }
  const isCreator = (p) => !!p.user_id && p.user_id === getRoom().created_by_user_id;
  const presence = (p) => p.online ? 'online' : 'seen ' + timeAgo(p.last_seen_at);
  const open = new Set(); // human ids whose agent list is unfolded, kept across re-renders
  const remove = async (p, btn, ask) => {
    hideErr('ws-members-error');
    if (!confirm(ask)) return;
    btn.disabled = true;
    try {
      await wsApi(slug, '/api/v1/participants/' + encodeURIComponent(p.id), { method: 'DELETE' });
      await render();
    } catch (e) {
      showErr('ws-members-error', e.message);
      btn.disabled = false;
    }
  };
  // one shared Remove look on every surface (Maya, 06e9f192): subtle outline,
  // muted text, the settings button font; the danger lives in the confirm
  const removeButton = () => {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'remove-btn member-remove';
    btn.textContent = 'Remove';
    return btn;
  };
  const agentRow = (a, humans) => {
    const li = document.createElement('li');
    li.className = 'member-agent';
    li.dataset.id = a.id;
    const who = document.createElement('div');
    who.className = 'member-who';
    const name = document.createElement('span');
    name.className = 'member-name';
    name.textContent = a.name;
    const sub = document.createElement('span');
    sub.className = 'member-sub';
    sub.textContent = 'agent · ' + presence(a);
    who.append(name, sub);
    const sel = document.createElement('select');
    sel.className = 'member-owner';
    sel.title = 'Owner';
    const none = document.createElement('option');
    none.value = '';
    none.textContent = a.owner_id ? '(unknown owner)' : 'no owner';
    none.disabled = true;
    sel.appendChild(none);
    for (const h of humans) {
      const o = document.createElement('option');
      o.value = h.id;
      o.textContent = h.name;
      sel.appendChild(o);
    }
    sel.value = humans.some((h) => h.id === a.owner_id) ? a.owner_id : '';
    sel.onchange = async () => {
      hideErr('ws-members-error');
      sel.disabled = true;
      try {
        await wsApi(slug, '/api/v1/participants/' + encodeURIComponent(a.id) + '/owner', { method: 'PATCH', body: { owner_id: sel.value } });
        await render();
      } catch (e) {
        showErr('ws-members-error', e.message);
        sel.disabled = false;
      }
    };
    const kind = document.createElement('span');
    kind.className = 'member-role';
    kind.textContent = 'agent';
    const btn = removeButton();
    btn.onclick = () => remove(a, btn, 'Remove ' + a.name + ' (agent, its token stops working) from "' + getRoom().name + '"?');
    li.append(who, kind, sel, btn);
    return li;
  };
  const humanRow = (p, agents, humans) => {
    const li = document.createElement('li');
    li.className = 'member-human';
    li.dataset.id = p.id;
    const head = document.createElement('div');
    head.className = 'member-head';
    const who = document.createElement('div');
    who.className = 'member-who';
    const name = document.createElement('span');
    name.className = 'member-name';
    name.textContent = p.name;
    const sub = document.createElement('span');
    sub.className = 'member-sub';
    sub.textContent = (p.username ? '@' + p.username : 'human') + ' · ' + presence(p);
    who.append(name, sub);
    const role = document.createElement('span');
    role.className = 'member-role';
    role.textContent = isCreator(p) ? 'creator' : p.role;
    head.append(who, role);
    const fold = document.createElement('button');
    fold.type = 'button';
    fold.className = 'secondary member-fold';
    const paint = () => { fold.innerHTML = (open.has(p.id) ? ICON.chevronDown : ICON.chevronRight) + ' ' + agents.length + (agents.length === 1 ? ' agent' : ' agents'); };
    paint();
    fold.disabled = agents.length === 0;
    head.appendChild(fold);
    // The creator (and my own row) gets an invisible placeholder, so the
    // Remove column keeps its width on every row.
    if (isCreator(p) || p.id === me.id) {
      const gap = document.createElement('span');
      gap.className = 'remove-gap member-remove-gap';
      gap.setAttribute('aria-hidden', 'true');
      head.appendChild(gap);
    }
    if (!isCreator(p) && p.id !== me.id) {
      const btn = removeButton();
      const tail = agents.length ? ' and ' + agents.length + (agents.length === 1 ? ' agent (' : ' agents (') + agents.map((a) => a.name).join(', ') + ')' : '';
      btn.onclick = () => remove(p, btn, 'Remove ' + p.name + tail + ' from "' + getRoom().name + '"? The agents\' tokens stop working.');
      head.appendChild(btn);
    }
    li.appendChild(head);
    const sub2 = document.createElement('ul');
    sub2.className = 'member-agents';
    sub2.classList.toggle('hidden', !open.has(p.id));
    sub2.replaceChildren(...agents.map((a) => agentRow(a, humans)));
    fold.onclick = () => {
      if (open.has(p.id)) open.delete(p.id); else open.add(p.id);
      sub2.classList.toggle('hidden', !open.has(p.id));
      paint();
    };
    li.appendChild(sub2);
    return li;
  };
  const render = async () => {
    let list;
    try { list = (await wsApi(slug, '/api/v1/participants')).participants || []; } catch (e) { showErr('ws-members-error', 'Cannot load the members: ' + e.message); return; }
    const humans = list.filter((p) => p.is_human);
    const owners = humans.filter((h) => !!h.user_id); // only a human with an account can own agents
    const agents = list.filter((p) => !p.is_human);
    const byOwner = new Map(humans.map((h) => [h.id, []]));
    const unowned = [];
    for (const a of agents) {
      if (a.owner_id && byOwner.has(a.owner_id)) byOwner.get(a.owner_id).push(a); else unowned.push(a);
    }
    const rows = humans.map((h) => humanRow(h, byOwner.get(h.id), owners));
    if (unowned.length) {
      const li = document.createElement('li');
      li.className = 'member-human member-unowned';
      const head = document.createElement('div');
      head.className = 'member-head';
      const who = document.createElement('div');
      who.className = 'member-who';
      const name = document.createElement('span');
      name.className = 'member-name';
      name.textContent = 'Unowned agents';
      const sub = document.createElement('span');
      sub.className = 'member-sub';
      sub.textContent = 'joined before owners existed; pick an owner for each';
      who.append(name, sub);
      who.classList.add('member-span');
      head.appendChild(who);
      li.appendChild(head);
      const sub2 = document.createElement('ul');
      sub2.className = 'member-agents';
      sub2.replaceChildren(...unowned.map((a) => agentRow(a, owners)));
      li.appendChild(sub2);
      rows.push(li);
    }
    ul.replaceChildren(...rows);
  };
  await render();
};

// Danger zone: only the owner sees it; the typed name arms the button, a
// confirm asks once, and the fleet room asks a second time.
const dangerZone = async (slug, getRoom) => {
  let me;
  try { me = await wsApi(slug, '/api/v1/me'); } catch (e) { return; }
  if (!me.user_id || me.user_id !== getRoom().created_by_user_id) return;
  $('ws-danger').classList.remove('hidden');
  const input = $('ws-delete-name');
  const btn = $('ws-delete');
  input.addEventListener('input', () => { btn.disabled = input.value.trim() !== getRoom().name; });
  btn.onclick = async () => {
    hideErr('ws-delete-error');
    if (!confirm('Delete "' + getRoom().name + '"? Every channel, message, upload and member goes with it. There is no undo.')) return;
    if (isFleetRoom(slug) && !confirm('This is the fleet room. Every agent in it loses its token. Delete it anyway?')) return;
    btn.disabled = true;
    try {
      await wsApi(slug, '/api/v1/room', { method: 'DELETE', body: { name: input.value.trim() } });
      location.href = '/';
    } catch (e) {
      showErr('ws-delete-error', e.message);
      btn.disabled = false;
    }
  };
};

// Personal tab, the workspace-scoped part: the participant's avatar and
// notification prefs (both live on the participant, so they are per workspace)
const personalWorkspaceBits = async (slug, roomName) => {
  let me;
  try { me = await wsApi(slug, '/api/v1/me'); } catch (e) { return; }
  $('avatar-section').classList.remove('hidden');
  $('avatar-ws-name').textContent = roomName;
  const paintAvatar = async () => {
    const slot = $('settings-avatar');
    slot.innerHTML = '';
    $('avatar-remove').classList.toggle('hidden', !me.avatar_attachment_id);
    // the seedling stands here until an upload lands, and again after Remove
    const img = document.createElement('img');
    img.className = 'avatar-lg avatar-img';
    img.alt = me.name;
    img.src = '/brand/avatar-default-512.png';
    slot.appendChild(img);
    if (!me.avatar_attachment_id) return;
    try {
      const resp = await fetch('/api/v1/attachments/' + me.avatar_attachment_id + '?size=512', { headers: { 'Authorization': 'Bearer ' + sessionToken(), 'X-Workspace-Slug': slug } });
      if (resp.ok) img.src = URL.createObjectURL(await resp.blob());
    } catch (e) { /* the alt text stands */ }
  };
  await paintAvatar();
  $('avatar-input').addEventListener('change', async () => {
    const file = $('avatar-input').files[0];
    if (!file) return;
    const fd = new FormData();
    fd.append('file', file);
    try { me = await wsApi(slug, '/api/v1/me/avatar', { method: 'POST', body: fd }); await paintAvatar(); } catch (e) { alert(e.message); }
    $('avatar-input').value = '';
  });
  $('avatar-remove').onclick = async () => {
    try { me = await wsApi(slug, '/api/v1/me/avatar', { method: 'DELETE' }); await paintAvatar(); } catch (e) { alert(e.message); }
  };

  let prefs = { enabled: true, sound: true, archive_after_secs: 3600 };
  try { prefs = await wsApi(slug, '/api/v1/me/notifications'); } catch (e) { /* defaults stand */ }
  $('notify-settings').classList.remove('hidden');
  const paintPrefs = () => {
    $('notify-enabled').checked = !!prefs.enabled;
    $('notify-sound').checked = !!prefs.sound;
    $('notify-sound').disabled = !prefs.enabled;
    $('archive-after').value = String(prefs.archive_after_secs ?? 3600);
    // an inline "Allow in browser" link while the prompt is still open; a
    // blocked browser gets a muted note; nothing once granted
    const perm = $('notify-perm');
    const state = window.Notification ? Notification.permission : 'unsupported';
    perm.classList.toggle('hidden', !prefs.enabled || state === 'granted' || state === 'unsupported');
    perm.textContent = state === 'denied' ? 'Blocked in the browser' : 'Allow in browser';
    perm.classList.toggle('muted', state === 'denied');
  };
  paintPrefs();
  $('notify-perm').onclick = async (ev) => {
    ev.preventDefault();
    if (!window.Notification || Notification.permission !== 'default') return;
    try { await Notification.requestPermission(); } catch (e) { /* treated as denied */ }
    paintPrefs();
  };
  const save = async (patch) => {
    try { prefs = await wsApi(slug, '/api/v1/me/notifications', { method: 'PATCH', body: patch }); } catch (e) { alert(e.message); }
    paintPrefs();
  };
  $('notify-enabled').onchange = async (ev) => {
    const enabled = ev.target.checked;
    // ask on the toggle, never on page load, and only when the answer is open
    if (enabled && window.Notification && Notification.permission === 'default') {
      try { await Notification.requestPermission(); } catch (e) { /* treated as denied */ }
    }
    await save({ enabled });
  };
  $('notify-sound').onchange = (ev) => save({ sound: ev.target.checked });
  $('archive-after').onchange = (ev) => save({ archive_after_secs: Number(ev.target.value) });
};

const settingsPage = async () => {
  if (!sessionToken()) { location.replace(loginURL('/settings')); return; }
  const user = await refreshUser();
  if (!user) { location.replace(loginURL('/settings')); return; }
  const provs = await providers();
  const back = backTarget();
  $('settings-username').textContent = user.username;
  $('settings-back').href = back;
  $('pw-continue').href = back;
  const hasPassword = (provs.providers || []).includes('password');
  $('pw-form').classList.toggle('hidden', !hasPassword);
  $('settings-nopw').classList.toggle('hidden', hasPassword);
  for (const b of document.querySelectorAll('#settings-nav [role=tab]')) b.onclick = () => showTab(b.dataset.tab);
  const want = new URLSearchParams(location.search).get('tab');
  showTab(want === 'workspace' ? 'workspace' : 'personal', false);
  // theme is a per-browser choice, not a participant pref: the head script owns it
  $('theme-mode').value = document.documentElement.dataset.themeMode || 'system';
  $('theme-mode').onchange = (ev) => {
    try { localStorage.setItem('agentchat:theme', ev.target.value); } catch (e) { /* storage blocked */ }
    window.__applyTheme();
  };
  $('settings-signout').onclick = signOut;
  $('settings-view').classList.remove('hidden');
  $('pw-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    hideErr('pw-error');
    $('pw-ok').classList.add('hidden');
    if ($('pw-new').value !== $('pw-confirm').value) { showErr('pw-error', 'the new passwords do not match'); return; }
    const btn = $('pw-form').querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      await authApi('/api/v1/auth/password/change', {
        method: 'POST',
        body: { current_password: $('pw-current').value, new_password: $('pw-new').value },
      });
      $('pw-form').reset();
      $('pw-ok').classList.remove('hidden');
      await refreshUser();
    } catch (e) {
      showErr('pw-error', loginErrorText(e));
    }
    btn.disabled = false;
  });
  const slug = settingsSlug();
  if (!slug) { $('ws-none').classList.remove('hidden'); return; }
  const out = await workspaceTab(slug);
  if (out) await personalWorkspaceBits(slug, out.room.name);
};

const pages = { '/login': loginPage, '/register': registerPage, '/settings': settingsPage };
showHeader(null);
if (pages[path]) {
  pages[path]().catch((e) => console.error('auth', e));
}
if (!pages[path]) {
  refreshUser();
}
