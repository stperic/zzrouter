import { el, field, select } from './dom.mjs';

const storageKey = 'zzrouter.console.theme';
const themes = [['claude-web', 'Claude Web (default)'], ['claude-web-dark', 'Claude Web dark'],
  ['claude-code-light', 'Claude Code light'], ['claude-code-dark', 'Claude Code dark']];
const validTheme = value => themes.some(([id]) => id === value) ? value : themes[0][0];

function applyTheme(value) {
  document.documentElement.dataset.theme = validTheme(value);
  window.dispatchEvent(new Event('console-theme-change'));
}

// Appearance is a browser preference, independent of router identity or policy.
export function initializeAppearance() {
  let saved, storage;
  try { storage = localStorage; saved = storage.getItem(storageKey); } catch { /* Storage can be disabled. */ }
  applyTheme(saved);
  window.addEventListener('storage', event => {
    if (storage && event.storageArea === storage && (event.key === storageKey || event.key === null)) applyTheme(event.newValue);
  });
}

export function themePicker(signal) {
  const choice = select('color_theme', themes, document.documentElement.dataset.theme);
  const status = el('small', { role: 'status' });
  const sync = () => {
    choice.value = document.documentElement.dataset.theme;
    try {
      const saved = localStorage.getItem(storageKey);
      status.textContent = saved === choice.value ? 'Saved in this browser.' :
        validTheme(saved) === choice.value ? 'Only affects this browser.' : 'Applied for this tab. This choice could not be saved.';
    } catch { status.textContent = 'Applied for this tab. Browser storage is unavailable.'; }
  };
  sync();
  window.addEventListener('console-theme-change', sync, { signal });
  choice.addEventListener('change', () => {
    const value = validTheme(choice.value);
    try { localStorage.setItem(storageKey, value); } catch { /* The theme still applies to this tab. */ }
    applyTheme(value);
  });
  return el('div', { class: 'theme-picker' }, field('Color theme', choice), status);
}
