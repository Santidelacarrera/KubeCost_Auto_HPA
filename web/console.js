'use strict';
const byId = id => document.getElementById(id);
let credential = '', tenant = '', base = '', view = 'overview', offset = 0, nextOffset = null, rows = [], requestGeneration = 0, sessionGeneration = 0;
byId('api').value = location.protocol.startsWith('http') ? location.origin : 'http://127.0.0.1:8000';
const titles = {alerts:'Alertas observadas',users:'Usuarios de la organización','predictions/backtests':'Evaluación temporal',overview:'Clústeres',rightsizing:'Recomendaciones',predictions:'Predicciones','consolidation/plans':'Planes de consolidación','finops/costs':'Costos importados','finops/reconciliations':'Conciliaciones','audit/decisions':'Auditoría',licenses:'Historial de licencias',plans:'Planes propuestos'};
function error(message=''){byId('error').textContent=message;byId('error').hidden=!message;}
async function api(path, payload){
  if(!credential) throw new Error('Conecta la API con una credencial.');
  const url=new URL(base+'/v1/'+path);url.searchParams.set('client_id',tenant);
  if(payload===undefined){url.searchParams.set('offset',offset);url.searchParams.set('limit',50);}
  const response=await fetch(url,{method:payload===undefined?'GET':'POST',headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},body:payload===undefined?undefined:JSON.stringify({...payload,client_id:tenant}),signal:AbortSignal.timeout(15000)});
  const data=await response.json();
  if(!response.ok) throw new Error(`${response.status}: ${data.error||'La operación falló'}`);
  return data;
}
function render(){
  const container=byId('results');container.replaceChildren();
  const term=byId('filter').value.toLowerCase();
  const visible=rows.filter(row=>JSON.stringify(row).toLowerCase().includes(term));
  byId('empty').hidden=visible.length>0;byId('empty').textContent=rows.length?'Sin coincidencias en esta página.':'Sin datos registrados para esta sección.';
  renderCharts(visible);
  for(const row of visible){
    const item=document.createElement('details');item.className='record';
    const summary=document.createElement('summary');summary.textContent=[row.cluster_id||row.id||row.name||row.decision_id||row.invoice_id||row.provider||'Registro',row.workload,row.status,row.currency].filter(Boolean).join(' · ');
    const content=document.createElement('pre');content.textContent=JSON.stringify(row,null,2);
    item.append(summary,content);
    if(view==='consolidation/plans' && row.status==='pending_approval'){
      const actions=document.createElement('div');actions.className='actions';
      for(const [label,decision] of [['Aprobar plan','approve'],['Rechazar','reject']]){
        const button=document.createElement('button');button.textContent=label;
        button.addEventListener('click',async()=>{button.disabled=true;try{await api('consolidation/decision',{id:row.id,version:row.version,decision});await refresh();}catch(e){error(e.message);}finally{button.disabled=false;}});
        actions.append(button);
      }
      const note=document.createElement('p');note.textContent='Requiere un aprobador distinto. La aprobación no despacha cambios a Kubernetes.';item.append(note,actions);
    }
    if(view==='alerts' && row.status==='active'){
      const button=document.createElement('button');button.textContent='Reconocer alerta';
      button.addEventListener('click',async()=>{button.disabled=true;try{await api('alerts/ack',{id:row.id,version:row.ack_version});await refresh();}catch(e){error(e.message);}finally{button.disabled=false;}});
      item.append(button);
    }
    container.append(item);
  }
}
async function refresh(){
  const generation=++requestGeneration;
  error();byId('loading').hidden=false;byId('refresh').disabled=true;
  rows=[];render();
  try{
    const data=await api(view==='overview'?'clusters':view);
    if(generation!==requestGeneration)return;
    rows=data.items||data.groups||data.decisions||(data.plans?Object.entries(data.plans).map(([name,limits])=>({name,...limits})):[]);
    nextOffset=data.next_offset??null;
    byId('view-title').textContent=titles[view];
    byId('page-label').textContent=`${offset+1}–${offset+rows.length} · ${data.total??rows.length} registros`;
    byId('previous').disabled=offset===0;byId('next').disabled=nextOffset===null;
    if(view==='overview'){
      byId('cluster-count').textContent=data.total;
      byId('online-count').textContent=rows.filter(r=>r.status==='ONLINE').length+(data.next_offset!==null?' (página)':'');
    }
    byId('connection').textContent='API conectada · '+tenant;render();
  }catch(e){if(generation===requestGeneration){error(e.message);byId('connection').textContent='Error de conexión o permisos';}}
  finally{if(generation===requestGeneration){byId('loading').hidden=true;byId('refresh').disabled=false;}}
}
byId('connect-form').addEventListener('submit',async event=>{
  event.preventDefault();error();
  try{
    const url=new URL(byId('api').value);
    if(url.protocol!=='https:' && !(url.protocol==='http:' && ['127.0.0.1','localhost','[::1]'].includes(url.hostname)))throw new Error('La API remota requiere HTTPS.');
    if(url.username||url.password||url.search||url.hash)throw new Error('Usa una URL de API sin credenciales ni parámetros.');
    sessionGeneration++;
    byId('admin-result').textContent='';
    byId('admin-status').textContent='';
    byId('operation-status').textContent='';
    byId('checkout-link').hidden=true;
    byId('checkout-link').removeAttribute('href');
    base=url.href.replace(/\/$/,'');tenant=byId('tenant').value.trim();credential=byId('credential').value;
    byId('credential').value='';offset=0;await refresh();
  }catch(e){error(e.message);}
});
byId('disconnect').addEventListener('click',()=>{credential='';tenant='';rows=[];sessionGeneration++;adminRender();operationRender();byId('operation-status').textContent='';requestGeneration++;byId('credential').value='';byId('cluster-count').textContent='—';byId('online-count').textContent='—';byId('connection').textContent='Sin conexión';byId('loading').hidden=true;byId('refresh').disabled=false;byId('previous').disabled=true;byId('next').disabled=true;error();render();});
document.querySelectorAll('[data-view]').forEach(button=>button.addEventListener('click',()=>{view=button.dataset.view;offset=0;document.querySelectorAll('[data-view]').forEach(b=>b.setAttribute('aria-current',String(b===button)));refresh();}));
byId('filter').addEventListener('input',render);byId('refresh').addEventListener('click',refresh);
byId('previous').addEventListener('click',()=>{offset=Math.max(0,offset-50);refresh();});byId('next').addEventListener('click',()=>{if(nextOffset!==null){offset=nextOffset;refresh();}});
const targetFields = [
  ['cluster_id', 'Clúster'], ['workload', 'Workload'],
  ['namespace', 'Namespace'], ['pod_name', 'Pod'], ['container_name', 'Contenedor']
];
const operationFields = {
  'agents/register': [
    ['cluster_id', 'Identificador del clúster'], ['cloud_provider', 'Proveedor', ['gcp', 'azure', 'other']],
    ['execution_mode', 'Modo', ['OBSERVE_ONLY', 'ASSISTED', 'AUTOMATED']]
  ],
  rightsizing: [...targetFields, ['current.cpu_request_millicores', 'CPU solicitada (millicores)', 'number'],
    ['current.cpu_limit_millicores', 'Límite de CPU (millicores)', 'number'],
    ['current.memory_request_mib', 'Memoria solicitada (MiB)', 'number'],
    ['current.memory_limit_mib', 'Límite de memoria (MiB)', 'number']],
  predictions: targetFields,
  'predictions/backtest': targetFields,
  'consolidation/plans': [['cluster_id', 'Identificador del clúster'], ['mode', 'Modo', ['observe-only', 'assisted']],
    ['execution_strategy', 'Ejecución', ['empty-only', 'stateless-eviction']],
    ['nodes', 'Inventario de nodos (.json)', 'file']],
  'finops/import': [['provider', 'Proveedor', ['gcp', 'azure', 'focus']], ['records', 'Registros (.json)', 'file']],
  'finops/reconcile': [['invoice', 'Factura (.json)', 'file']],
  'licenses/revoke': [['id', 'Licencia'], ['version', 'Versión', 'number'], ['reason', 'Motivo']],
  'licenses/activate': [['license', 'Licencia firmada']]
};
operationFields['billing/checkout'] = [['plan', 'Plan del catálogo'], ['request_id', 'Referencia de la operación']];
byId('operation').add(new Option('Abrir pago en Stripe', 'billing/checkout'));
const telemetryOperations = new Set(['rightsizing', 'predictions', 'predictions/backtest']);
function operationRender(){
  const host = byId('operation-fields');
  host.replaceChildren();
  for(const [name, title, type = 'text'] of operationFields[byId('operation').value]){
    const label = document.createElement('label');
    label.textContent = title;
    const input = document.createElement(Array.isArray(type) ? 'select' : 'input');
    input.name = name;
    if(Array.isArray(type)){
      for(const value of type) input.add(new Option(value, value));
    }else{
      input.type = type;
      if(type === 'file') input.accept = '.json,application/json';
      if(type === 'number'){ input.min = '1'; input.step = 'any'; }
    }
    label.append(input);
    host.append(label);
  }
  byId('payload').value = '';
  byId('operation-json').open = false;
  byId('checkout-link').hidden = true;
  byId('checkout-link').removeAttribute('href');
  if(byId('operation').value === 'billing/checkout'){
    host.querySelector('[name=request_id]').value = crypto.randomUUID();
  }
}
async function operationPayload(route){
  const raw = byId('payload').value.trim();
  if(byId('operation-json').open && raw) return JSON.parse(raw);
  const payload = telemetryOperations.has(route) ? {source: 'telemetry'} : {};
  for(const input of byId('operation-fields').querySelectorAll('input,select')){
    if(!input.value || (input.type === 'number' && !input.validity.valid)){
      throw new Error('Completa el campo ' + input.parentElement.firstChild.textContent + '.');
    }
    if(input.type === 'file' && input.files[0].size > 1048576){
      throw new Error('El archivo supera el límite de 1 MiB.');
    }
    const value = input.type === 'file' ? JSON.parse(await input.files[0].text())
      : input.type === 'number' ? input.valueAsNumber : input.value.trim();
    const [root, child] = input.name.split('.');
    if(child) (payload[root] ??= {})[child] = value;
    else payload[root] = value;
  }
  return payload;
}
byId('operation').addEventListener('change', operationRender);
operationRender();
byId('operation-form').addEventListener('submit', async event => {
  event.preventDefault();
  error();
  const button = event.target.querySelector('button');
  const generation = sessionGeneration;
  button.disabled = true;
  byId('operation-status').textContent = 'Validando…';
  byId('checkout-link').hidden = true;
  byId('checkout-link').removeAttribute('href');
  try{
    const route = byId('operation').value;
    const payload = await operationPayload(route);
    if(!payload || Array.isArray(payload) || typeof payload !== 'object') throw new Error('La solicitud debe ser un objeto JSON.');
    if(generation !== sessionGeneration) return;
    const result = await api(route, payload);
    if(generation !== sessionGeneration) return;
    if(route === 'billing/checkout'){
      const link = byId('checkout-link');
      link.hidden = true;
      link.removeAttribute('href');
      if(result.checkout_url){
        const url = new URL(result.checkout_url);
        if(url.origin !== 'https://checkout.stripe.com') throw new Error('Destino de pago inválido.');
        link.href = url.href;
        link.hidden = false;
      }
      byId('operation-status').textContent = `Stripe · ${result.livemode ? 'Producción' : 'Prueba'} · ${result.status}`;
    }else{
      byId('operation-status').textContent = JSON.stringify(result, null, 2);
    }
    await refresh();
  }catch(e){
    if(generation !== sessionGeneration) return;
    error(e.message);
    byId('operation-status').textContent = 'Operación rechazada.';
  }finally{
    button.disabled = false;
  }
});

function renderCharts(visible){
  const host=byId('charts');host.replaceChildren();
  const series=new Map();
  function add(unit,label,value){
    if(value===null||value===undefined||!Number.isFinite(Number(value)))return;
    if(!series.has(unit))series.set(unit,[]);
    series.get(unit).push({label,value:Number(value)});
  }
  for(const row of visible){
    const label=row.workload||row.cluster_id||row.id||row.group||'Registro';
    if(view==='predictions'){
      const unit=row.provenance?.demand_metric||'Demanda (unidad suministrada)';
      add(unit,label+' · predicción',row.predicted_demand);
      add(unit,label+' · observación',row.observed_demand);
    }
    if(view==='rightsizing'){
      add('CPU millicores',label+' · p95 observado',row.observed?.cpu_p95_millicores);
      add('CPU millicores',label+' · request recomendado',row.recommendation?.cpu_request_millicores);
      add('Memoria MiB',label+' · p99 observado',row.observed?.memory_p99_mib);
      add('Memoria MiB',label+' · request recomendado',row.recommendation?.memory_request_mib);
    }
    if(view==='finops/costs')add('Costo facturado '+row.currency,label,row.billed_cost);
  }
  for(const [unit,points] of series){
    const figure=document.createElement('figure');figure.className='data-chart';
    const title=document.createElement('figcaption');title.textContent=unit+' · página y filtro actuales';figure.append(title);
    const min=Math.min(0,...points.map(p=>p.value)), max=Math.max(1,...points.map(p=>p.value));
    for(const point of points){
      const line=document.createElement('label');line.textContent=point.label;
      const meter=document.createElement('meter');meter.min=min;meter.max=max;meter.value=point.value;meter.setAttribute('aria-label',point.label);
      const value=document.createElement('span');value.textContent=point.value.toLocaleString(undefined,{maximumFractionDigits:3});
      line.append(meter,value);figure.append(line);
    }
    host.append(figure);
  }
}

const adminFields={
  users:[['id','Identificador','text'],['display_name','Nombre visible','text'],['role','Rol',['viewer','operator','approver','owner']]],
  'users/update':[['id','Identificador','text'],['display_name','Nombre visible','text'],['role','Rol',['viewer','operator','approver','owner']],['status','Estado',['active','disabled']],['version','Versión actual','number',1]],
  'users/credentials':[['id','Identificador de usuario','text'],['expires_in_days','Vigencia en días','number',30]],
  policies:[['cluster_id','Cluster','text'],['execution_mode','Modo',['OBSERVE_ONLY','ASSISTED','AUTOMATED']],['slo_p99_ms','SLO p99 (ms)','number',200],['min_replicas','Réplicas mínimas','number',2],['max_replicas','Réplicas máximas','number',50],['enable_prewarm','Prewarm','checkbox'],['enable_spot_drain','Spot drain','checkbox']],
  'maintenance/retention':[['days','Conservar últimos días','number',90],['dry_run','Solo vista previa (sin eliminar)','checkbox',true]]
};
function adminRender(){
  const host=byId('admin-fields');host.replaceChildren();byId('admin-result').textContent='';byId('admin-status').textContent='';
  for(const [name,title,type,value] of adminFields[byId('admin-action').value]){
    const label=document.createElement('label');label.textContent=title;
    const input=document.createElement(Array.isArray(type)?'select':'input');input.name=name;input.id='admin-'+name;
    if(Array.isArray(type)){for(const option of type){const node=document.createElement('option');node.value=option;node.textContent=option;input.append(node);}}
    else{input.type=type;if(type==='checkbox')input.checked=Boolean(value);else{input.required=true;input.value=value??'';if(type==='number'){input.min='1';input.step='1';}}}
    label.append(input);host.append(label);
  }
}
byId('admin-action').addEventListener('change',adminRender);adminRender();
byId('admin-form').addEventListener('submit',async event=>{
  event.preventDefault();const button=event.target.querySelector('button');button.disabled=true;byId('admin-result').textContent='';byId('admin-status').textContent='Procesando…';
  const generation=sessionGeneration;
  const payload={};for(const input of byId('admin-fields').querySelectorAll('input,select'))payload[input.name]=input.type==='checkbox'?input.checked:input.type==='number'?Number(input.value):input.value;
  try{const result=await api(byId('admin-action').value,payload);if(generation!==sessionGeneration)return;byId('admin-status').textContent=result.api_key?'Guarda la credencial: se muestra una sola vez.':'Operación completada';byId('admin-result').textContent=JSON.stringify(result,null,2);await refresh();}catch(e){if(generation===sessionGeneration)byId('admin-status').textContent=e.message;}finally{button.disabled=false;}
});
