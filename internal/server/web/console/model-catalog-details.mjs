import { el, facts } from './dom.mjs';
import { bytes } from './model-data.mjs';
import { externalURL, modelLink, modelMarkdown } from './discovery-markdown.mjs';

const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' });
const rateFormat = new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 8 });
const hasValue = (value) => value !== null && value !== undefined && value !== '' &&
  (typeof value !== 'object' || Object.values(value).some(hasValue));

function abbreviatedDate(value) {
  if (!value) return undefined;
  if (/\bago$/i.test(value)) return value;
  const date = Date.parse(value);
  if (Number.isFinite(date)) return el('time', { datetime: value }, dateFormat.format(date));
}

export function discoveryDates(model) {
  return [['Updated', abbreviatedDate(model.modified_at)], ['Published', abbreviatedDate(model.created_at)]].filter(([, value]) => value);
}

function labels(values) {
  return [...new Set(values.flatMap(value => Array.isArray(value) ? value : [value])
    .filter(value => typeof value === 'string' && value.trim()))];
}

function textValue(value, base) {
  if (typeof value === 'boolean') return value ? 'Yes' : 'No';
  if (typeof value === 'number') return value.toLocaleString();
  if (typeof value === 'string') return /^https?:\/\//i.test(value) ? modelLink(value, value, base) || value : value;
  if (Array.isArray(value)) {
    const parts = value.filter(hasValue);
    if (parts.every(item => typeof item !== 'object')) return el('span', {}, parts.map((item, index) => [index ? ', ' : '', textValue(item, base)]));
    return el('ul', {}, parts.map(item => el('li', {}, textValue(item, base))));
  }
  return facts(Object.entries(value).filter(([, item]) => hasValue(item)).map(([name, item]) => [metadataLabel(name), textValue(item, base)]));
}

function metadataLabel(key) {
  const words = key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replaceAll('_', ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function expandableDetails(label, content, className) {
  const section = el('details', { class: className }, el('summary', {}, label));
  let loaded = false;
  section.addEventListener('toggle', () => {
    if (!section.open || loaded) return;
    loaded = true;
    section.append(content());
  });
  return section;
}

const commonFields = new Set([
  'api_id', 'model_id', 'id', 'name', 'display_name', 'provider', 'description', 'readme', 'url', 'links',
  'downloads', 'pulls', 'likes', 'size', 'size_bytes', 'context_length', 'context_window', 'max_input_tokens',
  'contextWindow', 'inputTokenLimit', 'max_output_tokens', 'max_tokens', 'outputTokenLimit',
  'created', 'created_at', 'createdAt', 'updated_at', 'modified_at', 'lastModified', 'last_modified',
  'input_types', 'output_types', 'input_modalities', 'output_modalities', 'inputModalities', 'outputModalities',
  'litellm_input_types', 'parameter_sizes', 'variant_count', 'has_cloud',
  'tags', 'license', 'author', 'owned_by', 'pipeline_tag', 'library_name', 'hugging_face_id',
]);

function pricePerMillion(rate, provider) {
  if (!hasValue(rate)) return undefined;
  const value = Number(rate);
  if (value === -1 && provider === 'openrouter') return 'Varies by routed model';
  if (Number.isFinite(value) && value >= 0) return `${rateFormat.format(value * 1_000_000)} / 1M tokens`;
}

export function providerCardInfo(model, native) {
  const base = externalURL(model.url);
  const repository = model.provider === 'huggingface' && base;
  const filesBase = repository ? base.replace(/\/+$/, '') + '/blob/main/' : base;
  const inputs = labels([native.input_types, native.input_modalities, native.architecture?.input_modalities,
    native.litellm_input_types, native.inputModalities]).join(', ');
  const outputs = labels([native.output_types, native.output_modalities, native.architecture?.output_modalities,
    native.outputModalities]).join(', ');
  const number = (...values) => values.find(value => typeof value === 'number' && value > 0)?.toLocaleString();
  const context = number(model.context_length, native.contextWindow, native.inputTokenLimit);
  const maxInput = number(native.max_input_tokens);
  const entries = [
    ['Model ID', model.api_id], ...discoveryDates(model), ['Author', native.author || native.owned_by],
    ['License', native.license], ['Task', native.pipeline_tag], ['Library', native.library_name],
    ['Context length', context], ['Maximum input tokens', maxInput === context ? undefined : maxInput],
    ['Maximum output tokens', number(native.max_output_tokens, native.max_tokens, native.outputTokenLimit, native.top_provider?.max_completion_tokens)],
    ['Inputs', inputs], ['Outputs', outputs], ['Parameters', number(model.parameter_count)],
    ['Parameter sizes', labels([model.parameter_sizes]).join(', ')], ['Versions', number(model.variant_count)],
    ['Storage size', !model.is_cloud && model.provider !== 'ollama' && model.size_bytes > 0 ? bytes(model.size_bytes) : undefined],
    ['Downloads', number(model.downloads)], ['Likes', number(model.likes)],
    ['Trending score', number(model.trending_score)], ['Hosted versions', model.has_cloud ? 'Available' : undefined],
    ['Input price', pricePerMillion(model.price_prompt, model.provider)],
    ['Output price', pricePerMillion(model.price_complete, model.provider)],
    ['Cached input price', pricePerMillion(native.pricing?.input_cache_read, model.provider)],
  ].filter(([, value]) => value !== undefined && value !== null && value !== '');

  const providerLinks = [
    modelLink('View model on provider', base),
    ...Object.entries(native.links || {}).filter(([, value]) => typeof value === 'string')
      .map(([key, url]) => modelLink(key === 'details' ? 'Endpoint details' : metadataLabel(key), url, base)),
    typeof native.hugging_face_id === 'string' && /^[\w.-]+\/[\w.-]+$/.test(native.hugging_face_id)
      ? modelLink('Hugging Face repository', 'https://huggingface.co/' + native.hugging_face_id) : null,
  ].filter(Boolean);
  const remaining = Object.entries(native).filter(([key, value]) =>
    !commonFields.has(key) && !['discovery', 'siblings', 'pricing'].includes(key) && hasValue(value));
  const priceExtras = Object.entries(native.pricing || {}).filter(([key, value]) => !['prompt', 'completion', 'input_cache_read'].includes(key) && hasValue(value));
  if (priceExtras.length) remaining.push(['additional_pricing', Object.fromEntries(priceExtras)]);
  const files = Array.isArray(native.siblings) ? native.siblings.filter(file => file?.rfilename) : [];
  const readme = typeof model.readme === 'string' && model.readme.replace(/^---\r?\n[\s\S]*?\r?\n---(?:\r?\n|$)/, '').trim();

  return {
    base,
    links: providerLinks.length ? el('div', { class: 'actions discovery-links' }, providerLinks) : null,
    facts: facts(entries),
    extras: [
      readme ? expandableDetails('Model card', () => modelMarkdown(readme, filesBase, base), 'discovery-readme') : null,
      remaining.length ? expandableDetails('Provider details', () => facts(remaining.map(([key, value]) => [metadataLabel(key), textValue(value, base)])), 'discovery-extra') : null,
      files.length ? expandableDetails(`Repository files (${files.length.toLocaleString()})`, () =>
        el('ul', { class: 'discovery-files' }, files.map(file => {
          const filename = file.rfilename;
          const href = repository ? filename.split('/').map(encodeURIComponent).join('/') : undefined;
          return el('li', {}, modelLink(filename, href, filesBase) || filename, file.size > 0 ? el('small', {}, bytes(file.size)) : null);
        })), 'discovery-extra') : null,
    ].filter(Boolean),
  };
}
