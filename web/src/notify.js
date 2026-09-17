// notify.js: the one place the page tells a person that something failed.
//
// Four surfaces, picked by how long the problem lasts and where the person is
// looking: toast() for a transient failure of one action (with Retry when the
// same call can succeed later), banner() for a state that stays until it is
// fixed (session gone, feed down, offline), inlineError() for a form field,
// and guard() as the render boundary of one region, so a bad message or
// channel shows a fallback card in that region instead of blanking the room.
// busy() keeps a mutation button to one request at a time.
import { classify, KIND } from './errors.js';

const byId = (id) => document.getElementById(id);
const resolve = (target) => (typeof target === 'string' ? byId(target) : target);

const make = (tag, className, text) => {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
};

const button = (label, className, run) => {
  const b = make('button', className, label);
  b.type = 'button';
  b.addEventListener('click', (ev) => { ev.stopPropagation(); run(); });
  return b;
};

// ---------- toasts ----------

const MAX_TOASTS = 3;
const TOAST_TTL = { info: 2600, error: 7000 };

const toastHost = () => {
  let host = byId('toasts');
  if (host) return host;
  host = make('div');
  host.id = 'toasts';
  host.setAttribute('role', 'status');
  host.setAttribute('aria-live', 'polite');
  document.body.appendChild(host);
  return host;
};

const liveToasts = new Map(); // text -> handle, so a repeat updates instead of stacking

// toast(text, {kind: 'info'|'error', action: {label, run}, ttl}) -> {el, dismiss}
export const toast = (text, opts = {}) => {
  const kind = opts.kind === 'error' ? 'error' : 'info';
  const ttl = opts.ttl === undefined ? TOAST_TTL[kind] : opts.ttl;
  const same = liveToasts.get(text);
  if (same) { same.bump(ttl); return same; }
  const host = toastHost();
  const el = make('div', 'toast' + (kind === 'error' ? ' err' : ''));
  el.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  el.appendChild(make('span', 'toast-text', text));
  let timer = null;
  const handle = {
    el,
    dismiss: () => {
      clearTimeout(timer);
      if (liveToasts.get(text) === handle) liveToasts.delete(text);
      el.remove();
    },
    bump: (ms) => {
      clearTimeout(timer);
      if (ms > 0) timer = setTimeout(handle.dismiss, ms);
    },
  };
  if (opts.action && typeof opts.action.run === 'function') {
    el.appendChild(button(opts.action.label || 'Retry', 'toast-action', () => { handle.dismiss(); opts.action.run(); }));
  }
  if (kind === 'error') el.appendChild(button('×', 'toast-close', handle.dismiss));
  liveToasts.set(text, handle);
  host.appendChild(el);
  while (host.children.length > MAX_TOASTS) {
    const oldest = host.firstElementChild;
    for (const h of liveToasts.values()) if (h.el === oldest) { h.dismiss(); break; }
    if (oldest.isConnected) oldest.remove();
  }
  handle.bump(ttl);
  return handle;
};

// failToast shows a thrown value as an error toast. `retry` adds a Retry
// button when the failure is the kind that can pass on a second try; `prefix`
// names the action ("Could not join") so the sentence says what failed.
export const failToast = (err, opts = {}) => {
  const e = classify(err);
  if (e.kind === KIND.cancelled) return null;
  const text = opts.prefix ? `${opts.prefix}: ${e.message}` : e.message;
  const action = opts.retry && (e.retryable || opts.alwaysRetry) ? { label: 'Retry', run: opts.retry } : null;
  return toast(text, { kind: 'error', action, ttl: opts.ttl });
};

// ---------- banners ----------

const bannerHost = () => {
  let host = byId('status-banners');
  if (host) return host;
  host = make('div');
  host.id = 'status-banners';
  document.body.appendChild(host);
  return host;
};

// banner(id, text, {kind: 'warn'|'error'|'info', action: {label, run}}) keeps
// one bar per id: a second call with the same id updates it in place.
export const banner = (id, text, opts = {}) => {
  const host = bannerHost();
  let el = host.querySelector(`[data-banner="${id}"]`);
  if (!el) {
    el = make('div', 'status-banner');
    el.dataset.banner = id;
    el.setAttribute('role', opts.kind === 'error' ? 'alert' : 'status');
    host.appendChild(el);
  }
  el.className = 'status-banner ' + (opts.kind || 'warn');
  el.replaceChildren(make('span', 'status-banner-text', text));
  if (opts.action && typeof opts.action.run === 'function') {
    el.appendChild(button(opts.action.label, 'status-banner-action', opts.action.run));
  }
  document.body.classList.add('has-status-banner');
  return el;
};

export const clearBanner = (id) => {
  const host = byId('status-banners');
  if (!host) return;
  const el = host.querySelector(`[data-banner="${id}"]`);
  if (el) el.remove();
  if (!host.children.length) document.body.classList.remove('has-status-banner');
};

export const hasBanner = (id) => {
  const host = byId('status-banners');
  return !!(host && host.querySelector(`[data-banner="${id}"]`));
};

// ---------- inline ----------

// inlineError(target, errOrText) fills a form's error slot; the slot is any
// element with the .hidden convention the pages already use.
export const inlineError = (target, err) => {
  const el = resolve(target);
  if (!el) return;
  el.textContent = typeof err === 'string' ? err : classify(err).message;
  el.classList.remove('hidden');
  el.setAttribute('role', 'alert');
};

export const clearInline = (target) => {
  const el = resolve(target);
  if (!el) return;
  el.textContent = '';
  el.classList.add('hidden');
};

// ---------- boundary ----------

const fallbackCard = (label) => {
  const card = make('div', 'region-error');
  card.setAttribute('role', 'alert');
  card.appendChild(make('p', 'region-error-text', `Something went wrong while showing the ${label}.`));
  const row = make('div', 'region-error-actions');
  row.appendChild(button('Reload', 'btn-primary', () => location.reload()));
  card.appendChild(row);
  return card;
};

// guard(region, label, fn) runs one render step for a region. A throw, sync
// or async, is logged and replaced by a fallback card inside that region
// only; the rest of the page keeps working. Returns fn's value, or undefined
// when it failed.
export const guard = (region, label, fn) => {
  const fail = (e) => {
    console.error('render ' + label, e);
    const box = resolve(region);
    if (box) box.replaceChildren(fallbackCard(label));
    return undefined;
  };
  try {
    const out = fn();
    if (out && typeof out.then === 'function') return out.then((v) => v, fail);
    return out;
  } catch (e) {
    return fail(e);
  }
};

// ---------- busy ----------

// busy(btn, fn) runs fn once per click burst: a second click while the first
// is in flight is dropped, and the button greys out until it settles.
export const busy = async (btn, fn) => {
  if (!btn) return fn();
  if (btn.dataset.busy) return undefined;
  btn.dataset.busy = '1';
  const wasDisabled = btn.disabled;
  btn.disabled = true;
  try {
    return await fn();
  } finally {
    delete btn.dataset.busy;
    btn.disabled = wasDisabled;
  }
};

// ---------- page-wide ----------

const OFFLINE_BANNER = 'offline';

// installGlobalHandlers turns an uncaught throw or a forgotten rejection into
// a toast plus a console line, and follows the browser's online/offline
// signal with a banner. Idempotent.
let installed = false;
export const installGlobalHandlers = (win = typeof window === 'undefined' ? null : window) => {
  if (installed || !win) return;
  installed = true;
  win.addEventListener('error', (ev) => {
    console.error('uncaught', ev.error || ev.message);
    toast('Something went wrong. Reload the page if it keeps happening.', { kind: 'error' });
  });
  win.addEventListener('unhandledrejection', (ev) => {
    const e = classify(ev.reason);
    if (e.kind === KIND.cancelled) return;
    console.error('unhandled', ev.reason);
    toast(e.message, { kind: 'error' });
  });
  win.addEventListener('offline', () => {
    banner(OFFLINE_BANNER, 'You are offline. Nothing will send until the connection is back.', { kind: 'warn' });
  });
  win.addEventListener('online', () => clearBanner(OFFLINE_BANNER));
  if (win.navigator && win.navigator.onLine === false) {
    banner(OFFLINE_BANNER, 'You are offline. Nothing will send until the connection is back.', { kind: 'warn' });
  }
};

// resetForTest forgets module state between unit tests.
export const resetForTest = () => {
  liveToasts.clear();
  installed = false;
};
