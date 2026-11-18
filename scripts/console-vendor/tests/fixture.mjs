import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { featureRequest } from './features-fixture.mjs';
const assets = new URL('../../../internal/server/web/console/', import.meta.url);
const now = () => new Date().toISOString();
const node = (name, gpu, memory, health, os = 'Linux') => ({ name, health_status: health, os, version: 'UX preview', address: name + '.example:9091', last_seen_at: now(), memory: { total_gb: memory }, gpu: { gpus: [{ name: gpu }], unified_memory: os === 'macOS' }, providers: [{ key: 'ollama', name: 'Ollama', kind: 'external', state: 'live', run_lifecycle: 'served', running: health === 'healthy' }, { key: 'comfyui', name: 'ComfyUI', kind: 'python', running: false }] });
const nodes = [node('demo-coordinator', 'Apple M5 Max', 128, 'healthy', 'macOS'), node('studio-gpu', 'RTX PRO 6000', 192, 'healthy'), node('office-pc', 'GeForce RTX 4060', 32, 'healthy', 'Windows'), node('spare-pc', 'GeForce RTX 3090', 64, 'down', 'Windows')];
nodes.find(n => n.name === 'studio-gpu').providers.push({ key: 'vllm', name: 'vLLM', kind: 'python', state: 'live', run_lifecycle: 'process', running: true });
const catalog = [{ key: 'ollama', name: 'Ollama', kind: 'external' }, { key: 'comfyui', name: 'ComfyUI', kind: 'python' }, { key: 'vllm', name: 'vLLM', kind: 'python' }, ...['groq','openrouter'].map(key => ({ key, name: key === 'groq' ? 'Groq' : 'OpenRouter', kind: 'cloud', is_cloud: true, available: true, connection_supported: true })), { key: 'huggingface', name: 'Hugging Face', kind: 'registry', connection_supported: true }];
for (const provider of catalog) provider.protocol = ['huggingface','comfyui'].includes(provider.key) ? '' : provider.key === 'ollama' ? 'ollama' : 'openai';
const providers = nodes.flatMap(n => n.providers.map(p => ({ name: p.key, node: n.name, kind: p.kind, enabled: p.key !== 'comfyui', installed: true })));
providers.push(...['groq','openrouter'].map(name => ({ name, node: nodes[0].name, kind: 'cloud', enabled: true })));
const clients = [{ key_id: 'editor', name: 'Code editor', source: 'managed', protocol: 'inference-mcp', authentication: 'oauth', active_requests: 1, request_count: 37 }, { key_id: 'automation', name: 'Automation', source: 'managed', protocol: 'inference-api', authentication: 'api-key', active_requests: 0, request_count: 128 }].map(c => ({...c, first_seen_at: now(), last_seen_at: now()}));
const teams = [
 { id:'engineering',name:'Engineering',kind:'shared',membership:{people:2,app_keys:2,invitations:0,enrollment_grants:0,owner_protected:true,can_delete:false,can_suspend:false},provider_access:{mode:'selected',providers:['ollama','vllm','groq']},allowed_models:[],spend_limit:100,reset_period:'monthly' },
 { id:'local-only',name:'Local development',kind:'shared',membership:{people:0,app_keys:0,invitations:0,enrollment_grants:0,owner_protected:false,can_delete:true,can_suspend:true},provider_access:{mode:'selected',providers:['ollama','vllm','comfyui']},allowed_models:[],spend_limit:0,reset_period:'monthly' },
];
const keys = [
 {id:'editor',name:'Code editor',role:'user',team_id:'engineering',team_role:'member',spend_limit:25,reset_period:'monthly'},
 {id:'automation',name:'Automation',role:'user',team_id:'engineering',team_role:'member',spend_limit:50,reset_period:'monthly'},
 {id:'lab',name:'Lab notebooks',role:'user',team_id:'local-only',team_role:'owner',spend_limit:0,reset_period:'monthly'},
];
teams[0].route_access={mode:'selected',routes:['coding']};
const routes=[{name:'coding',description:'Fast local coding with cloud fallback',strategy:'priority',replicas:[{name:'primary',node:'studio-gpu',provider:'vllm',model:'qwen3-coder',priority:1},{name:'fallback',node:'office-pc',provider:'groq',model:'llama-3.3-70b-versatile',priority:2}]},{name:'local-chat',description:'Private local inference',strategy:'least-load',replicas:[{name:'chat',node:'studio-gpu',provider:'ollama',model:'llama3.3:70b',priority:1}]}];
let revision=1;
const version=()=>JSON.stringify('preview-version-'+revision);
const connections = new Map(catalog.filter(entry => entry.connection_supported).map(entry => [entry.key, { revision: '1', configured: !!entry.available, enabled: !!entry.available, managed_available: true, scope: 'coordinator', mode: 'managed', present: entry.available ? ['api_key'] : [], fields: [{ id: 'api_key', label: 'API key', type: 'secret', secret: true, required: true }] }]));
const sample = (used,total,stale=false) => ({used_bytes:used*1073741824,total_bytes:total*1073741824,age_seconds:8,observed_at:new Date(Date.now()-8000).toISOString(),stale});
const activity = state => ({state,active_requests:state==='active'?1:0,busy_engines:0,observed_at:now(),age_seconds:0,valid_for_seconds:8,stale:false});
for (const n of nodes) { n.memory.sample=sample(n.name==='studio-gpu'?51:17,n.memory.total_gb,n.health_status!=='healthy');n.activity=activity(n.name==='studio-gpu'?'active':'idle');n.gpu.gpus[0].sample=sample(n.name==='office-pc'?0:12,n.name==='studio-gpu'?96:24,n.health_status!=='healthy');n.gpu.gpus[0].index=0; }
nodes[3].last_error='The worker did not answer the authenticated health probe before its deadline.';
for(const p of providers){p.key=p.name;p.state=p.enabled?'live':'dormant';p.run_lifecycle=p.kind==='cloud'?'none':p.name==='ollama'?'served':'process';p.run_scope=p.name==='comfyui'?'node':'model';}
providers.find(p=>p.name==='vllm').service={supervisor:'zzRouter',running:false,desired_running:true,backoff:'Waiting 15 seconds before retrying the service.',last_exit:{at:now(),reason:'Process exited',detail:{exit_code:1,error_tail:['GPU initialization failed: device unavailable.']}},job_id:'readiness'};
const runs=[{id:'existing',node:'studio-gpu',model:'qwen3-coder',provider:'vllm',status:'running',activity:activity('active'),active_requests:1,started_at:now(),cache:{context_tokens:32768,kv_tokens:65536,sharing:'shared'},memory_commitments_mib:{'GPU-A':20000}},{id:'pin',node:'office-pc',provider:'ollama',model:'llama3.3:70b',status:'stopped',record_only:true,pinned:true}];
const jobRows=Array.from({length:28},(_,index)=>({job_id:'download-'+index,node:'studio-gpu',kind:index%2?'install':'download',epoch:'fixture',seq:10,at:now(),phase:index<2?'running':'done',percent:index<2?42:100,step:index<2?'Downloading model weights':'Complete',bytes:{done:4*1073741824,total:10*1073741824},meta:{model:index<2?'org/new-model':'org/older-'+index},expires_at:index<2?undefined:now()}));
let runReads=0;
let scenario='populated';
const expandedNodes=[...nodes,...Array.from({length:16},(_,i)=>({...structuredClone(nodes[1]),name:'compute-'+String(i+5).padStart(2,'0')}))];
const expandedProviders=[...providers,...expandedNodes.slice(4).flatMap(n=>n.providers.map(p=>({name:p.key,key:p.key,node:n.name,kind:p.kind,enabled:p.key!=='comfyui',installed:true,run_lifecycle:'process',run_scope:'model'})))];
let issuer, signedOut = false;
let initialized = true;
const peopleRows = [{ id: 'owner', name: 'Alex Owner', username: 'alex', role: 'admin', team_id: 'engineering', owner: true, suspended: false, created_at: now() }, { id: 'member', name: 'Morgan Developer', username: 'morgan', role: 'user', team_id: 'engineering', owner: false, suspended: false, created_at: now() }];
const invitations = [];
const grants = new Map();
const progress = { step: '', dismissed: false, provider_ready: true, model_ready: true, response_verified: false, app_connected: false };
const issue = (purpose, values = {}) => {
 const id = 'invitation-' + (grants.size + 1), token = 'synthetic-' + purpose + '-' + id;
 const grant = { id, purpose, team_id: 'engineering', role: 'user', created_at: now(), expires_at: new Date(Date.now() + 3600000).toISOString(), status: 'pending', ...values };
 grants.set(token, grant); if (purpose !== 'setup') invitations.push(grant);
 return { ...grant, token, url: issuer + '/console/#' + (purpose === 'recovery' ? 'recover' : 'invite') + '?token=' + token };
};
const requests = [];
const server = http.createServer(async (req, res) => {
 try {
  const url = new URL(req.url, issuer);
  if(url.pathname==='/__scenario'){scenario=url.searchParams.get('state')||'populated';if(scenario==='setup'){initialized=false;signedOut=true;}else if(scenario==='person'){initialized=true;signedOut=false;}res.writeHead(200,{'Content-Type':'application/json'}).end(JSON.stringify({scenario}));return;}
  if(url.pathname==='/__requests'){res.writeHead(200,{'Content-Type':'application/json'}).end(JSON.stringify(requests));return;}

  res.setHeader('Cache-Control', 'no-store');
  res.setHeader('Content-Security-Policy', "default-src 'none'; script-src 'self'; worker-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'");
  if (url.pathname.startsWith('/console/')) {
   const name = url.pathname.slice(9) || 'index.html';
   if (!/^(?:[a-z-]+\.(?:html|css|mjs)|(?:icons|brand)\/[a-z-]+\.svg|vendor\/(?:events\.mjs|chat\.mjs|chat-transport\.mjs|discovery-markdown\.mjs|graph\.mjs|elk-worker\.min\.js|THIRD_PARTY_NOTICES\.txt))$/.test(name)) { res.writeHead(404).end(); return; }
   res.setHeader('Content-Type', name.endsWith('.mjs') || name.endsWith('.js') ? 'text/javascript' : name.endsWith('.svg') ? 'image/svg+xml' : name.endsWith('.css') ? 'text/css' : 'text/html');
   res.end(await readFile(new URL(name, assets))); return;
  }
  let body = '';
  for await (const chunk of req) body += chunk;
  const payload = body ? JSON.parse(body) : {};
  const path = url.pathname.replace('/zzrouter/v1','');
  requests.push({path,method:req.method,payload,query:Object.fromEntries(url.searchParams),node:req.headers['x-node']});
  if (path === '/oauth/authorize' && req.method === 'GET') {
   res.setHeader('Content-Type', 'text/html');
   res.end('<!doctype html><html lang="en"><title>Review connection</title><main id="content"><h1>Review connection</h1><p>Sign-in is complete. Allowing the application still requires consent.</p><form method="post" action="/oauth/authorize"><button name="action" value="allow">Allow connection</button></form></main></html>');return;
  }
  if (featureRequest({ req, res, path, url, payload, scenario, nodes })) return;
  if(scenario==='field-errors' && path.startsWith('/model-groups/') && req.method==='PATCH'){
   res.writeHead(400,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Check the destination model.',errors:[{key:'replicas[1].model',message:'Choose an available model.'}]}));return;
  }
  let data;
  if (path === '/session') {
   if(scenario==='unverified'){res.writeHead(503,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Session verification unavailable'}));return;}
   if(req.method==='DELETE')signedOut=true;
   if(req.method==='POST'){signedOut=false;if(scenario==='login')scenario='populated';}
   data = { authenticated: !signedOut&&scenario!=='login', csrf: scenario==='changed'?'changed-preview-proof':'synthetic-preview-proof', issuer, sharing: 'local_only', node_name: 'demo-coordinator',expires_at:new Date(Date.now()+3600000).toISOString() };
  }
  else if (path === '/session/password') {
   if (!payload.username || !payload.password) { res.writeHead(401, {'Content-Type':'application/json'}).end(JSON.stringify({detail:'Invalid username or password'})); return; }
   signedOut=false;scenario='person'; data={authenticated:true,csrf:'synthetic-preview-proof',issuer,sharing:'local_only',node_name:'demo-coordinator'};
  }
  else if (path === '/access/setup') {
   if (req.method==='GET') data={initialized};
   else {initialized=true;signedOut=false;scenario='person';Object.assign(peopleRows[0],{username:payload.username,name:payload.name});data={authenticated:true};}
  }
  else if (path === '/access/console-grants/exchange') {
   if(payload.token==='synthetic-session-code'){signedOut=false;scenario='person';res.setHeader('Content-Type','application/json');res.end(JSON.stringify({data:{session:true}}));return;}
   const source=payload.token==='synthetic-setup-code'?{purpose:'setup',team_id:'engineering',role:'admin',expires_at:new Date(Date.now()+600000).toISOString()}:grants.get(payload.token);
   if(!source){res.writeHead(400,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Invalid or expired code'}));return;}
   grants.delete(payload.token);const token='bound-'+payload.token;grants.set(token,source);data={...source,token};
  }
  else if (path === '/access/invitations/inspect') {
   data=grants.get(payload.token);if(!data){res.writeHead(400,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Invitation unavailable'}));return;}
  }
  else if (path === '/access/invitations/accept' || path === '/access/recovery') {
   const grant=grants.get(payload.token);if(!grant){res.writeHead(400,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Invitation already used'}));return;}
   grant.status='accepted';grants.delete(payload.token);signedOut=false;scenario='person';data={authenticated:true};
  }
  else if (path === '/access/invitations') data=req.method==='POST'?issue('invitation',payload):invitations;
  else if (path.startsWith('/access/invitations/')) {const grant=invitations.find(row=>row.id===path.split('/')[3]);if(grant)grant.status='revoked';data={};}
  else if (path === '/access/people') data=peopleRows;
  else if (path.startsWith('/access/people/')) {
   const record=peopleRows.find(person=>person.id===path.split('/')[3]);
   if(path.endsWith('/recovery'))data=issue('recovery',{person_id:record.id,team_id:record.team_id,role:record.role});
   else if(req.method==='PATCH'){Object.assign(record,payload);data=record;}else data=record;
  }
  else if (path === '/access/onboarding') {if(req.method==='PATCH')Object.assign(progress,payload);data=progress;}
  else if (path === '/access/password' || path === '/access/sessions/revoke') data={authenticated:true};
  else if (path === '/access/audit') data={events:[{time:now(),action:'person.created',actor:'person-owner',subject:url.searchParams.has('cursor')?'person-older':'person-member',request_id:'fixture-request',metadata:{team_id:'engineering'}}],next:url.searchParams.has('cursor')?undefined:'fixture-next',skipped:0};
  else if (path === '/system/recovery') data={version:1,storage_config_sha256:'fixture-only',release:'UX preview',platform:'darwin',node:'demo-coordinator',config_dir:'/Library/Application Support/zzrouter',data_dir:'/Library/Application Support/zzrouter',provider_dir:'/Library/Application Support/zzrouter/providers-runtime',key_file:'/Library/Application Support/zzrouter/private/credentials.key',model_paths:['/Library/Application Support/zzrouter/models'],sources:[{name:'config',path:'/Library/Application Support/zzrouter',required:true}],separate:[{path:'/Library/Application Support/zzrouter/models',reason:'Model weights require a separate backup or download.'}]};
  else if(path==='/v1/models')data=scenario==='empty'?[]:[{id:'coding',owned_by:'zzrouter-route',capabilities:{chat:true}},{id:'qwen3-coder@studio-gpu',capabilities:{chat:true},zzrouter:{node:'studio-gpu',provider:'vllm'}},{id:'llama3.3:70b@studio-gpu',capabilities:{chat:true},zzrouter:{node:'studio-gpu',provider:'ollama'}}];
  else if(path==='/v1/chat/completions'){
   res.writeHead(200,{'Content-Type':'text/event-stream','X-zzrouter-Node':'studio-gpu','X-zzrouter-Provider':'vllm'});
   if(scenario==='streaming'){
    res.write('data: '+JSON.stringify({choices:[{delta:{content:'Partial response'},finish_reason:null}]})+'\n\n');
    const timer=setInterval(()=>res.write(': waiting\n\n'),1000);res.on('close',()=>clearInterval(timer));return;
   }
   const content='Here is a quick status check:\n\n- **3 compute nodes healthy**\n- One node needs attention.\n\n```python\nprint("Ready")\n```';
   const events=[{choices:[{delta:{content},finish_reason:null}]},{choices:[{delta:{},finish_reason:'stop'}]},{choices:[],usage:{prompt_tokens:36,completion_tokens:43,total_tokens:79}}];
   for(const event of events)res.write('data: '+JSON.stringify(event)+'\n\n');res.end('data: [DONE]\n\n');return;
  }
  else if (path === '/me' && scenario === 'person') data={...peopleRows[0],kind:'person',limits:{},team:{...teams[0],limits:{spend_limit:100,reset_period:'monthly'}}};
  else if (path === '/me') data = { id: 'demo', name: 'Sample cluster preview', role:scenario==='user'?'user':'admin',is_virtual:scenario==='user',team:scenario==='user'?{...teams[0],limits:{spend_limit:100,reset_period:'monthly'}}:undefined,limits:{} };
  else if (path === '/nodes') { for(const n of nodes){n.activity=activity(n.name==='studio-gpu'?'active':'idle');} data=scenario==='empty'?[{...nodes[0],providers:[]}]:scenario==='large'?expandedNodes:nodes; }
  else if (path === '/clients') { res.setHeader('Content-Type','application/json'); res.end(JSON.stringify({ data: scenario==='empty'?[]:clients, metadata: { observed_since: now(), truncated: false } })); return; }
  else if (path === '/providers/catalog') data=scenario==='empty'?catalog.map(p=>({...p,available:false})):catalog;
  else if (path === '/providers') data = (scenario==='empty'?[]:scenario==='large'?expandedProviders:providers).filter(p => !url.searchParams.get('node') || p.node === url.searchParams.get('node'));
  else if (path.endsWith('/connection/test')) data={status:'ok'};
  else if (path.endsWith('/connection') || path.endsWith('/connection/use-external')) {
    const key=path.split('/')[2], status=connections.get(key);
    if (req.method==='GET') data=status;
    else { if(payload.expected_revision!==status.revision){res.writeHead(409,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Revision changed'}));return;}
      status.revision=String(Number(status.revision)+1);status.configured=req.method!=='DELETE';status.enabled=status.configured&&(payload.activate||status.enabled);status.present=status.configured?['api_key']:[];
      catalog.find(entry=>entry.key===key).available=status.configured;
      data={outcome:'saved',saved:true,durable:true,active:status.enabled,published:true};
    }
  }
  else if (path==='/cluster/pairing/setup') data={coordinator_url:'https://coordinator.example:9091',ca_fingerprint:'sha256:synthetic',worker_port:9091,version:'UX preview',download_url:'https://github.com/stperic/zzrouter/releases',platforms:[['darwin','macOS'],['windows','Windows'],['linux','Linux']].map(([os,label])=>({os,label,archives:os+'-amd64 or arm64',installations:[{id:'service',label:'System service',shell:os==='windows'?'PowerShell as Administrator':'Terminal',startup:os==='darwin'?'Starts when you log in':'Starts at boot',install_command:os==='windows'?'powershell -File .\\install.ps1 -Role worker -Service':'bash ./install.sh --role worker',pair_command:'zzrouter-node cluster pair --no-tui --secure --coordinator-url https://coordinator.example:9091 --ca-fingerprint sha256:synthetic'}]}))};
  else if (path==='/cluster/pairing/accept') { const name='new-worker'; if(!nodes.some(n=>n.name===name))nodes.push(node(name,'CPU',16,'healthy'));res.setHeader('Content-Type','application/json');res.end(JSON.stringify({node_name:name}));return; }
  else if (path.startsWith('/providers/') && req.method === 'PATCH') { const row = providers.find(p => p.name === path.split('/')[2] && p.node === url.searchParams.get('node')); if (row) row.enabled = payload.enabled; data = row; }
  else if (path === '/me/connections') data = [{ id: 'demo-grant', client: 'Code editor', resource: issuer + '/inference/mcp', created_at: now(), expires_at: now() }];
  else if (path === '/models') data = [{name:'llama3.3:70b',node:'studio-gpu',assigned_app:'ollama'}, {name:'qwen3-coder',node:'studio-gpu',assigned_app:'vllm'}].filter(row=>!url.searchParams.get('node')||row.node===url.searchParams.get('node'));
  else if(path==='/runs/preview') data={assessment:{status:'fit',reason:'The reported memory budget admits this request.',required_mib:20000,capacity_mib:60000},environment:{SECRET:'never-display'},full_command:'never-display'};
  else if(path==='/runs'){
   if(scenario==='errors'&&url.searchParams.get('node')==='studio-gpu'){res.writeHead(503,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'The worker did not answer the runs request before its deadline.',code:'node_unavailable',node:'studio-gpu',request_id:'fixture-request-7be34ac2'}));return;}
   if(scenario==='empty'){data=[];}else
   if(req.method==='POST'){const run={id:'started',node:payload.node,provider:payload.provider,model:payload.model,status:'starting',started_at:now()};runs.push(run);runReads=0;data={id:run.id,job_id:'launch-job',node:run.node,status:'starting',accepted:true};}
   else if(url.searchParams.get('node')==='spare-pc'){res.writeHead(503,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Worker is down',code:'node_unavailable',node:'spare-pc'}));return;}
   else data=runs.filter(run=>run.node===url.searchParams.get('node'));
  }
  else if(path.startsWith('/runs/')){const id=decodeURIComponent(path.split('/')[2]);const run=runs.find(run=>run.id===id&&run.node===url.searchParams.get('node'));
   if(!run){res.writeHead(404,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Run no longer listed'}));return;}
   if(req.method==='DELETE'){runs.splice(runs.indexOf(run),1);data={id,status:'stopped'};}
   else {if(id==='started'&&++runReads>1)run.status='running';data=run;}
  }
  else if(path==='/jobs'){const offset=Number(url.searchParams.get('offset')||0),items=jobRows.filter(row=>row.node===url.searchParams.get('node'));res.setHeader('Content-Type','application/json');res.end(JSON.stringify({data:items.slice(offset,offset+25),total:items.length,has_more:offset+25<items.length}));return;}
  else if(path.startsWith('/jobs/')){const id=decodeURIComponent(path.split('/')[2]);data=jobRows.find(row=>row.job_id===id)||{job_id:id,node:url.searchParams.get('node'),kind:'launch',phase:'done',percent:100,epoch:'fixture',seq:5,at:now()};}
  else if (path === '/model-groups') data=scenario==='empty'?[]:routes;
  else if (path.startsWith('/model-groups/')) {
   const [, , encoded, suffix] = path.split('/'); const name=decodeURIComponent(encoded); const record=routes.find(route=>route.name===name);
   if (suffix === 'state') data={replicas:record.replicas.map(replica=>({...replica,health_state:'healthy'}))};
   else if (suffix === 'preview') {
    const allowed=teams.find(team=>team.id===payload.team_id)?.route_access;
    const denied=allowed?.mode==='selected'&&!allowed.routes.includes(name);
    data={team_id:payload.team_id,unchecked:['key_limits','rate_limits','concurrency','endpoint_and_input','model_readiness','memory_capacity'],assignments:[{route:name,route_version:version(),strategy:record.strategy,replica:denied?undefined:record.replicas[0].name,eligible:denied?[]:record.replicas.map(replica=>replica.name),denied_reason:denied?'route_access_denied':undefined,projected_cost_micro:0,projected_cost_source:'zzrouter'}]};
   } else if (req.method==='PATCH') { if(req.headers['if-match']!==version()){res.writeHead(412,{'Content-Type':'application/json'}).end(JSON.stringify({detail:'Route changed. Reload before saving.'}));return;} Object.assign(record,payload); revision++;data=record; }
   else if (req.method==='DELETE') { routes.splice(routes.indexOf(record),1); data={};revision++; }
   else if (req.method==='PUT') { data={name,...payload};routes.push(data);revision++; }
   else { data=record;res.setHeader('ETag',version()); }
  }
  else if (/^\/(keys|teams)(?:\/|$)/.test(path)) {
   const [,kind,id,suffix] = path.split('/');
   const rows = kind === 'keys' ? keys : teams;
   const record = rows.find(row => row.id === id);
   if (req.method === 'POST' && !id) { data = {...payload,kind:kind === 'teams' ? 'shared':undefined}; rows.push(data); if (kind === 'keys') data = {...data,key:'synthetic-preview-key-only'}; }
   else if (req.method === 'DELETE' && record) { rows.splice(rows.indexOf(record),1);data={}; }
   else if (suffix==='rotate' && record) { data={...record,key:'synthetic-rotated-preview-key'}; }
   else if (req.method === 'PATCH' && record) { Object.assign(record,payload); data = record; }
   else if (suffix === 'usage' && record) { const limit=record.spend_limit; data={ tracked:limit>0, spend_limit:limit, spend_usd:limit*.28,reserved_usd:limit*.04,remaining_usd:limit*.68,period:record.reset_period,reset_at:'2026-12-01T00:00:00Z' }; }
   else data=id?record:scenario==='empty'?[]:rows;
  }
  else { res.writeHead(400, { 'Content-Type':'application/json' }).end(JSON.stringify({ detail: 'This sample cluster does not perform this operation.' })); return; }
  res.setHeader('Content-Type','application/json'); res.end(JSON.stringify({ data, total: Array.isArray(data) ? data.length : undefined, has_more: false }));
 } catch { res.writeHead(500).end(); }
});
server.listen(Number(process.argv[2] || 55265), '127.0.0.1', () => { issuer = 'http://127.0.0.1:' + server.address().port; console.log(issuer + '/console/'); });
