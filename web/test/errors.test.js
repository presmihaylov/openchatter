import { describe, expect, it, vi } from 'vitest';
import { ApiError, DEFAULT_TIMEOUT_MS, KIND, UPLOAD_TIMEOUT_MS, backoffDelay, classify, errorText, fromStatus, kindOfStatus, request } from '../src/errors.js';

// A Response the way fetch hands it over: status, headers, a body read once.
const reply = (status, body, headers = {}) => {
  const text = body === undefined ? '' : (typeof body === 'string' ? body : JSON.stringify(body));
  return {
    ok: status >= 200 && status < 300,
    status,
    type: 'basic',
    headers: { get: (k) => headers[k.toLowerCase()] || null },
    text: async () => text,
  };
};

const json = (status, body) => reply(status, body, { 'content-type': 'application/json' });
const html = (status, body = '<!doctype html><html><body>Sign in</body></html>') => reply(status, body, { 'content-type': 'text/html' });

const fetchWith = (...responses) => {
  const calls = [];
  const fn = vi.fn(async (path, init) => {
    calls.push({ path, init });
    const next = responses.shift();
    if (next instanceof Error) throw next;
    if (typeof next === 'function') return next(path, init);
    return next;
  });
  fn.calls = calls;
  return fn;
};

const failure = (p) => p.then(() => { throw new Error('resolved'); }, (e) => e);

describe('kindOfStatus', () => {
  it('maps every status family the server uses', () => {
    expect(kindOfStatus(401, 'session_invalid')).toBe(KIND.session_invalid);
    expect(kindOfStatus(401, 'other')).toBe(KIND.unauthorized);
    expect(kindOfStatus(403)).toBe(KIND.forbidden);
    expect(kindOfStatus(404)).toBe(KIND.not_found);
    expect(kindOfStatus(409)).toBe(KIND.conflict);
    expect(kindOfStatus(422)).toBe(KIND.validation);
    expect(kindOfStatus(400)).toBe(KIND.validation);
    expect(kindOfStatus(429)).toBe(KIND.rate_limited);
    expect(kindOfStatus(500)).toBe(KIND.server);
    expect(kindOfStatus(502)).toBe(KIND.server);
    expect(kindOfStatus(504)).toBe(KIND.server);
  });
});

describe('fromStatus', () => {
  it('keeps the server sentence for a 4xx and carries code and reason', () => {
    const e = fromStatus(422, { error: 'unknown handle @nobody', code: 'unknown_mention' }, '/api/x');
    expect(e).toBeInstanceOf(ApiError);
    expect(e.kind).toBe(KIND.validation);
    expect(e.message).toBe('unknown handle @nobody');
    expect(e.code).toBe('unknown_mention');
    expect(e.status).toBe(422);
    expect(e.path).toBe('/api/x');
    expect(e.retryable).toBe(false);
    const f = fromStatus(403, { error: 'no', code: 'workspace_forbidden', reason: 'revoked' });
    expect(f.reason).toBe('revoked');
  });

  it('never shows a 5xx body, a stack, JSON or HTML as the message', () => {
    expect(fromStatus(500, { error: 'pq: relation "x" does not exist' }).message).toMatch(/unavailable/);
    expect(fromStatus(400, { error: '{"deep":"json"}' }).message).toMatch(/rejected/);
    expect(fromStatus(400, { error: '<html>nope</html>' }).message).toMatch(/rejected/);
    expect(fromStatus(400, { error: 'Error: boom\n    at foo (app.js:1)' }).message).toMatch(/rejected/);
    expect(fromStatus(400, { error: 'x'.repeat(301) }).message).toMatch(/rejected/);
    expect(fromStatus(404, null).message).toMatch(/no longer exists/);
  });

  it('marks the session kinds and the retryable kinds', () => {
    expect(fromStatus(401, { code: 'session_invalid' }).isSession).toBe(true);
    expect(fromStatus(401, {}).isSession).toBe(true);
    expect(fromStatus(403, {}).isSession).toBe(false);
    expect(fromStatus(502, null).retryable).toBe(true);
    expect(fromStatus(429, null).retryable).toBe(true);
    expect(fromStatus(422, null).retryable).toBe(false);
  });
});

describe('classify', () => {
  it('is idempotent on an ApiError', () => {
    const e = new ApiError(KIND.server, 'x');
    expect(classify(e)).toBe(e);
  });

  it('reads a fetch TypeError as offline or network by navigator.onLine', () => {
    expect(classify(new TypeError('Failed to fetch'), { online: false }).kind).toBe(KIND.offline);
    expect(classify(new TypeError('Failed to fetch'), { online: true }).kind).toBe(KIND.network);
    expect(classify(new TypeError('Failed to fetch'), { online: true }).message).not.toMatch(/Failed to fetch/);
  });

  it('tells our timeout from a caller abort', () => {
    const abort = new DOMException('The user aborted a request.', 'AbortError');
    expect(classify(abort, { timedOut: true }).kind).toBe(KIND.timeout);
    expect(classify(abort).kind).toBe(KIND.cancelled);
  });

  it('upgrades an old-style Error with a status', () => {
    const old = Object.assign(new Error('HTTP 404'), { status: 404, code: 'workspace_not_found' });
    const e = classify(old);
    expect(e.kind).toBe(KIND.not_found);
    expect(e.code).toBe('workspace_not_found');
    expect(e.cause).toBe(old);
  });

  it('gives an unknown throw a sentence, never its raw text', () => {
    const e = classify(new Error('TypeError: cannot read properties of null'));
    expect(e.kind).toBe(KIND.unknown);
    expect(e.message).toBe('Something went wrong. Try again.');
    expect(errorText('some string')).toBe('Something went wrong. Try again.');
    expect(errorText(undefined)).toBe('Something went wrong. Try again.');
  });
});

describe('request', () => {
  it('returns the parsed JSON on success and null on 204', async () => {
    const f = fetchWith(json(200, { ok: 1 }), reply(204));
    expect(await request('/a', { fetch: f })).toEqual({ ok: 1 });
    expect(await request('/b', { fetch: f })).toBeNull();
  });

  it('serialises an object body, passes FormData through, keeps redirects manual', async () => {
    const f = fetchWith(json(200, {}), json(200, {}));
    await request('/a', { fetch: f, method: 'POST', body: { x: 1 }, headers: { 'X-Workspace-Slug': 'w' } });
    const first = f.calls[0].init;
    expect(first.method).toBe('POST');
    expect(first.body).toBe('{"x":1}');
    expect(first.headers['Content-Type']).toBe('application/json');
    expect(first.headers['X-Workspace-Slug']).toBe('w');
    expect(first.redirect).toBe('manual');
    const fd = new FormData();
    await request('/b', { fetch: f, method: 'POST', body: fd });
    expect(f.calls[1].init.body).toBe(fd);
    expect(f.calls[1].init.headers['Content-Type']).toBeUndefined();
  });

  it('turns a 4xx body into an ApiError with the server text', async () => {
    const f = fetchWith(json(422, { error: 'unknown handle @ghost', code: 'unknown_mention' }));
    const e = await failure(request('/a', { fetch: f }));
    expect(e.kind).toBe(KIND.validation);
    expect(e.message).toBe('unknown handle @ghost');
    expect(e.code).toBe('unknown_mention');
  });

  it('turns 401 session_invalid, 403 and 404 into their kinds', async () => {
    const f = fetchWith(json(401, { error: 'bad', code: 'session_invalid' }), json(403, { code: 'workspace_forbidden', reason: 'revoked' }), json(404, { code: 'workspace_not_found' }));
    expect((await failure(request('/a', { fetch: f }))).kind).toBe(KIND.session_invalid);
    const forb = await failure(request('/b', { fetch: f }));
    expect(forb.kind).toBe(KIND.forbidden);
    expect(forb.reason).toBe('revoked');
    expect((await failure(request('/c', { fetch: f }))).kind).toBe(KIND.not_found);
  });

  it('reads a 502 HTML page from the proxy as a server outage, not as text', async () => {
    const f = fetchWith(html(502, '<html><body><h1>502 Bad Gateway</h1></body></html>'));
    const e = await failure(request('/a', { fetch: f }));
    expect(e.kind).toBe(KIND.server);
    expect(e.status).toBe(502);
    expect(e.message).not.toMatch(/502|html/i);
    expect(e.retryable).toBe(true);
  });

  it('reads an empty 500 as a server outage with a sentence', async () => {
    const e = await failure(request('/a', { fetch: fetchWith(reply(500)) }));
    expect(e.kind).toBe(KIND.server);
    expect(e.message).toMatch(/unavailable/);
  });

  it('reads 429 as rate limited', async () => {
    const e = await failure(request('/a', { fetch: fetchWith(json(429, { error: 'slow down', code: 'rate_limited' })) }));
    expect(e.kind).toBe(KIND.rate_limited);
    expect(e.code).toBe('rate_limited');
  });

  it('names the Cloudflare Access redirect instead of following it', async () => {
    const opaque = { ok: false, status: 0, type: 'opaqueredirect', headers: { get: () => null }, text: async () => '' };
    const e = await failure(request('/a', { fetch: fetchWith(opaque) }));
    expect(e.kind).toBe(KIND.access_expired);
    expect(e.isSession).toBe(true);
    expect(e.message).toMatch(/Cloudflare Access/);
    const e2 = await failure(request('/a', { fetch: fetchWith(html(302, '')) }));
    expect(e2.kind).toBe(KIND.access_expired);
  });

  it('refuses a 200 HTML page as data', async () => {
    const e = await failure(request('/a', { fetch: fetchWith(html(200)) }));
    expect(e.kind).toBe(KIND.bad_response);
    expect(e.status).toBe(200);
  });

  it('refuses a 200 that is neither JSON nor empty', async () => {
    const e = await failure(request('/a', { fetch: fetchWith(reply(200, 'not json at all')) }));
    expect(e.kind).toBe(KIND.bad_response);
  });

  it('treats a 200 with an empty body as null, not as an error', async () => {
    expect(await request('/a', { fetch: fetchWith(reply(200, '')) })).toBeNull();
  });

  it('parses JSON even when the content-type header is missing', async () => {
    expect(await request('/a', { fetch: fetchWith(reply(200, { fine: true })) })).toEqual({ fine: true });
  });

  it('turns a thrown TypeError into offline or network', async () => {
    const spy = vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false);
    const e = await failure(request('/a', { fetch: fetchWith(new TypeError('Failed to fetch')) }));
    expect(e.kind).toBe(KIND.offline);
    spy.mockReturnValue(true);
    const e2 = await failure(request('/a', { fetch: fetchWith(new TypeError('Failed to fetch')) }));
    expect(e2.kind).toBe(KIND.network);
    expect(e2.message).not.toMatch(/Failed to fetch/);
  });

  it('aborts a request that outlives its deadline and reports a timeout', async () => {
    vi.useFakeTimers();
    try {
      const f = vi.fn((path, init) => new Promise((resolve, reject) => {
        init.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
      }));
      const p = failure(request('/slow', { fetch: f, timeoutMs: 1000 }));
      await vi.advanceTimersByTimeAsync(1001);
      const e = await p;
      expect(e.kind).toBe(KIND.timeout);
      expect(e.retryable).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('gives an upload minutes, not the 15s of a JSON call', async () => {
    vi.useFakeTimers();
    try {
      const f = vi.fn((path, init) => new Promise((resolve, reject) => {
        init.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
      }));
      const p = failure(request('/upload', { fetch: f, method: 'POST', body: new FormData() }));
      await vi.advanceTimersByTimeAsync(DEFAULT_TIMEOUT_MS + 1);
      expect(f.mock.calls[0][1].signal.aborted).toBe(false);
      await vi.advanceTimersByTimeAsync(UPLOAD_TIMEOUT_MS);
      expect((await p).kind).toBe(KIND.timeout);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports a caller abort as cancelled, not as a failure', async () => {
    const ctl = new AbortController();
    const f = vi.fn((path, init) => new Promise((resolve, reject) => {
      init.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
    }));
    const p = failure(request('/x', { fetch: f, signal: ctl.signal }));
    ctl.abort();
    expect((await p).kind).toBe(KIND.cancelled);
  });

  it('lets go of a shared caller signal once the request settles', async () => {
    const ctl = new AbortController();
    const add = vi.spyOn(ctl.signal, 'addEventListener');
    const remove = vi.spyOn(ctl.signal, 'removeEventListener');
    await request('/x', { fetch: fetchWith(reply(200, json({ ok: 1 }))), signal: ctl.signal });
    await failure(request('/x', { fetch: fetchWith(reply(500, '')), signal: ctl.signal }));
    expect(add).toHaveBeenCalledTimes(2);
    expect(remove).toHaveBeenCalledTimes(2);
    expect(remove.mock.calls[0][1]).toBe(add.mock.calls[0][1]);
  });
});

describe('backoffDelay', () => {
  it('doubles from the base, caps at max, and jitters around the step', () => {
    const fixed = { random: () => 0.5 };
    expect(backoffDelay(1, fixed)).toBe(1000);
    expect(backoffDelay(2, fixed)).toBe(2000);
    expect(backoffDelay(3, fixed)).toBe(4000);
    expect(backoffDelay(10, fixed)).toBe(30000);
    expect(backoffDelay(3, { random: () => 0 })).toBe(3200);
    expect(backoffDelay(3, { random: () => 1 })).toBe(4800);
    expect(backoffDelay(0, fixed)).toBe(1000);
  });
});
