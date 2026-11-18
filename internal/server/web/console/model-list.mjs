import { el, leaveButton } from './dom.mjs';

export const modelCount = count => `${count.toLocaleString()} ${count === 1 ? 'model' : 'models'}`;

export function modelToolbar(...controls) {
  return el('div', { class: 'model-toolbar' }, controls);
}

export function modelList(filters) {
  const feedback = el('div', { 'aria-live': 'polite' });
  const results = el('div', { class: 'model-list-results' });
  const pagination = el('div', { class: 'actions' });
  const workspace = el('div', { class: 'entity-workspace' },
    el('section', { class: 'card entity-list model-list' }, filters, feedback, results, pagination));
  return { workspace, feedback, results, pagination };
}

export function modelRow(api, { key, name, open, date, tags, source, description, stats = [] }) {
  const control = leaveButton('', open, api, 'network-item');
  control.setAttribute('data-selection', key);
  control.setAttribute('aria-label', name);
  const sourceText = source ? el('span', { class: 'network-meta' }, source) : null;
  const summary = description ? el('span', { class: 'model-row-description' }, description) : null;
  const metadata = stats.length ? el('span', { class: 'network-meta model-row-stats' }, stats) : null;
  const detail = [date, tags, sourceText, summary, metadata].filter(Boolean);
  if (detail.length) {
    const ids = detail.map((element, index) => {
      const id = `model-row-${key}-${index}`;
      element.setAttribute('id', id);
      return id;
    });
    control.setAttribute('aria-describedby', ids.join(' '));
  }
  control.append(el('span', { class: 'model-row-heading' },
    el('span', { class: 'model-row-name' }, el('strong', {}, name), date), tags));
  for (const element of [sourceText, summary, metadata].filter(Boolean)) control.append(element);
  return control;
}
