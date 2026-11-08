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
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e9ef;--mut:#8b93a3;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--acc:#58a6ff;--acc2:#1f6feb}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,"Noto Sans SC",sans-serif}
a{color:var(--acc)}.hidden{display:none!important}
.login-wrap{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}
.login-card{width:100%;max-width:420px;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:26px}
.login-card h1{font-size:20px;margin:0 0 6px}.login-card p{margin:0 0 18px;color:var(--mut);font-size:13px}
header{display:flex;align-items:center;gap:10px;padding:12px 20px;border-bottom:1px solid var(--line);flex-wrap:wrap;position:sticky;top:0;background:rgba(15,17,21,.94);backdrop-filter:blur(8px);z-index:10}
h1{font-size:16px;margin:0;font-weight:600}
.badge{font-size:12px;padding:2px 8px;border-radius:999px;border:1px solid var(--line);color:var(--mut);white-space:nowrap}
.badge.ok{color:var(--ok);border-color:#1f4327}.badge.warn{color:var(--warn);border-color:#4a3a12}.badge.bad{color:var(--bad);border-color:#4a1f1f}
main{padding:14px 20px 60px;max-width:1320px;margin:0 auto}
.tabs{display:flex;gap:6px;flex-wrap:wrap;margin:4px 0 16px}
.tabs button{background:#161a22;color:var(--mut);border:1px solid var(--line);border-radius:8px;padding:7px 12px;cursor:pointer;font-size:13px}
.tabs button[aria-pressed="true"]{background:var(--acc2);border-color:var(--acc2);color:#fff}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:10px;margin-bottom:16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.card .k{color:var(--mut);font-size:12px;margin-bottom:4px}
.card .v{font-size:20px;font-variant-numeric:tabular-nums}
section{margin-top:18px}
section h2{font-size:13px;letter-spacing:.02em;color:var(--mut);text-transform:uppercase;margin:0 0 8px}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:10px;overflow:hidden}
th,td{padding:8px 10px;border-bottom:1px solid var(--line);text-align:left;font-size:13px;vertical-align:middle}
th{color:var(--mut);font-weight:500;background:#141821;white-space:nowrap}
tr:last-child td{border-bottom:0}
td.num,th.num{font-variant-numeric:tabular-nums;text-align:right}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;background:var(--mut)}
.dot.ok{background:var(--ok)}.dot.bad{background:var(--bad)}.dot.warn{background:var(--warn)}
button{background:#21262d;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:5px 10px;cursor:pointer;font-size:12px}
button:hover{border-color:#3d444d}button.primary{background:var(--acc2);border-color:var(--acc2);color:#fff}button:disabled{opacity:.5;cursor:default}
input,select,textarea{background:#0d1117;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:8px 9px;font-size:13px;width:100%}
textarea{min-height:90px;resize:vertical}
label{display:block;color:var(--mut);font-size:12px;margin-bottom:4px}
.form{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;align-items:end}
pre{background:#0d1117;border:1px solid var(--line);border-radius:10px;padding:12px;overflow:auto;max-height:360px;font-size:12px;margin:0;white-space:pre-wrap;word-break:break-all}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.right{margin-left:auto}.muted{color:var(--mut)}.small{font-size:12px}
.toast{position:fixed;right:16px;bottom:16px;background:var(--acc2);color:#fff;padding:9px 14px;border-radius:8px;opacity:0;transition:opacity .2s;pointer-events:none;max-width:70vw;z-index:40}
.toast.show{opacity:1}.toast.err{background:#b62324}
.hint{font-size:12px;color:var(--mut);margin-top:6px;line-height:1.5}
.kv{display:grid;grid-template-columns:150px 1fr auto auto;gap:8px;align-items:center;padding:8px 0;border-bottom:1px solid var(--line)}
.kv:last-child{border-bottom:0}.kv .k{color:var(--mut);font-size:12px}
.secret{letter-spacing:.08em}
.pill{display:inline-block;padding:1px 7px;border-radius:999px;background:#1b2431;color:#9db7d6;font-size:11px;margin:2px 4px 2px 0}
.two{display:grid;grid-template-columns:1fr 1fr;gap:12px}
@media(max-width:900px){.two{grid-template-columns:1fr}.kv{grid-template-columns:1fr auto auto}.kv .k{grid-column:1/-1}}
</style>
</head>
<body>
<div id="loginView" class="login-wrap">
  <form class="login-card" onsubmit="doLogin(event)">
    <h1>WARP Pool Console</h1>
    <p>使用池 API Token 登录。Token 就是池容器的 API_TOKEN / POOL_API_TOKEN。</p>
    <label for="loginToken">API Token</label>
    <input id="loginToken" type="password" autocomplete="current-password" placeholder="pool-..." autofocus>
    <div style="height:14px"></div>
    <button class="primary" type="submit" style="width:100%;padding:9px">登录</button>
    <p id="loginError" class="hint" style="color:var(--bad);min-height:18px"></p>
    <p class="hint">登录后可在「连接与鉴权」里复制 API 地址、Token、SOCKS 账号。</p>
  </form>
</div>

<div id="appView" class="hidden">
<header>
  <h1>WARP Pool Console</h1>
  <span id="poolId" class="badge mono">-</span>
  <span id="healthBadge" class="badge">unknown</span>
  <span id="regionBadge" class="badge">-</span>
  <div class="right row">
    <button onclick="loadAll()">刷新</button>
    <button onclick="logout(true)">退出</button>
  </div>
</header>
<main>
  <div class="tabs" id="tabs">
    <button data-tab="overview" aria-pressed="true" onclick="showTab('overview')">概览</button>
    <button data-tab="nodes" aria-pressed="false" onclick="showTab('nodes')">节点</button>
    <button data-tab="connect" aria-pressed="false" onclick="showTab('connect')">连接与鉴权</button>
    <button data-tab="deploy" aria-pressed="false" onclick="showTab('deploy')">部署参数</button>
    <button data-tab="scale" aria-pressed="false" onclick="showTab('scale')">扩容与区域</button>
    <button data-tab="logs" aria-pressed="false" onclick="showTab('logs')">日志</button>
  </div>

  <div data-panel="overview">
    <div class="grid">
      <div class="card"><div class="k">健康节点</div><div class="v" id="mHealthy">-</div></div>
      <div class="card"><div class="k">启用节点</div><div class="v" id="mEnabled">-</div></div>
      <div class="card"><div class="k">节点总数</div><div class="v" id="mCount">-</div></div>
      <div class="card"><div class="k">地区</div><div class="v" id="mRegions">-</div></div>
      <div class="card"><div class="k">节点端口范围</div><div class="v small" id="mPorts">-</div></div>
      <div class="card"><div class="k">轮换间隔</div><div class="v" id="mRotation">-</div></div>
    </div>
    <div class="two">
      <div class="card">
        <h2 style="margin-top:0">区域分布</h2>
        <table><thead><tr><th>地区</th><th class="num">节点</th><th class="num">健康</th><th>出口 /64 前缀</th></tr></thead><tbody id="regionRows"><tr><td colspan="4" class="muted">加载中…</td></tr></tbody></table>
        <p class="hint">WARP 走 Anycast，出口地区由池容器所在地区决定；同一池里所有节点通常落在同一个地区。</p>
      </div>
      <div class="card">
        <h2 style="margin-top:0">网关接入（最短路径）</h2>
        <div class="kv"><span class="k">API 地址</span><code id="ovApi" class="mono">-</code><button onclick="copyVal('ovApi')">复制</button><span></span></div>
        <div class="kv"><span class="k">API Token</span><code id="ovToken" class="mono secret">-</code><button onclick="toggleSecret('apiToken')">显示</button><button onclick="copySecret('apiToken')">复制</button></div>
        <div class="kv"><span class="k">范围</span><code id="ovScope" class="mono">private</code><button onclick="copyVal('ovScope')">复制</button><span></span></div>
        <p class="hint">同平台网关填私网地址；其它平台填公网地址。把这两项加到网关「配置中心 → 代理 → 代理中心 → 静态池 → 新增池」。</p>
      </div>
    </div>
  </div>

  <div data-panel="nodes" class="hidden">
    <div class="row" style="margin-bottom:8px">
      <h2 style="margin:0">节点</h2>
      <div class="right row">
        <button onclick="loadStatus()">刷新</button>
        <button onclick="post('/api/v1/reroll')">全部轮换</button>
      </div>
    </div>
    <table>
      <thead><tr><th class="num">#</th><th>用户</th><th class="num">端口</th><th>状态</th><th>出口 IPv6</th><th>地区</th><th>入口 endpoint</th><th class="num">连接</th><th class="num">上行</th><th class="num">下行</th><th>更新时间</th><th>操作</th></tr></thead>
      <tbody id="nodes"><tr><td colspan="12" class="muted">加载中…</td></tr></tbody>
    </table>
  </div>

  <div data-panel="connect" class="hidden">
    <div class="two">
      <div class="card">
        <h2 style="margin-top:0">连接信息</h2>
        <div class="kv"><span class="k">Pool ID</span><code id="poolIdValue" class="mono">-</code><button onclick="copyVal('poolIdValue')">复制</button><span></span></div>
        <div class="kv"><span class="k">API 地址（私网）</span><code id="apiPrivate" class="mono">-</code><button onclick="copyVal('apiPrivate')">复制</button><span></span></div>
        <div class="kv"><span class="k">API 地址（公网）</span><code id="apiPublic" class="mono">-</code><button onclick="copyVal('apiPublic')">复制</button><span></span></div>
        <div class="kv"><span class="k">API Token</span><code id="apiToken" class="mono secret">-</code><button onclick="toggleSecret('apiToken')">显示</button><button onclick="copySecret('apiToken')">复制</button></div>
        <div class="kv"><span class="k">SOCKS 用户</span><code id="socksUser" class="mono">-</code><button onclick="copyVal('socksUser')">复制</button><span></span></div>
        <div class="kv"><span class="k">SOCKS 密码</span><code id="socksPass" class="mono secret">-</code><button onclick="toggleSecret('socksPass')">显示</button><button onclick="copySecret('socksPass')">复制</button></div>
        <div class="kv"><span class="k">Mux 端口</span><code id="muxPortValue" class="mono">-</code><button onclick="copyVal('muxPortValue')">复制</button><span></span></div>
        <p class="hint">API Token 既是本控制台登录密码，也是网关静态池要填的 Bearer token。SOCKS 密码用于直连 mux 的客户端；网关同步端点时会自动带上。</p>
      </div>
      <div class="card">
        <h2 style="margin-top:0">填到网关静态池</h2>
        <p class="hint">在网关「配置中心 → 代理 → 代理中心 → 静态池 → 新增池」填：</p>
        <pre id="gatewayExample" class="mono">-</pre>
        <div class="row" style="margin-top:8px">
          <button class="primary" onclick="copyVal('gatewayExample')">复制接入信息</button>
        </div>
        <p class="hint">同平台（例如 FRA 网关 + 同项目池）用私网地址；跨平台或本地电脑用公网地址。范围 private/public 决定网关从池里取哪一组 SOCKS 主机名。</p>
      </div>
    </div>
  </div>

  <div data-panel="deploy" class="hidden">
    <div class="row" style="margin-bottom:8px">
      <h2 style="margin:0">部署参数</h2>
      <div class="right row"><button onclick="copyVal('deployJson')">复制 JSON</button></div>
    </div>
    <div class="two">
      <div class="card">
        <table><thead><tr><th>参数</th><th>当前值</th></tr></thead><tbody id="deployRows"><tr><td colspan="2" class="muted">加载中…</td></tr></tbody></table>
      </div>
      <div class="card">
        <h2 style="margin-top:0">部署 env（JSON）</h2>
        <pre id="deployJson" class="mono">-</pre>
        <p class="hint">这些是容器启动参数。改 INSTANCE_COUNT / START_PORT 后重新部署才会改变启动规模；运行中扩缩容请用「扩容与区域」页，不用改 env。</p>
      </div>
    </div>
    <div class="card" style="margin-top:12px">
      <h2 style="margin-top:0">运行时调参</h2>
      <div class="form">
        <div><label>轮换间隔（秒）</label><input id="cfgRotation" type="number" min="60" max="86400"></div>
        <div><label>每轮轮换数</label><input id="cfgBatch" type="number" min="1"></div>
        <div><label>健康检查间隔（秒）</label><input id="cfgHealth" type="number" min="30" max="86400"></div>
        <div><label>最小健康数</label><input id="cfgMin" type="number" min="0"></div>
        <div><button class="primary" onclick="saveConfig()">保存并热加载</button></div>
      </div>
      <p class="hint">写入 /data/control/config.json，entrypoint 在下一个循环读取，不需要重启。</p>
    </div>
  </div>

  <div data-panel="scale" class="hidden">
    <div class="two">
      <div class="card">
        <h2 style="margin-top:0">在线扩缩容</h2>
        <div class="kv"><span class="k">当前节点数</span><code id="scaleCurrent" class="mono">-</code><span></span><span></span></div>
        <div class="kv"><span class="k">节点端口范围</span><code id="scalePorts" class="mono">-</code><span></span><span></span></div>
        <div class="kv"><span class="k">Mux 端口</span><code id="scaleMux" class="mono">-</code><span></span><span></span></div>
        <div class="form" style="margin-top:12px">
          <div><label>目标节点数（1-256）</label><input id="scaleCount" type="number" min="1" max="256"></div>
          <div><button class="primary" onclick="applyScale()">应用扩缩容</button></div>
        </div>
        <p class="hint">扩容会启动新 wireproxy 并重启 mux，API 和 SOCKS 会中断几秒；已注册账号保留。缩容会停掉尾部节点。目标值持久化到 /data/control/scale，重启后仍生效。</p>
        <p class="hint">端口上限由 <code>scale_max</code> 决定；超过 256 或与 mux 端口冲突会被拒绝。</p>
      </div>
      <div class="card">
        <h2 style="margin-top:0">换地区 / 多池</h2>
        <p class="hint">WARP 是 Anycast：出口地区由池容器所在地区决定（当前 <code id="scaleRegion">-</code>），改节点数不会换地区。</p>
        <p class="hint">要覆盖更多地区，在目标地区再部署一个同样的池服务，设置不同的 <code>POOL_ID</code>、<code>STATE_DIR</code>（或独立卷）和端口，然后把它的 API 地址 + token 加到网关静态池。网关会合并去重所有启用池的端点。</p>
        <div class="row" style="margin-top:8px">
          <button class="primary" onclick="copyVal('deployEnv')">复制新池部署 env</button>
        </div>
        <pre id="deployEnv" class="mono" style="margin-top:8px">-</pre>
      </div>
    </div>
    <div class="card" style="margin-top:12px">
      <h2 style="margin-top:0">WARP 入口 endpoint 候选</h2>
      <p class="hint">每个节点从下列 Cloudflare Anycast 地址里随机选一个入口；换 endpoint 只影响入口，不改变出口地区。</p>
      <div id="endpointPills"><span class="muted">加载中…</span></div>
    </div>
  </div>

  <div data-panel="logs" class="hidden">
    <div class="row" style="margin-bottom:8px">
      <h2 style="margin:0">日志</h2>
      <div class="right row">
        <select id="logLines" style="width:110px" onchange="loadLogs()"><option>100</option><option selected>200</option><option>500</option><option>1000</option></select>
        <button onclick="loadLogs()">刷新</button>
      </div>
    </div>
    <pre id="logs" class="mono">-</pre>
  </div>
</main>
</div>
<div id="toast" class="toast"></div>
<script>
let token=localStorage.getItem('warp_pool_token')||'';
let timer=null,scope='private';
const state={meta:null,status:null,revealed:{}};
const $=id=>document.getElementById(id);
const esc=v=>String(v==null?'':v).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function toast(msg,err){const t=$('toast');t.textContent=msg;t.className='toast show'+(err?' err':'');clearTimeout(t._h);t._h=setTimeout(()=>t.className='toast',3000)}
function fmt(n){n=Number(n||0);if(n<1024)return n+' B';if(n<1048576)return (n/1024).toFixed(1)+' KB';if(n<1073741824)return (n/1048576).toFixed(1)+' MB';return (n/1073741824).toFixed(2)+' GB'}
function ago(ts){if(!ts)return '-';const s=Math.max(0,Math.floor(Date.now()/1000-Number(ts)));if(s<60)return s+'s 前';if(s<3600)return Math.floor(s/60)+'m 前';return Math.floor(s/3600)+'h 前'}
function showLogin(){$('loginView').classList.remove('hidden');$('appView').classList.add('hidden');setTimeout(()=>$('loginToken').focus(),50)}
function showApp(){$('loginView').classList.add('hidden');$('appView').classList.remove('hidden')}
function logout(notify){token='';localStorage.removeItem('warp_pool_token');state.meta=null;state.status=null;showLogin();if(notify)toast('已退出')}
async function api(path,opt={}){
  const r=await fetch(path,{...opt,headers:{'Authorization':'Bearer '+token,...(opt.headers||{})}});
  if(r.status===401){logout(false);throw new Error('登录已失效')}
  const ct=r.headers.get('content-type')||'';
  const body=ct.includes('json')?await r.json():await r.text();
  if(!r.ok)throw new Error(typeof body==='string'?body:(body.error||r.status));
  return body;
}
async function doLogin(e){
  e.preventDefault();
  const value=$('loginToken').value.trim();
  if(!value){$('loginError').textContent='请输入 API Token';return}
  token=value;
  $('loginError').textContent='';
  try{
    await api('/api/v1/status');
    localStorage.setItem('warp_pool_token',token);
    $('loginToken').value='';
    showApp();
    await loadAll();
    toast('已登录');
  }catch(err){
    token='';
    $('loginError').textContent='登录失败：'+err.message;
  }
}
function showTab(name){
  for(const b of document.querySelectorAll('#tabs button'))b.setAttribute('aria-pressed',b.dataset.tab===name?'true':'false');
  for(const p of document.querySelectorAll('[data-panel]'))p.classList.toggle('hidden',p.dataset.panel!==name);
  if(name==='logs')loadLogs();
}
async function loadAll(){
  await Promise.allSettled([loadStatus(),loadMeta(),loadConfig(),loadEndpoints(),loadLogs()]);
}
async function loadStatus(){
  try{
    const st=await api('/api/v1/status');
    state.status=st;
    const p=st.pool;
    $('poolId').textContent=p.id||'pool';
    $('mHealthy').textContent=p.healthy+' / '+p.count;
    $('mEnabled').textContent=p.enabled;
    $('mCount').textContent=p.count;
    $('mRotation').textContent=p.rotation_interval_seconds+'s';
    $('mPorts').textContent=(state.meta?state.meta.deploy.port_start:p.mux_port+1)+'-'+(state.meta?state.meta.deploy.port_end:p.mux_port+p.count);
    const hb=$('healthBadge');
    hb.textContent=p.healthy>=(p.min_healthy||0)?'healthy':'degraded';
    hb.className='badge '+(p.healthy>=(p.min_healthy||0)?'ok':'bad');
    const regions={};
    for(const n of st.nodes||[]){
      const loc=n.loc||'-';
      const r=regions[loc]||(regions[loc]={loc,count:0,healthy:0,prefixes:[]});
      r.count++;
      if(n.healthy)r.healthy++;
      if(n.prefix64&&!r.prefixes.includes(n.prefix64))r.prefixes.push(n.prefix64);
    }
    const list=Object.values(regions).sort((a,b)=>b.count-a.count);
    $('regionBadge').textContent=list.length?list.map(r=>r.loc+' '+r.count).join(' · '):'-';
    $('mRegions').textContent=list.length?list.map(r=>r.loc).join(', '):'-';
    $('regionRows').innerHTML=list.length?list.map(r=>'<tr><td>'+esc(r.loc)+'</td><td class="num">'+r.count+'</td><td class="num">'+r.healthy+'</td><td class="mono">'+esc(r.prefixes.slice(0,3).join(', '))+(r.prefixes.length>3?' …':'')+'</td></tr>').join(''):'<tr><td colspan="4" class="muted">暂无节点</td></tr>';
    renderNodes(st.nodes||[]);
  }catch(e){toast('状态获取失败: '+e.message,true)}
}
function renderNodes(nodes){
  $('nodes').innerHTML=nodes.length?nodes.map(n=>{
    const cls=!n.enabled?'warn':(n.healthy?'ok':'bad');
    const st=!n.enabled?'disabled':(n.healthy?'healthy':(n.state||'unknown'));
    return '<tr>'
      +'<td class="num">'+n.index+'</td>'
      +'<td class="mono">'+esc(n.user)+'</td>'
      +'<td class="num">'+esc(n.port)+'</td>'
      +'<td><span class="dot '+cls+'"></span>'+esc(st)+'</td>'
      +'<td class="mono">'+esc(n.exit_ipv6||n.prefix64||'-')+'</td>'
      +'<td>'+esc(n.loc||'-')+'</td>'
      +'<td class="mono">'+esc((n.endpoint||'-').replace(/\s+$/,''))+'</td>'
      +'<td class="num">'+Number(n.connections||0)+'</td>'
      +'<td class="num">'+fmt(n.bytes_up)+'</td>'
      +'<td class="num">'+fmt(n.bytes_down)+'</td>'
      +'<td class="muted">'+ago(n.updated_at)+'</td>'
      +'<td><div class="row">'
      +'<button onclick="nodeAction(\''+esc(n.user)+'\',\'rotate\')">轮换</button>'
      +'<button onclick="nodeAction(\''+esc(n.user)+'\',\''+(n.enabled?'disable':'enable')+'\')">'+(n.enabled?'停用':'启用')+'</button>'
      +'</div></td></tr>';
  }).join(''):'<tr><td colspan="12" class="muted">池里还没有节点</td></tr>';
}
async function loadMeta(){
  try{
    const m=await api('/api/v1/meta');
    state.meta=m;
    renderConnect(m);
    renderDeploy(m);
    renderScale(m);
    if(state.status)loadStatus();
  }catch(e){toast('元数据获取失败: '+e.message,true)}
}
function renderConnect(m){
  const pub=m.api.public_url||location.origin;
  const priv=m.api.private_url||'';
  $('poolIdValue').textContent=m.pool.id||'-';
  $('apiPrivate').textContent=priv||'(未配置 private_host)';
  $('apiPublic').textContent=pub;
  $('apiToken').textContent=state.revealed.apiToken?m.api.token:'••••••••••••'+String(m.api.token||'').slice(-4);
  $('socksUser').textContent=m.socks.user||'-';
  $('socksPass').textContent=state.revealed.socksPass?m.socks.pass:'••••••••';
  $('muxPortValue').textContent=m.socks.mux_port||m.deploy.mux_port||'-';
  const use=priv||pub;
  $('ovApi').textContent=use;
  $('ovToken').textContent=state.revealed.apiToken?m.api.token:'••••••••••••'+String(m.api.token||'').slice(-4);
  $('gatewayExample').textContent='API 地址: '+use+'\nToken: '+m.api.token+'\n范围: private';
}
function renderDeploy(m){
  const d=m.deploy;
  const rows=[
    ['POOL_ID',m.pool.id],['STATE_DIR',d.state_dir],['START_PORT',d.start_port],['INSTANCE_COUNT',d.instance_count],
    ['INSTANCE_COUNT（env）',d.instance_count_env],['MUX_PORT',d.mux_port],['端口范围',d.port_start+'-'+d.port_end],
    ['SOCKS_USER',m.socks.user],['REROLL_INTERVAL',d.reroll_interval],['REROLL_BATCH',d.reroll_batch],
    ['HEALTH_INTERVAL',d.health_interval],['MIN_HEALTHY',d.min_healthy],['DEDUP_RETRIES',d.dedup_retries],
    ['REGISTER_RETRIES',d.register_retries],['PROBE_URL',d.probe_url],['PROBE_TIMEOUT',d.probe_timeout],
    ['ENDPOINTS_FILE',d.endpoints_file],['ENDPOINTS_COUNT',d.endpoints_count],['DENY_48_PREFIXES',d.deny_48_prefixes],
    ['POOL_PUBLIC_HOST',d.pool_public_host||'-'],['POOL_PRIVATE_HOST',d.pool_private_host||'-'],['EXTERNAL_HOST',d.external_host||'-'],
    ['WGCF_KEEPALIVE',d.keepalive],['SCALE_MIN',d.scale_min],['SCALE_MAX',d.scale_max],
  ];
  $('deployRows').innerHTML=rows.map(r=>'<tr><td class="mono">'+esc(r[0])+'</td><td class="mono">'+esc(r[1])+'</td></tr>').join('');
  const env={POOL_ID:m.pool.id+'-REGION',STATE_DIR:d.state_dir,START_PORT:d.start_port,MUX_PORT:d.mux_port,INSTANCE_COUNT:d.instance_count,SOCKS_USER:m.socks.user,SOCKS_PASS:m.socks.pass,API_TOKEN:m.api.token,REROLL_INTERVAL:d.reroll_interval,REROLL_BATCH:d.reroll_batch,HEALTH_INTERVAL:d.health_interval,MIN_HEALTHY:d.min_healthy,DEDUP_RETRIES:d.dedup_retries,REGISTER_RETRIES:d.register_retries,PROBE_URL:d.probe_url,PROBE_TIMEOUT:d.probe_timeout,ENDPOINTS_FILE:d.endpoints_file,DENY_48_PREFIXES:d.deny_48_prefixes,POOL_PUBLIC_HOST:'NEW_POOL_HOST',POOL_PRIVATE_HOST:'NEW_POOL_PRIVATE_HOST'};
  $('deployJson').textContent=JSON.stringify(env,null,2);
}
function renderScale(m){
  const d=m.deploy;
  $('scaleCurrent').textContent=d.instance_count+' 个（env '+d.instance_count_env+'）';
  $('scalePorts').textContent=d.port_start+'-'+d.port_end;
  $('scaleMux').textContent=d.mux_port;
  const regions=(state.status&&state.status.nodes||[]).map(n=>n.loc).filter(Boolean);
  const uniq=[...new Set(regions)];
  $('scaleRegion').textContent=uniq.length?uniq.join(', '):'-';
  $('scaleCount').value=d.instance_count;
  $('scaleCount').max=d.scale_max;
  if(!$('deployEnv').dataset.ready){
    $('deployEnv').textContent=JSON.stringify({POOL_ID:m.pool.id+'-REGION',STATE_DIR:d.state_dir,START_PORT:d.start_port,MUX_PORT:d.mux_port,INSTANCE_COUNT:d.instance_count,SOCKS_USER:m.socks.user,SOCKS_PASS:m.socks.pass,API_TOKEN:m.api.token,REROLL_INTERVAL:d.reroll_interval,REROLL_BATCH:d.reroll_batch,HEALTH_INTERVAL:d.health_interval,MIN_HEALTHY:d.min_healthy,PROBE_URL:d.probe_url,PROBE_TIMEOUT:d.probe_timeout,ENDPOINTS_FILE:d.endpoints_file,DENY_48_PREFIXES:d.deny_48_prefixes,POOL_PUBLIC_HOST:'NEW_POOL_HOST',POOL_PRIVATE_HOST:'NEW_POOL_PRIVATE_HOST'},null,2);
    $('deployEnv').dataset.ready='1';
  }
}
function renderEndpointPills(items){
  const host=$('endpointPills');
  host.innerHTML=(items||[]).map(x=>'<span class="pill mono">'+esc(x)+'</span>').join('')||'<span class="muted">没有读取到 endpoint 文件</span>';
}
async function loadEndpoints(){
  try{
    const m=state.meta||await api('/api/v1/meta');
    renderEndpointPills(m.endpoints.items);
  }catch(e){$('endpointPills').innerHTML='<span class="muted">读取失败: '+esc(e.message)+'</span>'}
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
async function loadLogs(){
  try{ $('logs').textContent=await api('/api/v1/logs?lines='+$('logLines').value) }
  catch(e){ $('logs').textContent='错误: '+e.message }
}
function toggleSecret(key){
  state.revealed[key]=!state.revealed[key];
  if(state.meta)renderConnect(state.meta);
}
function copyVal(id){
  const text=$(id).textContent||'';
  navigator.clipboard.writeText(text).then(()=>toast('已复制'),()=>toast('复制失败',true));
}
function copySecret(key){
  if(!state.meta)return;
  const v=key==='apiToken'?state.meta.api.token:state.meta.socks.pass;
  navigator.clipboard.writeText(v||'').then(()=>toast('已复制'),()=>toast('复制失败',true));
}
async function post(path){
  try{await api(path,{method:'POST'});toast('已排队');setTimeout(loadStatus,1500)}
  catch(e){toast('失败: '+e.message,true)}
}
function nodeAction(user,action){post('/api/v1/nodes/'+encodeURIComponent(user)+'/'+action)}
async function saveConfig(){
  const body={rotation_interval_seconds:+$('cfgRotation').value,reroll_batch:+$('cfgBatch').value,health_interval_seconds:+$('cfgHealth').value,min_healthy:+$('cfgMin').value};
  try{await api('/api/v1/config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});toast('已保存并热加载');loadAll()}
  catch(e){toast('保存失败: '+e.message,true)}
}
async function applyScale(){
  const count=Number($('scaleCount').value);
  if(!Number.isInteger(count)||count<1||count>256){toast('节点数需为 1-256 的整数',true);return}
  if(!confirm('扩缩容会重启 mux，API 和 SOCKS 会中断几秒。继续？'))return;
  try{
    await api('/api/v1/scale',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({count})});
    toast('已提交扩缩容，等待 mux 重启');
    setTimeout(loadAll,5000);
  }catch(e){
    if(String(e.message).includes('登录已失效'))return;
    toast('提交失败: '+e.message,true);
  }
}
(function boot(){
  if(!token){showLogin();return}
  api('/api/v1/status').then(()=>{showApp();return loadAll()}).catch(()=>showLogin());
  clearInterval(timer);
  timer=setInterval(()=>{if(token&&!$('appView').classList.contains('hidden'))loadStatus()},5000);
})();
</script>
</body>
</html>`
