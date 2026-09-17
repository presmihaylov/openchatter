import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, KIND } from '../src/errors.js';
import {
  accessExpiredBanner, banner, busy, clearBanner, clearInline, failToast, guard, hasBanner, inlineError,
  installGlobalHandlers, resetForTest, toast,
} from '../src/notify.js';

const toasts = () => Array.from(document.querySelectorAll('#toasts .toast'));
const toastTexts = () => toasts().map((t) => t.querySelector('.toast-text').textContent);
const banners = () => Array.from(document.querySelectorAll('#status-banners .status-banner'));

beforeEach(() => {
  document.body.innerHTML = '';
  document.body.className = '';
  resetForTest();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('toast', () => {
  it('shows text, marks an error as an alert with a close button, and expires', () => {
    toast('Saved', { kind: 'info' });
    const err = toast('Boom', { kind: 'error' });
    expect(toastTexts()).toEqual(['Saved', 'Boom']);
    expect(err.el.getAttribute('role')).toBe('alert');
    expect(err.el.classList.contains('err')).toBe(true);
    expect(err.el.querySelector('.toast-close')).not.toBeNull();
    expect(toasts()[0].querySelector('.toast-close')).toBeNull();
    vi.advanceTimersByTime(2601);
    expect(toastTexts()).toEqual(['Boom']);
    vi.advanceTimersByTime(7000);
    expect(toastTexts()).toEqual([]);
  });

  it('dedupes a repeat by text and extends its life instead of stacking', () => {
    const a = toast('Same', { kind: 'error' });
    vi.advanceTimersByTime(6000);
    const b = toast('Same', { kind: 'error' });
    expect(b).toBe(a);
    expect(toasts()).toHaveLength(1);
    vi.advanceTimersByTime(6000);
    expect(toasts()).toHaveLength(1);
    vi.advanceTimersByTime(1001);
    expect(toasts()).toHaveLength(0);
  });

  it('keeps at most three, dropping the oldest', () => {
    toast('one'); toast('two'); toast('three'); toast('four');
    expect(toastTexts()).toEqual(['two', 'three', 'four']);
    toast('one');
    expect(toastTexts()).toEqual(['three', 'four', 'one']);
  });

  it('runs the action and dismisses on click, and the close button dismisses', () => {
    const run = vi.fn();
    const t = toast('Failed', { kind: 'error', action: { label: 'Retry', run } });
    const btn = t.el.querySelector('.toast-action');
    expect(btn.textContent).toBe('Retry');
    btn.click();
    expect(run).toHaveBeenCalledTimes(1);
    expect(toasts()).toHaveLength(0);
    const u = toast('Other', { kind: 'error' });
    u.el.querySelector('.toast-close').click();
    expect(toasts()).toHaveLength(0);
  });

  it('never expires with ttl 0', () => {
    toast('Sticky', { ttl: 0 });
    vi.advanceTimersByTime(60000);
    expect(toastTexts()).toEqual(['Sticky']);
  });
});

describe('failToast', () => {
  let spy;
  beforeEach(() => { spy = vi.spyOn(console, 'error').mockImplementation(() => {}); });

  it('shows the classified sentence with a prefix and logs the raw error', () => {
    const raw = new TypeError('Failed to fetch');
    failToast(raw, { prefix: 'Could not join' });
    expect(toastTexts()[0]).toMatch(/^Could not join: /);
    expect(toastTexts()[0]).not.toMatch(/Failed to fetch/);
    expect(spy).toHaveBeenCalledWith('Could not join', raw);
  });

  it('offers Retry only for a retryable failure unless forced', () => {
    const retry = vi.fn();
    const a = failToast(new ApiError(KIND.server, 'down'), { retry });
    expect(a.el.querySelector('.toast-action')).not.toBeNull();
    const b = failToast(new ApiError(KIND.validation, 'bad handle'), { retry });
    expect(b.el.querySelector('.toast-action')).toBeNull();
    const c = failToast(new ApiError(KIND.validation, 'try anyway'), { retry, alwaysRetry: true });
    expect(c.el.querySelector('.toast-action')).not.toBeNull();
    c.el.querySelector('.toast-action').click();
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it('shows nothing for a cancelled request or an expired Access session', () => {
    expect(failToast(new ApiError(KIND.cancelled, 'x'))).toBeNull();
    expect(failToast(new ApiError(KIND.access_expired, 'x'), { prefix: 'Could not send' })).toBeNull();
    expect(toasts()).toHaveLength(0);
    expect(spy).not.toHaveBeenCalled();
  });
});

describe('banner', () => {
  it('keeps one bar per id and updates it in place', () => {
    banner('feed', 'Reconnecting…', { kind: 'warn' });
    banner('feed', 'Reconnecting… (attempt 3)', { kind: 'warn' });
    expect(banners()).toHaveLength(1);
    expect(banners()[0].textContent).toBe('Reconnecting… (attempt 3)');
    expect(banners()[0].classList.contains('warn')).toBe(true);
    expect(document.body.classList.contains('has-status-banner')).toBe(true);
    expect(hasBanner('feed')).toBe(true);
  });

  it('carries an action button and clears by id', () => {
    const run = vi.fn();
    banner('session', 'Login expired', { kind: 'error', action: { label: 'Sign in', run } });
    banner('offline', 'Offline');
    const b = banners()[0];
    expect(b.getAttribute('role')).toBe('alert');
    b.querySelector('.status-banner-action').click();
    expect(run).toHaveBeenCalledTimes(1);
    clearBanner('session');
    expect(hasBanner('session')).toBe(false);
    expect(document.body.classList.contains('has-status-banner')).toBe(true);
    clearBanner('offline');
    expect(document.body.classList.contains('has-status-banner')).toBe(false);
    clearBanner('never-there');
  });
});

describe('accessExpiredBanner', () => {
  it('keeps one error bar whose only action is a reload', () => {
    const reload = vi.fn();
    const orig = window.location;
    Object.defineProperty(window, 'location', { configurable: true, value: { ...orig, reload } });
    try {
      accessExpiredBanner();
      accessExpiredBanner();
      expect(banners()).toHaveLength(1);
      expect(hasBanner('access')).toBe(true);
      expect(banners()[0].classList.contains('error')).toBe(true);
      expect(banners()[0].textContent).toMatch(/Cloudflare Access session has expired/);
      const btn = banners()[0].querySelector('.status-banner-action');
      expect(btn.textContent).toBe('Reload');
      btn.click();
      expect(reload).toHaveBeenCalledTimes(1);
    } finally {
      Object.defineProperty(window, 'location', { configurable: true, value: orig });
    }
  });
});

describe('inlineError', () => {
  it('fills the slot with a sentence, by id or element, and clears it', () => {
    document.body.innerHTML = '<form><div id="slot" class="error hidden"></div></form>';
    inlineError('slot', new ApiError(KIND.validation, 'unknown handle @ghost'));
    const slot = document.getElementById('slot');
    expect(slot.textContent).toBe('unknown handle @ghost');
    expect(slot.classList.contains('hidden')).toBe(false);
    expect(slot.getAttribute('role')).toBe('alert');
    inlineError(slot, 'plain text');
    expect(slot.textContent).toBe('plain text');
    clearInline('slot');
    expect(slot.textContent).toBe('');
    expect(slot.classList.contains('hidden')).toBe(true);
    inlineError('missing', 'x');
    clearInline('missing');
  });
});

describe('guard', () => {
  let spy;
  beforeEach(() => { spy = vi.spyOn(console, 'error').mockImplementation(() => {}); });

  it('returns the value on success and leaves the region alone', () => {
    document.body.innerHTML = '<div id="r"><p>ok</p></div>';
    expect(guard('r', 'thread', () => 42)).toBe(42);
    expect(document.getElementById('r').innerHTML).toBe('<p>ok</p>');
    expect(spy).not.toHaveBeenCalled();
  });

  it('replaces the region with a fallback card on a sync throw', () => {
    document.body.innerHTML = '<div id="r"><p>old</p></div><div id="other">still here</div>';
    const out = guard('r', 'thread', () => { throw new Error('null.foo'); });
    expect(out).toBeUndefined();
    const card = document.querySelector('#r .region-error');
    expect(card).not.toBeNull();
    expect(card.textContent).toMatch(/Something went wrong while showing the thread\./);
    expect(card.textContent).not.toMatch(/null\.foo/);
    expect(card.querySelector('button').textContent).toBe('Reload');
    expect(document.getElementById('other').textContent).toBe('still here');
    expect(spy).toHaveBeenCalledWith('render thread', expect.any(Error));
  });

  it('catches an async rejection the same way', async () => {
    document.body.innerHTML = '<div id="r"><p>old</p></div>';
    const out = await guard(document.getElementById('r'), 'channel list', async () => { throw new Error('later'); });
    expect(out).toBeUndefined();
    expect(document.querySelector('#r .region-error')).not.toBeNull();
    expect(await guard('r', 'x', async () => 'fine')).toBe('fine');
  });

  it('reloads on the fallback button', () => {
    document.body.innerHTML = '<div id="r"></div>';
    const reload = vi.fn();
    const orig = window.location;
    Object.defineProperty(window, 'location', { configurable: true, value: { ...orig, reload } });
    try {
      guard('r', 'thread', () => { throw new Error('x'); });
      document.querySelector('#r .region-error button').click();
      expect(reload).toHaveBeenCalledTimes(1);
    } finally {
      Object.defineProperty(window, 'location', { configurable: true, value: orig });
    }
  });
});

describe('busy', () => {
  it('drops a second click while the first request is in flight, then restores the button', async () => {
    const btn = document.createElement('button');
    let release;
    const fn = vi.fn(() => new Promise((r) => { release = r; }));
    const first = busy(btn, fn);
    expect(btn.disabled).toBe(true);
    const second = busy(btn, fn);
    expect(await second).toBeUndefined();
    expect(fn).toHaveBeenCalledTimes(1);
    release('done');
    expect(await first).toBe('done');
    expect(btn.disabled).toBe(false);
    expect(btn.dataset.busy).toBeUndefined();
  });

  it('restores the button after a throw and rethrows', async () => {
    const btn = document.createElement('button');
    await expect(busy(btn, async () => { throw new Error('nope'); })).rejects.toThrow('nope');
    expect(btn.disabled).toBe(false);
    const again = await busy(btn, async () => 'ok');
    expect(again).toBe('ok');
  });

  it('runs plainly without a button', async () => {
    expect(await busy(null, async () => 1)).toBe(1);
  });
});

describe('installGlobalHandlers', () => {
  let spy;
  beforeEach(() => { spy = vi.spyOn(console, 'error').mockImplementation(() => {}); });

  it('toasts an uncaught error and an unhandled rejection, once each', () => {
    installGlobalHandlers(window);
    installGlobalHandlers(window);
    window.dispatchEvent(new ErrorEvent('error', { error: new Error('x'), message: 'x' }));
    expect(toastTexts()).toEqual(['Something went wrong. Reload the page if it keeps happening.']);
    const ev = new Event('unhandledrejection');
    ev.reason = new ApiError(KIND.server, 'The server is unavailable right now.');
    window.dispatchEvent(ev);
    expect(toastTexts()).toEqual([
      'Something went wrong. Reload the page if it keeps happening.',
      'The server is unavailable right now.',
    ]);
    for (const kind of [KIND.cancelled, KIND.access_expired]) {
      const quiet = new Event('unhandledrejection');
      quiet.reason = new ApiError(kind, 'x');
      window.dispatchEvent(quiet);
    }
    expect(toasts()).toHaveLength(2);
    expect(spy).toHaveBeenCalledTimes(2);
  });

  it('follows offline and online with a banner', () => {
    installGlobalHandlers(window);
    window.dispatchEvent(new Event('offline'));
    expect(hasBanner('offline')).toBe(true);
    window.dispatchEvent(new Event('online'));
    expect(hasBanner('offline')).toBe(false);
  });

  it('shows the offline banner at install when the browser is already offline', () => {
    vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false);
    installGlobalHandlers(window);
    expect(hasBanner('offline')).toBe(true);
  });
});
