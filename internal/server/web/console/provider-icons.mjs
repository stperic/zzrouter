import { el } from './dom.mjs';

// Identity marks only. Configuration and observed health remain separate labels.
const marks = {
  openai: 'openai', anthropic: 'anthropic', azure: 'azure', bedrock: 'bedrock',
  cloudflare: 'cloudflare', gemini: 'gemini', groq: 'groq', openrouter: 'openrouter',
  'ollama-cloud': 'ollama', ollama: 'ollama', comfyui: 'comfyui', vllm: 'vllm',
  huggingface: 'huggingface', mlx: 'mlx', llamacpp: 'llamacpp', strata: 'strata',
};

export const providerIcon = key => Object.hasOwn(marks, key) ? `/console/icons/provider-${marks[key]}.svg` : '/console/icons/provider.svg';
export const providerName = (key, name, tag = 'span') => el(tag, { class: 'provider-name' },
  el('img', { src: providerIcon(key), alt: '', class: 'provider-mark', width: 24, height: 24 }), name);
