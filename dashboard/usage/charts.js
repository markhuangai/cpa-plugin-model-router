const chartInteractions=new Map();
const chartActive={id:'',index:-1,anchor:null,key:null};
let chartResizeFrame=0;
let dashboardResizeObserver=null;
function chartColor(name){return getComputedStyle(document.documentElement).getPropertyValue(name).trim()}
function prepareChart(id){
  const canvas=document.getElementById(id);
  const rect=canvas.getBoundingClientRect();
  const width=Math.max(280,Math.floor(rect.width||600));
  const height=Math.max(220,Math.floor(rect.height||260));
  const scale=Math.min(2,window.devicePixelRatio||1);
  canvas.width=Math.floor(width*scale);canvas.height=Math.floor(height*scale);
  const context=canvas.getContext('2d');
  context.setTransform(scale,0,0,scale,0,0);
  context.clearRect(0,0,width,height);
  return{canvas,context,width,height,left:52,right:18,top:14,bottom:34,plotWidth:width-70,plotHeight:height-48};
}
function visibleChartPoints(kind){
  const points=usageState.overview&&usageState.overview.series||[];
  const zoom=Math.max(1,Number(usageState.zoom[kind]||1));
  const count=Math.max(2,Math.ceil(points.length/zoom));
  return count>=points.length?points:points.slice(points.length-count);
}
function chartMaximum(points,value,floor){
  return points.reduce((maximum,point)=>Math.max(maximum,value(point)),floor);
}
function chartBucketTitle(value){
  const date=new Date(value);
  return Number.isNaN(date.getTime())?'Unknown time':new Intl.DateTimeFormat(undefined,{year:'numeric',month:'short',day:'numeric',hour:'2-digit',minute:'2-digit'}).format(date);
}
function drawEmptyChart(chart,message){
  chart.context.fillStyle=chartColor('--muted');
  chart.context.font='600 12px "Segoe UI",sans-serif';
  chart.context.textAlign='center';chart.context.textBaseline='middle';
  chart.context.fillText(message,chart.width/2,chart.height/2);
}
function roundedRectPath(context,x,y,width,height,radius){
  const corner=Math.max(0,Math.min(radius,width/2,height/2));
  context.beginPath();context.moveTo(x+corner,y);context.lineTo(x+width-corner,y);context.quadraticCurveTo(x+width,y,x+width,y+corner);context.lineTo(x+width,y+height-corner);context.quadraticCurveTo(x+width,y+height,x+width-corner,y+height);context.lineTo(x+corner,y+height);context.quadraticCurveTo(x,y+height,x,y+height-corner);context.lineTo(x,y+corner);context.quadraticCurveTo(x,y,x+corner,y);context.closePath();
}
function fillRoundedRect(context,x,y,width,height,radius){
  if(width<=0||height<=0)return;
  roundedRectPath(context,x,y,width,height,radius);context.fill();
}
function strokeRoundedRect(context,x,y,width,height,radius){
  if(width<=0||height<=0)return;
  roundedRectPath(context,x,y,width,height,radius);context.stroke();
}
function drawPlotBackdrop(chart){
  const context=chart.context;context.save();context.fillStyle=chartColor('--chart-backdrop');fillRoundedRect(context,chart.left,chart.top,chart.plotWidth,chart.plotHeight,9);context.restore();
}
function drawGrid(chart,maxValue,formatter=formatCompact,rightFormatter=null){
  const context=chart.context;
  drawPlotBackdrop(chart);
  context.font='10px "Segoe UI",sans-serif';context.textBaseline='middle';
  for(let index=0;index<=4;index++){
    const y=chart.top+chart.plotHeight*index/4;
    context.strokeStyle=chartColor('--chart-grid');context.lineWidth=1;context.setLineDash(index===4?[]:[2,4]);
    context.beginPath();context.moveTo(chart.left,y);context.lineTo(chart.left+chart.plotWidth,y);context.stroke();
    const value=maxValue*(1-index/4);
    context.fillStyle=chartColor('--muted');context.textAlign='right';context.fillText(formatter(value),chart.left-7,y);
    if(rightFormatter){context.textAlign='left';context.fillText(rightFormatter(1-index/4),chart.left+chart.plotWidth+6,y)}
  }
  context.setLineDash([]);
}
function bucketLabel(value){
  const date=new Date(value);
  if(Number.isNaN(date.getTime()))return '';
  const options=usageState.range==='5h'||usageState.range==='24h'?{hour:'2-digit',minute:'2-digit'}:{month:'short',day:'numeric'};
  return new Intl.DateTimeFormat(undefined,options).format(date);
}
function drawXLabels(chart,points,centered=true){
  if(!points.length)return;
  const context=chart.context,step=Math.max(1,Math.ceil(points.length/6));
  context.fillStyle=chartColor('--muted');context.font='10px "Segoe UI",sans-serif';context.textAlign='center';context.textBaseline='top';
  points.forEach((point,index)=>{if(index%step&&index!==points.length-1)return;const x=centered?chart.left+chart.plotWidth*(index+.5)/points.length:chart.left+chart.plotWidth*(points.length===1?.5:index/(points.length-1));context.fillText(bucketLabel(point.time),x,chart.top+chart.plotHeight+8)});
}
function tooltipContent(content){
  const title=document.createElement('div'),rows=document.createElement('div');
  title.className='chart-tooltip-title';title.textContent=content.title;
  rows.className='chart-tooltip-rows';
  for(const item of content.rows){
    const row=document.createElement('div'),swatch=document.createElement('span'),label=document.createElement('span'),value=document.createElement('span');
    row.className='chart-tooltip-row';swatch.className='chart-tooltip-swatch';label.className='chart-tooltip-label';value.className='chart-tooltip-value';
    if(item.color)swatch.style.backgroundColor=chartColor(item.color)||item.color;
    label.textContent=item.label;value.textContent=item.value;
    row.appendChild(swatch);row.appendChild(label);row.appendChild(value);rows.appendChild(row);
  }
  chartTooltipEl.replaceChildren(title,rows);
}
function tooltipAnchorPoint(interaction,item,anchor){
  if(anchor&&anchor.type==='pointer')return{x:anchor.x,y:anchor.y};
  if(anchor&&anchor.type==='element'&&anchor.element&&anchor.element.isConnected){
    const rect=anchor.element.getBoundingClientRect();return{x:rect.right,y:rect.top+rect.height/2};
  }
  const rect=interaction.canvas.getBoundingClientRect(),point=item.anchor||{x:interaction.canvas.clientWidth/2,y:20};
  return{x:rect.left+point.x*rect.width/interaction.width,y:rect.top+point.y*rect.height/interaction.height};
}
function showChartTooltip(){
  const interaction=chartInteractions.get(chartActive.id),item=interaction&&interaction.items[chartActive.index];
  if(!interaction||!item){chartTooltipEl.hidden=true;return}
  tooltipContent(interaction.describe(item,chartActive.index));
  chartTooltipEl.hidden=false;chartTooltipEl.style.left='0px';chartTooltipEl.style.top='0px';
  const anchor=tooltipAnchorPoint(interaction,item,chartActive.anchor),rect=chartTooltipEl.getBoundingClientRect();
  const left=Math.max(8,Math.min(window.innerWidth-rect.width-8,anchor.x+14));
  const top=Math.max(8,Math.min(window.innerHeight-rect.height-8,anchor.y+14));
  chartTooltipEl.style.left=left+'px';chartTooltipEl.style.top=top+'px';
}
function renderChartByID(id){
  if(id==='token-chart')renderTokenChart();
  else if(id==='model-chart')renderModelChart();
  else if(id==='cost-chart')renderCostChart();
  else if(id==='efficiency-chart')renderEfficiencyChart();
}
function activeChartIndex(id,count){return chartActive.id===id&&chartActive.index>=0&&chartActive.index<count?chartActive.index:-1}
function chartItemKey(id,item){return id==='model-chart'?item&&item.key:item&&item.point&&item.point.time}
function remapChartActive(id,items){
  if(chartActive.id!==id)return-1;
  if(chartActive.key!==null&&chartActive.key!==undefined){
    const index=items.findIndex(item=>chartItemKey(id,item)===chartActive.key);
    if(index<0){clearChartActive(false);return-1}
    chartActive.index=index;
  }
  return activeChartIndex(id,items.length);
}
function setChartInteraction(id,interaction){
  chartInteractions.set(id,interaction);
  if(chartActive.id===id&&chartActive.index>=interaction.items.length){chartActive.id='';chartActive.index=-1;chartActive.anchor=null;chartActive.key=null;chartTooltipEl.hidden=true}
}
function clearChartInteraction(id){
  chartInteractions.delete(id);
  if(chartActive.id===id){chartActive.id='';chartActive.index=-1;chartActive.anchor=null;chartActive.key=null;chartTooltipEl.hidden=true}
}
function clearChartActive(redraw=true){
  const previous=chartActive.id;
  chartActive.id='';chartActive.index=-1;chartActive.anchor=null;chartActive.key=null;chartTooltipEl.hidden=true;
  if(redraw&&previous)renderChartByID(previous);
}
function resetChartInteractions(){
  chartInteractions.clear();
  chartActive.id='';chartActive.index=-1;chartActive.anchor=null;chartActive.key=null;chartTooltipEl.hidden=true;
}
function setChartActive(id,index,anchor){
  const interaction=chartInteractions.get(id);
  if(!interaction||index<0||index>=interaction.items.length){clearChartActive();return}
  const item=interaction.items[index],previous=chartActive.id,changed=previous!==id||chartActive.index!==index;
  chartActive.id=id;chartActive.index=index;chartActive.anchor=anchor||null;chartActive.key=chartItemKey(id,item);
  if(changed){if(previous&&previous!==id)renderChartByID(previous);renderChartByID(id)}
  showChartTooltip();
}
function chartPointerIndex(interaction,event){
  const rect=interaction.canvas.getBoundingClientRect();
  if(!rect.width||!rect.height)return-1;
  return interaction.hitTest((event.clientX-rect.left)*interaction.width/rect.width,(event.clientY-rect.top)*interaction.height/rect.height);
}
function pointerAnchor(event){return{type:'pointer',x:event.clientX,y:event.clientY}}
function initializeChartInteractions(){
  document.querySelectorAll('.chart-canvas').forEach(canvas=>{
    canvas.addEventListener('pointermove',event=>{
      const interaction=chartInteractions.get(canvas.id);if(!interaction)return;
      const index=chartPointerIndex(interaction,event);
      if(index>=0)setChartActive(canvas.id,index,pointerAnchor(event));
      else if(chartActive.id===canvas.id&&chartActive.anchor&&chartActive.anchor.type==='pointer')clearChartActive();
    });
    canvas.addEventListener('pointerleave',()=>{if(chartActive.id===canvas.id&&chartActive.anchor&&chartActive.anchor.type==='pointer')clearChartActive()});
    canvas.addEventListener('focus',()=>{const interaction=chartInteractions.get(canvas.id);if(interaction&&interaction.items.length)setChartActive(canvas.id,Math.max(0,activeChartIndex(canvas.id,interaction.items.length)),{type:'canvas'})});
    canvas.addEventListener('blur',()=>{if(chartActive.id===canvas.id&&(!chartActive.anchor||chartActive.anchor.type!=='pointer'))clearChartActive()});
    canvas.addEventListener('click',event=>{
      const interaction=chartInteractions.get(canvas.id);if(!interaction||!interaction.activate)return;
      const index=chartPointerIndex(interaction,event);if(index>=0)interaction.activate(interaction.items[index]);
    });
    canvas.addEventListener('keydown',event=>{
      const interaction=chartInteractions.get(canvas.id);if(!interaction||!interaction.items.length)return;
      let index=activeChartIndex(canvas.id,interaction.items.length);
      if(event.key==='ArrowRight'||event.key==='ArrowDown')index=Math.min(interaction.items.length-1,index<0?0:index+1);
      else if(event.key==='ArrowLeft'||event.key==='ArrowUp')index=Math.max(0,index<0?0:index-1);
      else if(event.key==='Home')index=0;
      else if(event.key==='End')index=interaction.items.length-1;
      else if(event.key==='Escape'){event.preventDefault();clearChartActive();return}
      else if((event.key==='Enter'||event.key===' ')&&interaction.activate&&index>=0){event.preventDefault();interaction.activate(interaction.items[index]);return}
      else return;
      event.preventDefault();setChartActive(canvas.id,index,{type:'canvas'});
    });
  });
}
function tokenChartValue(point,item,definitions){
  let value=item.key==='cache_read'?effectiveCacheReadValue(point):Number(point[item.field]||(item.fallback&&point[item.fallback])||0);
  if(item.key==='input'&&definitions.some(definition=>definition.key==='cache_read'))value=Math.max(0,value-Number(point.cache_read_included_tokens||0));
  if(item.key==='output'&&definitions.some(definition=>definition.key==='reasoning'))value=Math.max(0,value-Number(point.reasoning_included_tokens||0));
  return value;
}
function renderTokenChart(){
  const chart=prepareChart('token-chart'),points=visibleChartPoints('tokens');
  const definitions=[
    {key:'input',label:'Input',field:'input_tokens',color:'--chart-input'},
    {key:'output',label:'Output',field:'output_tokens',color:'--chart-output'},
    {key:'cache_read',label:'Cache read',field:'effective_cache_read_tokens',color:'--chart-cache'},
    {key:'reasoning',label:'Reasoning',field:'reasoning_tokens',color:'--chart-reasoning'}
  ].filter(item=>!usageState.hiddenTokenSeries.has(item.key)).map(item=>({...item,resolvedColor:chartColor(item.color)}));
  if(!points.length){clearChartInteraction('token-chart');drawEmptyChart(chart,'No token usage in this range.');return}
  if(!definitions.length){clearChartInteraction('token-chart');drawEmptyChart(chart,'All token series are hidden.');return}
  const total=point=>definitions.reduce((sum,item)=>sum+tokenChartValue(point,item,definitions),0),max=chartMaximum(points,total,1);
  const slot=chart.plotWidth/points.length,barWidth=Math.max(4,Math.min(30,slot*.62)),items=points.map((point,index)=>({point,anchor:{x:chart.left+slot*(index+.5),y:chart.top+10}})),active=remapChartActive('token-chart',items);
  drawGrid(chart,max);
  if(active>=0){chart.context.save();chart.context.globalAlpha=.9;chart.context.fillStyle=chartColor('--chart-highlight');fillRoundedRect(chart.context,chart.left+slot*active+1,chart.top+1,Math.max(0,slot-2),chart.plotHeight-2,8);chart.context.restore()}
  points.forEach((point,index)=>{
    const activeBucket=index===active,currentBarWidth=activeBucket?Math.min(slot*.84,barWidth*1.2):barWidth,barX=chart.left+slot*index+(slot-currentBarWidth)/2;
    let bottom=chart.top+chart.plotHeight;
    definitions.forEach(item=>{
      const value=tokenChartValue(point,item,definitions),height=value/max*chart.plotHeight;
      if(height>0){const context=chart.context,gap=Math.min(2,height*.16),segmentHeight=Math.max(1,height-gap);context.save();context.fillStyle=item.resolvedColor;context.globalAlpha=activeBucket?.98:.9;if(activeBucket){context.shadowColor=item.resolvedColor;context.shadowBlur=10}fillRoundedRect(context,barX,bottom-height+gap/2,currentBarWidth,segmentHeight,Math.min(5,currentBarWidth/3,segmentHeight/2));context.restore()}
      bottom-=height;
    });
    if(activeBucket&&total(point)>0){const height=total(point)/max*chart.plotHeight,context=chart.context;context.save();context.strokeStyle=chartColor('--blue');context.lineWidth=1.5;context.shadowColor=chartColor('--blue');context.shadowBlur=8;strokeRoundedRect(context,barX-3,chart.top+chart.plotHeight-height-3,currentBarWidth+6,height+6,6);context.shadowBlur=0;context.fillStyle=chartColor('--blue');context.beginPath();context.arc(barX+currentBarWidth/2,chart.top+chart.plotHeight-height-8,3.5,0,Math.PI*2);context.fill();context.restore()}
  });
  drawXLabels(chart,points);
  setChartInteraction('token-chart',{canvas:chart.canvas,width:chart.width,height:chart.height,items,hitTest:(x,y)=>x>=chart.left&&x<=chart.left+chart.plotWidth&&y>=chart.top&&y<=chart.top+chart.plotHeight+18?Math.min(points.length-1,Math.floor((x-chart.left)/slot)):-1,describe:item=>({title:chartBucketTitle(item.point.time),rows:[...definitions.map(definition=>({label:definition.label,value:formatNumber(tokenChartValue(item.point,definition,definitions)),color:definition.color})),{label:'Visible total',value:formatNumber(total(item.point))},{label:'Requests',value:formatNumber(item.point.requests)},{label:'Estimated cost',value:formatUSD(item.point.cost_usd)}]})});
}
function linePointX(chart,count,index){return chart.left+chart.plotWidth*(count===1?.5:index/(count-1))}
function linePointY(chart,point,value,maxValue){return chart.top+chart.plotHeight-Math.max(0,value(point))/maxValue*chart.plotHeight}
function drawLine(chart,points,value,color,maxValue,activeIndex=-1){
  const context=chart.context,coordinates=points.map((point,index)=>({x:linePointX(chart,points.length,index),y:linePointY(chart,point,value,maxValue)}));
  if(!coordinates.length)return;
  context.save();context.lineJoin='round';context.lineCap='round';context.beginPath();coordinates.forEach((coordinate,index)=>{if(index===0)context.moveTo(coordinate.x,coordinate.y);else context.lineTo(coordinate.x,coordinate.y)});
  context.strokeStyle=color;context.globalAlpha=.28;context.lineWidth=9;context.shadowColor=color;context.shadowBlur=14;context.stroke();
  context.globalAlpha=1;context.shadowBlur=0;context.lineWidth=2.5;context.stroke();
  if(points.length<=24)coordinates.forEach((coordinate,index)=>{const active=index===activeIndex;context.save();context.fillStyle=chartColor('--paper');context.strokeStyle=color;context.lineWidth=active?2.8:2;context.shadowColor=color;context.shadowBlur=active?11:0;context.beginPath();context.arc(coordinate.x,coordinate.y,active?4.8:3.2,0,Math.PI*2);context.fill();context.stroke();context.restore()});
  context.restore();
}
function drawActiveLineGuide(chart,x,markers){
  const context=chart.context;context.save();context.strokeStyle=chartColor('--blue');context.globalAlpha=.7;context.lineWidth=1.5;context.setLineDash([2,5]);context.beginPath();context.moveTo(x,chart.top);context.lineTo(x,chart.top+chart.plotHeight);context.stroke();context.setLineDash([]);
  for(const marker of markers){context.save();context.fillStyle=chartColor('--paper');context.strokeStyle=marker.color;context.lineWidth=3;context.shadowColor=marker.color;context.shadowBlur=15;context.beginPath();context.arc(x,marker.y,7,0,Math.PI*2);context.fill();context.stroke();context.shadowBlur=0;context.lineWidth=2;context.beginPath();context.arc(x,marker.y,4,0,Math.PI*2);context.stroke();context.restore()}
  context.restore();
}
function lineHitIndex(chart,count,x,y){
  if(!count||x<chart.left-10||x>chart.left+chart.plotWidth+10||y<chart.top||y>chart.top+chart.plotHeight+18)return-1;
  if(count===1)return 0;
  return Math.max(0,Math.min(count-1,Math.round((x-chart.left)/chart.plotWidth*(count-1))));
}
function renderCostChart(){
  const chart=prepareChart('cost-chart'),points=visibleChartPoints('cost');
  if(!points.length){clearChartInteraction('cost-chart');drawEmptyChart(chart,'No cost data in this range.');return}
  const value=point=>Number(point.cost_usd||0),max=chartMaximum(points,value,.000001),items=points.map((point,index)=>({point,anchor:{x:linePointX(chart,points.length,index),y:chart.top+chart.plotHeight-value(point)/max*chart.plotHeight}})),active=remapChartActive('cost-chart',items);
  drawGrid(chart,max,item=>formatUSD(item));drawLine(chart,points,value,chartColor('--chart-cost'),max,active);drawXLabels(chart,points,false);
  if(active>=0){const x=linePointX(chart,points.length,active),y=chart.top+chart.plotHeight-value(points[active])/max*chart.plotHeight;drawActiveLineGuide(chart,x,[{y,color:chartColor('--chart-cost')}])}
  setChartInteraction('cost-chart',{canvas:chart.canvas,width:chart.width,height:chart.height,items,hitTest:(x,y)=>lineHitIndex(chart,points.length,x,y),describe:item=>({title:chartBucketTitle(item.point.time),rows:[{label:'Estimated cost',value:formatUSD(item.point.cost_usd),color:'--chart-cost'},{label:'Requests',value:formatNumber(item.point.requests)}]})});
}
function renderEfficiencyChart(){
  const chart=prepareChart('efficiency-chart'),points=visibleChartPoints('efficiency');
  if(!points.length){clearChartInteraction('efficiency-chart');drawEmptyChart(chart,'No timing data in this range.');return}
  const latency=point=>Number(point.average_latency_ns||0)/1e6,ttft=point=>Number(point.average_ttft_ns||0)/1e6,tps=point=>Number(point.average_tps||0);
  const timingMax=chartMaximum(points,point=>Math.max(latency(point),ttft(point)),1),tpsMax=chartMaximum(points,tps,1),scaledTPS=point=>tps(point)*timingMax/tpsMax;
  const items=points.map((point,index)=>({point,anchor:{x:linePointX(chart,points.length,index),y:chart.top+10}})),active=remapChartActive('efficiency-chart',items);
  drawGrid(chart,timingMax,value=>value>=1000?(value/1000).toFixed(1)+'s':Math.round(value)+'ms',ratio=>(tpsMax*ratio).toFixed(0));
  drawLine(chart,points,latency,chartColor('--chart-input'),timingMax,active);drawLine(chart,points,ttft,chartColor('--chart-cache'),timingMax,active);drawLine(chart,points,scaledTPS,chartColor('--chart-output'),timingMax,active);drawXLabels(chart,points,false);
  if(active>=0){const point=points[active],x=linePointX(chart,points.length,active),toY=value=>chart.top+chart.plotHeight-value/timingMax*chart.plotHeight;drawActiveLineGuide(chart,x,[{y:toY(latency(point)),color:chartColor('--chart-input')},{y:toY(ttft(point)),color:chartColor('--chart-cache')},{y:toY(scaledTPS(point)),color:chartColor('--chart-output')}])}
  setChartInteraction('efficiency-chart',{canvas:chart.canvas,width:chart.width,height:chart.height,items,hitTest:(x,y)=>lineHitIndex(chart,points.length,x,y),describe:item=>({title:chartBucketTitle(item.point.time),rows:[{label:'Avg latency',value:formatDuration(item.point.average_latency_ns),color:'--chart-input'},{label:'Avg TTFT',value:formatDuration(item.point.average_ttft_ns),color:'--chart-cache'},{label:'Tokens/sec',value:formatTPS(item.point.average_tps),color:'--chart-output'},{label:'Requests',value:formatNumber(item.point.requests)}]})});
}
function setProviderModelFilter(model){
  usageState.providerModel=usageState.providerModel===model?'':model;
  clearChartActive(false);renderUsageFilterOptions();usageControlChanged();
}
function modelTooltip(item,total){
  return{title:item.model,rows:[{label:'Requests',value:formatNumber(item.requests),color:item.color},{label:'Share',value:(item.requests/total*100).toFixed(2)+'%'},{label:'Total tokens',value:formatNumber(item.total_tokens)},{label:'Estimated cost',value:formatUSD(item.cost_usd)}]};
}
function renderModelLegend(items,total,focusedKey=''){
  const legend=document.getElementById('model-legend'),signature=JSON.stringify(items.map(item=>[item.model,item.requests,item.total_tokens,item.cost_usd,item.filterable]));
  if(legend.dataset.signature!==signature){
    const fragment=document.createDocumentFragment();
    items.forEach((item,index)=>{
      const entry=document.createElement(item.filterable?'button':'span'),swatch=document.createElement('span'),label=document.createElement('span');
      if(item.filterable){entry.type='button';entry.setAttribute('aria-pressed','false')}else{entry.tabIndex=0;entry.setAttribute('role','img')}
      entry.className='model-legend-entry';entry.dataset.modelIndex=String(index);entry.title=item.model+' · '+(item.requests/total*100).toFixed(1)+'%';entry.setAttribute('aria-label',entry.title);
      swatch.className='legend-swatch';swatch.style.backgroundColor=item.color;label.className='legend-label';label.textContent=item.model+' '+(item.requests/total*100).toFixed(1)+'%';entry.appendChild(swatch);entry.appendChild(label);
      entry.addEventListener('pointerenter',event=>setChartActive('model-chart',index,pointerAnchor(event)));
      entry.addEventListener('pointermove',event=>setChartActive('model-chart',index,pointerAnchor(event)));
      entry.addEventListener('pointerleave',()=>{if(chartActive.id==='model-chart'&&chartActive.anchor&&chartActive.anchor.type==='pointer')clearChartActive()});
      entry.addEventListener('focus',()=>setChartActive('model-chart',index,{type:'element',element:entry}));
      entry.addEventListener('blur',()=>{if(chartActive.id==='model-chart'&&chartActive.anchor&&chartActive.anchor.type==='element')clearChartActive()});
      if(item.filterable)entry.addEventListener('click',()=>setProviderModelFilter(item.model));
      fragment.appendChild(entry);
    });
    if(focusedKey&&chartActive.id==='model-chart'&&chartActive.anchor&&chartActive.anchor.type==='element')chartActive.anchor=null;
    legend.replaceChildren(fragment);legend.dataset.signature=signature;
    if(focusedKey){
      const index=items.findIndex(item=>item.key===focusedKey),entry=index<0?null:legend.querySelector('[data-model-index="'+index+'"]');
      if(entry)entry.focus({preventScroll:true});
    }
  }
  legend.querySelectorAll('[data-model-index]').forEach(entry=>{const index=Number(entry.dataset.modelIndex),item=items[index],active=activeChartIndex('model-chart',items.length)===index||Boolean(item&&item.filterable&&usageState.providerModel===item.model);entry.classList.toggle('is-active',active);if(item&&item.filterable)entry.setAttribute('aria-pressed',String(usageState.providerModel===item.model))});
}
function renderModelChart(){
  const previousInteraction=chartInteractions.get('model-chart'),focusedEntry=document.activeElement&&document.activeElement.closest('#model-legend [data-model-index]'),focusedKey=focusedEntry&&previousInteraction?((previousInteraction.items[Number(focusedEntry.dataset.modelIndex)]||{}).key||''):'',chart=prepareChart('model-chart'),source=(usageState.overview&&usageState.overview.provider_models||[]).filter(item=>Number(item.requests||0)>0).sort((left,right)=>Number(right.requests||0)-Number(left.requests||0)),legend=document.getElementById('model-legend');
  if(!source.length){clearChartInteraction('model-chart');drawEmptyChart(chart,'No provider model requests.');legend.replaceChildren();legend.dataset.signature='';return}
  const palette=['#2d6cdf','#24836b','#a96708','#8657c8','#b83b43','#47a7dc','#70a84f','#b88755'];
  const items=source.slice(0,7).map((item,index)=>({model:item.model,key:'model:'+item.model,requests:Number(item.requests||0),total_tokens:Number(item.total_tokens||0),cost_usd:Number(item.cost_usd||0),filterable:true,color:palette[index%palette.length]}));
  if(source.length>7){const rest=source.slice(7);items.push({model:'Other',key:'aggregate:other',requests:rest.reduce((sum,item)=>sum+Number(item.requests||0),0),total_tokens:rest.reduce((sum,item)=>sum+Number(item.total_tokens||0),0),cost_usd:rest.reduce((sum,item)=>sum+Number(item.cost_usd||0),0),filterable:false,color:palette[7%palette.length]})}
  const total=items.reduce((sum,item)=>sum+item.requests,0),context=chart.context,cx=chart.width/2,cy=chart.height/2-4,radius=Math.min(chart.width,chart.height)*.31,lineWidth=Math.max(22,radius*.28),remappedActive=remapChartActive('model-chart',items),gapAngle=Math.min(.07,Math.PI*2/items.length*.24);
  context.save();context.lineWidth=lineWidth+4;context.strokeStyle=chartColor('--chart-backdrop');context.beginPath();context.arc(cx,cy,radius,0,Math.PI*2);context.stroke();context.restore();
  let angle=-Math.PI/2;
  items.forEach((item,index)=>{item.start=angle;item.end=angle+item.requests/total*Math.PI*2;const span=item.end-item.start,segmentGap=Math.min(gapAngle,span*.3);item.drawStart=item.start+segmentGap;item.drawEnd=item.end-segmentGap;const middle=(item.start+item.end)/2;item.anchor={x:cx+Math.cos(middle)*radius,y:cy+Math.sin(middle)*radius};const selected=item.filterable&&usageState.providerModel===item.model,isActive=index===remappedActive||selected,isDimmed=(remappedActive>=0&&remappedActive!==index)||(usageState.providerModel&&!selected);context.save();if(isDimmed)context.globalAlpha=.2;context.lineWidth=isActive?lineWidth+8:lineWidth;context.strokeStyle=item.color;context.lineCap='butt';context.shadowColor=isActive?item.color:'transparent';context.shadowBlur=isActive?14:0;const lift=isActive?5:0,offsetX=Math.cos(middle)*lift,offsetY=Math.sin(middle)*lift;context.beginPath();context.arc(cx+offsetX,cy+offsetY,radius,item.drawStart,item.drawEnd);context.stroke();context.restore();angle=item.end});
  context.fillStyle=chartColor('--ink');context.textAlign='center';context.textBaseline='middle';context.font='750 23px "Cascadia Code",monospace';context.fillText(formatCompact(total),cx,cy-5);context.fillStyle=chartColor('--muted');context.font='11px "Segoe UI",sans-serif';context.fillText('requests',cx,cy+17);
  const hitTest=(x,y)=>{const distance=Math.hypot(x-cx,y-cy);if(distance<radius-lineWidth/2-8||distance>radius+lineWidth/2+12)return-1;let point=Math.atan2(y-cy,x-cx);if(point < -Math.PI/2)point+=Math.PI*2;return items.findIndex(item=>point>=item.drawStart&&point<=item.drawEnd)};
  setChartInteraction('model-chart',{canvas:chart.canvas,width:chart.width,height:chart.height,items,hitTest,describe:item=>modelTooltip(item,total),activate:item=>{if(item.filterable)setProviderModelFilter(item.model)}});
  renderModelLegend(items,total,focusedKey);
}
function syncTokenLegend(){
  document.querySelectorAll('[data-token-series]').forEach(button=>button.setAttribute('aria-pressed',String(!usageState.hiddenTokenSeries.has(button.dataset.tokenSeries))));
}
function renderUsageCharts(){
  if(!usageState.active||usagePanelEl.hidden||!usageState.overview)return;
  renderTokenChart();renderModelChart();renderCostChart();renderEfficiencyChart();
  if(chartActive.id)showChartTooltip();
}
function scheduleResponsiveDashboardRender(){
  scheduleMetricFit();
  if(chartResizeFrame)return;
  chartResizeFrame=requestAnimationFrame(()=>{chartResizeFrame=0;renderUsageCharts()});
}
function initializeDashboardResizeObserver(){
  if(!window.ResizeObserver)return;
  dashboardResizeObserver=new ResizeObserver(scheduleResponsiveDashboardRender);
  document.querySelectorAll('.metric-grid,.chart-card').forEach(element=>dashboardResizeObserver.observe(element));
}

function initializeUsageChartEvents() {
  usagePanelEl.addEventListener('click',event=>{
    const series=event.target.closest('[data-token-series]');
    if(series){const key=series.dataset.tokenSeries;if(usageState.hiddenTokenSeries.has(key))usageState.hiddenTokenSeries.delete(key);else usageState.hiddenTokenSeries.add(key);clearChartActive(false);syncTokenLegend();renderTokenChart();scheduleUsagePreferencesSave();return}
    const zoom=event.target.closest('[data-chart][data-zoom]');
    if(!zoom)return;
    const key=zoom.dataset.chart,action=zoom.dataset.zoom;
    if(action==='reset')usageState.zoom[key]=1;
    else if(action==='in')usageState.zoom[key]=Math.min(8,usageState.zoom[key]*1.6);
    else usageState.zoom[key]=Math.max(1,usageState.zoom[key]/1.6);
    clearChartActive(false);
    renderUsageCharts();
  });
}
