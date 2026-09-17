// One vocabulary for everything that goes wrong between the page and the API:
// request() turns every failure (status, dead network, timeout, unreadable body,
// Access redirect) into an ApiError with a `kind` and a sentence a person can act on.

export const KIND = Object.freeze({
  offline: 'offline',            // navigator says there is no network
  network: 'network',            // fetch threw while online: DNS, refused, CORS
  timeout: 'timeout',            // our own deadline fired
  access_expired: 'access_expired', // Cloudflare Access answered with its login redirect
  session_invalid: 'session_invalid', // 401 session_invalid: the login is gone
  unauthorized: 'unauthorized',  // any other 401
  forbidden: 'forbidden',        // 403
  not_found: 'not_found',        // 404
  conflict: 'conflict',          // 409
  validation: 'validation',      // 400, 413, 422: the request itself was wrong
  rate_limited: 'rate_limited',  // 429
  server: 'server',              // 5xx, including the 502 of a restart
  bad_response: 'bad_response',  // 2xx with a body that is not the JSON we expect
  cancelled: 'cancelled',        // the caller's own abort: nothing to show
  unknown: 'unknown',
});

// Which failures are worth a Retry button: the same request can succeed later.
const RETRYABLE = new Set([KIND.offline, KIND.network, KIND.timeout, KIND.rate_limited, KIND.server, KIND.bad_response]);

// Which failures end the session rather than one request: they get a
// persistent banner, not a toast.
const SESSION_KINDS = new Set([KIND.access_expired, KIND.session_invalid, KIND.unauthorized]);

export class ApiError extends Error {
  constructor(kind, message, extra = {}) {
    super(message);
    this.name = 'ApiError';
    this.kind = kind;
    this.status = extra.status ?? 0;
    this.code = extra.code ?? null;
    this.reason = extra.reason ?? null;
    this.data = extra.data ?? null;
    this.path = extra.path ?? null;
    if (extra.cause) this.cause = extra.cause;
  }
  get retryable() { return RETRYABLE.has(this.kind); }
  get isSession() { return SESSION_KINDS.has(this.kind); }
}

// Fixed sentences for the failures the server cannot describe. A 4xx keeps the
// server's own text, which already names the field or the handle.
const TEXT = {
  [KIND.offline]: 'You are offline. Check the connection and try again.',
  [KIND.network]: 'Cannot reach the server. Try again in a moment.',
  [KIND.timeout]: 'The server took too long to answer. Try again.',
  [KIND.access_expired]: 'Your Cloudflare Access session has expired. Reload the page to sign in again.',
  [KIND.session_invalid]: 'Your login has expired. Sign in again.',
  [KIND.unauthorized]: 'Your login is not valid here. Sign in again.',
  [KIND.forbidden]: 'You do not have permission to do that.',
  [KIND.not_found]: 'That no longer exists. It may have been deleted.',
  [KIND.conflict]: 'That conflicts with a change somebody else made. Reload and try again.',
  [KIND.validation]: 'The server rejected that request.',
  [KIND.rate_limited]: 'Too many requests. Wait a minute and try again.',
  [KIND.server]: 'The server is unavailable right now. It may be restarting; try again in a moment.',
  [KIND.bad_response]: 'The server sent an answer the page could not read. Try again.',
  [KIND.cancelled]: 'The request was cancelled.',
  [KIND.unknown]: 'Something went wrong. Try again.',
};

export const kindOfStatus = (status, code) => {
  if (status === 401) return code === 'session_invalid' ? KIND.session_invalid : KIND.unauthorized;
  if (status === 403) return KIND.forbidden;
  if (status === 404) return KIND.not_found;
  if (status === 409) return KIND.conflict;
  if (status === 429) return KIND.rate_limited;
  if (status >= 500) return KIND.server;
  if (status >= 400) return KIND.validation;
  return KIND.unknown;
};

// A server message is user text only when it is a short sentence. Anything
// that looks like JSON, a stack or an HTML page is diagnostics, not copy.
const usable = (s) => typeof s === 'string' && s.trim().length > 0 && s.length <= 300
  && !/^[\[{<]/.test(s.trim()) && !/\n\s+at /.test(s);

export const fromStatus = (status, data, path) => {
  const code = data && typeof data === 'object' ? data.code || null : null;
  const kind = kindOfStatus(status, code);
  const serverText = data && typeof data === 'object' ? data.error : null;
  // a 4xx is the server describing the caller's mistake: keep its words when
  // they read as a sentence; a 5xx body is a stack or a proxy page, never copy
  const message = (status < 500 && usable(serverText)) ? serverText : TEXT[kind];
  return new ApiError(kind, message, { status, code, reason: data && data.reason, data, path });
};

// classify turns anything a request can throw into an ApiError. Idempotent.
export const classify = (err, ctx = {}) => {
  if (err instanceof ApiError) return err;
  const path = ctx.path || null;
  const online = ctx.online !== undefined ? ctx.online : (typeof navigator === 'undefined' ? true : navigator.onLine !== false);
  if (err && (err.name === 'AbortError' || err.code === 20)) {
    // our own deadline is a failure to show; a caller's abort is not
    const kind = ctx.timedOut ? KIND.timeout : KIND.cancelled;
    return new ApiError(kind, TEXT[kind], { path, cause: err });
  }
  if (err instanceof TypeError) {
    const kind = online ? KIND.network : KIND.offline;
    return new ApiError(kind, TEXT[kind], { path, cause: err });
  }
  if (err && typeof err.status === 'number' && err.status > 0) {
    // an Error carrying a status from older code: same mapping, keep its text if usable
    const e = fromStatus(err.status, { error: err.message, code: err.code, reason: err.reason }, path);
    e.cause = err;
    return e;
  }
  return new ApiError(KIND.unknown, TEXT[KIND.unknown], { path, cause: err });
};

// errorText is the one sentence to show for any thrown value.
export const errorText = (err) => classify(err).message;

// textOf is the fixed sentence for a kind, for a surface that names the failure itself.
export const textOf = (kind) => TEXT[kind] || TEXT[KIND.unknown];

// readBody parses what came back without trusting the headers: a proxy error
// page says text/html with a 502, an empty 204 has no body, a JSON body may
// arrive without its content-type from a misconfigured front.
const readBody = async (resp) => {
  let text = '';
  try { text = await resp.text(); } catch (e) { return { data: null, text: '', json: false }; }
  if (!text.trim()) return { data: null, text, json: false };
  try { return { data: JSON.parse(text), text, json: true }; }
  catch (e) { return { data: null, text, json: false }; }
};

export const DEFAULT_TIMEOUT_MS = 15000;
// an upload carries the file in the request: a slow link needs minutes, not seconds
export const UPLOAD_TIMEOUT_MS = 120000;

// request(path, opts) -> parsed JSON (null on 204), or throws ApiError.
// opts: method, headers, body (object -> JSON, FormData as is), timeoutMs,
// signal (caller abort), fetch (injected under test), expectJSON (default true).
export const request = async (path, opts = {}) => {
  const doFetch = opts.fetch || (typeof fetch === 'function' ? fetch : null);
  if (!doFetch) throw new ApiError(KIND.unknown, TEXT[KIND.unknown], { path });
  const headers = Object.assign({}, opts.headers || {});
  let body = opts.body;
  const multipart = typeof FormData !== 'undefined' && body instanceof FormData;
  if (body !== undefined && body !== null && !multipart && typeof body !== 'string') {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(body);
  }
  const timeoutMs = opts.timeoutMs === undefined ? (multipart ? UPLOAD_TIMEOUT_MS : DEFAULT_TIMEOUT_MS) : opts.timeoutMs;
  const ctl = typeof AbortController === 'function' ? new AbortController() : null;
  let timedOut = false;
  let timer = null;
  if (ctl && timeoutMs > 0) timer = setTimeout(() => { timedOut = true; ctl.abort(); }, timeoutMs);
  const onAbort = () => ctl.abort();
  if (ctl && opts.signal) {
    if (opts.signal.aborted) ctl.abort();
    else opts.signal.addEventListener('abort', onAbort);
  }
  let resp;
  try {
    resp = await doFetch(path, {
      method: opts.method || 'GET',
      headers,
      body,
      // Cloudflare Access answers an expired session with a 302 to its login
      // page on another origin. Followed, that is a CORS failure that looks
      // like being offline; kept manual, it is an opaque redirect we can name.
      redirect: 'manual',
      signal: ctl ? ctl.signal : undefined,
    });
  } catch (err) {
    throw classify(err, { path, timedOut });
  } finally {
    if (timer) clearTimeout(timer);
    if (ctl && opts.signal) opts.signal.removeEventListener('abort', onAbort);
  }
  if (resp.type === 'opaqueredirect' || (resp.status >= 300 && resp.status < 400)) {
    throw new ApiError(KIND.access_expired, TEXT[KIND.access_expired], { status: resp.status || 302, path });
  }
  if (resp.status === 204) return null;
  const { data, json, text } = await readBody(resp);
  if (!resp.ok) throw fromStatus(resp.status, json ? data : null, path);
  if (opts.expectJSON === false) return data;
  if (!json) {
    // a 200 that is not JSON is a front-end page or an empty answer, never
    // data the caller can use: say so instead of handing back null
    const looksHTML = /^\s*</.test(text || '');
    if (looksHTML) throw new ApiError(KIND.bad_response, TEXT[KIND.bad_response], { status: resp.status, path });
    if (!text || !text.trim()) return null;
    throw new ApiError(KIND.bad_response, TEXT[KIND.bad_response], { status: resp.status, path });
  }
  return data;
};

// backoffDelay grows 1s, 2s, 4s ... up to max, then +-jitter so a fleet of
// tabs does not stampede a server that just came back.
export const backoffDelay = (attempt, { base = 1000, max = 30000, jitter = 0.2, random = Math.random } = {}) => {
  const exp = Math.min(max, base * Math.pow(2, Math.max(0, attempt - 1)));
  const spread = exp * jitter;
  return Math.round(exp - spread + random() * spread * 2);
};
