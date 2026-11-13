const BASE = '/zzrouter/v1';

export class APIError extends Error {
  constructor(message, { status = 0, code = '', uncertain = false } = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.uncertain = uncertain;
  }
}

export class Obsolete extends Error {}

// Each page owns its requests. Invalidating a page also prevents an already
// completed response from rendering after its owner has gone away.
export class Lifetime {
  next() {
    this.end();
    const controller = new AbortController();
    this.controller = controller;
    return { signal: controller.signal, current: () => this.controller === controller && !controller.signal.aborted };
  }
  end() {
    this.controller?.abort();
    this.controller = null;
  }
}

export class API {
  constructor({ fetcher = globalThis.fetch.bind(globalThis), origin = globalThis.location?.origin, cleared = () => {}, rejected = () => {}, activity = () => {} } = {}) {
    this.fetcher = fetcher;
    this.origin = origin;
    this.cleared = cleared;
    this.rejected = rejected;
    this.activity = activity;
    this.generation = 0;
    this.state = null;
    this.pending = new Set();
    this.unknownChange = false;
    this.unverified = false;
  }

  canLeave() { return this.pending.size === 0; }
  canChange() { return this.canLeave() && !this.unverified; }

  clear(reason = 'Checking your session…') {
    // Retain only a generic warning across identity/page boundaries. A pending
    // request may already have committed even when its response is aborted.
    if (this.pending.size) this.unknownChange = true;
    this.pending.clear();
    this.unverified = false;
    this.generation++;
    this.refreshController?.abort();
    this.state = null;
    this.cleared(reason);
    this.activity();
    return this.generation;
  }

  async request(path, { method = 'GET', body, signal, proof = this.state?.csrf, notifyAuth = true, effectful = method !== 'GET', beforeReturn } = {}) {
    const generation = this.generation;
    if (!(path.startsWith(BASE + '/') || path === '/health')) throw new APIError('Unsupported console request.');
    const mutation = method !== 'GET';
    if (mutation && !proof) throw new APIError('Refresh your session before making changes.', { code: 'session_refresh_required' });
    if (mutation && !this.canChange()) throw new APIError(this.unverified ? 'Verify your session before making another change.' : 'Wait for the current change to finish.');
    if (signal?.aborted) throw new Obsolete();
    const headers = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (mutation) headers['X-CSRF-Token'] = proof;
    const timeout = AbortSignal.timeout(15000);
    // The tracker never retains request bodies, resource names or credentials.
    const operation = effectful ? {} : null;
    if (operation) { this.pending.add(operation); this.activity(); }
    let finished = false;
    const finish = error => {
      if (!operation || finished) return;
      finished = true;
      this.pending.delete(operation);
      if (error?.uncertain) this.unknownChange = true;
      this.activity();
    };
    try {
      let response;
      try {
        response = await this.fetcher(path, {
          method, headers, body: body === undefined ? undefined : JSON.stringify(body),
          credentials: 'same-origin', cache: 'no-store', redirect: 'error',
          signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
        });
      } catch {
        throw new APIError(effectful ? 'The response was interrupted. The change may have completed. Reload current state before doing anything else.' : 'Could not reach the coordinator. Check your connection and try again.', { uncertain: effectful });
      }
      let result;
      try { result = await response.json(); }
      catch { throw new APIError(effectful ? 'The server response could not be read. Reload current state to check whether the change completed.' : 'The server returned an unreadable response.', { status: response.status, uncertain: effectful }); }
      if (!result || typeof result !== 'object') throw new APIError('The server returned an invalid response. Reload current state before another change.', { status: response.status, uncertain: effectful });
      if (!response.ok || result.success === false) {
        const knownRefusal = ['provider_connection_unavailable', 'provider_config_unavailable', 'managed_credentials_unavailable', 'provider_connection_persistence_failed'].includes(result.code);
        const error = new APIError(result.detail || result.message || `Request failed (${response.status}).`, { status: response.status, code: result.code, uncertain: effectful && response.status >= 500 && !knownRefusal });
        // An explicit refusal is settled before an auth callback can reset the
        // owner; that reset must not turn this refusal into an unknown outcome.
        finish(error);
        if (notifyAuth && (response.status === 401 || response.status === 403) && path !== BASE + '/session' && generation === this.generation && !signal?.aborted) this.rejected(error);
        throw error;
      }
      // One-time results may need to await an overlapping identity check. Keep
      // the write/navigation owner until that transient result can be shown.
      await beforeReturn?.(result);
      finish();
      return result;
    } catch (error) {
      finish(error);
      throw error;
    }
  }

  async data(path, options) { return (await this.request(BASE + path, options)).data; }

  async list(path, options = {}) {
    const items = [];
    for (let page = 0; page < 100; page++) {
      const suffix = page ? `${path.includes('?') ? '&' : '?'}offset=${items.length}` : '';
      const result = await this.request(BASE + path + suffix, options);
      if (!Array.isArray(result.data)) throw new APIError('The server returned an invalid list.');
      items.push(...result.data);
      if (!result.has_more) return items;
      if (!result.data.length) break;
    }
    throw new APIError('The list could not be fully loaded. Reload before making changes.');
  }

  async refresh() {
    const generation = this.clear();
    const controller = new AbortController();
    this.refreshController = controller;
    const options = { signal: controller.signal, notifyAuth: false };
    const first = await this.data('/session', options);
    let identity = null;
    const mismatch = first.issuer !== this.origin;
    if (first.authenticated && !mismatch) {
      identity = await this.data('/me', options);
      const last = await this.data('/session', options);
      if (!last.authenticated || last.csrf !== first.csrf || last.issuer !== first.issuer) {
        throw new APIError('The browser identity changed during refresh. Refresh again to continue.', { code: 'session_changed' });
      }
    }
    if (generation !== this.generation) throw new Obsolete();
    this.state = { ...first, identity, mismatch, csrf: mismatch ? null : first.csrf };
    return this.state;
  }

  async logout() {
    const proof = this.state?.csrf;
    this.clear('Signing out…');
    await this.data('/session', { method: 'DELETE', proof });
  }
}

export function changedFields(before, after) {
  return Object.fromEntries(Object.entries(after).filter(([key, value]) => JSON.stringify(value) !== JSON.stringify(before[key])));
}

export function applySummary(result) {
  return [
    result.saved ? 'Saved' : 'Not saved',
    result.durable ? 'credential durability confirmed' : 'credential durability not confirmed',
    result.active ? 'connection active' : 'connection not active',
    result.published ? 'catalog published' : 'catalog publication not confirmed',
    result.outcome === 'saved_activation_failed' ? 'provider activation reported an error; reload before retrying' : null,
  ].filter(Boolean).join(' · ');
}
