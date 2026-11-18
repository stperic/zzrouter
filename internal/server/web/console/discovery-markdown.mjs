import { el } from './dom.mjs';
import { markdownTree } from './vendor/discovery-markdown.mjs';

const discarded = new Set(['script', 'style', 'iframe', 'object', 'embed', 'form', 'video', 'audio', 'svg', 'math', 'template']);
const allowed = new Set('p br hr div span strong em b i del s code pre blockquote ol ul li table thead tbody tfoot tr th td sub sup details summary dl dt dd'.split(' '));

export function externalURL(value, base) {
  if (typeof value !== 'string' || !value.trim()) return undefined;
  try {
    const url = new URL(value, base);
    if (['https:', 'http:'].includes(url.protocol) && !url.username && !url.password) return url.href;
  } catch { /* Ignore malformed provider URLs. */ }
}

export function modelLink(label, href, base, title) {
  const url = externalURL(href, base);
  return url ? el('a', { href: url, title, target: '_blank', rel: 'noopener noreferrer' }, label) : null;
}

function renderNode(node, base, page, inLink = false) {
  if (node.type === 'text') return node.value;
  if (discarded.has(node.tagName)) return null;
  const properties = node.properties || {};
  const children = (node.children || []).map(child => renderNode(child, base, page, inLink || node.tagName === 'a'));
  if (node.tagName === 'a') {
    const label = children.length ? children : properties.href;
    const linkBase = properties.href?.startsWith('#') ? page : base;
    return modelLink(label, properties.href, linkBase, properties.title) || children;
  }
  if (node.tagName === 'img') {
    const label = properties.alt || 'Open image';
    return inLink ? label : modelLink(label, properties.src, base) || label;
  }
  if (node.tagName === 'input') return properties.type === 'checkbox' ? (properties.checked ? '☑ ' : '☐ ') : null;
  if (/^h[1-6]$/.test(node.tagName)) return el('h' + Math.min(6, Number(node.tagName[1]) + 2), {}, children);
  if (!allowed.has(node.tagName)) return children;
  const attrs = node.tagName === 'pre' ? { tabindex: '0', 'aria-label': 'Code block' } : {};
  for (const name of ['colSpan', 'rowSpan', 'start']) {
    if (Number.isInteger(properties[name]) && properties[name] > 0) attrs[name.toLowerCase()] = properties[name];
  }
  return el(node.tagName, attrs, children);
}

function plainNode(node) {
  if (node.type === 'text') return node.value;
  if (discarded.has(node.tagName)) return '';
  if (node.tagName === 'img') return node.properties?.alt || '';
  const value = (node.children || []).map(plainNode).join('');
  return ['root', 'p', 'li', 'br', 'div', 'blockquote', 'pre', 'td', 'th'].includes(node.tagName || node.type) ? ` ${value} ` : value;
}

export function descriptionPreview(text) {
  if (!text || !/[\[<*_~`\r\n]/.test(text)) return text || '';
  return plainNode(markdownTree(text)).replace(/\s+/g, ' ').trim();
}

export function modelMarkdown(source, base, page = base) {
  const contents = renderNode(markdownTree(source), base, page);
  return el('div', { class: 'discovery-markdown' }, contents);
}
