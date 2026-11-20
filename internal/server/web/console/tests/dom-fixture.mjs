import assert from 'node:assert/strict';

// Small DOM/event fixture for the real console modules. Browser-native input
// constraints, layout, clipboard permissions and unload prompts remain covered
// by the coordinating Chrome acceptance journey.
export class Element extends EventTarget {
  constructor(tag) {
    super();
    this.tag = tag;
    this.childNodes = [];
    this.attributes = new Map();
    this.dataset = {};
    this.listeners = new Map();
    this.classList = { add() {}, remove() {} };
    this.disabled = false;
    this.checked = false;
  }
  get isConnected() { return this === document.querySelector('#app') || !!this.parent?.isConnected; }
  get parentElement() { return this.parent; }
  get open() { return this.attributes.has('open'); }
  set open(value) { if (value) this.setAttribute('open', ''); else this.removeAttribute('open'); }
  get children() { return this.childNodes.filter(child => child.tag !== '#text'); }
  get childElementCount() { return this.children.length; }
  get firstChild() { return this.childNodes[0]; }
  get textContent() { return (this._text || '') + this.childNodes.map(child => child.textContent).join(''); }
  set textContent(value) { this._text = String(value); this.childNodes = []; }
  get value() {
    if (this._value !== undefined) return this._value;
    if (this.tag === 'select') {
      const options = walk(this).filter(child => child.tag === 'option');
      return (options.find(child => child.selected) || options[0])?.value || '';
    }
    return this.tag === 'textarea' ? this.textContent : '';
  }
  set value(value) { this._value = String(value); }
  get type() { return this.attributes.get('type') || ''; }
  set type(value) { this.attributes.set('type', value); }
  setAttribute(key, value) { this.attributes.set(key, String(value)); }
  getAttribute(key) { return this.attributes.get(key); }
  removeAttribute(key) { this.attributes.delete(key); }
  append(...children) {
    for (const child of children) {
      const node = child instanceof Element ? child : new Element('#text');
      if (node !== child) node.textContent = String(child);
      node.parent = this;
      this.childNodes.push(node);
    }
  }
  prepend(...children) { for (const child of children) child.parent = this; this.childNodes.unshift(...children); }
  after(node) {
    const siblings = this.parent.childNodes;
    node.parent = this.parent;
    siblings.splice(siblings.indexOf(this) + 1, 0, node);
  }
  replaceChildren(...children) { this._text = ''; this.childNodes = []; this.append(...children); }
  remove() { this.parent.childNodes = this.parent.childNodes.filter(child => child !== this); }
  addEventListener(name, handler, options) { this.listeners.set(name, handler); super.addEventListener(name, handler, options); }
  querySelectorAll(selector) {
    return walk(this).slice(1).filter(node => {
      if (selector.startsWith('input[')) return node.type === 'password' || node.attributes.has('data-sensitive');
      if (selector === 'input, select, textarea') return ['input', 'select', 'textarea'].includes(node.tag);
      const attribute = selector.match(/^\[([^\]]+)\]$/)?.[1];
      return attribute ? node.attributes.has(attribute) : false;
    });
  }
  contains(node) { return walk(this).includes(node); }
  showModal() { this.setAttribute('open', ''); }
  show() { this.setAttribute('open', ''); }
  close() { this.removeAttribute('open'); }
  scrollIntoView() {}
  reportValidity() { return true; }
  setCustomValidity(message) { this.validationMessage = message; }
  focus() { document.activeElement = this; }
  select() {}
}
export function walk(root) { return [root, ...root.childNodes.flatMap(walk)]; }
export function find(root, predicate) { const found = walk(root).find(predicate); assert.ok(found, 'rendered control missing'); return found; }
export function named(root, name) { return find(root, node => node.getAttribute('name') === name); }
export function button(root, label) { return find(root, node => node.tag === 'button' && node.textContent === label); }
export const drain = () => new Promise(resolve => setImmediate(resolve));
export function installDOM(t, overrides = {}) {
  const app = new Element('div');
  const values = {
    Node: Element, BroadcastChannel: undefined, confirm: () => true,
    window: new EventTarget(), location: { origin: 'https://router.example' },
    document: { documentElement: new Element('html'), querySelector: () => app, createElement: tag => new Element(tag),
      createElementNS: (namespaceURI, tag) => Object.assign(new Element(tag), { namespaceURI }),
      createTextNode: value => { const node = new Element('#text'); node.textContent = value; return node; } },
    ...overrides,
  };
  for (const [key, value] of Object.entries(values)) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, key);
    Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
    t.after(() => { if (previous) Object.defineProperty(globalThis, key, previous); else delete globalThis[key]; });
  }
  return app;
}
