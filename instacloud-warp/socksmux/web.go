package main

import (
	"net/http"
)

// handleUI serves the embedded single-page console. The page talks to the
// management API on the same origin and keeps the bearer token in localStorage.
func (a *apiServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(uiHTML))
}

const uiHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WARP Pool Console</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e9ef;--mut:#8b93a3;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--acc:#58a6ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,"Noto Sans SC",sans-serif}
header{display:flex;align-items:center;gap:12px;padding:14px 20px;border-bottom:1px solid var(--line);flex-wrap:wrap}
h1{font-size:16px;margin:0;font-weight:600}
.badge{font-size:12px;padding:2px 8px;border-radius:999px;border:1px solid var(--line);color:var(--mut)}
.badge.ok{color:var(--ok);border-color:#1f4327}.badge.warn{color:var(--warn);border-color:#4a3a12}.badge.bad{color:var(--bad);border-color:#4a1f1f}
main{padding:16px 20px 40px;max-width:1280px;margin:0 auto}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px;margin-bottom:16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.card .k{color:var(--mut);font-size:12px;margin-bottom:4px}
.card .v{font-size:20px;font-variant-numeric:tabular-nums}
section{margin-top:18px}
section h2{font-size:13px;letter-spacing:.02em;color:var(--mut);text-transform:uppercase;margin:0 0 8px}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:10px;overflow:hidden}
th,td{padding:8px 10px;border-bottom:1px solid var(--line);text-align:left;font-size:13px;vertical-align:middle}
th{color:var(--mut);font-weight:500;background:#141821}
tr:last-child td{border-bottom:0}
td.num{font-variant-numeric:tabular-nums}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;background:var(--mut)}
.dot.ok{background:var(--ok)}.dot.bad{background:var(--bad)}.dot.warn{background:var(--warn)}
button{background:#21262d;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:5px 10px;cursor:pointer;font-size:12px}
button:hover{border-color:#3d444d}button.primary{background:#1f6feb;border-color:#1f6feb}button:disabled{opacity:.5;cursor:default}
input,select{background:#0d1117;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:7px 9px;font-size:13px;width:100%}
label{display:block;color:var(--mut);font-size:12px;margin-bottom:4px}
.form{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;align-items:end}
pre{background:#0d1117;border:1px solid var(--line);border-radius:10px;padding:12px;overflow:auto;max-height:320px;font-size:12px;margin:0}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.right{margin-left:auto}
.muted{color:var(--mut)}
.toast{position:fixed;right:16px;bottom:16px;background:#1f6feb;color:#fff;padding:9px 14px;border-radius:8px;opacity:0;transition:opacity .2s;pointer-events:none;max-width:60vw}
.toast.show{opacity:1}.toast.err{background:#b62324}
.tabs{display:flex;gap:6px;margin-bottom:8px}.tabs button[aria-pressed="true"]{background:#1f6feb;border-color:#1f6feb}
.hint{font-size:12px;color:var(--mut);margin-top:6px}
</style>
</head>
<body>
<header>
  <h1>WARP Pool Console</h1>
  <span id="poolId" class="badge mono">-</span>
  <span id="healthBadge" class="badge">unknown</span>
  <div class="right row">
    <input id="token" type="password" placeholder="API token" style="width:220px">
    <button class="primary" onclick="saveToken()">连接</button>
  </div>
</header>
<main>
  <div class="grid">
    <div class="card"><div class="k">健康节点</div><div class="v" id="mHealthy">-</div></div>
    <div class="card"><div class="k">启用节点</div><div class="v" id="mEnabled">-</div></div>
    <div class="card"><div class="k">节点总数</div><div class="v" id="mCount">-</div></div>
    <div class="card"><div class="k">轮换间隔</div><div class="v" id="mRotation">-</div></div>
    <div class="card"><div class="k">Mux 端口</div><div class="v" id="mPort">-</div></div>
  </div>

  <section>
    <div class="row"><h2 style="margin:0">节点</h2><div class="right row">
      <button onclick="loadAll()">刷新</button>
      <button onclick="post('/api/v1/reroll')">全部轮换</button>
    </div></div>
    <table>
      <thead><tr><th>#</th><th>用户</th><th>状态</th><th>出口 /64</th><th>地区</th><th class="num">连接</th><th class="num">上行</th><th class="num">下行</th><th>更新时间</th><th>操作</th></tr></thead>
      <tbody id="nodes"><tr><td colspan="10" class="muted">未连接</td></tr></tbody>
    </table>
  </section>

  <section>
    <h2>参数</h2>
    <div class="card">
      <div class="form">
        <div><label>轮换间隔（秒）</label><input id="cfgRotation" type="number" min="60" max="86400"></div>
        <div><label>每轮轮换数</label><input id="cfgBatch" type="number" min="1"></div>
        <div><label>健康检查间隔（秒）</label><input id="cfgHealth" type="number" min="30" max="86400"></div>
        <div><label>最小健康数</label><input id="cfgMin" type="number" min="0"></div>
        <div><button class="primary" onclick="saveConfig()">保存并热加载</button></div>
      </div>
      <div class="hint">参数写入 /data/control/config.json，entrypoint 在下一个循环读取。</div>
    </div>
  </section>

  <section>
    <h2>对接地址</h2>
    <div class="card">
      <div class="tabs">
        <button id="tabPrivate" aria-pressed="true" onclick="setScope('private')">私网</button>
        <button id="tabPublic" aria-pressed="false" onclick="setScope('public')">公网</button>
        <button class="right" onclick="copyEndpoints()">复制</button>
      </div>
      <pre id="endpoints" class="mono">-</pre>
      <div class="hint" id="hostHint"></div>
    </div>
  </section>

  <section>
    <div class="row"><h2 style="margin:0">日志</h2><div class="right row">
      <select id="logLines" style="width:110px" onchange="loadLogs()"><option>100</option><option selected>200</option><option>500</option></select>
      <button onclick="loadLogs()">刷新</button>
    </div></div>
    <pre id="logs" class="mono">-</pre>
  </section>
</main>
<div id="toast" class="toast"></div>
<script>
let scope='private', timer=null;
const $=id=>document.getElementById(id);
function tok(){return localStorage.getItem('warp_pool_token')||''}
function saveToken(){localStorage.setItem('warp_pool_token',$('token').value.trim());loadAll();toast('已保存 token')}
async function api(path,opt={}){
  const r=await fetch(path,{...opt,headers:{'Authorization':'Bearer '+tok(),...(opt.headers||{})}});
  const ct=r.headers.get('content-type')||'';
  const body=ct.includes('json')?await r.json():await r.text();
  if(!r.ok)throw new Error(typeof body==='string'?body:(body.error||r.status));
  return body;
}
function toast(msg,err){const t=$('toast');t.textContent=msg;t.className='toast show'+(err?' err':'');clearTimeout(t._h);t._h=setTimeout(()=>t.className='toast',2600)}
function fmt(n){if(n<1024)return n+' B';if(n<1048576)return (n/1024).toFixed(1)+' KB';if(n<1073741824)return (n/1048576).toFixed(1)+' MB';return (n/1073741824).toFixed(2)+' GB'}
function ago(ts){if(!ts)return '-';const s=Math.max(0,Math.floor(Date.now()/1000-ts));if(s<60)return s+'s 前';if(s<3600)return Math.floor(s/60)+'m 前';return Math.floor(s/3600)+'h 前'}
async function loadStatus(){
  try{
    const st=await api('/api/v1/status');
    $('poolId').textContent=st.pool.id||'pool';
    const p=st.pool;
    $('mHealthy').textContent=p.healthy+' / '+p.count;
    $('mEnabled').textContent=p.enabled;
    $('mCount').textContent=p.count;
    $('mRotation').textContent=p.rotation_interval_seconds+'s';
    $('mPort').textContent=p.mux_port;
    const hb=$('healthBadge');
    hb.textContent=p.healthy>=p.min_healthy?'healthy':'degraded';
    hb.className='badge '+(p.healthy>=p.min_healthy?'ok':'bad');
    $('nodes').innerHTML=st.nodes.map(n=>{
      const cls=!n.enabled?'warn':(n.healthy?'ok':'bad');
      const state=!n.enabled?'disabled':(n.healthy?'healthy':(n.state||'unknown'));
      return '<tr>'
        +'<td class="num">'+n.index+'</td>'
        +'<td class="mono">'+n.user+'</td>'
        +'<td><span class="dot '+cls+'"></span>'+state+'</td>'
        +'<td class="mono">'+(n.prefix64||'-')+'</td>'
        +'<td>'+(n.loc||'-')+'</td>'
        +'<td class="num">'+n.connections+'</td>'
        +'<td class="num">'+fmt(n.bytes_up)+'</td>'
        +'<td class="num">'+fmt(n.bytes_down)+'</td>'
        +'<td class="muted">'+ago(n.updated_at)+'</td>'
        +'<td><div class="row">'
        +'<button onclick="nodeAction(\''+n.user+'\',\'rotate\')">轮换</button>'
        +'<button onclick="nodeAction(\''+n.user+'\',\''+(n.enabled?'disable':'enable')+'\')">'+(n.enabled?'停用':'启用')+'</button>'
        +'</div></td></tr>';
    }).join('');
  }catch(e){toast('状态获取失败: '+e.message,true)}
}
async function loadConfig(){
  try{
    const c=await api('/api/v1/config');
    $('cfgRotation').value=c.rotation_interval_seconds;
    $('cfgBatch').value=c.reroll_batch;
    $('cfgHealth').value=c.health_interval_seconds;
    $('cfgMin').value=c.min_healthy;
  }catch(e){toast('参数获取失败: '+e.message,true)}
}
async function loadEndpoints(){
  try{
    const txt=await api('/api/v1/endpoints?scope='+scope);
    $('endpoints').textContent=txt||'(空)';
    $('hostHint').textContent=scope==='private'?'私网地址供同平台主容器使用':'公网地址供外部机器使用';
  }catch(e){$('endpoints').textContent='错误: '+e.message}
}
async function loadLogs(){
  try{ $('logs').textContent=await api('/api/v1/logs?lines='+$('logLines').value) }
  catch(e){ $('logs').textContent='错误: '+e.message }
}
function loadAll(){loadStatus();loadConfig();loadEndpoints();loadLogs()}
function setScope(s){scope=s;$('tabPrivate').setAttribute('aria-pressed',s==='private');$('tabPublic').setAttribute('aria-pressed',s==='public');loadEndpoints()}
async function post(path){try{await api(path,{method:'POST'});toast('已排队');setTimeout(loadAll,1200)}catch(e){toast('失败: '+e.message,true)}}
function nodeAction(user,action){post('/api/v1/nodes/'+encodeURIComponent(user)+'/'+action)}
async function saveConfig(){
  const body={rotation_interval_seconds:+$('cfgRotation').value,reroll_batch:+$('cfgBatch').value,health_interval_seconds:+$('cfgHealth').value,min_healthy:+$('cfgMin').value};
  try{await api('/api/v1/config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});toast('已保存');loadAll()}catch(e){toast('保存失败: '+e.message,true)}
}
function copyEndpoints(){navigator.clipboard.writeText($('endpoints').textContent).then(()=>toast('已复制'),()=>toast('复制失败',true))}
$('token').value=tok();
if(tok())loadAll();else toast('先填入 API token');
clearInterval(timer);timer=setInterval(()=>{if(tok())loadStatus()},5000);
</script>
</body>
</html>`
