import test from 'node:test';
import assert from 'node:assert/strict';
import { access } from '../access.mjs';
import { providerFields, modelFields } from '../access-policy.mjs';
import { budgetFields, budgetUsage, budgetLabel } from '../access-budget.mjs';
import { find, named, button, walk, drain, installDOM } from './dom-fixture.mjs';

const catalog = [{key:'ollama',name:'Ollama',kind:'external'}, {key:'vllm',name:'vLLM',kind:'python'}, {key:'groq',name:'Groq',kind:'cloud'}, {key:'huggingface',name:'Hugging Face',kind:'registry'}];
const team = {id:'engineering',name:'Engineering',kind:'shared',provider_access:{mode:'selected',providers:['ollama','groq']},spend_limit:100};
const click = async (root, label) => { button(root,label).dispatchEvent(new Event('click')); await drain(); };
const submit = async root => { find(root,node=>node.tag==='form').dispatchEvent(new Event('submit',{cancelable:true})); await drain(); };
async function fixture(t, {keys=[],teams=[structuredClone(team)],data,catalogRead}={}) {
 const root=installDOM(t), abort=new AbortController(), calls=[]; let selection;
 const p={root,identity:{id:'admin',is_virtual:false},scope:{signal:abort.signal},current:()=>!abort.signal.aborted,go:async(page,item,mode)=>selection(item,mode),handleSelection:handler=>{selection=handler;},syncActivity(){},
  api:{canLeave:()=>true,canChange:()=>true,list:async path=>({'/keys':keys,'/teams':teams,'/providers/catalog?kind=all':catalogRead || catalog,'/models':[],'/model-groups':[]})[path],
   data:async(path,options={})=>{calls.push({path,...options});return data ? data(path,options) : !options.method && /^\/(keys|teams)\/[^/]+$/.test(path) ? [...keys,...teams].find(record=>record.id===path.split('/')[2]) : {...options.body};}},beforeKeyDisclosure:async()=>{}};
 t.after(()=>abort.abort()); await access(p);
 return {root,p,calls,abort};
}

test('provider picker preserves hidden and unavailable selections and separates registries', t=>{
 const root=installDOM(t);
 const f=providerFields({provider_access:{mode:'selected',providers:['groq','retired']}},catalog);root.append(f.root);
 assert.match(root.textContent,/Local providers.*Cloud providers.*Unavailable in catalog/s); assert.doesNotMatch(root.textContent,/Hugging Face/);
 named(root,'provider_search').value='ollama';named(root,'provider_search').dispatchEvent(new Event('input'));
 const checkbox=named(root,'provider');checkbox.checked=true;checkbox.dispatchEvent(new Event('change'));
 assert.deepEqual(f.read(),{mode:'selected',providers:['groq','ollama','retired']});
 named(root,'provider_mode').value='none';assert.deepEqual(f.read(),{mode:'none'});
 named(root,'provider_mode').value='all';assert.deepEqual(f.read(),{mode:'all'});
 named(root,'provider_mode').value='selected';assert.deepEqual(f.read().providers,['groq','ollama','retired']);
});

test('an empty selected provider or model policy never becomes unrestricted', t=>{
 installDOM(t);
 const providers=providerFields({},catalog);named(providers.root,'provider_mode').value='selected';assert.throws(()=>providers.read(),/at least one provider/);
 const models=modelFields({},[],[]);named(models.root,'model_mode').value='selected';assert.throws(()=>models.read(),/at least one model/);
});

test('No limit disables invalid amounts; microdollar budgets retain precision', t=>{
 installDOM(t);
 const f=budgetFields({spend_limit:10});named(f.root,'spend_limit').value='-1';
 named(f.root,'budget_mode').value='none';named(f.root,'budget_mode').dispatchEvent(new Event('change'));
 assert.equal(named(f.root,'spend_limit').disabled,true);assert.equal(f.read().spend_limit,0);
 assert.match(budgetLabel({spend_limit:0.00005}),/0\.00005/);
});

test('budget uses one current server snapshot and keeps untracked usage distinct', async t=>{
 const root=installDOM(t),p={scope:{signal:new AbortController().signal},current:()=>true,api:{data:async()=>({tracked:true,spend_limit:50,period:'daily',spend_usd:12,reserved_usd:3,remaining_usd:35,reset_at:'2030-01-02T00:00:00Z'})}};
 root.append(budgetUsage(p,'keys',{id:'key',spend_limit:100,reset_period:'monthly'}));await drain();
 assert.match(root.textContent,/50\.00 \/ daily/);assert.doesNotMatch(root.textContent,/100\.00|monthly/);assert.match(root.textContent,/35\.00.*UTC/s);
 p.api.data=async()=>({tracked:false,spend_limit:50,period:'monthly'});root.replaceChildren(budgetUsage(p,'keys',{id:'key',spend_limit:50}));await drain();assert.match(root.textContent,/not yet tracked/);assert.doesNotMatch(root.textContent,/Spent|\$0\.00/);
});

test('key creation requires a shared team and sends no individual policy or limits', async t=>{
 const f=await fixture(t);await click(f.root,'+ Create key');
 named(f.root,'name').value='Code editor';named(f.root,'name').dispatchEvent(new Event('input'));
 assert.equal(named(f.root,'id').value,'code-editor');
 await submit(f.root);assert.equal(f.calls.filter(call=>call.method).length,0);assert.match(f.root.textContent,/Choose a shared team, or create one/);
 named(f.root,'team_id').value='engineering';named(f.root,'team_id').dispatchEvent(new Event('change'));await drain();
 assert.match(f.root.textContent,/Engineering policy.*Groq.*Ollama/s);
 const create=walk(f.root).find(node=>node.tag==='button' && node.getAttribute('type')==='submit');assert.equal(create.parent,button(f.root,'Cancel').parent);
 await submit(f.root);const call=f.calls.find(call=>call.method);assert.equal(call.body.team_id,'engineering');assert.equal(call.body.team_role,'member');assert.equal(call.body.id,'code-editor');
 assert.deepEqual(Object.keys(call.body).sort(),['description','id','name','role','team_id','team_role']);
 for (const name of ['provider_mode','model_mode','spend_limit','reset_period','rpm_limit','tpm_limit','max_parallel_requests','default_max_tokens']) assert.equal(walk(f.root).some(node=>node.getAttribute('name')===name),false);
 assert.equal(walk(f.root).filter(node=>node.tag==='option').some(node=>node.value==='__personal__'),false);
});

test('non-Latin names reveal the editable required identifier', async t=>{
 const f=await fixture(t);await click(f.root,'+ Create key');
 named(f.root,'name').value='研究';named(f.root,'name').dispatchEvent(new Event('input'));
 const identifier=find(f.root,node=>node.tag==='details' && node.textContent.includes('Identifier & description'));
 assert.equal(identifier.getAttribute('open'),'');assert.equal(named(f.root,'id').required,true);
});

test('creating a shared team retains the key draft, completed usage, and selection focus', async t=>{
 let resolveUsage;
 const usage=new Promise(resolve=>{resolveUsage=resolve;});
 const f=await fixture(t,{data:async(path,options)=>options.method==='POST'?{...options.body,kind:'shared'}:usage});
 await click(f.root,'+ Create key');named(f.root,'name').value='Draft client';named(f.root,'name').dispatchEvent(new Event('input'));
 named(f.root,'team_id').value='engineering';named(f.root,'team_id').dispatchEvent(new Event('change'));
 await click(f.root,'+ Create shared team');
 resolveUsage({tracked:true,spend_limit:100,period:'monthly',spend_usd:2,reserved_usd:0,remaining_usd:98});await drain();
 await click(f.root,'Cancel');
 assert.equal(named(f.root,'team_id').value,'engineering');assert.doesNotMatch(f.root.textContent,/Loading usage/);assert.match(f.root.textContent,/98\.00/);
 await click(f.root,'+ Create shared team');
 named(f.root,'name').value='New team';named(f.root,'name').dispatchEvent(new Event('input'));
 await submit(f.root);
 assert.equal(named(f.root,'name').value,'Draft client');assert.equal(named(f.root,'id').value,'draft-client');assert.equal(named(f.root,'team_id').value,'new-team');assert.equal(document.activeElement,named(f.root,'team_id'));
 assert.equal(f.calls.filter(call=>call.method).length,1);assert.equal(f.calls.find(call=>call.method).path,'/teams');
});

test('uncertain team creation reloads teams while preserving an unissued key draft', async t=>{
 const f=await fixture(t,{data:async(path,options)=>{if(options.method)throw Object.assign(new Error('Lost response'),{uncertain:true});return {};}});
 await click(f.root,'+ Create key');named(f.root,'name').value='Keep me';await click(f.root,'+ Create shared team');
 named(f.root,'name').value='Maybe created';named(f.root,'name').dispatchEvent(new Event('input'));await submit(f.root);assert.match(f.root.textContent,/Do not resubmit/);
 await click(f.root,'Reload access');assert.equal(named(f.root,'name').value,'Keep me');assert.match(f.root.textContent,/Teams reloaded.*key has not been created/s);assert.equal(f.calls.filter(call=>call.method).length,1);
});

test('an old team recovery response cannot replace a newly opened draft', async t=>{
 const f=await fixture(t,{data:async(path,options)=>{if(options.method)throw Object.assign(new Error('Lost response'),{uncertain:true});return {};}});
 await click(f.root,'+ Create key');await click(f.root,'+ Create shared team');
 named(f.root,'name').value='Old team';named(f.root,'name').dispatchEvent(new Event('input'));await submit(f.root);
 let release;
 f.p.api.list=()=>new Promise(resolve=>{release=resolve;});
 await click(f.root,'Reload access');await click(f.root,'Cancel');await click(f.root,'+ Create shared team');
 named(f.root,'name').value='New draft';release([team]);await drain();
 assert.equal(named(f.root,'name').value,'New draft');assert.doesNotMatch(f.root.textContent,/Teams reloaded/);
});

test('slow access choices do not steal focus after the user moves to search', async t=>{
 let release;
 const f=await fixture(t,{catalogRead:new Promise(resolve=>{release=resolve;})});
 await click(f.root,'+ Create key');const search=named(f.root,'access_search');search.focus();
 release(catalog);await drain();
 assert.equal(document.activeElement,search);assert.ok(named(f.root,'name'));
});

test('personal-team policy opens as a read-only view', async t=>{
 const f=await fixture(t,{teams:[{...team,id:'personal-client',kind:'personal'}]});
 await access(f.p,'teams');await click(f.root,'Engineering');
 assert.match(f.root.textContent,/This personal team belongs to an existing key/);assert.ok(find(f.root,node=>node.tag==='dialog'));
});

test('pagination guards boundaries independently of global activity updates and search aborts old rows', async t=>{
 const teams=Array.from({length:25},(_,i)=>({...team,id:`team-${i}`,name:`Team ${i}`,spend_limit:5}));
 const f=await fixture(t,{teams,data:async()=>({tracked:false,spend_limit:5})});await access(f.p,'teams');
 const originalSignals=f.calls.map(call=>call.signal);assert.equal(originalSignals.length,20);const previous=button(f.root,'Previous');assert.equal(previous.disabled,true);
 previous.dispatchEvent(new Event('click'));assert.match(f.root.textContent,/25 teams · 1–20/);
 await click(f.root,'Next');assert.match(f.root.textContent,/25 teams · 21–25/);assert.ok(originalSignals.every(signal=>signal.aborted));
 named(f.root,'access_search').value='Team 24';named(f.root,'access_search').dispatchEvent(new Event('input'));await new Promise(resolve=>setTimeout(resolve,170));
 assert.match(f.root.textContent,/1 teams · 1–1/);assert.equal(walk(f.root).filter(node=>node.tag==='tr').length,2);
});


test('key details show inherited limits and preserve existing individual restrictions on rename', async t=>{
 const record={id:'client',name:'Client',team_id:team.id,role:'user',spend_limit:7,rpm_limit:10,tpm_limit:200,max_parallel_requests:1,default_max_tokens:50};
 const inherited={...team,rpm_limit:60,tpm_limit:6000,max_parallel_requests:4,default_max_tokens:500};
 const f=await fixture(t,{keys:[record],teams:[inherited],data:async(path,options)=>options.method ? {...record,...options.body} : path.endsWith('/usage') ? {tracked:false,spend_limit:path.startsWith('/teams')?100:7} : record});
 const row=find(f.root,node=>node.tag==='tbody');
 assert.match(row.textContent,/100\.00 \/ monthly/);assert.match(row.textContent,/Additional key budget:.*7\.00/s);
 await click(f.root,'Client');await click(f.root,'Edit');
 assert.match(f.root.textContent,/Engineering policy.*Usage limits.*Requests per minute60.*Tokens per minute6,000.*Parallel requests4/s);
 assert.match(f.root.textContent,/Additional key limits.*read-only.*Requests per minute10/s);
 assert.equal(walk(f.root).some(node=>node.getAttribute('name')==='spend_limit'),false);assert.equal(walk(f.root).some(node=>node.getAttribute('name')==='rpm_limit'),false);
 named(f.root,'name').value='Renamed';await submit(f.root);
 assert.deepEqual(f.calls.find(call=>call.method==='PATCH').body,{name:'Renamed'});
});

test('team editor owns provider access, budget and throughput settings', async t=>{
 const f=await fixture(t);await access(f.p,'teams');await click(f.root,'+ Create team');
 named(f.root,'name').value='Local design';named(f.root,'name').dispatchEvent(new Event('input'));
 named(f.root,'provider_mode').value='selected';named(f.root,'provider_mode').dispatchEvent(new Event('change'));
 const ollama=walk(f.root).find(node=>node.getAttribute('name')==='provider' && node.value==='ollama');ollama.checked=true;ollama.dispatchEvent(new Event('change'));
 named(f.root,'budget_mode').value='limited';named(f.root,'budget_mode').dispatchEvent(new Event('change'));named(f.root,'spend_limit').value='25';
 named(f.root,'rpm_limit').value='60';await submit(f.root);
 const call=f.calls.find(call=>call.method==='POST');assert.equal(call.path,'/teams');assert.deepEqual(call.body.provider_access,{mode:'selected',providers:['ollama']});assert.equal(call.body.spend_limit,25);assert.equal(call.body.rpm_limit,60);
});

test('personal key retains inspection and lifecycle actions without individual limit editors', async t=>{
 const record={id:'client',name:'Client',team_id:'personal-client',role:'user',spend_limit:5};
 const f=await fixture(t,{keys:[record],teams:[{id:'personal-client',name:'Client',kind:'personal'}]});await click(f.root,'Client');await click(f.root,'Edit');
 assert.match(f.root.textContent,/personal team.*replacement key.*shared team/s);
 for (const label of ['Rotate key','Suspend','Revoke key']) assert.ok(button(f.root,label));
 assert.equal(walk(f.root).some(node=>node.getAttribute('name')==='spend_limit'),false);assert.equal(walk(f.root).some(node=>node.getAttribute('name')==='provider_mode'),false);
});
