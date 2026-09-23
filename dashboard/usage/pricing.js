function cloneValue(value){return JSON.parse(JSON.stringify(value))}
function setPricingError(message=''){document.getElementById('pricing-error').textContent=message}
function setPricingEditorEnabled(enabled){
  for(const control of document.querySelectorAll('#pricing-dialog input, #pricing-dialog select, #pricing-dialog textarea, #pricing-dialog button')){
    if(control.id==='pricing-close'||control.id==='pricing-cancel')continue;
    control.disabled=!enabled;
  }
}
function pricingDraftDirty(){return Boolean(usageState.pricingSettingsDirty||usageState.priceCollectionDirty)||(Array.isArray(usageState.priceDraft)&&usageState.priceDraft.some(item=>item.dirty))}
function freshManagementURL(url){return url+(url.includes('?')?'&':'?')+'fresh='+Date.now()}
function priceBookStatus(book){
  let message='Revision '+Number(book&&book.revision||0)+' · '+formatNumber(Object.keys(book&&book.prices||{}).length)+' priced models';
  if(book&&book.last_sync){const sync=new Date(book.last_sync.completed_at);message+=' · last sync '+(Number.isNaN(sync.getTime())?'completed':sync.toLocaleString())+' ('+formatNumber(book.last_sync.matched)+' matched, '+formatNumber(book.last_sync.unmatched)+' unmatched)'}
  return message;
}
function applyPriceBook(book){
  usageState.priceBook=book||{revision:0,prices:{},sync_settings:{provider_priority:[],ignored_suffixes:[],mappings:[]}};
  const settings=usageState.priceBook.sync_settings||{};
  document.getElementById('pricing-provider-priority').value=(settings.provider_priority||[]).join(', ');
  document.getElementById('pricing-ignored-suffixes').value=(settings.ignored_suffixes||[]).join(', ');
  document.getElementById('pricing-mappings').value=JSON.stringify(settings.mappings||[],null,2);
  usageState.pricingSettingsDirty=false;
  usageState.priceCollectionDirty=false;
  usageState.priceDraft=Object.entries(usageState.priceBook.prices||{}).sort((left,right)=>left[0].localeCompare(right[0])).map(([name,value])=>({
    originalName:name,name,input:String(Number(value.input||0)),output:String(Number(value.output||0)),cache_read:String(Number(value.cache_read||0)),cache_creation:String(Number(value.cache_creation||0)),
    accounting_mode:String(value.accounting_mode||''),context_tiers:JSON.stringify(value.context_tiers||[],null,2),service_tiers:JSON.stringify(value.service_tiers||{},null,2),
    source:String(value.source||'manual'),catalog_provider:String(value.catalog_provider||''),catalog_model:String(value.catalog_model||''),updated_at:String(value.updated_at||''),dirty:false
  }));
  document.getElementById('pricing-status').textContent=priceBookStatus(usageState.priceBook);
  renderPriceDraft();
}
function priceInput(label,field,value,type='number'){
  const wrapper=document.createElement('label'),caption=document.createElement('span'),input=document.createElement('input');
  caption.textContent=label;input.type=type;input.value=value;input.dataset.priceField=field;
  if(type==='number'){input.min='0';input.max='1000000';input.step='any';input.inputMode='decimal'}
  wrapper.appendChild(caption);wrapper.appendChild(input);return wrapper;
}
function priceTextarea(label,field,value){
  const wrapper=document.createElement('label'),caption=document.createElement('span'),textarea=document.createElement('textarea');
  caption.textContent=label;textarea.value=value;textarea.dataset.priceField=field;textarea.spellcheck=false;
  wrapper.appendChild(caption);wrapper.appendChild(textarea);return wrapper;
}
function renderPriceDraft(){
  const list=document.getElementById('pricing-list'),fragment=document.createDocumentFragment(),draft=usageState.priceDraft||[];
  if(!draft.length){const empty=document.createElement('div');empty.className='price-empty';empty.textContent='No prices saved. Add a provider model or synchronize observed CPA models.';fragment.appendChild(empty)}
  draft.forEach((price,index)=>{
    const row=document.createElement('article');row.className='price-row';row.dataset.priceIndex=String(index);
    const grid=document.createElement('div');grid.className='price-grid';
    const model=priceInput('Provider model','name',price.name,'text');model.classList.add('price-model-field');
    const source=document.createElement('small');source.className='price-source';source.textContent=(price.dirty?'manual draft':price.source)+(price.catalog_provider?' · '+price.catalog_provider+'/'+price.catalog_model:'');source.title=source.textContent;model.appendChild(source);grid.appendChild(model);
    grid.appendChild(priceInput('Input','input',price.input));grid.appendChild(priceInput('Output','output',price.output));grid.appendChild(priceInput('Cache read','cache_read',price.cache_read));grid.appendChild(priceInput('Cache create','cache_creation',price.cache_creation));
    const accounting=document.createElement('label'),accountingCaption=document.createElement('span'),select=document.createElement('select');accountingCaption.textContent='Input accounting';select.dataset.priceField='accounting_mode';
    for(const [value,label] of [['','Provider default'],['input_includes_cache','Input includes cache'],['input_excludes_cache','Input excludes cache']]){const option=document.createElement('option');option.value=value;option.textContent=label;select.appendChild(option)}
    select.value=price.accounting_mode;accounting.appendChild(accountingCaption);accounting.appendChild(select);grid.appendChild(accounting);
    const remove=document.createElement('button');remove.type='button';remove.className='danger-button';remove.dataset.priceRemove=String(index);remove.textContent='Remove';grid.appendChild(remove);row.appendChild(grid);
    const advanced=document.createElement('details');advanced.className='price-advanced';const summary=document.createElement('summary');summary.textContent='Context and service-tier pricing';advanced.appendChild(summary);
    const advancedGrid=document.createElement('div');advancedGrid.className='price-advanced-grid';advancedGrid.appendChild(priceTextarea('Context tiers as JSON','context_tiers',price.context_tiers));advancedGrid.appendChild(priceTextarea('Service tiers as JSON','service_tiers',price.service_tiers));advanced.appendChild(advancedGrid);row.appendChild(advanced);fragment.appendChild(row);
  });
  list.replaceChildren(fragment);
}
function splitPricingList(value){return String(value||'').split(/[\n,]+/).map(item=>item.trim()).filter(Boolean)}
function pricingSyncSettings(){
  let mappings;
  try{mappings=JSON.parse(document.getElementById('pricing-mappings').value.trim()||'[]')}catch(error){throw new Error('Model mappings must be valid JSON: '+error.message)}
  if(!Array.isArray(mappings))throw new Error('Model mappings must be a JSON array.');
  return{provider_priority:splitPricingList(document.getElementById('pricing-provider-priority').value),ignored_suffixes:splitPricingList(document.getElementById('pricing-ignored-suffixes').value),mappings};
}
function parsedRate(value,model,label){
  const number=Number(value||0);
  if(!Number.isFinite(number)||number<0||number>1000000)throw new Error(model+' '+label+' must be between 0 and 1,000,000.');
  return number;
}
function parsePriceJSON(value,model,label,kind){
  let parsed;
  try{parsed=JSON.parse(String(value||'').trim()||(kind==='array'?'[]':'{}'))}catch(error){throw new Error(model+' '+label+' must be valid JSON: '+error.message)}
  if(kind==='array'&&!Array.isArray(parsed))throw new Error(model+' '+label+' must be a JSON array.');
  if(kind==='object'&&(Array.isArray(parsed)||!parsed||typeof parsed!=='object'))throw new Error(model+' '+label+' must be a JSON object.');
  return parsed;
}
function buildPriceSaveRequest(){
  const prices={},names=new Set();
  for(const draft of usageState.priceDraft||[]){
    const name=String(draft.name||'').trim(),key=name.toLowerCase();
    if(!name)throw new Error('Every price needs a provider model name.');
    if(names.has(key))throw new Error('Provider model '+name+' appears more than once.');
    names.add(key);
    const dirty=draft.dirty||name!==draft.originalName;
    prices[name]={
      input:parsedRate(draft.input,name,'input price'),output:parsedRate(draft.output,name,'output price'),cache_read:parsedRate(draft.cache_read,name,'cache-read price'),cache_creation:parsedRate(draft.cache_creation,name,'cache-creation price'),
      accounting_mode:draft.accounting_mode,context_tiers:parsePriceJSON(draft.context_tiers,name,'context tiers','array'),service_tiers:parsePriceJSON(draft.service_tiers,name,'service tiers','object'),
      source:dirty?'manual':draft.source,catalog_provider:dirty?'':draft.catalog_provider,catalog_model:dirty?'':draft.catalog_model
    };
  }
  return{revision:Number(usageState.priceBook&&usageState.priceBook.revision||0),prices,sync_settings:pricingSyncSettings()};
}
async function openPricingDialog(){
  const dialogGeneration=++usageState.pricingDialogGeneration;
  setPricingError();document.getElementById('pricing-status').textContent='Loading saved model prices…';
  usageState.priceBook=null;usageState.priceDraft=null;usageState.pricingSettingsDirty=false;usageState.priceCollectionDirty=false;
  document.getElementById('pricing-list').replaceChildren();setPricingEditorEnabled(false);
  if(!pricingDialogEl.open)pricingDialogEl.showModal();
  try{
    const pendingWrite=usageState.pricingWriteInFlight;
    if(pendingWrite){try{await pendingWrite}catch(_error){}}
    if(dialogGeneration!==usageState.pricingDialogGeneration)return;
    const book=await requestManagementJSON(freshManagementURL(USAGE_API+'/prices'),{cache:'no-store'});
    if(dialogGeneration!==usageState.pricingDialogGeneration)return;
    applyPriceBook(book);setPricingEditorEnabled(true);
  }catch(error){if(dialogGeneration===usageState.pricingDialogGeneration)setPricingError(error.message)}
}
function closePricingDialog(){usageState.pricingDialogGeneration++;if(pricingDialogEl.open)pricingDialogEl.close();usageState.priceDraft=null;setPricingError()}
function requestClosePricingDialog(){if(pricingDraftDirty()&&!window.confirm('Discard unsaved pricing changes?'))return;closePricingDialog()}
function addPriceModel(){
  const input=document.getElementById('pricing-model-name'),name=input.value.trim();
  if(!name){setPricingError('Enter a provider model name.');input.focus();return}
  if((usageState.priceDraft||[]).some(item=>item.name.toLowerCase()===name.toLowerCase())){setPricingError(name+' already has a price row.');return}
  if(!usageState.priceDraft)usageState.priceDraft=[];
  usageState.priceDraft.push({originalName:'',name,input:'0',output:'0',cache_read:'0',cache_creation:'0',accounting_mode:'',context_tiers:'[]',service_tiers:'{}',source:'manual',catalog_provider:'',catalog_model:'',updated_at:'',dirty:true});
  input.value='';setPricingError();renderPriceDraft();
  const rows=document.querySelectorAll('.price-row');const last=rows[rows.length-1];if(last)last.querySelector('[data-price-field="input"]').focus();
}
async function savePricing(){
  let payload;
  try{payload=buildPriceSaveRequest()}catch(error){setPricingError(error.message);return}
  const dialogGeneration=usageState.pricingDialogGeneration;
  setPricingEditorEnabled(false);setPricingError();
  const writeRequest=requestManagementJSON(USAGE_API+'/prices',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
  usageState.pricingWriteInFlight=writeRequest;
  try{
    const book=await writeRequest;
    if(dialogGeneration!==usageState.pricingDialogGeneration)return;
    applyPriceBook(book);closePricingDialog();showToast('Model pricing saved.','success');
    if(usageState.active)refreshUsage(false);
  }catch(error){if(dialogGeneration===usageState.pricingDialogGeneration)setPricingError(error.message)}finally{
    if(dialogGeneration===usageState.pricingDialogGeneration&&pricingDialogEl.open)setPricingEditorEnabled(true);
    if(usageState.pricingWriteInFlight===writeRequest)usageState.pricingWriteInFlight=null;
  }
}
async function syncPricing(){
  if(!usageState.priceBook){setPricingError('Load the price book before synchronizing.');return}
  if(pricingDraftDirty()&&!window.confirm('Discard unsaved pricing changes and synchronize the saved price book?'))return;
  let settings;
  try{settings=pricingSyncSettings()}catch(error){setPricingError(error.message);return}
  const models=new Set(state.availableModels);
  for(const item of usageState.overview&&usageState.overview.provider_models||[]){if(item.model)models.add(item.model)}
  for(const name of Object.keys(usageState.priceBook.prices||{}))models.add(name);
  if(!models.size){setPricingError('No CPA provider models are available to synchronize.');return}
  const dialogGeneration=usageState.pricingDialogGeneration;
  setPricingEditorEnabled(false);setPricingError();document.getElementById('pricing-status').textContent='Synchronizing '+formatNumber(models.size)+' models from models.dev…';
  const writeRequest=requestManagementJSON(USAGE_API+'/prices/sync',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({source:'models.dev',revision:Number(usageState.priceBook.revision||0),models:Array.from(models),sync_settings:settings})});
  usageState.pricingWriteInFlight=writeRequest;
  try{
    const book=await writeRequest;
    if(dialogGeneration!==usageState.pricingDialogGeneration)return;
    applyPriceBook(book);showToast('Model prices synchronized.','success');if(usageState.active)refreshUsage(false);
  }catch(error){if(dialogGeneration===usageState.pricingDialogGeneration){setPricingError(error.message);document.getElementById('pricing-status').textContent=priceBookStatus(usageState.priceBook)}}finally{
    if(dialogGeneration===usageState.pricingDialogGeneration)setPricingEditorEnabled(true);
    if(usageState.pricingWriteInFlight===writeRequest)usageState.pricingWriteInFlight=null;
  }
}
function openResetDialog(){
  document.getElementById('reset-confirmation').value='';document.getElementById('reset-error').textContent='';document.getElementById('reset-confirm').disabled=true;
  resetDialogEl.showModal();document.getElementById('reset-confirmation').focus();
}
async function resetUsage(){
  const confirmation=document.getElementById('reset-confirmation').value;
  if(confirmation!=='reset')return;
  const button=document.getElementById('reset-confirm');button.disabled=true;document.getElementById('reset-error').textContent='';
  try{
    await requestManagementJSON(USAGE_API+'/reset',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({confirm:'reset'})});
    resetDialogEl.close();usageState.group.offset=0;usageState.request.offset=0;showToast('Usage history reset. Prices and preferences were preserved.','success');await refreshUsage(false);
  }catch(error){document.getElementById('reset-error').textContent=error.message;button.disabled=false}
}
function updatePriceDraft(event){
  const target=event.target.closest('[data-price-field]'),row=target&&target.closest('.price-row');
  if(!target||!row||!usageState.priceDraft)return;
  const price=usageState.priceDraft[Number(row.dataset.priceIndex)];
  if(!price)return;
  price[target.dataset.priceField]=target.value;price.dirty=true;
  const source=row.querySelector('.price-source');if(source)source.textContent='manual draft';
}

function initializePricingEvents() {
  document.getElementById('usage-pricing').addEventListener('click',openPricingDialog);
  document.getElementById('pricing-close').addEventListener('click',requestClosePricingDialog);
  document.getElementById('pricing-cancel').addEventListener('click',requestClosePricingDialog);
  document.getElementById('pricing-add').addEventListener('click',addPriceModel);
  document.getElementById('pricing-model-name').addEventListener('keydown',event=>{if(event.key==='Enter'){event.preventDefault();addPriceModel()}});
  document.getElementById('pricing-save').addEventListener('click',savePricing);
  document.getElementById('pricing-sync').addEventListener('click',syncPricing);
  document.getElementById('pricing-list').addEventListener('input',updatePriceDraft);
  document.getElementById('pricing-list').addEventListener('change',updatePriceDraft);
  document.getElementById('pricing-list').addEventListener('click',event=>{
    const button=event.target.closest('[data-price-remove]');if(!button||!usageState.priceDraft)return;
    const index=Number(button.dataset.priceRemove),price=usageState.priceDraft[index];
    if(price&&!window.confirm('Remove '+price.name+' from this pricing draft?'))return;
    usageState.priceDraft.splice(index,1);usageState.priceCollectionDirty=true;renderPriceDraft();
  });
  for(const id of ['pricing-provider-priority','pricing-ignored-suffixes','pricing-mappings'])document.getElementById(id).addEventListener('input',()=>{usageState.pricingSettingsDirty=true});
  pricingDialogEl.addEventListener('cancel',event=>{event.preventDefault();requestClosePricingDialog()});
  document.getElementById('usage-reset').addEventListener('click',openResetDialog);
  document.getElementById('reset-cancel').addEventListener('click',()=>resetDialogEl.close());
  document.getElementById('reset-confirmation').addEventListener('input',event=>{document.getElementById('reset-confirm').disabled=event.target.value!=='reset'});
  document.getElementById('reset-confirmation').addEventListener('keydown',event=>{if(event.key==='Enter'&&event.target.value==='reset'){event.preventDefault();resetUsage()}});
  document.getElementById('reset-confirm').addEventListener('click',resetUsage);
}
