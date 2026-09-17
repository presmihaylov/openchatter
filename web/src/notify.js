// The one place the page tells a person that something failed: toast() for one
// action, banner() for a state that stays (session, feed, offline), inlineError()
// for a form field, guard() as a region's render boundary, busy() for one click at a time.
import { classify, KIND, textOf } from './errors.js';

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

// A cancelled call has nothing to say; an expired Access session already has
// its own bar (accessExpiredBanner), so a toast per failed call would only pile up.
const silent = (e) => e.kind === KIND.cancelled || e.kind === KIND.access_expired;

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

// failToast shows a thrown value as an error toast; `prefix` names the action,
// `retry` adds a Retry button when the same call can pass later. The raw error
// still goes to the console: the toast is for the person, the log for whoever debugs it.
export const failToast = (err, opts = {}) => {
  const e = classify(err);
  if (silent(e)) return null;
  console.error(opts.prefix || 'request failed', err);
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

const ACCESS_BANNER = 'access';

// accessExpiredBanner: Cloudflare Access wants a fresh login, and only a full
// page load reaches its login page, so the bar offers exactly that. Stays up
// until the reload; every later request would only hit the same redirect.
export const accessExpiredBanner = () => banner(ACCESS_BANNER, textOf(KIND.access_expired), {
  kind: 'error',
  action: { label: 'Reload', run: () => location.reload() },
});

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

// guard(region, label, fn) runs one render step. A throw, sync or async, is
// logged and replaced by a fallback card inside that region only; the rest of
// the page keeps working. Returns fn's value, or undefined when it failed.
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
    if (silent(e)) return;
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
