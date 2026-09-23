function setUsageStatus(message,tone='info'){
  usageStatusEl.textContent=message;
  usageStatusEl.dataset.tone=tone;
}
function activateTab(name,moveFocus=false){
  const usage=name==='usage';
  tabs.forEach(tab=>{
    const selected=tab.id===(usage?'usage-tab':'configuration-tab');
    tab.setAttribute('aria-selected',String(selected));
    tab.tabIndex=selected?0:-1;
    if(selected&&moveFocus)tab.focus();
  });
  configurationPanelEl.hidden=usage;
  usagePanelEl.hidden=!usage;
  updateConfigurationActionsClearance();
  usageState.active=usage;
  if(usage)startUsageDashboard();
  else{clearChartActive(false);stopUsagePolling()}
}
function handleTabKey(event){
  const current=tabs.indexOf(event.currentTarget);
  let next=current;
  if(event.key==='ArrowRight')next=(current+1)%tabs.length;
  else if(event.key==='ArrowLeft')next=(current-1+tabs.length)%tabs.length;
  else if(event.key==='Home')next=0;
  else if(event.key==='End')next=tabs.length-1;
  else return;
  event.preventDefault();
  activateTab(tabs[next].id==='usage-tab'?'usage':'configuration',true);
}
function stopUsagePolling(){
  window.clearTimeout(usageState.filterTimer);
  usageState.filterTimer=0;
  window.clearTimeout(usageState.timer);
  usageState.timer=0;
  usageState.generation++;
  if(usageState.controller)usageState.controller.abort();
  usageState.controller=null;
  usageState.loading=false;
  usagePanelEl.setAttribute('aria-busy','false');
  usageRefreshEl.disabled=false;
  usageLiveEl.hidden=true;
}
function scheduleUsagePoll(){
  window.clearTimeout(usageState.timer);
  usageState.timer=0;
  if(!usageState.active||document.hidden)return;
  usageState.timer=window.setTimeout(()=>refreshUsage(false),15000);
}
function startUsageDashboard(){
  usageLiveEl.hidden=document.hidden;
  if(!usageState.initialized){
    usageState.initialized=true;
    setUsageStatus('Loading saved dashboard preferences…','loading');
    const initialization=loadUsagePreferences().catch(error=>{usageState.preferencesLoaded=true;setUsageStatus('Preferences could not be loaded: '+error.message,'error')});
    usageState.initializationPromise=initialization;
    initialization.finally(()=>{
      if(usageState.initializationPromise!==initialization)return;
      usageState.initializationPromise=null;
      if(usageState.active&&!document.hidden)refreshUsage(false);
    });
    return;
  }
  if(usageState.initializationPromise)return;
  refreshUsage(false);
}
function localDateTimeValue(value){
  const pad=number=>String(number).padStart(2,'0');
  return value.getFullYear()+'-'+pad(value.getMonth()+1)+'-'+pad(value.getDate())+'T'+pad(value.getHours())+':'+pad(value.getMinutes())+':'+pad(value.getSeconds());
}
function initializeCustomRange(){
  if(!(usageState.customFrom&&usageState.customTo)){
    const now=new Date();
    usageState.customTo=localDateTimeValue(now);
    usageState.customFrom=localDateTimeValue(new Date(now.getTime()-24*60*60*1000));
  }
  usageFromEl.value=usageState.customFrom;
  usageToEl.value=usageState.customTo;
}
function resolvedUsageRange(){
  const now=new Date();
  let from,to=now;
  if(usageState.range==='5h')from=new Date(now.getTime()-5*60*60*1000);
  else if(usageState.range==='24h')from=new Date(now.getTime()-24*60*60*1000);
  else if(usageState.range==='7d')from=new Date(now.getTime()-7*24*60*60*1000);
  else if(usageState.range==='30d')from=new Date(now.getTime()-30*24*60*60*1000);
  else if(usageState.range==='month')from=new Date(now.getFullYear(),now.getMonth(),1);
  else{
    initializeCustomRange();
    from=new Date(usageState.customFrom);
    to=new Date(usageState.customTo);
  }
  if(Number.isNaN(from.getTime())||Number.isNaN(to.getTime())||from>=to)throw new Error('Choose a valid range with From earlier than To.');
  return{from:from.toISOString(),to:to.toISOString(),label:formatRangeLabel(from,to)};
}
function formatRangeLabel(from,to){
  const options={month:'short',day:'numeric',hour:'2-digit',minute:'2-digit'};
  return new Intl.DateTimeFormat(undefined,options).format(from)+' – '+new Intl.DateTimeFormat(undefined,options).format(to);
}
function usageBaseQuery(range){
  const params=new URLSearchParams({from:range.from,to:range.to,granularity:usageState.granularity});
  for(const [name,value] of [['attribution',usageState.attribution],['router_model',usageState.routerModel],['provider_model',usageState.providerModel],['source',usageState.source],['service_tier',usageState.serviceTier],['result',usageState.result]]){
    if(value)params.set(name,value);
  }
  return params;
}
function usageFilterKey(){
  return JSON.stringify([usageState.range,usageState.customFrom,usageState.customTo,usageState.attribution,usageState.routerModel,usageState.providerModel,usageState.source,usageState.serviceTier,usageState.result]);
}
function appendUsageTableQuery(params,kind,prefix=''){
  const table=usageState[kind];
  if(kind==='group')params.set(prefix+'dimension',table.dimension);
  for(const key of ['sort','order','offset','limit'])params.set(prefix+key,String(table[key]));
}
async function refreshUsage(resetOffsets=false,scope='all'){
  if(!usageState.active||document.hidden)return;
  window.clearTimeout(usageState.timer);usageState.timer=0;
  window.clearTimeout(usageState.filterTimer);usageState.filterTimer=0;
  if(resetOffsets){usageState.group.offset=0;usageState.request.offset=0}
  const filterKey=usageFilterKey();
  if(!usageState.displayedRange||usageState.pendingFullRefresh||usageState.loading||filterKey!==usageState.displayedFilterKey)scope='all';
  let range;
  try{range=scope==='all'?resolvedUsageRange():usageState.displayedRange}catch(error){stopUsagePolling();setUsageStatus(error.message,'error');return}
  if(usageState.controller)usageState.controller.abort();
  const controller=new AbortController();
  const generation=++usageState.generation;
  usageState.controller=controller;
  usageState.loading=true;
  if(scope==='all')usageState.pendingFullRefresh=true;
  usagePanelEl.setAttribute('aria-busy','true');
  usageRefreshEl.disabled=true;
  setUsageStatus(usageState.overview?'Refreshing usage; current data remains visible.':'Loading usage data…','loading');
  const common=usageBaseQuery(range);
  try{
    const options={signal:controller.signal,cache:'no-store'};
    let result;
    if(scope==='all'){
      appendUsageTableQuery(common,'group','group_');appendUsageTableQuery(common,'request','request_');
      result=await requestManagementJSON(USAGE_API+'/dashboard?'+common.toString(),options);
    }else if(scope==='overview'){
      result=await requestManagementJSON(USAGE_API+'/overview?'+common.toString(),options);
    }else if(scope==='group'){
      appendUsageTableQuery(common,'group');
      result=await requestManagementJSON(USAGE_API+'/groups?'+common.toString(),options);
    }else{
      appendUsageTableQuery(common,'request');
      result=await requestManagementJSON(USAGE_API+'/requests?'+common.toString(),options);
    }
    if(generation!==usageState.generation||!usageState.active||document.hidden)return;
    if(scope==='all'){
      usageState.overview=result.overview;usageState.groupPage=result.groups;usageState.requestPage=result.requests;
      usageState.displayedRange=range;usageState.displayedFilterKey=filterKey;usageState.pendingFullRefresh=false;
      renderUsageDashboard();
    }else if(scope==='overview'){
      usageState.overview=result;renderUsageSummary();renderUsageFilterOptions();renderUsageCharts();
    }else if(scope==='group'){
      usageState.groupPage=result;renderGroupTable();
    }else{
      usageState.requestPage=result;renderRequestTable();
    }
    const updated=new Date(result.generated_at);
    const updatedText=Number.isNaN(updated.getTime())?'now':updated.toLocaleTimeString();
    const storageError=usageState.overview&&usageState.overview.storage_error;
    setUsageStatus(range.label+' · updated '+updatedText+(storageError?' · storage warning: '+storageError:''),storageError?'error':'info');
    usageLiveEl.hidden=false;
  }catch(error){
    if(error.name!=='AbortError'&&generation===usageState.generation){
      usageState.pendingFullRefresh=true;
      setUsageStatus('Refresh failed: '+error.message+'. Existing data is unchanged.','error');
    }
  }finally{
    if(generation===usageState.generation){
      usageState.controller=null;
      usageState.loading=false;
      usagePanelEl.setAttribute('aria-busy','false');
      usageRefreshEl.disabled=false;
      scheduleUsagePoll();
    }
  }
}
function validHiddenColumns(values,definitions){
  const allowed=new Set(definitions.map(column=>column.key));
  const result=new Set((Array.isArray(values)?values:[]).filter(value=>allowed.has(value)));
  if(result.size>=definitions.length)result.delete(definitions[0].key);
  return result;
}
function applyUsagePreferences(preferences){
  preferences=preferences||{};
  usageState.range=['5h','24h','7d','30d','month','custom'].includes(preferences.time_range)?preferences.time_range:'24h';
  usageState.granularity=['minute','hour','day'].includes(preferences.granularity)?preferences.granularity:'hour';
  usageState.request.limit=Number(preferences.request_page_size)||50;
  usageState.group.limit=Number(preferences.group_page_size)||50;
  usageState.request.sort=String(preferences.request_sort||'time');
  usageState.request.order=preferences.request_order==='asc'?'asc':'desc';
  usageState.group.dimension=String(preferences.group_dimension||'provider_model');
  usageState.group.sort=String(preferences.group_sort||'total_tokens');
  usageState.group.order=preferences.group_order==='asc'?'asc':'desc';
  usageState.customFrom=String(preferences.custom_from||'');
  usageState.customTo=String(preferences.custom_to||'');
  usageState.hiddenRequestColumns=validHiddenColumns(preferences.hidden_request_columns,requestColumns);
  usageState.hiddenGroupColumns=validHiddenColumns(Array.isArray(preferences.hidden_group_columns)?preferences.hidden_group_columns:DEFAULT_HIDDEN_GROUP_COLUMNS,groupColumns);
  usageState.hiddenTokenSeries=new Set((Array.isArray(preferences.hidden_token_series)?preferences.hidden_token_series:[]).filter(value=>['input','output','cache_read','reasoning'].includes(value)));
  usageRangeEl.value=usageState.range;
  usageGranularityEl.value=usageState.granularity;
  document.getElementById('request-page-size').value=String(usageState.request.limit);
  document.getElementById('group-page-size').value=String(usageState.group.limit);
  document.getElementById('group-dimension').value=usageState.group.dimension;
  usageCustomRangeEl.hidden=usageState.range!=='custom';
  if(usageState.range==='custom')initializeCustomRange();
  renderColumnControls(requestColumns,usageState.hiddenRequestColumns,'request-column-options','request');
  renderColumnControls(groupColumns,usageState.hiddenGroupColumns,'group-column-options','group');
  syncTokenLegend();
}
async function loadUsagePreferences(){
  const preferences=await requestManagementJSON(USAGE_API+'/preferences',{cache:'no-store'});
  applyUsagePreferences(preferences);
  usageState.preferencesLoaded=true;
}
function usagePreferencesPayload(){
  return{
    request_page_size:usageState.request.limit,group_page_size:usageState.group.limit,
    hidden_request_columns:Array.from(usageState.hiddenRequestColumns),hidden_group_columns:Array.from(usageState.hiddenGroupColumns),
    time_range:usageState.range,granularity:usageState.granularity,request_sort:usageState.request.sort,request_order:usageState.request.order,
    group_dimension:usageState.group.dimension,group_sort:usageState.group.sort,group_order:usageState.group.order,
    hidden_token_series:Array.from(usageState.hiddenTokenSeries),custom_from:usageState.customFrom,custom_to:usageState.customTo
  };
}
function scheduleUsagePreferencesSave(){
  if(!usageState.preferencesLoaded)return;
  const generation=++usageState.preferenceSaveGeneration;
  window.clearTimeout(usageState.preferenceTimer);
  usageState.preferenceTimer=window.setTimeout(()=>flushUsagePreferencesSave(generation),250);
}
async function flushUsagePreferencesSave(generation){
  if(generation!==usageState.preferenceSaveGeneration)return;
  if(usageState.preferenceSaveInFlight){
    usageState.preferenceSaveQueued=true;
    return;
  }
  usageState.preferenceSaveInFlight=true;
  usageState.preferenceSaveQueued=false;
  try{
    await requestManagementJSON(USAGE_API+'/preferences',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(usagePreferencesPayload())});
  }catch(error){setUsageStatus('Dashboard preferences could not be saved: '+error.message,'error')}
  finally{
    usageState.preferenceSaveInFlight=false;
    if(usageState.preferenceSaveQueued||generation!==usageState.preferenceSaveGeneration){
      usageState.preferenceSaveQueued=false;
      window.clearTimeout(usageState.preferenceTimer);
      usageState.preferenceTimer=window.setTimeout(()=>flushUsagePreferencesSave(usageState.preferenceSaveGeneration),0);
    }
  }
}
function formatNumber(value){return new Intl.NumberFormat().format(Number(value||0))}
function formatCompact(value){return new Intl.NumberFormat(undefined,{notation:'compact',maximumFractionDigits:2}).format(Number(value||0))}
function formatUSD(value){return new Intl.NumberFormat(undefined,{style:'currency',currency:'USD',minimumFractionDigits:2,maximumFractionDigits:Number(value||0)<1?4:2}).format(Number(value||0))}
function formatDuration(value){
  const nanoseconds=Number(value||0);
  if(!nanoseconds)return '—';
  if(nanoseconds<1e6)return Math.round(nanoseconds/1e3)+' µs';
  if(nanoseconds<1e9)return (nanoseconds/1e6).toFixed(1)+' ms';
  return (nanoseconds/1e9).toFixed(2)+' s';
}
function formatTPS(value){const number=Number(value||0);return number?number.toFixed(number<10?2:1):'—'}
function formatRequestTime(value){
  const date=new Date(value);
  return Number.isNaN(date.getTime())?'—':new Intl.DateTimeFormat(undefined,{year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(date);
}
function routerDisplay(value){
  if(value&&value.attribution==='direct')return '—';
  if(value&&value.attribution==='unattributed')return 'Unattributed';
  return String(value&&value.router_model||'').trim()||'Unattributed';
}
function routerDetail(value){
  if(value&&value.attribution==='direct')return 'Direct provider call';
  if(value&&value.attribution==='unattributed')return 'Attribution could not be resolved';
  return 'Routed alias';
}
function resultDisplay(value){
  value=String(value||'');
  if(value==='success')return 'Success';
  if(value==='failed')return 'Failed';
  if(value.startsWith('http_'))return 'HTTP '+value.slice(5);
  return value||'—';
}
function fitMetricValue(element){
  element.classList.remove('metric-value-wrap');
  element.style.removeProperty('font-size');
  const available=element.clientWidth;
  if(!available)return;
  const maximum=Number.parseFloat(getComputedStyle(element).fontSize)||30;
  if(element.scrollWidth<=available+1)return;
  const fitted=Math.max(14,maximum*available/element.scrollWidth);
  element.style.fontSize=fitted.toFixed(2)+'px';
  if(element.scrollWidth>available+1)element.classList.add('metric-value-wrap');
}
function fitMetricValues(){
  metricFitFrame=0;
  document.querySelectorAll('.metric-value').forEach(fitMetricValue);
}
function scheduleMetricFit(){
  if(metricFitFrame)return;
  metricFitFrame=requestAnimationFrame(fitMetricValues);
}
function setMetric(id,value,detailID,detail){
  document.getElementById(id).textContent=value;
  document.getElementById(detailID).textContent=detail;
  scheduleMetricFit();
}
function effectiveCacheReadValue(value){
  if(!value)return 0;
  if(Object.prototype.hasOwnProperty.call(value,'effective_cache_read_tokens'))return Number(value.effective_cache_read_tokens||0);
  return Number(value.cache_read_tokens||value.cached_tokens||0);
}
function renderUsageSummary(){
  const overview=usageState.overview;
  if(!overview)return;
  const summary=overview.summary||{};
  const costs=overview.costs||{};
  setMetric('metric-tokens',formatCompact(summary.total_tokens),'metric-token-detail',formatNumber(summary.input_tokens)+' input · '+formatNumber(summary.output_tokens)+' output · '+formatNumber(effectiveCacheReadValue(summary))+' cache');
  setMetric('metric-cost',formatUSD(costs.total_usd),'metric-cost-detail',formatNumber(costs.priced_requests)+' of '+formatNumber(costs.requests)+' requests priced');
  const success=Math.max(0,Number(summary.requests||0)-Number(summary.failed_requests||0));
  const rate=Number(summary.requests||0)?success/Number(summary.requests)*100:0;
  setMetric('metric-requests',formatNumber(summary.requests),'metric-request-detail',rate.toFixed(1)+'% success · '+formatNumber(summary.failed_requests)+' failed');
  const routers=(overview.router_models||[]).filter(item=>item.attribution==='routed'&&item.model).sort((left,right)=>Number(right.requests||0)-Number(left.requests||0));
  const providers=(overview.provider_models||[]).slice().sort((left,right)=>Number(right.requests||0)-Number(left.requests||0));
  const router=routers[0];
  const provider=providers[0];
  setMetric('metric-router',router?router.model:'—','metric-router-detail',router?'Routed alias · '+formatNumber(router.requests)+' requests':'No routed requests in range');
  setMetric('metric-provider',provider&&provider.model?provider.model:'—','metric-provider-detail',provider?formatNumber(provider.requests)+' requests · '+formatCompact(provider.total_tokens)+' tokens':'No requests in range');
}
function mergeSelectOptions(select,values,allLabel,selectedValue){
  const merged=new Map();
  Array.from(select.options).forEach(option=>{if(option.value)merged.set(option.value,option.textContent)});
  values.forEach(item=>{if(item&&item.value)merged.set(String(item.value),String(item.label||item.value))});
  if(selectedValue&&!merged.has(selectedValue))merged.set(selectedValue,selectedValue);
  const entries=Array.from(merged.entries()).sort((left,right)=>left[1].localeCompare(right[1]));
  const signature=JSON.stringify([allLabel,entries]);
  if(select.dataset.optionsSignature!==signature){
    const fragment=document.createDocumentFragment();
    const all=document.createElement('option');
    all.value='';all.textContent=allLabel;fragment.appendChild(all);
    entries.forEach(([value,label])=>{const option=document.createElement('option');option.value=value;option.textContent=label;fragment.appendChild(option)});
    select.replaceChildren(fragment);
    select.dataset.optionsSignature=signature;
  }
  select.value=selectedValue;
}
function renderUsageFilterOptions(){
  const overview=usageState.overview||{};
  const routers=(overview.router_models||[]).map(item=>{
    if(item.attribution==='direct')return{value:ROUTER_FILTER_ATTRIBUTION_PREFIX+'direct',label:'Direct provider calls'};
    if(item.attribution==='unattributed')return{value:ROUTER_FILTER_ATTRIBUTION_PREFIX+'unattributed',label:'Unattributed'};
    return{value:item.model?ROUTER_FILTER_MODEL_PREFIX+item.model:'',label:item.model};
  });
  const selectedRouter=usageState.attribution?ROUTER_FILTER_ATTRIBUTION_PREFIX+usageState.attribution:(usageState.routerModel?ROUTER_FILTER_MODEL_PREFIX+usageState.routerModel:'');
  mergeSelectOptions(usageRouterModelEl,routers,'All router models',selectedRouter);
  mergeSelectOptions(usageProviderModelEl,(overview.provider_models||[]).map(item=>({value:item.model,label:item.model})),'All provider models',usageState.providerModel);
  mergeSelectOptions(usageSourceEl,(overview.sources||[]).map(value=>({value,label:value})),'All sources',usageState.source);
  mergeSelectOptions(usageServiceTierEl,(overview.service_tiers||[]).map(value=>({value,label:value})),'All service tiers',usageState.serviceTier);
  const results=(overview.results||[]).map(value=>({value,label:resultDisplay(value)}));
  mergeSelectOptions(usageResultEl,results,'All results',usageState.result);
}
function usageControlChanged(){
  clearChartActive(false);usageState.group.offset=0;usageState.request.offset=0;
  usageState.pendingFullRefresh=true;usageState.generation++;
  if(usageState.controller)usageState.controller.abort();
  window.clearTimeout(usageState.timer);usageState.timer=0;
  window.clearTimeout(usageState.filterTimer);
  usageState.filterTimer=window.setTimeout(()=>{usageState.filterTimer=0;refreshUsage(false)},250);
}

function initializeUsageEvents() {
  usageRangeEl.addEventListener('change',()=>{
    usageState.range=usageRangeEl.value;usageCustomRangeEl.hidden=usageState.range!=='custom';
    if(usageState.range==='custom')initializeCustomRange();
    scheduleUsagePreferencesSave();usageControlChanged();
  });
  usageGranularityEl.addEventListener('change',()=>{usageState.granularity=usageGranularityEl.value;scheduleUsagePreferencesSave();clearChartActive(false);refreshUsage(false,'overview')});
  usageRouterModelEl.addEventListener('change',()=>{
    const value=usageRouterModelEl.value;
    usageState.routerModel=value.startsWith(ROUTER_FILTER_MODEL_PREFIX)?value.slice(ROUTER_FILTER_MODEL_PREFIX.length):'';
    usageState.attribution=value.startsWith(ROUTER_FILTER_ATTRIBUTION_PREFIX)?value.slice(ROUTER_FILTER_ATTRIBUTION_PREFIX.length):'';
    usageControlChanged();
  });
  usageProviderModelEl.addEventListener('change',()=>{usageState.providerModel=usageProviderModelEl.value;usageControlChanged()});
  usageSourceEl.addEventListener('change',()=>{usageState.source=usageSourceEl.value;usageControlChanged()});
  usageServiceTierEl.addEventListener('change',()=>{usageState.serviceTier=usageServiceTierEl.value;usageControlChanged()});
  usageResultEl.addEventListener('change',()=>{usageState.result=usageResultEl.value;usageControlChanged()});
  usageFromEl.addEventListener('change',()=>{usageState.customFrom=usageFromEl.value;if(usageState.range==='custom'){scheduleUsagePreferencesSave();usageControlChanged()}});
  usageToEl.addEventListener('change',()=>{usageState.customTo=usageToEl.value;if(usageState.range==='custom'){scheduleUsagePreferencesSave();usageControlChanged()}});
  usageRefreshEl.addEventListener('click',()=>refreshUsage(false));
}
