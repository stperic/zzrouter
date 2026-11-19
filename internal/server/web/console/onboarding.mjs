import { el, text, button, leaveButton, actions, badge, message, problem } from './dom.mjs';

// Choices are saved by the API; successful requests are evidence owned by it.
export async function onboarding(p) {
  if (!p.identity.owner) return null;
  const root = el('section', { class: 'card onboarding', 'aria-label': 'Getting started', hidden: true });
  p.root.append(root);
  const read = async () => {
    const progress = await p.api.data('/access/onboarding', { signal: p.scope.signal });
    if (!p.current()) return;
    if (progress.dismissed || progress.response_verified) { root.remove(); return; }
    root.hidden = false;
    const feedback = el('div', { 'aria-live': 'polite' });
    const move = async (step, route, item) => {
      if (!p.api.canChange()) return;
      try {
        await p.api.data('/access/onboarding', { method: 'PATCH', body: { step }, signal: p.scope.signal });
        if (p.current()) await p.go(route, item);
      } catch (error) { if (p.current()) feedback.replaceChildren(problem(error)); }
    };
    const steps = [
      ['Account created', 'Your local owner account is ready.', true],
      ['Connect a provider', 'Connect a local provider, cloud provider, or existing endpoint.', progress.provider_ready, 'provider', 'providers'],
      ['Prepare a model', 'Choose or download a model from an available provider.', progress.model_ready, 'model', 'models'],
      ['Test in Chat', 'Send a message to confirm your model works.', progress.response_verified, 'test', 'chat'],
    ];
    const nextStep = steps.find(([, , done]) => !done)?.[3];
    const list = el('ol', { class: 'onboarding-steps' }, steps.map(([label, description, done, step, route, item]) => el('li', {},
      el('div', {}, el('strong', {}, label), text(description, 'muted')),
      done ? badge(['provider', 'model'].includes(step) ? 'Available' : 'Done', 'good') : leaveButton({ provider: 'Choose provider', model: 'Choose model', test: 'Open Chat' }[step], () => move(step, route, item), p.api, step === nextStep ? 'primary' : ''))));
    const dismiss = button('Skip setup for now', async () => {
      if (!p.api.canChange() || dismiss.disabled) return;
      dismiss.disabled = true;
      try {
        await p.api.data('/access/onboarding', { method: 'PATCH', body: { dismissed: true }, signal: p.scope.signal });
        if (p.current()) root.remove();
      } catch (error) { if (p.current()) { feedback.replaceChildren(problem(error)); dismiss.disabled = !!error.uncertain; } }
    }, 'link');
    dismiss.setAttribute('data-protected-actions', '');
    root.replaceChildren(el('h2', {}, 'Get your first model working'),
      text('Use this computer or a cloud provider for your first model. Additional compute nodes are optional.', 'muted'),
      list, feedback, actions(leaveButton('Check progress', refresh, p.api), dismiss));
    p.syncActivity();
  };
  const refresh = async () => {
    try { await read(); }
    catch (error) {
      if (!p.current()) return;
      root.hidden = false;
      root.replaceChildren(message('Setup progress is unavailable.', 'warning'), problem(error), leaveButton('Retry', refresh, p.api));
    }
  };
  await refresh();
  return root;
}
