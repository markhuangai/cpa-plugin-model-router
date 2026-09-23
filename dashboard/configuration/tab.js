function canonicalIntegerDraft(value){
  const raw=String(value===undefined||value===null?'':value);
  const trimmed=raw.trim();
  return /^-?\d+$/.test(trimmed)?{value:Number.parseInt(trimmed,10)}:{draft:raw};
}
function integerDraftValue(value){
  const draft=canonicalIntegerDraft(value);
  return Object.prototype.hasOwnProperty.call(draft,'value')?draft.value:Number.NaN;
}
function configurationSnapshot(){
  return JSON.stringify({
    enabled:Boolean(state.enabled),
    priority:canonicalIntegerDraft(state.priority),
    routes:state.routes.map(route=>({
      alias:String(route.alias===undefined||route.alias===null?'':route.alias).trim(),
      strategy:String(route.strategy===undefined||route.strategy===null?'':route.strategy),
      cooldown_seconds:canonicalIntegerDraft(route.cooldown_seconds),
      targets:(Array.isArray(route.targets)?route.targets:[]).map(target=>({
        model:String(target.model===undefined||target.model===null?'':target.model).trim(),
        weight:canonicalIntegerDraft(target.weight)
      }))
    }))
  });
}
function setConfigurationActionsAccessible(value){
  configurationActionsEl.inert=!value;
  configurationActionsEl.setAttribute('aria-hidden',String(!value));
  configurationActionsEl.querySelectorAll('button').forEach(button=>{
    if(value)button.removeAttribute('tabindex');
    else button.tabIndex=-1;
  });
}
function updateConfigurationActionsClearance(){
  const visible=!configurationActionsEl.hidden&&!configurationPanelEl.hidden&&!workspaceEl.hidden;
  const height=visible?Math.ceil(configurationActionsEl.getBoundingClientRect().height):0;
  document.documentElement.style.setProperty('--configuration-actions-clearance',height+'px');
}
function setConfigurationActionsVisible(value){
  setConfigurationActionsAccessible(value);
  if(configurationActionsAnimation&&configurationActionsAnimationTarget===value)return;
  if(!configurationActionsAnimation&&configurationActionsAnimationTarget===value&&configurationActionsEl.hidden===!value){
    updateConfigurationActionsClearance();
    return;
  }
  const wasHidden=configurationActionsEl.hidden;
  const currentStyle=wasHidden?null:getComputedStyle(configurationActionsEl);
  if(configurationActionsAnimation)configurationActionsAnimation.cancel();
  configurationActionsAnimation=null;
  configurationActionsAnimationTarget=value;
  if(value&&wasHidden)configurationActionsEl.hidden=false;
  if(typeof configurationActionsEl.animate!=='function'){
    configurationActionsEl.hidden=!value;
    updateConfigurationActionsClearance();
    return;
  }
  updateConfigurationActionsClearance();
  const reducedMotion=reducedMotionQuery.matches;
  const hiddenTransform=reducedMotion?'none':'translateY(18px)';
  const visibleTransform=reducedMotion?'none':'translateY(0)';
  const from=currentStyle
    ?{opacity:currentStyle.opacity,transform:reducedMotion?'none':currentStyle.transform}
    :(value?{opacity:0,transform:hiddenTransform}:{opacity:1,transform:visibleTransform});
  const to=value?{opacity:1,transform:visibleTransform}:{opacity:0,transform:hiddenTransform};
  const animation=configurationActionsEl.animate([from,to],{
    duration:value?280:220,
    easing:value?'cubic-bezier(.2,.8,.2,1)':'cubic-bezier(.4,0,1,1)',
    fill:'both'
  });
  configurationActionsAnimation=animation;
  animation.finished.then(()=>{
    if(configurationActionsAnimation!==animation)return;
    configurationActionsAnimation=null;
    animation.cancel();
    if(value===state.dirty){
      if(!value)configurationActionsEl.hidden=true;
      updateConfigurationActionsClearance();
      return;
    }
    setConfigurationActionsVisible(state.dirty);
  }).catch(()=>{});
}
function initializeConfigurationActions(){
  setConfigurationActionsAccessible(false);
  updateConfigurationActionsClearance();
  if(!window.ResizeObserver)return;
  configurationActionsResizeObserver=new ResizeObserver(updateConfigurationActionsClearance);
  configurationActionsResizeObserver.observe(configurationActionsBarEl);
}
function setDirty(value){
  state.dirty=value;
  saveStateEl.dataset.dirty=String(value);
  saveStateEl.textContent=value?'Unsaved changes':'Saved in CPA';
  setConfigurationActionsVisible(value);
}
function refreshDirty(){
  const value=state.baselineSnapshot!==null&&configurationSnapshot()!==state.baselineSnapshot;
  setDirty(value);
  return value;
}
function captureConfigurationBaseline(snapshot=configurationSnapshot()){
  state.baselineSnapshot=snapshot;
  refreshDirty();
}
async function loadModelCatalog(){
  const keyPayload=await requestManagementJSON(API_KEYS_API);
  const keys=Array.isArray(keyPayload['api-keys'])?keyPayload['api-keys']:[];
  const clientKey=keys.map(value=>String(value||'').trim()).find(Boolean)||'';
  return requestCPAJSON(MODELS_API,clientKey);
}
function normalizeModelCatalog(payload,routes){
  if(!payload||!Array.isArray(payload.data))throw new Error('CPA returned an invalid model catalog.');
  const aliases=new Set(routes.map(route=>route.alias.trim().toLowerCase()).filter(Boolean));
  const seen=new Set();
  const models=[];
  payload.data.forEach(item=>{
    const id=item&&typeof item.id==='string'?item.id.trim():'';
    const owner=item&&typeof item.owned_by==='string'?item.owned_by.trim().toLowerCase():'';
    if(!id||owner==='model-router'||aliases.has(id.toLowerCase())||seen.has(id))return;
    seen.add(id);
    models.push(id);
  });
  models.sort((left,right)=>left.localeCompare(right));
  if(models.length===0)throw new Error('CPA returned no selectable models.');
  return models;
}
function updateModelStatus(){
  if(state.catalogError){
    modelStatusEl.textContent='Models unavailable';
    modelStatusEl.dataset.tone='error';
    modelStatusEl.title=state.catalogError;
  }else if(state.catalogLoaded){
    modelStatusEl.textContent=state.availableModels.length+' '+(state.availableModels.length===1?'model':'models')+' available';
    modelStatusEl.dataset.tone='ready';
    modelStatusEl.removeAttribute('title');
  }else{
    modelStatusEl.textContent='Loading CPA models…';
    modelStatusEl.dataset.tone='loading';
    modelStatusEl.removeAttribute('title');
  }
}
function normalizeRoute(value,index){
  const route=value&&typeof value==='object'?value:{};
  const canonicalCooldown=Number.isInteger(route.cooldown_seconds)?route.cooldown_seconds:null;
  const legacyCooldown=Number.isInteger(route['cooldown-seconds'])?route['cooldown-seconds']:null;
  const cooldown=canonicalCooldown!==null?canonicalCooldown:(legacyCooldown!==null?legacyCooldown:60);
  const targets=Array.isArray(route.targets)?route.targets.map(target=>{
    const value=target&&typeof target==='object'?target:{};
    return{model:String(value.model||''),weight:String(Number.isInteger(value.weight)?value.weight:1)};
  }):(Array.isArray(route.models)?route.models.map(model=>({model:String(model||''),weight:'1'})):[]);
  return{
    alias:String(route.alias||('route-'+(index+1))),
    strategy:route.strategy==='round-robin'?'round-robin':'priority',
    cooldown_seconds:String(cooldown),
    targets
  };
}
async function loadConfiguration(){
  setBusy(true);
  setAuthNote('Loading configuration…');
  state.availableModels=[];
  state.catalogLoaded=false;
  state.catalogError='';
  updateModelStatus();
  try{
    const [configResult,catalogResult]=await Promise.allSettled([
      requestManagementJSON(CONFIG_API),
      loadModelCatalog()
    ]);
    if(configResult.status==='rejected')throw configResult.reason;
    const config=configResult.value;
    const source=Array.isArray(config.routes)?config.routes:(Array.isArray(config['model-routes'])?config['model-routes']:[]);
    state.enabled=config.enabled!==false;
    state.priority=Number.isInteger(config.priority)?String(config.priority):'0';
    state.routes=source.map(normalizeRoute);
    if(catalogResult.status==='fulfilled'){
      try{
        state.availableModels=normalizeModelCatalog(catalogResult.value,state.routes);
        state.catalogLoaded=true;
      }catch(error){state.catalogError=error.message}
    }else state.catalogError=catalogResult.reason.message||'CPA models could not be loaded.';
    enabledEl.checked=state.enabled;
    priorityEl.value=state.priority;
    updateModelStatus();
    renderRoutes();
    workspaceEl.hidden=false;
    captureConfigurationBaseline();
    hideFallbackKeyInput();
    setAuthNote('Connected with the CPAMC session.');
    if(state.catalogError)showToast('Configuration loaded, but CPA models are unavailable. '+state.catalogError,'error');
  }catch(error){
    refreshDirty();
    setAuthNote(error.message,'error');
    showToast(error.message,'error');
  }finally{setBusy(false)}
}
function createTargetRow(target,targetIndex,targetCount,weighted){
  const row=document.getElementById('target-template').content.firstElementChild.cloneNode(true);
  row.dataset.targetIndex=String(targetIndex);
  row.querySelector('.target-number').textContent=String(targetIndex+1).padStart(2,'0');
  const select=row.querySelector('[data-target-field="model"]');
  const model=target.model;
  const weight=row.querySelector('[data-target-field="weight"]');
  weight.value=target.weight;
  weight.setAttribute('aria-label','Round-robin weight for target '+(targetIndex+1));
  weight.hidden=!weighted;
  row.querySelector('.target-weight').hidden=!weighted;
  const placeholder=document.createElement('option');
  placeholder.value='';
  placeholder.textContent=state.catalogError?'Models unavailable':'Select a CPA model';
  placeholder.disabled=true;
  placeholder.selected=!model;
  select.appendChild(placeholder);
  const available=new Set(state.availableModels);
  if(model&&!available.has(model)){
    const unavailable=document.createElement('option');
    unavailable.value=model;
    unavailable.textContent=model+' (unavailable)';
    unavailable.disabled=true;
    unavailable.selected=true;
    select.appendChild(unavailable);
    select.title='This configured model is not currently available in CPA. Choose a live model to replace it.';
    row.dataset.modelAvailability='unavailable';
  }
  state.availableModels.forEach(availableModel=>{
    const option=document.createElement('option');
    option.value=availableModel;
    option.textContent=availableModel;
    select.appendChild(option);
  });
  if(model&&available.has(model))select.value=model;
  select.disabled=!state.catalogLoaded;
  row.querySelector('[data-action="target-up"]').disabled=targetIndex===0;
  row.querySelector('[data-action="target-down"]').disabled=targetIndex===targetCount-1;
  return row;
}
function createRouteCard(route,index){
  const card=document.getElementById('route-template').content.firstElementChild.cloneNode(true);
  card.dataset.index=String(index);
  card.querySelector('.ordinal').textContent=String(index+1).padStart(2,'0');
  card.querySelector('[data-field="alias"]').value=route.alias;
  card.querySelector('[data-field="strategy"]').value=route.strategy;
  card.querySelector('[data-field="cooldown_seconds"]').value=route.cooldown_seconds;
  card.querySelector('[data-action="up"]').disabled=index===0;
  card.querySelector('[data-action="down"]').disabled=index===state.routes.length-1;
  const addTarget=card.querySelector('[data-action="add-target"]');
  addTarget.disabled=!state.catalogLoaded;
  if(!state.catalogLoaded)addTarget.title=state.catalogError||'CPA models are still loading.';
  card.querySelector('.preview-alias').textContent=route.alias||'unnamed alias';
  const weighted=route.strategy==='round-robin';
  card.querySelector('[data-target-help]').hidden=!weighted;
  card.querySelector('.preview-targets').textContent=route.targets.length+(route.targets.length===1?' target':' targets');
  const targetList=card.querySelector('.target-list');
  if(route.targets.length===0){
    const empty=document.createElement('div');
    empty.className='target-empty';
    empty.textContent='A route needs at least one model target.';
    targetList.appendChild(empty);
  }else route.targets.forEach((target,targetIndex)=>targetList.appendChild(createTargetRow(target,targetIndex,route.targets.length,weighted)));
  return card;
}
function routeCardsInOrder(){return Array.from(routesEl.children).filter(child=>child.classList&&child.classList.contains('route-card'))}
function targetRowsInOrder(card){return card?Array.from(card.querySelectorAll('.target-row')):[]}
// Reordering swaps the live nodes instead of rebuilding the table. replaceChildren()
// removes the control that triggered the reorder, and a browser drops focus and resets
// the page scroll when the focused element is removed.
function swapNodes(nodes,index,next){
  const node=nodes[index],reference=nodes[next];
  if(!node||!reference)return false;
  if(next>index)reference.after(node);else reference.before(node);
  return true;
}
function renumberRouteCards(){
  const cards=routeCardsInOrder();
  cards.forEach((card,index)=>{
    card.dataset.index=String(index);
    card.querySelector('.ordinal').textContent=String(index+1).padStart(2,'0');
    card.querySelector('[data-action="up"]').disabled=index===0;
    card.querySelector('[data-action="down"]').disabled=index===cards.length-1;
    const rows=targetRowsInOrder(card);
    rows.forEach((row,rowIndex)=>{
      row.dataset.targetIndex=String(rowIndex);
      row.querySelector('.target-number').textContent=String(rowIndex+1).padStart(2,'0');
      row.querySelector('[data-action="target-up"]').disabled=rowIndex===0;
      row.querySelector('[data-action="target-down"]').disabled=rowIndex===rows.length-1;
      const weight=row.querySelector('[data-target-field="weight"]');
      if(weight)weight.setAttribute('aria-label','Round-robin weight for target '+(rowIndex+1));
    });
  });
}
function describeRouteControl(element){
  if(!element||typeof element.closest!=='function'||!routesEl.contains(element))return null;
  const button=element.closest('button[data-action]');
  if(!button)return null;
  const card=button.closest('.route-card');
  const row=button.closest('.target-row');
  return{action:button.dataset.action,routeIndex:card?Number.parseInt(card.dataset.index,10):-1,targetIndex:row?Number.parseInt(row.dataset.targetIndex,10):-1};
}
function restoreRouteFocus(descriptor){
  // Destructive controls are skipped: the row they belonged to no longer exists, and
  // focusing a different Remove button would let the next Enter key delete that row.
  if(!descriptor||descriptor.action.startsWith('remove'))return;
  const card=descriptor.routeIndex>=0?routesEl.querySelector('.route-card[data-index="'+descriptor.routeIndex+'"]'):null;
  if(!card)return;
  let scope=card;
  if(descriptor.targetIndex>=0){
    const row=targetRowsInOrder(card).find(candidate=>candidate.dataset.targetIndex===String(descriptor.targetIndex));
    if(row)scope=row;
  }
  const button=scope.querySelector('button[data-action="'+descriptor.action+'"]');
  if(button&&!button.disabled)button.focus({preventScroll:true});
}
function keepRouteControlFocus(node,action){
  if(!node||!action)return;
  // Re-inserting a node blurs it, so focus is restored explicitly. When the arrow that
  // triggered the move is disabled at either end of the list, focus moves to the arrow
  // still enabled in the same row. Remove buttons are never focused.
  const candidates=Array.from(node.querySelectorAll('button[data-action]')).filter(button=>!button.disabled&&!button.dataset.action.startsWith('remove'));
  const target=candidates.find(button=>button.dataset.action===action)||candidates[0];
  if(target)target.focus({preventScroll:true});
}
function renderRoutes(){
  // Rows are still rebuilt when a route or target is added or removed, so the scroll
  // offset and the focused control are restored around the rebuild.
  const preserve=!configurationPanelEl.hidden;
  const scrollX=window.scrollX,scrollY=window.scrollY;
  const focus=preserve?describeRouteControl(document.activeElement):null;
  routesEl.replaceChildren();
  countEl.textContent=state.routes.length+' '+(state.routes.length===1?'route':'routes');
  updateModelStatus();
  if(state.routes.length===0){
    routesEl.appendChild(document.getElementById('empty-template').content.cloneNode(true));
  }else{
    state.routes.forEach((route,index)=>routesEl.appendChild(createRouteCard(route,index)));
  }
  if(preserve){
    restoreRouteFocus(focus);
    window.scrollTo(scrollX,scrollY);
  }
}
function newRoute(){return{alias:'route-'+(state.routes.length+1),strategy:'priority',cooldown_seconds:'60',targets:[{model:'',weight:'1'}]}}
function addRoute(){state.routes.push(newRoute());renderRoutes();refreshDirty()}
function routeIndexFrom(target){
  const card=target.closest('.route-card');
  return card?Number.parseInt(card.dataset.index,10):-1;
}
function moveRoute(index,delta,action){
  const next=index+delta;
  if(next<0||next>=state.routes.length)return;
  const scrollX=window.scrollX,scrollY=window.scrollY;
  const moved=state.routes.splice(index,1)[0];
  state.routes.splice(next,0,moved);
  const cards=routeCardsInOrder(),card=cards[index];
  if(swapNodes(cards,index,next)){renumberRouteCards();keepRouteControlFocus(card,action)}
  else renderRoutes();
  refreshDirty();
  window.scrollTo(scrollX,scrollY);
}
function moveTarget(routeIndex,targetIndex,delta,action){
  const route=state.routes[routeIndex];
  if(!route)return;
  const targets=route.targets;
  const next=targetIndex+delta;
  if(next<0||next>=targets.length)return;
  const moved=targets.splice(targetIndex,1)[0];
  targets.splice(next,0,moved);
  const card=routesEl.querySelector('.route-card[data-index="'+routeIndex+'"]');
  const rows=targetRowsInOrder(card),row=rows[targetIndex];
  if(swapNodes(rows,targetIndex,next)){renumberRouteCards();keepRouteControlFocus(row,action)}
  else renderRoutes();
  refreshDirty();
}
function updateFromInput(target){
  const index=routeIndexFrom(target);
  if(index<0||!state.routes[index])return;
  const route=state.routes[index];
  const targetRow=target.closest('.target-row');
  if(targetRow&&target.dataset.targetField){
    const targetIndex=Number.parseInt(targetRow.dataset.targetIndex,10);
    if(targetIndex>=0)route.targets[targetIndex][target.dataset.targetField]=target.value;
    if(target.dataset.targetField==='model'&&state.availableModels.includes(target.value)){
      delete targetRow.dataset.modelAvailability;
      target.removeAttribute('title');
    }
  }else if(target.dataset.field)route[target.dataset.field]=target.value;
  const card=target.closest('.route-card');
  if(card&&target.dataset.field==='alias')card.querySelector('.preview-alias').textContent=target.value||'unnamed alias';
  if(target.dataset.field==='strategy')renderRoutes();
  refreshDirty();
}
routesEl.addEventListener('input',event=>updateFromInput(event.target));
routesEl.addEventListener('change',event=>updateFromInput(event.target));
routesEl.addEventListener('click',event=>{
  const button=event.target.closest('button[data-action]');
  if(!button)return;
  const action=button.dataset.action;
  if(action==='add-route'){addRoute();return}
  const index=routeIndexFrom(button);
  if(index<0)return;
  if(action==='up')moveRoute(index,-1,action);
  if(action==='down')moveRoute(index,1,action);
  if(action==='remove'){state.routes.splice(index,1);renderRoutes();refreshDirty()}
  if(action==='add-target'&&state.catalogLoaded){state.routes[index].targets.push({model:'',weight:'1'});renderRoutes();refreshDirty()}
  const row=button.closest('.target-row');
  const targetIndex=row?Number.parseInt(row.dataset.targetIndex,10):-1;
  if(action==='target-up'&&targetIndex>=0)moveTarget(index,targetIndex,-1,action);
  if(action==='target-down'&&targetIndex>=0)moveTarget(index,targetIndex,1,action);
  if(action==='remove-target'&&targetIndex>=0){state.routes[index].targets.splice(targetIndex,1);renderRoutes();refreshDirty()}
});
function serializeRoutes(){
  return state.routes.map(route=>({
    alias:route.alias.trim(),
    strategy:route.strategy,
    cooldown_seconds:integerDraftValue(route.cooldown_seconds),
    targets:route.targets.map(target=>({model:target.model.trim(),weight:integerDraftValue(target.weight)}))
  }));
}
function baseModel(value){
  const match=/^(.*)\([^()]+\)$/.exec(value);
  return (match?match[1]:value).trim().toLowerCase();
}
function validateClient(routes){
  const errors=[];
  const aliases=new Map();
  routes.forEach((route,index)=>{
    const label='Route '+(index+1);
    const aliasKey=route.alias.toLowerCase();
    if(!route.alias)errors.push(label+': alias is required.');
    else if(/\([^()]+\)$/.test(route.alias))errors.push(label+': aliases cannot include a thinking suffix.');
    else if(aliases.has(aliasKey))errors.push(label+': alias duplicates route '+aliases.get(aliasKey)+'.');
    else aliases.set(aliasKey,index+1);
    if(route.strategy!=='priority'&&route.strategy!=='round-robin')errors.push(label+': choose a strategy.');
    if(!Number.isInteger(route.cooldown_seconds)||route.cooldown_seconds<0)errors.push(label+': cooldown must be a non-negative integer.');
    if(route.targets.length===0)errors.push(label+': add at least one target.');
    const models=new Map();
    route.targets.forEach((target,targetIndex)=>{
      const model=target.model;
      const modelKey=model.toLowerCase();
      if(!model)errors.push(label+', target '+(targetIndex+1)+': model is required.');
      else if(!Number.isInteger(target.weight)||target.weight<1||target.weight>1000000)errors.push(label+', target '+(targetIndex+1)+': weight must be an integer from 1 to 1000000.');
      else if(models.has(modelKey))errors.push(label+', target '+(targetIndex+1)+': model duplicates target '+models.get(modelKey)+'.');
      else models.set(modelKey,targetIndex+1);
    });
  });
  routes.forEach((route,index)=>route.targets.forEach((target,targetIndex)=>{
    const targetAlias=aliases.get(baseModel(target.model));
    if(targetAlias)errors.push('Route '+(index+1)+', target '+(targetIndex+1)+': target references route alias '+targetAlias+'.');
  }));
  if(!/^-?\d+$/.test(state.priority.trim()))errors.push('Priority must be an integer.');
  return errors;
}
async function saveConfiguration(){
  if(state.busy||!state.dirty)return;
  const routes=serializeRoutes();
  const errors=validateClient(routes);
  if(errors.length){showToast(errors[0]+(errors.length>1?' '+(errors.length-1)+' more issue(s).':''),'error');return}
  const submittedSnapshot=configurationSnapshot();
  setBusy(true);
  saveStateEl.textContent='Validating…';
  try{
    await requestManagementJSON(VALIDATE_API,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:state.enabled,routes})});
    saveStateEl.textContent='Saving…';
    await requestManagementJSON(CONFIG_API,{method:'PATCH',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:state.enabled,priority:integerDraftValue(state.priority),routes,'model-routes':null})});
    captureConfigurationBaseline(submittedSnapshot);
    showToast('Model routes saved and CPA reload requested.','success');
  }catch(error){
    refreshDirty();
    if(state.dirty)saveStateEl.textContent='Save failed';
    showToast(error.message,'error');
  }finally{setBusy(false)}
}

function initializeConfigurationEvents() {
  connectEl.addEventListener('click',loadConfiguration);
  keyEl.addEventListener('keydown',event=>{if(event.key==='Enter')loadConfiguration()});
  enabledEl.addEventListener('change',()=>{state.enabled=enabledEl.checked;refreshDirty()});
  priorityEl.addEventListener('input',()=>{state.priority=priorityEl.value;refreshDirty()});
  addRouteEl.addEventListener('click',addRoute);
  reloadEl.addEventListener('click',()=>{if(!state.dirty||window.confirm('Discard unsaved changes and reload from CPA?'))loadConfiguration()});
  saveEl.addEventListener('click',saveConfiguration);
}
