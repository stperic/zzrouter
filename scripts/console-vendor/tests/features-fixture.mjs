// Synthetic management contracts. No provider, model or installed binary is changed.
const now = () => new Date().toISOString();
const discovery = { provider: 'huggingface', api_id: 'example/small-model', display_name: 'Small model', size_bytes: 1024 * 1024, downloads: 1000, tags: ['text-generation'] };
const configured = new Map();
const overrides = [{ provider: 'groq', model: 'llama-3.3-70b-versatile', input_cost_per_1m: 0.5, output_cost_per_1m: 0.8, cache_read_cost_per_1m: 0.1, note: 'Contract fixture' }];
const deployment = { id: 'model-download', model: discovery.api_id, registry: 'huggingface', status: 'downloading', created_at: now(), nodes_total: 1, nodes_complete: 0, nodes: [{ node: 'studio-gpu', status: 'downloading', progress: 40, bytes_downloaded: 400, bytes_total: 1000, job_id: 'download-0' }] };
const log = { id: 'request-one', timestamp: now(), model: 'coding', provider: 'vllm', node: 'studio-gpu', status: 'success', tokens_in: 20, tokens_out: 40, ttft_ms: 15, latency_ms: 100, key_id: 'editor', team_id: 'engineering', payload_available: true, system_prompt: 'Inline secret must not appear in list' };
const schedules = new Map();
const version = { major: 0, minor: 2, patch: 3, build_meta: 'fixture' };
export function featureRequest({ req, res, path, url, payload, scenario, nodes }) {
  const send = (value, status = 200, raw = false) => { res.writeHead(status, { 'Content-Type': 'application/json' }).end(JSON.stringify(raw ? value : { data: value })); return true; };
  const stream = events => {
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    for (const [event, data] of events) res.write(`event: ${event}\ndata: ${typeof data === 'string' ? data : JSON.stringify(data)}\n\n`);
    res.end(); return true;
  };
  if (scenario === 'feature-errors' && ['/update/status', '/inference-logs'].includes(path)) return send({ detail: 'Synthetic observation failure', code: 'unavailable' }, 503, true);
  if (path === '/search') return send(url.searchParams.get('type') === 'providers' ? { results: [{ name: 'huggingface', display_name: 'Hugging Face', sort_options: ['downloads'], has_tags: true }] } : { results: [{ discovery }], pagination: { limit: 30, has_next: false } });
  if (path.startsWith('/models/card/')) return send({ discovery, description: 'A small model for testing the download workflow.' });
  if (path === '/models/show') return send({ details: { family: 'qwen', parameter_size: '7B', format: 'safetensors' } });
  if (path === '/models' && req.method === 'DELETE') return send({ deleted: [], errors: [{ name: payload.models[0].name, error: 'Synthetic model is in use' }] });
  if (path === '/registries/huggingface/variants') return send({ variants: [{ file: 'small-Q4.gguf', quantization: 'Q4_K_M', size_bytes: 1024, size_human: '1 KiB', shards: 1 }], features: [] });
  if (path === '/nodes/compatible') return send({ data: [], metadata: { assessments: nodes.map(row => ({ node: row.name, provider: url.searchParams.get('provider'), status: 'unknown', reason: 'Model not cached. Capacity is not measured.' })) } }, 200, true);
  if (path === '/deployments') {
    if (req.method === 'POST') { Object.assign(deployment, { model: payload.model, registry: payload.registry, file: payload.file }); return send(deployment, 202); }
    return send([deployment]);
  }
  if (path.startsWith('/deployments/')) {
    if (req.method === 'DELETE') { deployment.status = 'cancelled'; deployment.nodes[0].status = 'cancelled'; }
    return send(deployment);
  }
  if (/^\/providers\/[^/]+\/schema$/.test(path)) return send({ parameters: { temperature: { type: 'number', default: 0.7, min: 0, max: 2, description: 'Sampling temperature' }, context_length: { type: 'integer', min: 1 } }, environment: { TEST_TOKEN: { type: 'string', secret: true } }, endpoints: { chat: { parameters: {} } }, diagnostics: { runtimes: { python: {} } } }, 200, true);
  if (/^\/providers\/[^/]+\/resolved$/.test(path)) return send({ parameters: { temperature: { value: '0.7', tier: 'defaults' }, context_length: { value: '4096', tier: 'defaults' } }, environment: { TEST_TOKEN: { value: 'synthetic-private-value', tier: 'defaults' } } }, 200, true);
  if (/^\/providers\/[^/]+\/parameters$/.test(path)) {
    const key = path + '?' + url.searchParams.toString();
    if (req.method === 'PATCH') return send({ applied: true }, 200, true);
    if (!configured.has(key)) configured.set(key, { provider: path.split('/')[2], scope: url.searchParams.get('scope'), node: url.searchParams.get('node'), model: url.searchParams.get('model'), available_endpoints: ['chat_completions', 'embeddings'], parameters: { temperature: '0.7' }, environment: { TEST_TOKEN: 'synthetic-private-value' } });
    return send(configured.get(key), 200, true);
  }
  if (/^\/providers\/[^/]+\/environment$/.test(path)) return send({ installed: true, runtime: 'python', desired_install: { recipe: { components: { audio: { description: 'Audio support' } } } }, installed_manifest: { selected_components: [] }, findings: [] });
  if (/^\/providers\/[^/]+\/install\/preflight$/.test(path)) return send({ all_ok: true, results: [{ check: 'Runtime', passed: true, message: 'Synthetic prerequisites passed' }] });
  if (/^\/providers\/[^/]+\/install\/plan$/.test(path)) return send({ plan_id: 'plan-1', provider: payload.runtime || 'python', version: payload.version || '1.0.0', action: 'install', install_dir: '/managed/provider', steps: [{ step: 0, description: 'Install runtime', command: 'printf synthetic-install' }], current_state: [], resolved_install: { recipe: { components: {} }, selected_components: payload.components || [] } });
  if (/^\/providers\/[^/]+\/install\/verify(?:-step)?$/.test(path)) return send({ all_ok: true, step: payload.step, passed: true, message: 'Synthetic verification passed', steps: [{ step: 0, passed: true, message: 'Available' }] });
  if (/^\/providers\/[^/]+\/(?:install(?:\/execute-step)?|upgrade)$/.test(path)) return send({ accepted: true, job_id: 'install-job', provider: path.split('/')[2], node: url.searchParams.get('node') }, 202);
  if (/^\/providers\/[^/]+\/service\/[^/]+$/.test(path)) return send({ provider: path.split('/')[2], node: url.searchParams.get('node'), running: true, status: 'running', accepted: req.method !== 'GET', job_id: req.method === 'GET' ? undefined : 'service-job', supervisor: 'zzRouter' }, 200, true);
  if (/^\/providers\/[^/]+\/versions$/.test(path)) return send({ provider: path.split('/')[2], latest: '1.0.1', pinned: '', nodes: [{ node: 'studio-gpu', installed: '1.0.0', installable_latest: '1.0.1', status: 'outdated' }] });
  if (path === '/providers/instances') return send({ name: payload.name, node: 'demo-coordinator', enabled: true }, 201);
  if (path !== '/providers/catalog' && /^\/providers\/[^/]+$/.test(path) && req.method === 'GET') return send({ name: path.split('/')[2], node: url.searchParams.get('node'), enabled: true, installed: true, managed: true, state: 'live', version: '1.0.0' });
  if (/^\/runs\/[^/]+\/logs$/.test(path)) return url.searchParams.get('follow') === 'true' ? stream([['log', 'repeated line'], ['log', 'repeated line'], ['done', 'Stream ended']]) : send({ lines: ['old tail'], total_lines: 1, truncated: false });
  if (path === '/inference-logs') return send({ data: [log], has_more: false, total: 1 }, 200, true);
  if (path === '/inference-logs/stream') return send({ job_id: 'inference-stream', stream_url: '/zzrouter/v1/jobs/inference-stream/stream' });
  if (path.endsWith('/payload') && path.startsWith('/inference-logs/')) return send({ request: { messages: [{ role: 'user', content: 'Private fixture prompt' }] }, response: { text: 'Fixture reply' } });
  if (path.startsWith('/inference-logs/')) return send(log);
  if (/^\/jobs\/[^/]+\/stream$/.test(path)) return stream([['events_dropped', { epoch: 'fixture', seq: 1 }], ['progress', { epoch: 'fixture', seq: 2, meta: { entry: { ...log, id: 'live-request' } } }], ['stream_closed', {}]]);
  if (/^\/jobs\/[^/]+$/.test(path) && req.method === 'DELETE') return send({ accepted: true, job_id: path.split('/')[2] }, 202, true);
  if (path === '/pricing/status') return send({ enabled: true, model_count: 1234, fetched_at: now(), unpriced_count: 1, unpriced: { 'custom/model': 3 } });
  if (path === '/pricing/overrides') {
    const target = req.method === 'POST' ? payload : Object.fromEntries(url.searchParams);
    const index = overrides.findIndex(row => row.provider === target.provider && row.model === target.model);
    if (req.method === 'DELETE' && index >= 0) overrides.splice(index, 1);
    if (req.method === 'POST') { if (index < 0) overrides.push(payload); else overrides[index] = payload; }
    return send(req.method === 'GET' ? { overrides, count: overrides.length } : target);
  }
  if (path === '/pricing') return send(url.searchParams.has('model') ? overrides[0] : overrides);
  if (path === '/update/check') return send({ update_available: true, latest_version: '0.2.4' });
  if (path === '/update/status' || path === '/update/history') {
    const selected = url.searchParams.get('nodes');
    return send({ nodes: nodes.filter(row => !selected || selected === 'all' || selected.split(',').includes(row.name)).map(row => row.health_status === 'down' ? { node: row.name, error: 'Node unavailable' } : path.endsWith('/history') ? { node: row.name, history: { entries: [{ timestamp: now(), from_version: version, to_version: version, success: true }] } } : { node: row.name, status: { state: 'idle', enabled: false, current_version: version, latest_version: { major: 0, minor: 2, patch: 4 }, last_check_time: now(), channel: 'stable' } }), rollouts: [] });
  }
  if (path === '/update/settings') {
    const node = url.searchParams.get('node'); if (req.method === 'PATCH') schedules.set(node, payload.enabled);
    return send({ enabled: schedules.get(node) || false });
  }
  if (path === '/update/apply') {
    if (scenario === 'update-interrupted') {
      res.writeHead(202, { 'Content-Type': 'application/json' }); res.write('{"data":');
      setTimeout(() => res.destroy(), 25); return true;
    }
    if (scenario === 'update-invalid') return send({ detail: 'Unknown release version', errors: [{ key: 'version', message: 'Choose an available release.' }] }, 400, true);
    return send({ job_id: 'update-job', version: payload.version, state: 'queued', nodes: payload.nodes.map(node => ({ node, state: 'queued' })) }, 202);
  }
  if (path === '/update/rollback') return send({ delegated_to: 'privileged updater' }, 202);
  return false;
}
