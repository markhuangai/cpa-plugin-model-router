'use strict';
const CONFIG_API='/v0/management/plugins/model-router/config';
const VALIDATE_API='/v0/management/plugins/model-router/validate';
const API_KEYS_API='/v0/management/api-keys';
const MODELS_API='/v1/models';
const USAGE_API='/v0/management/plugins/model-router/usage';
const ROUTER_FILTER_MODEL_PREFIX='model:';
const ROUTER_FILTER_ATTRIBUTION_PREFIX='attribution:';
const DEFAULT_HIDDEN_GROUP_COLUMNS=['provider','result','router_model','service_tier','source'];
const CPA_STORAGE_PREFIX='enc::v1::';
const CPA_STORAGE_SALT='cli-proxy-api-webui::secure-storage';
const SESSION_KEY='cpa_model_router_key';
const REJECTED_KEY='cpa_model_router_rejected_key';
const REJECTED_AT='cpa_model_router_rejected_at';
const state={enabled:true,priority:'0',routes:[],availableModels:[],catalogLoaded:false,catalogError:'',baselineSnapshot:null,dirty:false,busy:false};
const usageState={
  active:false,initialized:false,preferencesLoaded:false,loading:false,generation:0,controller:null,timer:0,filterTimer:0,displayedRange:null,displayedFilterKey:'',pendingFullRefresh:false,preferenceTimer:0,preferenceSaveGeneration:0,preferenceSaveInFlight:false,preferenceSaveQueued:false,resizeTimer:0,
  overview:null,groupPage:null,requestPage:null,range:'24h',granularity:'hour',customFrom:'',customTo:'',attribution:'',routerModel:'',providerModel:'',source:'',serviceTier:'',result:'',pricingDialogGeneration:0,pricingWriteInFlight:null,
  group:{dimension:'provider_model',sort:'total_tokens',order:'desc',offset:0,limit:50},
  request:{sort:'time',order:'desc',offset:0,limit:50},
  hiddenRequestColumns:new Set(),hiddenGroupColumns:new Set(DEFAULT_HIDDEN_GROUP_COLUMNS),hiddenTokenSeries:new Set(),zoom:{tokens:1,cost:1,efficiency:1},priceBook:null,priceDraft:null,pricingSettingsDirty:false,priceCollectionDirty:false
};
const requestColumns=[
  {key:'time',label:'Time',sort:'time',text:true},{key:'router_model',label:'Router model',sort:'router_model',text:true},{key:'provider_model',label:'Provider model',sort:'provider_model',text:true},
  {key:'provider',label:'Provider',text:true},{key:'source',label:'Source',sort:'source',text:true},{key:'service_tier',label:'Service tier',sort:'service_tier',text:true},
  {key:'result',label:'Result',sort:'result',text:true},{key:'latency',label:'Latency',sort:'latency'},{key:'ttft',label:'TTFT',sort:'ttft'},{key:'tps',label:'Tokens/sec',sort:'tps'},
  {key:'input_tokens',label:'Input'},{key:'output_tokens',label:'Output'},{key:'reasoning_tokens',label:'Reasoning'},{key:'cache_read_tokens',label:'Cache read'},
  {key:'cache_creation_tokens',label:'Cache create'},{key:'total_tokens',label:'Total tokens',sort:'total_tokens'},{key:'cost',label:'Cost',sort:'cost'},{key:'api_key',label:'API key',text:true}
];
const groupColumns=[
  {key:'key',label:'Group',sort:'key',text:true},{key:'router_model',label:'Router model',text:true},{key:'provider_model',label:'Provider model',text:true},
  {key:'provider',label:'Provider',text:true},{key:'source',label:'Source',text:true},{key:'service_tier',label:'Service tier',text:true},{key:'result',label:'Result',text:true},
  {key:'requests',label:'Requests',sort:'requests'},{key:'failed_requests',label:'Failed',sort:'failed_requests'},{key:'input_tokens',label:'Input',sort:'input_tokens'},
  {key:'output_tokens',label:'Output',sort:'output_tokens'},{key:'reasoning_tokens',label:'Reasoning'},{key:'cache_read_tokens',label:'Cache read'},
  {key:'cache_creation_tokens',label:'Cache create'},{key:'total_tokens',label:'Total tokens',sort:'total_tokens'},{key:'latency',label:'Avg latency',sort:'latency'},
  {key:'ttft',label:'Avg TTFT',sort:'ttft'},{key:'tps',label:'Avg tokens/sec',sort:'tps'},{key:'cost',label:'Cost',sort:'cost'}
];
const mastheadEl=document.getElementById('masthead');
const authDockEl=document.getElementById('auth-dock');
const keyEl=document.getElementById('management-key');
const connectEl=document.getElementById('connect');
const authNoteEl=document.getElementById('auth-note');
const workspaceEl=document.getElementById('workspace');
const enabledEl=document.getElementById('plugin-enabled');
const priorityEl=document.getElementById('plugin-priority');
const saveStateEl=document.getElementById('save-state');
const routesEl=document.getElementById('routes');
const countEl=document.getElementById('route-count');
const modelStatusEl=document.getElementById('model-status');
const saveEl=document.getElementById('save');
const reloadEl=document.getElementById('reload');
const configurationActionsEl=document.getElementById('configuration-actions');
const configurationActionsBarEl=document.getElementById('configuration-actions-bar');
const addRouteEl=document.getElementById('add-route');
const toastEl=document.getElementById('toast');
const tabs=Array.from(document.querySelectorAll('[role="tab"]'));
const configurationPanelEl=document.getElementById('configuration-panel');
const usagePanelEl=document.getElementById('usage-panel');
const usageRangeEl=document.getElementById('usage-range');
const usageGranularityEl=document.getElementById('usage-granularity');
const usageRouterModelEl=document.getElementById('usage-router-model');
const usageProviderModelEl=document.getElementById('usage-provider-model');
const usageSourceEl=document.getElementById('usage-source');
const usageServiceTierEl=document.getElementById('usage-service-tier');
const usageResultEl=document.getElementById('usage-result');
const usageCustomRangeEl=document.getElementById('usage-custom-range');
const usageFromEl=document.getElementById('usage-from');
const usageToEl=document.getElementById('usage-to');
const usageStatusEl=document.getElementById('usage-status');
const usageLiveEl=document.getElementById('usage-live');
const usageRefreshEl=document.getElementById('usage-refresh');
const pricingDialogEl=document.getElementById('pricing-dialog');
const resetDialogEl=document.getElementById('reset-dialog');
const chartTooltipEl=document.getElementById('chart-tooltip');
let toastTimer=0;
let metricFitFrame=0;
let configurationActionsAnimation=null;
let configurationActionsAnimationTarget=false;
let configurationActionsResizeObserver=null;
const reducedMotionQuery=window.matchMedia('(prefers-reduced-motion: reduce)');

function safeLocalStorage(){try{return window.localStorage}catch(_error){return null}}
function safeSessionStorage(){try{return window.sessionStorage}catch(_error){return null}}
function readStorageText(storage,name){try{return String(storage&&storage.getItem(name)||'').trim()}catch(_error){return ''}}
function writeStorageText(storage,name,value){try{if(storage)storage.setItem(name,value)}catch(_error){}}
function removeStorageValue(storage,name){try{if(storage)storage.removeItem(name)}catch(_error){}}
function firstText(...values){
  for(const value of values){if(typeof value==='string'&&value.trim())return value.trim()}
  return '';
}
function parseStoredValue(value){
  if(!value)return null;
  try{return JSON.parse(value)}catch(_error){return value}
}
function decodeCPAStorage(value){
  if(!value||!value.startsWith(CPA_STORAGE_PREFIX))return value;
  try{
    const data=Uint8Array.from(atob(value.slice(CPA_STORAGE_PREFIX.length)),character=>character.charCodeAt(0));
    const key=new TextEncoder().encode(CPA_STORAGE_SALT+'|'+window.location.host+'|'+navigator.userAgent);
    const decoded=new Uint8Array(data.length);
    for(let index=0;index<data.length;index++)decoded[index]=data[index]^key[index%key.length];
    return new TextDecoder().decode(decoded);
  }catch(_error){return ''}
}
function readCPAStorageValue(storage,name){
  const raw=readStorageText(storage,name);
  if(!raw)return null;
  const decoded=decodeCPAStorage(raw);
  return parseStoredValue(decoded||raw);
}
function readCPAAuthStoreKey(storage){
  const auth=readCPAStorageValue(storage,'cli-proxy-auth');
  if(!auth||typeof auth!=='object')return '';
  return firstText(auth.state&&auth.state.managementKey,auth.managementKey);
}
function cpaStoredManagementKey(){
  const session=safeSessionStorage();
  const local=safeLocalStorage();
  return firstText(
    readCPAStorageValue(session,'managementKey'),
    readCPAStorageValue(local,'managementKey'),
    readCPAAuthStoreKey(session),
    readCPAAuthStoreKey(local)
  );
}
function recentRejectedManagementKey(){
  const storage=safeSessionStorage();
  const key=readStorageText(storage,REJECTED_KEY);
  if(!key)return '';
  const rejectedAt=Number(readStorageText(storage,REJECTED_AT)||0);
  if(!rejectedAt||Date.now()-rejectedAt>5*60*1000){
    removeStorageValue(storage,REJECTED_KEY);
    removeStorageValue(storage,REJECTED_AT);
    return '';
  }
  return key;
}
function showFallbackKeyInput(message='Enter the CPA management key for this browser session.'){
  authDockEl.hidden=false;
  mastheadEl.classList.add('needs-auth');
  setAuthNote(message);
}
function hideFallbackKeyInput(){
  authDockEl.hidden=true;
  mastheadEl.classList.remove('needs-auth');
}
function rejectManagementKey(key){
  const storage=safeSessionStorage();
  if(key){
    writeStorageText(storage,REJECTED_KEY,key);
    writeStorageText(storage,REJECTED_AT,String(Date.now()));
  }
  removeStorageValue(storage,SESSION_KEY);
  keyEl.value='';
  showFallbackKeyInput('The saved CPAMC management key was rejected. Enter the current key.');
}
function managementKey(){
  const typed=keyEl.value.trim();
  const rejected=recentRejectedManagementKey();
  let key=typed;
  if(!key){
    for(const candidate of [cpaStoredManagementKey(),readStorageText(safeSessionStorage(),SESSION_KEY)]){
      const value=firstText(candidate);
      if(value&&value!==rejected){key=value;break}
    }
  }
  if(key){
    keyEl.value=key;
    writeStorageText(safeSessionStorage(),SESSION_KEY,key);
    removeStorageValue(safeSessionStorage(),REJECTED_KEY);
    removeStorageValue(safeSessionStorage(),REJECTED_AT);
    hideFallbackKeyInput();
  }
  return key;
}
function normalizeColor(value){
  const color=String(value||'').trim();
  if(!color)return '';
  if(/^#|^rgb|^hsl|^oklch|^color\(/i.test(color))return color;
  if(/^-?\d+(\.\d+)?\s+\d+(\.\d+)?%\s+\d+(\.\d+)?%/.test(color))return 'hsl('+color+')';
  return '';
}
function isDarkColor(value){
  const color=String(value||'').trim().toLowerCase();
  let red,green,blue;
  if(color[0]==='#'){
    if(color.length===4){red=parseInt(color[1]+color[1],16);green=parseInt(color[2]+color[2],16);blue=parseInt(color[3]+color[3],16)}
    else if(color.length>=7){red=parseInt(color.slice(1,3),16);green=parseInt(color.slice(3,5),16);blue=parseInt(color.slice(5,7),16)}
  }else{
    const match=color.match(/rgba?\((\d+)[,\s]+(\d+)[,\s]+(\d+)/);
    if(match){red=Number(match[1]);green=Number(match[2]);blue=Number(match[3])}
  }
  return Number.isFinite(red)&&Number.isFinite(green)&&Number.isFinite(blue)&&(red*299+green*587+blue*114)/1000<128;
}
function applyHostTheme(){
  const root=document.documentElement;
  const sources=[document.documentElement,document.body];
  let hostLooksDark=false;
  let hostLooksLight=false;
  try{
    if(window.parent&&window.parent!==window&&window.parent.document){
      sources.unshift(window.parent.document.documentElement,window.parent.document.body);
      const parentRoot=window.parent.document.documentElement;
      const parentTheme=(parentRoot.getAttribute('data-theme')||parentRoot.getAttribute('class')||'').toLowerCase();
      hostLooksDark=parentTheme.includes('dark')||parentTheme.includes('black');
      hostLooksLight=parentTheme.includes('light')||parentTheme.includes('white');
    }
  }catch(_error){}
  const pick=names=>{
    for(const source of sources){
      if(!source)continue;
      const styles=getComputedStyle(source);
      for(const name of names){
        const value=normalizeColor(styles.getPropertyValue(name));
        if(value)return value;
      }
    }
    return '';
  };
  const set=(name,names)=>{const value=pick(names);if(value)root.style.setProperty(name,value)};
  const hostBackground=pick(['--bg-secondary','--cpa-bg','--background','--color-background','--body-bg']);
  const prefersDark=(()=>{try{return matchMedia('(prefers-color-scheme: dark)').matches}catch(_error){return false}})();
  const dark=hostLooksDark||(!hostLooksLight&&(isDarkColor(hostBackground)||(!hostBackground&&prefersDark)));
  root.dataset.hostTheme=dark?'dark':'light';
  root.style.colorScheme=dark?'dark':'light';
  set('--fog',['--bg-secondary','--cpa-bg','--background','--color-background','--body-bg']);
  set('--paper',['--bg-primary','--surface','--color-surface']);
  set('--surface-soft',['--bg-tertiary','--muted-bg','--surface-2']);
  set('--input',['--floating-surface','--bg-primary','--surface']);
  set('--ink',['--text-primary','--foreground','--color-text']);
  set('--muted',['--text-secondary','--muted-foreground','--color-text-secondary']);
  set('--line',['--border-color','--border-secondary','--color-border']);
  set('--line-strong',['--border-primary','--border-hover','--color-border-strong']);
  set('--blue',['--primary-color','--cpa-primary','--color-primary']);
  set('--amber',['--amber-color','--warning-color','--cpa-warning','--color-warning']);
  set('--teal',['--success-color','--cpa-success','--color-success']);
  set('--danger',['--danger-color','--error-color','--cpa-danger','--color-destructive']);
}
function observeHostTheme(){
  const refresh=()=>{applyHostTheme();renderUsageCharts()};
  try{matchMedia('(prefers-color-scheme: dark)').addEventListener('change',refresh)}catch(_error){}
  try{
    if(window.parent&&window.parent!==window&&window.parent.document){
      new MutationObserver(refresh).observe(window.parent.document.documentElement,{attributes:true,attributeFilter:['class','style','data-theme']});
    }
  }catch(_error){}
}
function setBusy(value){
  state.busy=value;
  connectEl.disabled=value;
  saveEl.disabled=value;
  reloadEl.disabled=value;
  addRouteEl.disabled=value;
}
function showToast(message,tone='info'){
  window.clearTimeout(toastTimer);
  toastEl.textContent=message;
  toastEl.dataset.tone=tone;
  toastEl.classList.add('show');
  toastTimer=window.setTimeout(()=>toastEl.classList.remove('show'),4200);
}
function setAuthNote(message,tone='info'){
  authNoteEl.textContent=message;
  authNoteEl.dataset.tone=tone;
}
async function requestCPAJSON(url,key,options={},rejectKey=false){
  const headers=Object.assign({Accept:'application/json'},options.headers||{});
  if(key)headers.Authorization='Bearer '+key;
  const response=await fetch(url,Object.assign({},options,{headers}));
  const text=await response.text();
  let payload={};
  if(text){try{payload=JSON.parse(text)}catch(_error){payload={message:text}}}
  if(!response.ok){
    if(rejectKey&&response.status===401)rejectManagementKey(key);
    const error=new Error(payload.message||payload.error||('CPA returned HTTP '+response.status));
    error.status=response.status;
    throw error;
  }
  return payload;
}
async function requestManagementJSON(url,options={}){
  const key=managementKey();
  if(!key)throw new Error('Enter the CPA management key.');
  return requestCPAJSON(url,key,options,true);
}
