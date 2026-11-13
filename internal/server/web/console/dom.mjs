export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith('on')) node.addEventListener(key.slice(2).toLowerCase(), value);
    else if (key === 'class') node.className = value;
    else if (['value', 'checked', 'disabled', 'readOnly', 'required', 'selected'].includes(key)) node[key] = value;
    else node.setAttribute(key, value === true ? '' : String(value));
  }
  for (const child of children.flat(Infinity)) {
    if (child !== null && child !== undefined && child !== false) node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export const text = (value, cls = '') => el('p', { class: cls }, value);
export const button = (label, action, cls = 'secondary') => el('button', { type: 'button', class: cls, onclick: action }, label);
export function leaveButton(label, action, api, cls = 'secondary') {
  return el('button', { type: 'button', class: cls, 'data-leave-view': true, disabled: !api.canLeave(), onclick: event => {
    if (api.canLeave()) return action(event);
  } }, label);
}
export const badge = (label, tone = '') => el('span', { class: `badge ${tone}` }, label);
export const actions = (...children) => el('div', { class: 'actions' }, children);
export const message = (value, tone = '') => el('div', { class: `notice ${tone}`, role: tone === 'error' ? 'alert' : 'status' }, value);
export const card = (title, ...children) => el('section', { class: 'card' }, el('h2', {}, title), children);
export const heading = (title, description, ...controls) => el('header', { class: 'page-heading' }, el('div', {}, el('p', { class: 'eyebrow' }, 'ZZROUTER CONSOLE'), el('h1', { tabindex: '-1' }, title), text(description, 'muted')), actions(controls));
export const field = (label, input, hint = '') => el('label', { class: 'field' }, el('span', {}, label), input, hint ? el('small', {}, hint) : null);
export const input = (name, value = '', attrs = {}) => el('input', { name, value, autocomplete: 'off', ...attrs });
export function select(name, choices, value) {
  return el('select', { name }, choices.map(([key, label]) => el('option', { value: key, selected: String(value ?? '') === key }, label)));
}
export function clearSecrets(root) {
  for (const node of root.querySelectorAll('input[type="password"], [data-sensitive]')) {
    if ('value' in node) node.value = '';
    else node.textContent = '';
  }
}
export function facts(entries) {
  return el('dl', { class: 'facts' }, entries.map(([label, value]) => [el('dt', {}, label), el('dd', {}, value ?? 'Unavailable')]));
}
export function copyField(label, value, sensitive = false) {
  const copyInput = input('copy-value', value, { readOnly: true, type: sensitive ? 'password' : 'text', 'data-sensitive': sensitive, 'aria-label': label });
  const feedback = el('small', { role: 'status' });
  const buttons = [button('Copy', async () => {
    try { await navigator.clipboard.writeText(copyInput.value); feedback.textContent = 'Copied.'; }
    catch { copyInput.focus(); copyInput.select(); feedback.textContent = 'Select and copy this value.'; }
  })];
  if (sensitive) buttons.push(button('Show', event => {
    copyInput.type = copyInput.type === 'password' ? 'text' : 'password';
    event.currentTarget.textContent = copyInput.type === 'password' ? 'Show' : 'Hide';
  }));
  return el('div', { class: 'copy-field' }, field(label, copyInput), actions(buttons), feedback);
}
export function problem(error) {
  return message(error.uncertain ? `${error.message} Do not resubmit automatically.` : error.message, 'error');
}
export const date = value => value ? new Date(value).toLocaleString() : 'No expiry';
export const money = value => Number(value || 0).toLocaleString(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 4 });
