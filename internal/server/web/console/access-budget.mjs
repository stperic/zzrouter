import { el, text, field, input, select, facts, invalidField } from './dom.mjs';

const dollars = value => Number(value || 0).toLocaleString(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 6 });

export const budgetLabel = record => Number(record?.spend_limit) > 0 ? `${dollars(record.spend_limit)} / ${record.reset_period || 'monthly'}` : 'No budget limit';

export function budgetFields(record) {
  const mode = select('budget_mode', [['none', 'No limit'], ['limited', 'Set a budget']], record.spend_limit > 0 ? 'limited' : 'none');
  const amount = input('spend_limit', record.spend_limit || '', { type: 'number', min: '0', step: 'any' });
  const period = select('reset_period', [['daily', 'Daily'], ['weekly', 'Weekly'], ['monthly', 'Monthly']], record.reset_period || 'monthly');
  const values = el('div', { class: 'form-grid' }, field('Budget (USD)', amount), field('Resets', period, 'Calendar period, at midnight UTC.'));
  const update = () => { values.hidden = mode.value !== 'limited'; amount.required = !values.hidden; amount.disabled = values.hidden; };
  mode.addEventListener('change', update);
  update();
  return {
    fields: { spend_limit: amount, reset_period: period },
    root: el('section', { class: 'access-section' }, el('h3', {}, 'Budget'), field('Spending', mode), values,
      text('Shared by all keys in this team. Applies to priced requests across allowed providers. Free local requests can continue after a budget is used.', 'muted')),
    read: () => {
      if (mode.value === 'limited' && !(Number(amount.value) > 0)) throw invalidField('Enter a budget greater than $0, or choose No limit.', amount);
      return { spend_limit: mode.value === 'limited' ? Number(amount.value) : 0, reset_period: period.value };
    },
  };
}

// The server owns window rollover and reservation arithmetic. Unknown never becomes $0.
export function usageDetails(usage, compact = false) {
  if (usage?.tracked !== true) return text('Usage not yet tracked', 'muted');
  return el('div', {}, el('progress', { max: usage.spend_limit, value: Math.min(usage.spend_limit, usage.spend_usd + (usage.reserved_usd || 0)), 'aria-label': 'Spent and reserved budget' }),
    compact ? text(`${dollars(usage.spend_usd)} spent · ${dollars(usage.reserved_usd)} reserved · ${usage.remaining_usd == null ? 'Available amount unknown' : `${dollars(usage.remaining_usd)} available`}`, 'muted') : [
      facts([['Spent', dollars(usage.spend_usd)], ['Reserved for active requests', dollars(usage.reserved_usd)], ['Available', usage.remaining_usd == null ? 'Unavailable' : dollars(usage.remaining_usd)], ['Resets', usage.reset_at ? `${new Date(usage.reset_at).toLocaleString(undefined, { timeZone: 'UTC' })} UTC` : 'Unavailable']]),
      text('Usage is an estimate from recorded requests, not a provider invoice.', 'muted'),
    ]);
}

export function budgetUsage(p, type, record, { compact = false } = {}) {
  const root = el('div', { class: compact ? 'budget-cell' : 'budget-summary' });
  const label = el('strong', {}, budgetLabel(record));
  root.append(label);
  if (!(record?.spend_limit > 0)) {
    if (!compact) root.append(text('Spending is not tracked at this scope without a budget.', 'muted'));
    return root;
  }
  const status = text('Loading usage…', 'muted');
  root.append(status);
  void p.api.data(`/${type}/${encodeURIComponent(record.id)}/usage`, { signal: p.scope.signal }).then(usage => {
    if (!p.current()) return;
    label.textContent = budgetLabel({ spend_limit: usage.spend_limit, reset_period: usage.period });
    root.replaceChildren(label, usageDetails(usage, compact));
  }).catch(() => { if (p.current()) status.textContent = 'Usage unavailable. The configured limit still applies.'; });
  return root;
}
