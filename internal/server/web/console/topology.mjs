import { computeCard, layoutInput, layoutPositions, topologySignature } from './topology-data.mjs';
import { providerIcon } from './provider-icons.mjs';

const icons = { client: 'client', compute: 'node', coordinator: 'coordinator', cloud: 'cloud', provider: 'provider' };
const dots = { good: 'healthy', bad: 'down', warn: 'degraded' };
function graphStyle(container) {
  const palette = getComputedStyle(container);
  const color = name => palette.getPropertyValue('--' + name).trim();
  return [
    { selector: 'node', style: { width: 48, height: 48, shape: 'round-rectangle', 'background-opacity': 0, 'border-width': 0,
      label: 'data(label)', color: color('text'), 'font-size': 24, 'font-family': 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif', 'font-weight': 500,
      'text-valign': 'bottom', 'text-margin-y': 10, 'text-wrap': 'wrap', 'text-max-width': 220, 'text-overflow-wrap': 'whitespace', 'line-height': 1.35,
      'text-events': 'yes', 'text-background-opacity': 1, 'text-background-padding': 3,
      'text-background-color': node => node.data('kind') === 'coordinator' ? color('surface') : color('surface-muted'),
      'background-image': node => [node.data('providerKey') ? providerIcon(node.data('providerKey')) : `/console/icons/${icons[node.data('kind')] || 'provider'}.svg`, `/console/icons/status-${dots[node.data('tone')] || 'unknown'}.svg`],
      'background-width': [36, 16], 'background-height': [36, 16], 'background-fit': ['none', 'none'],
      'background-position-x': ['50%', '100%'], 'background-position-y': ['50%', '0%'] } },
    { selector: 'node[kind="coordinator"]', style: { width: 60, height: 60, 'background-width': [44, 16], 'background-height': [44, 16] } },
    { selector: 'node[kind="provider"]', style: { width: 42, height: 42, 'background-width': [30, 14], 'background-height': [30, 14], 'text-max-width': 110 } },
    { selector: 'node[kind="client"], node[kind="cloud"]', style: { 'text-max-width': 180 } },
    { selector: 'node[kind="compute"]', style: { width: computeCard.width, height: computeCard.height,
      'background-color': color('surface'), 'background-opacity': 1, 'border-width': 1.5, 'border-color': color('line-strong'),
      'background-position-x': [20, 46], 'background-position-y': [16, 8],
      'text-valign': 'center', 'text-halign': 'right', 'text-margin-x': -computeCard.width + 78, 'text-margin-y': -computeCard.height / 2 + 34,
      'text-wrap': 'ellipsis', 'text-max-width': computeCard.width - 98, 'text-background-opacity': 0, 'font-weight': 600 } },
    { selector: 'node[kind="metrics"]', style: { width: 1, height: 1, 'background-image': 'none', 'font-size': 22, 'line-height': 1.2, 'font-weight': 400,
      'text-valign': 'bottom', 'text-halign': 'right', 'text-justification': 'left', 'text-margin-y': 0,
      'text-max-width': computeCard.width - 40, 'text-wrap': 'wrap', 'text-background-opacity': 0 } },
    { selector: 'node[kind="activity"]', style: { width: 24, height: 24, shape: 'ellipse', label: '', 'background-image': 'none', 'background-color': color('dot-good'), 'background-opacity': 1, opacity: 0 } },
    { selector: ':parent', style: { 'background-color': color('surface-muted'), 'background-opacity': 0.6, 'border-color': color('line'), 'border-width': 1, 'border-style': 'dashed',
      'background-image': 'none', 'text-valign': 'top', 'text-halign': 'center', 'text-margin-x': 0, 'text-margin-y': 22, 'font-size': 18, 'font-weight': 600,
      'text-wrap': 'none', 'text-background-opacity': 0, padding: 28, 'text-max-width': 260, 'compound-sizing-wrt-labels': 'include' } },
    { selector: 'edge', style: { width: 1.5, 'line-color': color('line-strong'), 'curve-style': 'round-taxi', 'taxi-direction': 'downward', 'taxi-turn': '50%', 'taxi-radius': 8, 'target-arrow-shape': 'none' } },
    { selector: 'edge[tone="bad"]', style: { 'line-color': color('dot-bad'), 'line-style': 'dashed' } },
    { selector: 'node.selected[!owner]', style: { 'underlay-color': color('focus'), 'underlay-padding': 7, 'underlay-opacity': 0.14 } },
    { selector: 'node.selected[kind="compute"]', style: { 'border-color': color('selected-line'), 'background-color': color('selected') } },
  ];
}

// Membership or label-size changes run ELK. Later layouts invalidate earlier ones.
export function graphLayout(cy, elk, signal, failed) {
  let signature;
  let revision = 0;
  let completion = Promise.resolve(false);
  return async elements => {
    if (signal.aborted) return;
    let own = revision;
    try {
      const attached = elements.filter(({ data }) => data.kind === 'compute').flatMap(({ data }) => [
        { data: { id: `metrics:${data.id}`, owner: data.id, kind: 'metrics', selection: data.selection, parent: data.parent, label: [data.caption, data.metrics].filter(Boolean).join('\n') } },
        { data: { id: `activity:${data.id}`, owner: data.id, kind: 'activity', selection: data.selection, parent: data.parent, until: data.activityUntil || 0, active: data.active === true } },
      ]);
      cy.batch(() => {
        const ids = new Set([...elements, ...attached].map(element => element.data.id));
        cy.elements().filter(element => !ids.has(element.id())).remove();
        for (const element of [...elements, ...attached]) {
          const previous = cy.getElementById(element.data.id);
          if (previous.length) previous.data(element.data);
          else cy.add(element);
        }
      });
      const bounds = new Map(cy.nodes().filter(node => !node.isParent() && !node.data('owner')).map(node =>
        [node.id(), node.boundingBox({ includeOverlays: false, includeUnderlays: false })]));
      const input = layoutInput(elements, bounds);
      const next = topologySignature(elements, input);
      if (signature === next) { positionAttached(cy); return completion; }
      signature = next;
      own = ++revision;
      completion = elk.layout(input).then(result => {
        if (signal.aborted || own !== revision) return false;
        const positions = layoutPositions(result);
        cy.nodes().filter(node => !node.isParent() && !node.data('owner')).positions(node => positions.get(node.id()));
        positionAttached(cy);
        return true;
      }).catch(error => {
        if (!signal.aborted && own === revision) { signature = undefined; failed(error); }
        return false;
      });
      return completion;
    } catch (error) {
      if (!signal.aborted && own === revision) { signature = undefined; failed(error); }
    }
  };
}

function positionAttached(cy) {
  cy.nodes('[owner]').positions(node => {
    const point = cy.getElementById(node.data('owner')).position();
    return node.data('kind') === 'metrics' ? { x: point.x - computeCard.width / 2 + 20, y: point.y - computeCard.height / 2 + 72 }
      : { x: point.x - computeCard.width / 2 + 54, y: point.y - computeCard.height / 2 + 16 };
  });
}

// Animation owns its expiry independently of polling or the selected drawer.
export function activityMotion(cy, signal, { now = () => performance.now(), hidden = () => document.hidden, reduced = globalThis.matchMedia?.('(prefers-reduced-motion: reduce)') } = {}) {
  let paused = false, timer;
  function sync() {
    clearTimeout(timer);
    if (signal.aborted) return;
    let deadline = Infinity;
    cy.nodes('[kind="activity"]').forEach(dot => {
      const fresh = dot.data('until') > now();
      const allowed = fresh && dot.data('active') && !paused && !hidden() && !reduced?.matches;
      if (!allowed) { dot.stop(true, false); dot.style('opacity', 0); }
      else if (!dot.animated()) {
        const pulse = () => {
          if (signal.aborted || paused || hidden() || reduced?.matches || dot.removed() || !dot.data('active') || dot.data('until') <= now()) return;
          dot.animate({ style: { opacity: 0.15 } }, { duration: 900 }).animate({ style: { opacity: 0.6 } }, { duration: 900, complete: pulse });
        };
        pulse();
      }
      if (fresh) deadline = Math.min(deadline, dot.data('until'));
    });
    if (Number.isFinite(deadline)) { timer = setTimeout(sync, Math.max(1, deadline - now() + 1)); timer.unref?.(); }
  }
  document.addEventListener?.('visibilitychange', sync, { signal });
  reduced?.addEventListener('change', sync, { signal });
  signal.addEventListener('abort', () => { clearTimeout(timer); cy.nodes('[kind="activity"]').stop(true, false); }, { once: true });
  return { sync, pause(value) { paused = value; sync(); } };
}

// Opening a drawer can narrow the canvas past the clicked icon. Pan only enough
// to keep that icon and its label visible at the locked scale.
export function revealNode(cy, id) {
  const node = cy.getElementById(id);
  if (!node.length || cy.width() < 80 || cy.height() < 80) return;
  const box = node.union(cy.nodes().filter(item => item.data('owner') === id)).renderedBoundingBox();
  const shift = (start, end, size) => end - start > size - 80 ? (size - start - end) / 2 : start < 40 ? 40 - start : end > size - 40 ? size - 40 - end : 0;
  cy.panBy({ x: shift(box.x1, box.x2, cy.width()), y: shift(box.y1, box.y2, cy.height()) });
}

const mapPadding = 24;
const minimumZoom = 0.75;

function centerMap(cy, selection) {
  const node = cy.getElementById(selection || '');
  if (node.length) { cy.center(node); return; }
  const bounds = cy.elements().boundingBox();
  const zoom = cy.zoom();
  const coordinator = cy.getElementById('coordinator');
  const nearestNode = cy.nodes('[kind="compute"]').min(node => Math.abs(node.position('x') - (coordinator.position('x') || 0))).ele;
  const focus = (nearestNode ? coordinator.union(nearestNode) : coordinator).boundingBox();
  const offset = (start, end, first, last, size) => {
    const centered = (size - (start + end) * zoom) / 2;
    if ((end - start) * zoom <= size || !coordinator.length) return centered;
    const min = mapPadding - first * zoom;
    const max = size - mapPadding - last * zoom;
    return min <= max ? Math.max(min, Math.min(max, centered)) : (min + max) / 2;
  };
  cy.pan({ x: offset(bounds.x1, bounds.x2, focus.x1, focus.x2, cy.width()), y: offset(bounds.y1, bounds.y2, focus.y1, focus.y2, cy.height()) });
}

export function initialCamera(cy, selection) {
  if (cy.width() < 80 || cy.height() < 80) return false;
  const viewport = cy.getFitViewport(undefined, mapPadding);
  if (!viewport) return false;
  const zoom = Math.max(minimumZoom, Math.min(1, viewport.zoom));
  cy.zoom(zoom).minZoom(zoom).maxZoom(zoom);
  centerMap(cy, selection);
  return true;
}

export async function topology(container, signal, select, failed) {
  const { cytoscape, ELK } = await import('./vendor/graph.mjs');
  if (signal.aborted) return;
  const worker = new Worker('/console/vendor/elk-worker.min.js');
  let cy;
  try {
    const elk = new ELK({ algorithms: ['layered'], workerFactory: () => worker });
    cy = cytoscape({ container, elements: [], style: graphStyle(container), layout: { name: 'preset' }, userZoomingEnabled: false,
      autoungrabify: true, autounselectify: true, boxSelectionEnabled: false });
    window.addEventListener('console-theme-change', () => cy.style(graphStyle(container)).update(), { signal });
    const motion = activityMotion(cy, signal);
    const layout = graphLayout(cy, elk, signal, failed);
    let width = cy.width(), height = cy.height(), anchor, positioned = false, initialized = false;
    const resize = new ResizeObserver(() => {
      const pan = { ...cy.pan() };
      cy.resize();
      cy.pan({ x: pan.x + (cy.width() - width) / 2, y: pan.y + (cy.height() - height) / 2 });
      width = cy.width(); height = cy.height();
      if (positioned && !initialized) initialized = initialCamera(cy, anchor);
      if (anchor) revealNode(cy, anchor);
    });
    resize.observe(container);
    signal.addEventListener('abort', () => { resize.disconnect(); worker.terminate(); cy.destroy(); }, { once: true });
    worker.addEventListener('error', () => { if (!signal.aborted) failed(new Error('The automatic layout could not load. Use the list view.')); }, { signal });
    cy.on('tap', 'node', event => {
      const selection = event.target.data('selection');
      if (selection) {
        select(selection);
        if (event.target.hasClass('selected')) anchor = event.target.data('owner') || event.target.id();
      }
    });
    cy.on('mouseover', 'node', event => { container.style.cursor = event.target.data('selection') ? 'pointer' : 'grab'; });
    cy.on('mouseout', 'node', () => { container.style.cursor = 'grab'; });
    return {
      async update(elements) {
        const committed = await layout(elements);
        if (signal.aborted) return false;
        if (committed) positioned = true;
        if (positioned && !initialized) initialized = initialCamera(cy, anchor);
        motion.sync();
        return committed;
      },
      pause: value => motion.pause(value),
      select(selection) {
        cy.nodes().removeClass('selected');
        const selected = cy.nodes().filter(node => selection && node.data('selection') === selection).addClass('selected');
        if (!selected.some(node => node.id() === anchor)) anchor = selected.filter(node => node.data('kind') === 'compute')[0]?.id() || selected[0]?.id();
      },
      center() { if (initialized) centerMap(cy, anchor); },
      focus(selection) {
        const node = cy.getElementById(selection);
        if (node.length && cy.width() >= 80 && cy.height() >= 80) {
          anchor = node.id(); cy.center(node.union(node.outgoers('node[kind="provider"]'))); revealNode(cy, anchor);
        }
        container.focus({ preventScroll: true });
      },
    };
  } catch (error) { worker.terminate(); cy?.destroy(); throw error; }
}
