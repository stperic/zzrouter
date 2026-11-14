// One cancelable owner for snapshot reads. Failure stops automatic polling until
// an explicit refresh; a pending read is never overlapped or replayed as a write.
export function liveReads(p, read, { interval = 5000, eligible = () => true, changed = () => {}, failed = () => {} } = {}) {
  let timer, pending, paused = false, error = false;
  const canUpdate = () => !paused && !document.hidden && p.api.canChange() && eligible();
  const schedule = () => {
    clearTimeout(timer);
    if (!p.current() || paused || error) return;
    timer = setTimeout(async () => {
      if (canUpdate()) await refresh(false);
      else schedule();
    }, interval);
    timer.unref?.();
  };
  async function refresh(explicit = true, ...args) {
    if (pending) return pending;
    if (!p.current() || !p.api.canLeave()) return false;
    clearTimeout(timer);
    if (explicit) error = false;
    pending = (async () => {
      try { const ok = await read(!explicit, ...args); error = ok === false; return !error; }
      catch (reason) { error = true; if (p.current()) failed(reason); return false; }
      finally { pending = undefined; if (p.current()) { changed(); schedule(); } }
    })();
    return pending;
  }
  p.scope.signal.addEventListener('abort', () => clearTimeout(timer), { once: true });
  return { refresh, canUpdate, settle: () => pending, get paused() { return paused; }, get failed() { return error; },
    toggle() { paused = !paused; changed(); schedule(); }, start: schedule };
}
