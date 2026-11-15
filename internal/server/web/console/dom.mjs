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
export const heading = (title, description, ...controls) => el('header', { class: 'page-heading' }, el('div', {}, el('h1', { tabindex: '-1' }, title), text(description, 'muted')), actions(controls));
export function field(label, control, hint = '') {
  if (!control.getAttribute('aria-label')) control.setAttribute('aria-label', label instanceof Node ? label.textContent : label);
  return el('label', { class: 'field' }, el('span', {}, label), control, hint ? el('small', {}, hint) : null);
}
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
  const root = message(error.uncertain ? `${error.message} Do not resubmit automatically.` : error.message, 'error');
  const context = [['Code', error.code], ['Node', error.node], ['Request ID', error.request_id]].filter(([, value]) => value);
  if (error.mutation_applied) root.append(text('The server reports that the change was applied. Inspect current state to verify the result.'));
  const technical = el('details', { class: 'error-details' }, el('summary', {}, 'Technical details'));
  if (context.length) technical.append(facts(context));
  for (const item of Array.isArray(error.errors) ? error.errors.slice(0, 12) : []) {
    technical.append(text([item.key, item.code, item.message, item.hint].filter(value => typeof value === 'string').map(value => value.slice(0, 2048)).join(' · ')));
    const evidence = ['got', 'want', 'min', 'max'].filter(key => item[key] !== undefined).map(key => [key, safeEvidence(item[key])]);
    if (evidence.length) technical.append(facts(evidence));
  }
  if (technical.children.length > 1) root.append(technical);
  return root;
}

export const invalidField = (message, field) => Object.assign(new Error(message), { field });
let feedbackID = 0;
function revealField(control) {
  for (let parent = control.parentElement; parent; parent = parent.parentElement) {
    if (parent.tagName === 'DETAILS') parent.open = true;
  }
  control.focus({ preventScroll: true });
  control.scrollIntoView?.({ block: 'nearest' });
}

// The form maps server paths to its controls. Never infer a target from prose.
export function formFeedback(form, region, target = () => undefined) {
  const marked = new Map();
  const clear = () => {
    for (const [control, previous] of marked) {
      control.removeAttribute('aria-invalid');
      if (previous) control.setAttribute('aria-describedby', previous);
      else control.removeAttribute('aria-describedby');
    }
    marked.clear();
  };
  const show = (error, entries = []) => {
    clear();
    const root = problem(error);
    const links = el('ul', { class: 'error-links' });
    for (const { control, message } of entries) {
      if (!control || marked.has(control)) continue;
      const id = `form-error-${++feedbackID}`;
      if (!control.id) control.id = `${id}-field`;
      marked.set(control, control.getAttribute('aria-describedby'));
      control.setAttribute('aria-invalid', 'true');
      control.setAttribute('aria-describedby', [marked.get(control), id].filter(Boolean).join(' '));
      links.append(el('li', {}, el('a', { id, href: `#${control.id}`, onclick: event => { event.preventDefault(); revealField(control); } }, message)));
    }
    if (links.children.length) root.append(links);
    region.replaceChildren(root); region.setAttribute('tabindex', '-1');
    region.focus({ preventScroll: true }); region.scrollIntoView?.({ block: 'nearest' });
  };
  let invalid = [];
  form.addEventListener('invalid', event => {
    event.preventDefault();
    invalid.push({ control: event.target, message: `${event.target.getAttribute('aria-label') || 'Field'}: ${event.target.validationMessage}` });
    if (invalid.length === 1) queueMicrotask(() => { show(new Error('Check the highlighted fields.'), invalid); invalid = []; });
  }, true);
  form.addEventListener('input', clear);
  form.addEventListener('change', clear);
  return { clear, error: error => show(error, error.field ? [{ control: error.field, message: error.message }] :
    (Array.isArray(error.errors) ? error.errors : []).map(item => ({ control: target(item.key), message: item.message || item.hint || 'Check this value.' }))) };
}
function safeEvidence(value) {
  if (value === null || typeof value !== 'object') return String(value).slice(0, 2048);
  // Structured diagnostics may grow. Only numeric/boolean evidence is rendered
  // from unknown objects, never environment maps, request bodies or credentials.
  return JSON.stringify(Object.fromEntries(Object.entries(value).filter(([, item]) => typeof item === 'number' || typeof item === 'boolean').slice(0, 20))).slice(0, 2048);
}
export const date = value => value ? new Date(value).toLocaleString() : 'No expiry';
export const money = value => Number(value || 0).toLocaleString(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 4 });

// Rows keep native table semantics; the name button provides keyboard activation.
export function selectableRow(key, selected, open, ...cells) {
  return el('tr', { class: 'selectable-row', 'data-selection': key, 'aria-current': String(key === selected), onclick: event => {
    if (!event.target.closest?.('button, a, input, select, textarea')) open();
  } }, cells);
}
export function markSelection(root, selected) {
  for (const item of root.querySelectorAll('[data-selection]')) item.setAttribute('aria-current', String(item.getAttribute('data-selection') === selected));
}
export function editorActions(save, cancel, api) {
  save.setAttribute('data-protected-actions', '');
  return el('div', { class: 'editor-actions' }, save, leaveButton('Cancel', cancel, api));
}
