function visibleColumns(definitions,hidden){return definitions.filter(column=>!hidden.has(column.key))}
function renderColumnControls(definitions,hidden,containerID,kind){
  const container=document.getElementById(containerID);
  const fragment=document.createDocumentFragment();
  const visibleCount=visibleColumns(definitions,hidden).length;
  definitions.forEach(column=>{
    const label=document.createElement('label');
    const input=document.createElement('input');
    const text=document.createElement('span');
    input.type='checkbox';
    input.checked=!hidden.has(column.key);
    input.disabled=input.checked&&visibleCount<=1;
    input.dataset.columnKind=kind;
    input.dataset.columnKey=column.key;
    text.textContent=column.label;
    label.appendChild(input);label.appendChild(text);fragment.appendChild(label);
  });
  container.replaceChildren(fragment);
}
function renderTableHeaders(definitions,hidden,rowID,tableID,kind,sort,order){
  const visible=visibleColumns(definitions,hidden);
  const row=document.getElementById(rowID);
  const signature=visible.map(column=>column.key).join(',')+'|'+sort+'|'+order;
  if(row.dataset.signature===signature)return visible;
  const fragment=document.createDocumentFragment();
  visible.forEach(column=>{
    const th=document.createElement('th');
    th.dataset.column=column.key;
    if(column.text)th.className='text-cell';
    const active=column.sort===sort;
    th.setAttribute('aria-sort',active?(order==='asc'?'ascending':'descending'):'none');
    if(column.sort){
      const button=document.createElement('button');
      button.type='button';button.className='sort-button';button.dataset.tableKind=kind;button.dataset.sortField=column.sort;
      const label=document.createElement('span');label.textContent=column.label;button.appendChild(label);
      if(active){const indicator=document.createElement('span');indicator.setAttribute('aria-hidden','true');indicator.textContent=order==='asc'?'↑':'↓';button.appendChild(indicator)}
      th.appendChild(button);
    }else th.textContent=column.label;
    fragment.appendChild(th);
  });
  row.replaceChildren(fragment);
  row.dataset.signature=signature;
  document.getElementById(tableID).style.minWidth=Math.max(720,visible.length*112)+'px';
  return visible;
}
function tableCell(row,value,text=false){
  const cell=document.createElement('td');
  cell.textContent=value===undefined||value===null||value===''?'—':String(value);
  if(text)cell.className='text-cell';
  row.appendChild(cell);
  return cell;
}
function statusCell(row,result){
  const cell=document.createElement('td');
  cell.className='text-cell';
  const badge=document.createElement('span');
  badge.className='status-pill '+(result==='success'?'success':'failure');
  badge.textContent=resultDisplay(result);
  cell.appendChild(badge);row.appendChild(cell);
}
function groupCell(row,item,column){
  switch(column.key){
  case 'key':return tableCell(row,usageState.group.dimension==='router_model'?routerDisplay(item):item.key,true);
  case 'router_model':return tableCell(row,routerDisplay(item),true);
  case 'provider_model':case 'provider':case 'source':case 'service_tier':return tableCell(row,item[column.key],true);
  case 'result':return statusCell(row,item.result);
  case 'cache_read_tokens':return tableCell(row,formatNumber(effectiveCacheReadValue(item)));
  case 'latency':return tableCell(row,formatDuration(item.average_latency_ns));
  case 'ttft':return tableCell(row,formatDuration(item.average_ttft_ns));
  case 'tps':return tableCell(row,formatTPS(item.average_tps));
  case 'cost':return tableCell(row,formatUSD(item.cost_usd));
  default:return tableCell(row,formatNumber(item[column.key]));
  }
}
function requestCell(row,item,column){
  switch(column.key){
  case 'time':return tableCell(row,formatRequestTime(item.requested_at),true);
  case 'router_model':return tableCell(row,routerDisplay(item),true);
  case 'provider_model':case 'provider':case 'source':case 'service_tier':return tableCell(row,item[column.key],true);
  case 'result':return statusCell(row,item.result);
  case 'latency':return tableCell(row,formatDuration(item.latency_ns));
  case 'ttft':return tableCell(row,formatDuration(item.ttft_ns));
  case 'tps':return tableCell(row,formatTPS(item.tps));
  case 'cost':return tableCell(row,item.estimated_cost&&item.estimated_cost.priced?formatUSD(item.estimated_cost.total_usd):'—');
  case 'api_key':return tableCell(row,item.masked_api_key,true);
  case 'cache_read_tokens':return tableCell(row,formatNumber(effectiveCacheReadValue(item)));
  default:return tableCell(row,formatNumber(item[column.key]));
  }
}
function replaceTableRows(body,scroll,fragment){
  const top=scroll.scrollTop,left=scroll.scrollLeft;
  body.replaceChildren(fragment);
  scroll.scrollTop=top;scroll.scrollLeft=left;
}
function renderGroupTable(){
  const page=usageState.groupPage;
  const columns=renderTableHeaders(groupColumns,usageState.hiddenGroupColumns,'group-headers','group-table','group',usageState.group.sort,usageState.group.order);
  const fragment=document.createDocumentFragment();
  if(!page||!Array.isArray(page.items)||page.items.length===0){
    const row=document.createElement('tr'),cell=document.createElement('td');cell.colSpan=columns.length;cell.className='empty-row';cell.textContent='No usage groups match this range.';row.appendChild(cell);fragment.appendChild(row);
  }else page.items.forEach(item=>{const row=document.createElement('tr');columns.forEach(column=>groupCell(row,item,column));fragment.appendChild(row)});
  replaceTableRows(document.getElementById('usage-groups'),document.getElementById('group-table-scroll'),fragment);
  const total=Number(page&&page.total||0),start=total?usageState.group.offset+1:0,end=Math.min(usageState.group.offset+Number(page&&page.items&&page.items.length||0),total);
  document.getElementById('group-page-status').textContent=start+'–'+end+' of '+formatNumber(total)+' groups';
  document.getElementById('group-prev').disabled=usageState.group.offset<=0;
  document.getElementById('group-next').disabled=usageState.group.offset+usageState.group.limit>=total;
}
function renderRequestTable(){
  const page=usageState.requestPage;
  const columns=renderTableHeaders(requestColumns,usageState.hiddenRequestColumns,'request-headers','request-table','request',usageState.request.sort,usageState.request.order);
  const fragment=document.createDocumentFragment();
  if(!page||!Array.isArray(page.items)||page.items.length===0){
    const row=document.createElement('tr'),cell=document.createElement('td');cell.colSpan=columns.length;cell.className='empty-row';cell.textContent='No requests match this range.';row.appendChild(cell);fragment.appendChild(row);
  }else page.items.forEach(item=>{const row=document.createElement('tr');columns.forEach(column=>requestCell(row,item,column));fragment.appendChild(row)});
  replaceTableRows(document.getElementById('usage-requests'),document.getElementById('request-table-scroll'),fragment);
  const total=Number(page&&page.total||0),start=total?usageState.request.offset+1:0,end=Math.min(usageState.request.offset+Number(page&&page.items&&page.items.length||0),total);
  document.getElementById('request-page-status').textContent=start+'–'+end+' of '+formatNumber(total)+' requests';
  document.getElementById('request-prev').disabled=usageState.request.offset<=0;
  document.getElementById('request-next').disabled=usageState.request.offset+usageState.request.limit>=total;
}
function renderUsageDashboard(){
  renderUsageSummary();
  renderUsageFilterOptions();
  renderGroupTable();
  renderRequestTable();
  renderUsageCharts();
}
function sortUsageTable(event){
  const button=event.target.closest('[data-table-kind][data-sort-field]');
  if(!button)return;
  const kind=button.dataset.tableKind,table=usageState[kind],field=button.dataset.sortField,definitions=kind==='group'?groupColumns:requestColumns,column=definitions.find(item=>item.sort===field);
  if(table.sort===field)table.order=table.order==='asc'?'desc':'asc';
  else{table.sort=field;table.order=column&&column.text?'asc':'desc'}
  table.offset=0;scheduleUsagePreferencesSave();refreshUsage(false,kind);
}
function changeColumnVisibility(event){
  const input=event.target.closest('[data-column-kind][data-column-key]');
  if(!input)return;
  const request=input.dataset.columnKind==='request',hidden=request?usageState.hiddenRequestColumns:usageState.hiddenGroupColumns,definitions=request?requestColumns:groupColumns;
  if(input.checked)hidden.delete(input.dataset.columnKey);
  else if(visibleColumns(definitions,hidden).length>1)hidden.add(input.dataset.columnKey);
  renderColumnControls(definitions,hidden,request?'request-column-options':'group-column-options',request?'request':'group');
  if(request)renderRequestTable();else renderGroupTable();
  scheduleUsagePreferencesSave();
}

function initializeUsageTableEvents() {
  document.getElementById('group-dimension').addEventListener('change',event=>{usageState.group.dimension=event.target.value;usageState.group.offset=0;scheduleUsagePreferencesSave();refreshUsage(false,'group')});
  document.getElementById('group-page-size').addEventListener('change',event=>{usageState.group.limit=Number(event.target.value);usageState.group.offset=0;scheduleUsagePreferencesSave();refreshUsage(false,'group')});
  document.getElementById('request-page-size').addEventListener('change',event=>{usageState.request.limit=Number(event.target.value);usageState.request.offset=0;scheduleUsagePreferencesSave();refreshUsage(false,'request')});
  document.getElementById('group-prev').addEventListener('click',()=>{usageState.group.offset=Math.max(0,usageState.group.offset-usageState.group.limit);refreshUsage(false,'group')});
  document.getElementById('group-next').addEventListener('click',()=>{usageState.group.offset+=usageState.group.limit;refreshUsage(false,'group')});
  document.getElementById('request-prev').addEventListener('click',()=>{usageState.request.offset=Math.max(0,usageState.request.offset-usageState.request.limit);refreshUsage(false,'request')});
  document.getElementById('request-next').addEventListener('click',()=>{usageState.request.offset+=usageState.request.limit;refreshUsage(false,'request')});
  document.getElementById('group-headers').addEventListener('click',sortUsageTable);
  document.getElementById('request-headers').addEventListener('click',sortUsageTable);
  document.getElementById('group-column-options').addEventListener('change',changeColumnVisibility);
  document.getElementById('request-column-options').addEventListener('change',changeColumnVisibility);
}
