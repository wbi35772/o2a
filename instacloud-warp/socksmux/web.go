package main

import (
	"io"
	"net/http"
)

// handleUI serves the single-page console. Authentication is enforced by the
// API layer: the page renders, the first API call returns 401 and the login
// form takes over.
func (a *apiServer) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, uiHTML)
}

const uiHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WARP Pool Console</title>
<style>
:root {
  --bg:#0d0f14; --panel:#171a21; --panel2:#1b1f27; --line:#252b36; --line2:#2f3745;
  --fg:#e6e9ef; --muted:#8b94a7; --accent:#3b82f6; --ok:#35c48a; --warn:#e0a63a; --bad:#e05a5a;
  --mono:ui-monospace,SFMono-Regular,Consolas,monospace;
}
* { box-sizing:border-box }
body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans SC",sans-serif }
header { position:sticky; top:0; z-index:20; display:flex; align-items:center; gap:14px; padding:12px 20px; background:rgba(13,15,20,.94); border-bottom:1px solid var(--line) }
h1 { font-size:16px; margin:0; font-weight:600; letter-spacing:.3px }
.pill { display:inline-flex; align-items:center; gap:6px; padding:2px 9px; border:1px solid var(--line2); border-radius:999px; font-size:12px; color:var(--muted) }
.pill.ok { color:var(--ok); border-color:rgba(53,196,138,.4) }
.pill.bad { color:var(--bad); border-color:rgba(224,90,90,.4) }
.spacer { flex:1 }
button, .btn { font:inherit; color:var(--fg); background:var(--panel2); border:1px solid var(--line2); border-radius:8px; padding:6px 12px; cursor:pointer }
button:hover { border-color:var(--accent) }
button.primary { background:var(--accent); border-color:var(--accent); color:#fff }
button.danger { color:var(--bad); border-color:rgba(224,90,90,.45) }
button.small { padding:3px 9px; font-size:12px }
button:disabled { opacity:.5; cursor:default }
nav { display:flex; flex-wrap:wrap; gap:8px; padding:12px 20px 0 }
nav button { border-radius:8px 8px 0 0; border-bottom-color:transparent; color:var(--muted) }
nav button.active { color:var(--fg); background:var(--panel); border-color:var(--line2); border-bottom-color:var(--panel) }
main { padding:16px 20px 60px; max-width:1400px }
.tab { display:none }
.tab.active { display:block }
.card { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:16px 18px; margin-bottom:16px }
.card h2 { font-size:15px; margin:0 0 10px; font-weight:600 }
.card h3 { font-size:13px; margin:16px 0 8px; color:var(--muted); font-weight:600; letter-spacing:.4px; text-transform:uppercase }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(170px,1fr)); gap:12px }
.stat { background:var(--panel2); border:1px solid var(--line); border-radius:10px; padding:12px 14px }
.stat .k { color:var(--muted); font-size:12px }
.stat .v { font-size:22px; font-weight:650; margin-top:2px; font-family:var(--mono) }
.two { display:grid; grid-template-columns:1fr 1fr; gap:16px }
@media (max-width:980px){ .two { grid-template-columns:1fr } }
table { width:100%; border-collapse:collapse }
th, td { text-align:left; padding:7px 10px; border-bottom:1px solid var(--line); vertical-align:top }
th { color:var(--muted); font-weight:500; font-size:12px; white-space:nowrap }
tr:hover td { background:rgba(59,130,246,.05) }
.mono { font-family:var(--mono); font-size:12.5px; word-break:break-all }
.muted { color:var(--muted) }
.kv { display:grid; grid-template-columns:150px 1fr; gap:6px 12px; align-items:start }
.kv .k { color:var(--muted) }
.kv .v { font-family:var(--mono); font-size:12.5px; word-break:break-all }
.tag { display:inline-block; padding:1px 7px; border-radius:999px; font-size:11px; border:1px solid var(--line2); color:var(--muted) }
.tag.ok { color:var(--ok); border-color:rgba(53,196,138,.45) }
.tag.bad { color:var(--bad); border-color:rgba(224,90,90,.45) }
.tag.relay { color:#7cc4ff; border-color:rgba(59,130,246,.5) }
.tag.de { color:var(--warn); border-color:rgba(224,166,58,.45) }
.note { background:rgba(59,130,246,.07); border:1px solid rgba(59,130,246,.25); border-radius:8px; padding:10px 12px; color:#c7d2e5; font-size:13px; margin-top:10px }
.note b { color:#fff }
.warnbox { background:rgba(224,166,58,.08); border:1px solid rgba(224,166,58,.35); border-radius:8px; padding:10px 12px; font-size:13px; margin-top:10px }
pre { background:#0b0d12; border:1px solid var(--line); border-radius:8px; padding:12px; overflow:auto; max-height:520px; font-family:var(--mono); font-size:12px; margin:0 }
input, select { font:inherit; color:var(--fg); background:#0b0d12; border:1px solid var(--line2); border-radius:8px; padding:7px 10px; min-width:0 }
input[type=text], input[type=password] { width:100% }
.row { display:flex; flex-wrap:wrap; gap:8px; align-items:center }
.row > input[type=text] { flex:1 1 200px }
.login { position:fixed; inset:0; display:none; align-items:center; justify-content:center; background:rgba(9,11,15,.92); z-index:100 }
.login.show { display:flex }
.login .card { width:min(380px,92vw); margin:0 }
.login h2 { text-align:center; margin-bottom:16px }
.err { color:var(--bad); font-size:13px; min-height:18px; margin-top:8px }
.toast { position:fixed; right:18px; bottom:18px; background:#1d2430; border:1px solid var(--line2); border-left:3px solid var(--accent); border-radius:8px; padding:10px 14px; font-size:13px; max-width:420px; opacity:0; transform:translateY(8px); transition:.18s; z-index:120; pointer-events:none }
.toast.show { opacity:1; transform:none }
.toolbar { display:flex; flex-wrap:wrap; gap:10px; align-items:center; margin-bottom:12px }
.pager { display:flex; gap:8px; align-items:center; justify-content:flex-end; margin-top:10px; color:var(--muted) }
.hint { color:var(--muted); font-size:12.5px }
details { margin-top:10px }
summary { cursor:pointer; color:var(--muted) }
code { font-family:var(--mono); background:#0b0d12; border:1px solid var(--line); border-radius:4px; padding:0 4px }
</style>
</head>
<body>
<header>
  <h1>WARP Pool Console</h1>
  <span class="pill" id="poolId">-</span>
  <span class="pill" id="healthPill">-</span>
  <span class="pill" id="regionPill">-</span>
  <span class="spacer"></span>
  <button onclick="loadAll()">刷新</button>
  <button onclick="logout()">退出</button>
</header>
<nav id="nav"></nav>
<main>
  <section class="tab" id="tab-overview"></section>
  <section class="tab" id="tab-nodes"></section>
  <section class="tab" id="tab-connect"></section>
  <section class="tab" id="tab-deploy"></section>
  <section class="tab" id="tab-scale"></section>
  <section class="tab" id="tab-logs"></section>
  <section class="tab" id="tab-merged"></section>
</main>

<div class="login" id="login">
  <div class="card">
    <h2>登录 WARP Pool Console</h2>
    <form onsubmit="doLogin(event)">
      <div style="margin-bottom:10px"><input type="text" id="lu" placeholder="用户名（默认 admin）" autocomplete="username"></div>
      <div style="margin-bottom:10px"><input type="password" id="lp" placeholder="密码" autocomplete="current-password"></div>
      <button class="primary" style="width:100%" type="submit">登录</button>
      <div class="err" id="lerr"></div>
    </form>
    <div class="hint" style="margin-top:12px">账号由容器环境变量 POOL_ADMIN_USER / POOL_ADMIN_PASSWORD 控制。API Token 仍可用于网关与脚本调用，登录只用于控制台。</div>
  </div>
</div>
<div class="toast" id="toast"></div>

<script>
var STATE = { status:null, meta:null, tokens:null, relay:null, tab:"overview", nodePage:1, nodeQuery:"", nodeFilter:"all", nodePageSize:20, authed:false };
var TABS = [["overview","概览"],["nodes","节点"],["connect","连接与鉴权"],["deploy","部署参数"],["scale","扩容与区域"],["logs","日志"],["merged","多池聚合"]];

function esc(v){ return String(v==null?"":v).replace(/[&<>"']/g,function(c){return {"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c];}); }
function fmtBytes(n){ n=Number(n||0); if(!n) return "0 B"; var u=["B","KB","MB","GB","TB"]; var i=0; while(n>=1024&&i<u.length-1){n/=1024;i++} return (i?n.toFixed(1):n)+" "+u[i]; }
function ago(ts){ if(!ts) return "-"; var d=Math.max(0,Math.floor(Date.now()/1000-ts)); if(d<60) return d+"s 前"; if(d<3600) return Math.floor(d/60)+"m 前"; if(d<86400) return Math.floor(d/3600)+"h 前"; return Math.floor(d/86400)+"d 前"; }
function toast(msg){ var t=document.getElementById("toast"); t.textContent=msg; t.classList.add("show"); clearTimeout(t._h); t._h=setTimeout(function(){t.classList.remove("show")},2600); }
function copy(text){ if(!text){ toast("没有可复制内容"); return; } if(navigator.clipboard&&navigator.clipboard.writeText){ navigator.clipboard.writeText(text).then(function(){toast("已复制")},function(){fallbackCopy(text)}); } else { fallbackCopy(text); } }
function fallbackCopy(text){ var a=document.createElement("textarea"); a.value=text; document.body.appendChild(a); a.select(); try{document.execCommand("copy"); toast("已复制")}catch(e){toast("复制失败")} document.body.removeChild(a); }

function api(path, opts){
  opts = opts || {};
  opts.credentials = "same-origin";
  var headers = opts.headers || {};
  if (opts.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
  opts.headers = headers;
  return fetch("/api/v1" + path, opts).then(function(res){
    if(res.status===401){ showLogin(true); throw new Error("unauthorized"); }
    return res.text().then(function(text){
      var data = null;
      try { data = text ? JSON.parse(text) : null } catch(e) { data = text }
      if(!res.ok){ throw new Error((data&&data.error)||("HTTP "+res.status)); }
      return data;
    });
  });
}
function showLogin(show){ document.getElementById("login").classList.toggle("show", !!show); if(show) setTimeout(function(){document.getElementById("lu").focus()},50); }
function doLogin(ev){
  ev.preventDefault();
  var err=document.getElementById("lerr"); err.textContent="";
  fetch("/api/v1/login",{method:"POST",credentials:"same-origin",headers:{"Content-Type":"application/json"},body:JSON.stringify({username:document.getElementById("lu").value,password:document.getElementById("lp").value})})
  .then(function(r){ return r.text().then(function(t){ var d=null; try{d=JSON.parse(t)}catch(e){d=t} if(!r.ok) throw new Error((d&&d.error)||"登录失败"); return d; }) })
  .then(function(){ STATE.authed=true; showLogin(false); document.getElementById("lp").value=""; loadAll(); })
  .catch(function(e){ err.textContent=e.message; });
}
function logout(){ api("/logout",{method:"POST"}).then(function(){ STATE.authed=false; showLogin(true); }).catch(function(){ showLogin(true); }); }

function renderNav(){
  document.getElementById("nav").innerHTML = TABS.map(function(t){
    return "<button class=\""+(STATE.tab===t[0]?"active":"")+"\" onclick=\"switchTab('"+t[0]+"')\">"+t[1]+"</button>";
  }).join("");
}
function switchTab(id){ STATE.tab=id; renderNav(); TABS.forEach(function(t){ document.getElementById("tab-"+t[0]).classList.toggle("active",t[0]===id); }); if(id==="logs") loadLogs(); if(id==="merged") loadMerged(); if(id==="connect") loadTokens(); }

function loadAll(){
  Promise.all([api("/status"), api("/meta"), api("/relay").catch(function(){ return null; })]).then(function(r){
    STATE.status=r[0]; STATE.meta=r[1]; STATE.relay=r[2]; STATE.authed=true; showLogin(false);
    renderHeader(); renderOverview(); renderNodes(); renderConnect(); renderDeploy(); renderScale();
    if(STATE.tab==="logs") loadLogs(); if(STATE.tab==="merged") loadMerged(); if(STATE.tab==="connect") loadTokens();
  }).catch(function(e){ if(e.message!=="unauthorized") toast("加载失败: "+e.message); });
}
function renderHeader(){
  var s=STATE.status, m=STATE.meta; if(!s||!m) return;
  var p=s.pool;
  document.getElementById("poolId").textContent=p.id||"-";
  var ok=p.healthy>=(p.min_healthy||0)&&p.healthy>0;
  var hp=document.getElementById("healthPill");
  hp.textContent="健康 "+p.healthy+" / "+(p.count||0);
  hp.className="pill "+(ok?"ok":"bad");
  var rp=document.getElementById("regionPill");
  rp.textContent=(m.regions||[]).map(function(x){return x.loc+" "+x.count}).join(" · ")||"-";
  rp.className="pill";
}

function renderOverview(){
  var s=STATE.status, m=STATE.meta, p=s.pool;
  var regions=(m.regions||[]).map(function(r){
    return "<tr><td><span class=\"tag "+(r.loc==="-"?"relay":"de")+"\">"+esc(r.loc)+"</span></td><td class=\"mono\">"+r.count+"</td><td class=\"mono\">"+r.healthy+"</td><td class=\"mono\">"+(r.prefixes||[]).map(esc).join(", ")+(r.prefixes&&r.prefixes.length?" …":"")+"</td></tr>";
  }).join("");
  var api=m.api||{}, rt=m.runtime||{};
  var html="";
  html+="<div class=\"grid\">";
  html+=stat("健康节点",p.healthy+" / "+(p.count||0));
  html+=stat("启用节点",p.enabled);
  html+=stat("节点总数",p.count);
  html+=stat("地区",(m.regions||[]).map(function(r){return r.loc}).join(", ")||"-");
  html+=stat("节点端口范围",(m.deploy.port_start||"-")+"-"+m.deploy.port_end);
  html+=stat("轮换间隔",p.rotation_interval_seconds+"s");
  html+="</div>";

  html+="<div class=\"card\" style=\"margin-top:16px\"><h2>运行时调参</h2><div class=\"row\">";
  html+="<label class=\"hint\">轮换间隔（秒）<br><input type=\"text\" id=\"rtRotation\" value=\""+(rt.rotation_interval_seconds||"")+"\" style=\"width:150px\"></label>";
  html+="<label class=\"hint\">每轮轮换数<br><input type=\"text\" id=\"rtBatch\" value=\""+(rt.reroll_batch||"")+"\" style=\"width:120px\"></label>";
  html+="<label class=\"hint\">健康检查间隔（秒）<br><input type=\"text\" id=\"rtHealth\" value=\""+(rt.health_interval_seconds||"")+"\" style=\"width:170px\"></label>";
  html+="<label class=\"hint\">最小健康数<br><input type=\"text\" id=\"rtMin\" value=\""+(rt.min_healthy||"")+"\" style=\"width:130px\"></label>";
  html+="<button class=\"primary\" onclick=\"saveRuntime()\">保存并热加载</button></div>";
  html+="<div class=\"hint\" style=\"margin-top:8px\">写入 /data/control/config.json，entrypoint 在下一个循环读取，不需要重启。健康检查间隔决定探活一轮的周期；最小健康数只影响徽标颜色，不影响下发。</div></div>";

  html+="<div class=\"two\">";
  html+="<div class=\"card\"><h2>区域分布</h2><table><thead><tr><th>地区</th><th>节点数</th><th>健康</th><th>出口 /64 前缀</th></tr></thead><tbody>"+(regions||"<tr><td colspan=\"4\" class=\"muted\">暂无数据</td></tr>")+"</tbody></table>";
  html+="<div class=\"note\"><b>地区</b>：Cloudflare WARP 的出口地区（探活 trace 的 loc），由 WARP 接入点/容器所在地区决定，与订阅节点无关。<br><b>节点数</b>：wireproxy 实例数；一个实例 = 一个 wgcf 账号 = 一个独立出口 /64。<br><b>出口 /64</b>：该账号的 IPv6 前缀，网关下载的每个 SOCKS 出口都落在其中。<br><b>- 行</b>：中继预留槽位（warpuser 末尾 10 个），池内不启动 wireproxy；它们由中继容器经订阅 UDP 隧道产生，实际出口地区以中继 API 为准（当前 KR）。</div></div>";

  html+="<div class=\"card\"><h2>网关接入（最短路径）</h2>";
  html+="<div class=\"kv\">";
  html+="<div class=\"k\">API 地址（私网）</div><div class=\"v\">"+esc(api.private_url||"-")+" <button class=\"small\" onclick=\"copy('"+jsq(api.private_url)+"')\">复制</button></div>";
  html+="<div class=\"k\">API 地址（公网）</div><div class=\"v\">"+esc(api.public_url||"-")+" <button class=\"small\" onclick=\"copy('"+jsq(api.public_url)+"')\">复制</button></div>";
  html+="<div class=\"k\">API Token</div><div class=\"v\">••••••••"+(api.token||"").slice(-4)+" <button class=\"small\" onclick=\"copy('"+jsq(api.token)+"')\">复制</button> <button class=\"small\" onclick=\"toggleSecret('gwToken','"+jsq(api.token)+"')\">显示</button></div>";
  html+="<div class=\"k\">范围</div><div class=\"v\">private <button class=\"small\" onclick=\"copy('private')\">复制</button> / public <button class=\"small\" onclick=\"copy('public')\">复制</button></div>";
  html+="</div>";
  html+="<div class=\"note\">同平台网关填私网地址；其它平台的容器（或本地电脑）填公网地址。范围 private/public 决定网关从池里取哪一组 SOCKS 主机名。Token 可在此页下方「连接与鉴权」里新增多个，用不同 Token 给不同容器隔离鉴权。</div></div>";
  html+="</div>";

  html+=renderRelayCard();

  document.getElementById("tab-overview").innerHTML=html;
}
function renderRelayCard(){
  var r=STATE.relay;
  var html="<div class=\"card\" style=\"margin-top:16px\"><h2>中继（订阅 UDP → WARP）</h2>";
  if(!r || !r.enabled){
    html+="<div class=\"muted\">本容器未启用中继（缺少 "+esc((r&&r.state_dir)||"/data/relay-root")+"/relay-mode）。池的中继预留槽位不会产生出口。</div></div>";
    return html;
  }
  var sub=r.subscription||{}, t=r.tunnels||{}, err=r.error||"";
  html+="<div class=\"kv\">";
  html+="<div class=\"k\">订阅</div><div class=\"v\">"+esc(sub.name||"家宽订阅（UDP 中继）")+"</div>";
  html+="<div class=\"k\">订阅节点</div><div class=\"v\">"+esc(sub.node||"-")+"</div>";
  html+="<div class=\"k\">入口地址</div><div class=\"v\">"+esc((sub.host||"-")+":"+(sub.port||443))+" <span class=\"tag\">"+(sub.transport?esc(sub.transport):"trojan/WS/TLS")+"</span>"+(sub.ws_path?" <span class=\"mono\">"+esc(sub.ws_path)+"</span>":"")+"</div>";
  html+="<div class=\"k\">SNI</div><div class=\"v\">"+esc(sub.sni||sub.host||"-")+"</div>";
  html+="</div>";
  html+="<div class=\"grid\" style=\"margin-top:14px\">";
  html+=stat("隧道数",t.count||0);
  html+=stat("可用",""+(t.available||0));
  html+=stat("健康",(t.healthy||0)+" / "+(t.count||0));
  html+=stat("失败",t.failed||0);
  html+=stat("重复 /64",t.duplicates||0);
  html+=stat("唯一 /64",t.unique64||0);
  html+="</div>";
  var regions=(r.regions||[]).map(esc).join(" · ");
  if(regions) html+="<div class=\"hint\" style=\"margin-top:10px\">出口地区："+regions+"</div>";
  var rows=(r.nodes||[]).map(function(n){
    var st=n.state==="ok"?"<span class=\"tag ok\">ok</span>":"<span class=\"tag bad\">"+esc(n.state||"unknown")+"</span>";
    var dup=n.duplicate?"<span class=\"tag bad\">重复</span>":"<span class=\"tag ok\">唯一</span>";
    return "<tr><td class=\"mono\">"+n.index+"</td><td class=\"mono\">"+n.port+"</td><td class=\"mono\">"+esc(n.prefix||"-")+"</td><td>"+(n.loc?esc(n.loc):"-")+"</td><td>"+st+"</td><td>"+dup+"</td><td class=\"mono\">"+ago(n.updated)+"</td></tr>";
  }).join("");
  html+="<table style=\"margin-top:12px\"><thead><tr><th>#</th><th>本地端口</th><th>出口 /64</th><th>地区</th><th>状态</th><th>去重</th><th>更新时间</th></tr></thead><tbody>"+(rows||"<tr><td colspan=\"7\" class=\"muted\">还没有中继会话</td></tr>")+"</tbody></table>";
  html+="<div class=\"note\"><b>来源</b>："+esc(sub.name||"订阅")+" → 节点 <span class=\"mono\">"+esc(sub.host||"-")+"</span> 的 UDP 通道，容器内起 "+ (t.count||0) +" 条 sing-box 隧道，每条各注册一个 WARP 账号，得到 "+ (t.unique64||0) +" 个唯一 /64。<br><b>健康</b>：探活 trace 成功的会话数；<b>重复</b>：与其他会话落在同一 /64 的会话数（这些会共享额度桶，应轮换收口）。<br>这些出口不在池的 512 个 wireproxy 里，而是以末尾 "+(STATE.meta&&STATE.meta.relay_slots||10)+" 个中继预留槽位的形式并入 mux。</div>";
  if(err) html+="<div class=\"warnbox\">读取失败："+esc(err)+"</div>";
  html+="</div>";
  return html;
}
function stat(k,v){ return "<div class=\"stat\"><div class=\"k\">"+k+"</div><div class=\"v\">"+esc(v)+"</div></div>"; }
function jsq(v){ return String(v==null?"":v).replace(/\\/g,"\\\\").replace(/'/g,"\\'").replace(/\r|\n/g,""); }
function toggleSecret(id,val){ var el=document.getElementById(id); if(!el){ return; } el.textContent = el.textContent.indexOf("•")>=0 ? val : "••••••••••••"+String(val).slice(-4); }

function saveRuntime(){
  var body={
    rotation_interval_seconds:Number(document.getElementById("rtRotation").value||0),
    reroll_batch:Number(document.getElementById("rtBatch").value||0),
    health_interval_seconds:Number(document.getElementById("rtHealth").value||0),
    min_healthy:Number(document.getElementById("rtMin").value||0)
  };
  api("/config",{method:"PUT",body:JSON.stringify(body)}).then(function(){ toast("已保存并热加载"); loadAll(); }).catch(function(e){ toast("保存失败: "+e.message); });
}

function nodeFiltered(){
  var nodes=(STATE.status&&STATE.status.nodes)||[];
  var q=STATE.nodeQuery.trim().toLowerCase();
  return nodes.filter(function(n){
    if(STATE.nodeFilter==="reserved" && !n.reserved) return false;
    if(STATE.nodeFilter==="healthy" && !n.healthy) return false;
    if(STATE.nodeFilter==="bad" && (n.healthy||n.reserved)) return false;
    if(!q) return true;
    return [n.user,n.state,n.loc,n.exit_ipv6,n.prefix64,n.endpoint].join(" ").toLowerCase().indexOf(q)>=0;
  });
}
function renderNodes(){
  var list=nodeFiltered();
  var size=STATE.nodePageSize, pages=Math.max(1,Math.ceil(list.length/size));
  if(STATE.nodePage>pages) STATE.nodePage=pages;
  var start=(STATE.nodePage-1)*size, page=list.slice(start,start+size);
  var rows=page.map(function(n){
    var loc=n.reserved?"<span class=\"tag relay\">中继预留</span>":(n.loc?esc(n.loc):"-");
    var st=n.healthy?"<span class=\"tag ok\">healthy</span>":(n.state==="ok"?"<span class=\"tag bad\">stale</span>":"<span class=\"tag bad\">"+esc(n.state)+"</span>");
    return "<tr><td class=\"mono\">"+n.index+"</td><td class=\"mono\">"+esc(n.user)+"</td><td class=\"mono\">"+n.port+"</td><td>"+st+"</td><td class=\"mono\">"+esc(n.exit_ipv6||"-")+"</td><td>"+loc+"</td><td class=\"mono\">"+esc(n.endpoint||"-")+"</td><td class=\"mono\">"+n.connections+"</td><td class=\"mono\">"+fmtBytes(n.bytes_up)+"</td><td class=\"mono\">"+fmtBytes(n.bytes_down)+"</td><td class=\"mono\">"+ago(n.updated_at)+"</td><td><button class=\"small\" onclick=\"actionNode('"+jsq(n.user)+"','reroll')\">轮换</button> <button class=\"small\" onclick=\"actionNode('"+jsq(n.user)+"','disable')\">停用</button></td></tr>";
  }).join("");
  var html="";
  html+="<div class=\"card\"><h2>节点</h2>";
  html+="<div class=\"toolbar\">";
  html+="<input type=\"text\" id=\"nodeQ\" placeholder=\"搜索用户 / 前缀 / endpoint\" value=\""+esc(STATE.nodeQuery)+"\" oninput=\"STATE.nodeQuery=this.value;STATE.nodePage=1;renderNodes()\" style=\"max-width:280px\">";
  html+="<select onchange=\"STATE.nodeFilter=this.value;STATE.nodePage=1;renderNodes()\"><option value=\"all\""+(STATE.nodeFilter==="all"?" selected":"")+">全部</option><option value=\"healthy\""+(STATE.nodeFilter==="healthy"?" selected":"")+">仅健康</option><option value=\"bad\""+(STATE.nodeFilter==="bad"?" selected":"")+">仅异常</option><option value=\"reserved\""+(STATE.nodeFilter==="reserved"?" selected":"")+">中继预留</option></select>";
  html+="<select onchange=\"STATE.nodePageSize=Number(this.value);STATE.nodePage=1;renderNodes()\"><option"+(STATE.nodePageSize===20?" selected":"")+">20</option><option"+(STATE.nodePageSize===50?" selected":"")+">50</option><option"+(STATE.nodePageSize===100?" selected":"")+">100</option></select>";
  html+="<span class=\"spacer\"></span><button onclick=\"loadAll()\">刷新</button><button class=\"danger\" onclick=\"rerollAll()\">全部轮换</button></div>";
  html+="<table><thead><tr><th>#</th><th>用户</th><th>端口</th><th>状态</th><th>出口 IPv6</th><th>地区</th><th>入口 endpoint</th><th>连接</th><th>上行</th><th>下行</th><th>更新时间</th><th>操作</th></tr></thead><tbody>"+(rows||"<tr><td colspan=\"12\" class=\"muted\">没有匹配的节点</td></tr>")+"</tbody></table>";
  html+="<div class=\"pager\"><span>共 "+list.length+" 条，第 "+STATE.nodePage+" / "+pages+" 页</span><button class=\"small\" onclick=\"pageNode(-1)\" "+(STATE.nodePage<=1?"disabled":"")+">上一页</button><button class=\"small\" onclick=\"pageNode(1)\" "+(STATE.nodePage>=pages?"disabled":"")+">下一页</button></div>";
  html+="<div class=\"note\"><b>轮换</b>：停掉该节点的 wireproxy，重新注册一个 wgcf 设备并重新握手 WARP，拿到新的出口 /64。连接会瞬断，旧账号被替换；不会换地区。<b>停用</b>：从下发列表里摘掉该节点，账号保留。</div>";
  html+="</div>";
  document.getElementById("tab-nodes").innerHTML=html;
}
function pageNode(d){ STATE.nodePage+=d; renderNodes(); }
function actionNode(user,action){
  api("/nodes/"+encodeURIComponent(user)+"/"+action,{method:"POST"}).then(function(){ toast("已排队: "+user+" "+action); setTimeout(loadAll,1200); }).catch(function(e){ toast("操作失败: "+e.message); });
}
function rerollAll(){ if(!confirm("轮换全部节点会让所有 SOCKS 出口瞬断，确认？")) return; api("/reroll",{method:"POST"}).then(function(){ toast("已排队全部轮换"); setTimeout(loadAll,1500); }).catch(function(e){ toast("失败: "+e.message); }); }

function renderConnect(){
  var m=STATE.meta, s=STATE.status; if(!m||!s) return;
  var api=m.api||{}, socks=m.socks||{}, d=m.deploy||{};
  var html="";
  html+="<div class=\"two\">";
  html+="<div class=\"card\"><h2>连接信息</h2><div class=\"kv\">";
  html+=kvRow("Pool ID",d.pool_id||s.pool.id);
  html+=kvRow("API 地址（私网）",api.private_url);
  html+=kvRow("API 地址（公网）",api.public_url);
  html+=kvRow("默认 API Token",api.token);
  html+=kvRow("SOCKS 用户",socks.user);
  html+=kvRow("SOCKS 密码",socks.pass);
  html+=kvRow("Mux 端口",socks.mux_port);
  html+="</div><div class=\"note\">控制台登录（用户名/密码）与 API Token 是两套鉴权：登录只给这个网页用，Token 给网关/脚本用。SOCKS 密码用于直连 mux 的客户端；网关同步端点时会自动带上。</div></div>";
  html+="<div class=\"card\"><h2>填到网关静态池</h2><pre style=\"max-height:none\">API 地址: "+esc(api.private_url||api.public_url)+"\nToken: "+esc(api.token)+"\n范围: private</pre>";
  html+="<div class=\"row\" style=\"margin-top:10px\"><button onclick=\"copyGateway()\">复制接入信息</button></div>";
  html+="<div class=\"note\">同平台（例如 FRA 网关 + 同项目池）用私网地址；跨平台或本地电脑用公网地址。范围 private/public 决定网关从池里取哪一组 SOCKS 主机名。</div></div>";
  html+="</div>";

  html+="<div class=\"card\"><h2>Token 管理</h2><div class=\"hint\">为不同容器/团队各发一个 Token，并发调用时互不共享，删掉一个不影响其它网关。默认 Token 由容器环境变量 API_TOKEN 固定，不能在这里修改。</div>";
  html+="<div class=\"row\" style=\"margin-top:12px\"><input type=\"text\" id=\"tokName\" placeholder=\"名称（例如 gateway-fra）\" style=\"flex:1 1 200px\"><input type=\"text\" id=\"tokVal\" placeholder=\"自定义 Token（留空自动生成）\" style=\"flex:2 1 260px\"><button class=\"primary\" onclick=\"addToken()\">新增 Token</button></div>";
  html+="<div id=\"tokenList\" style=\"margin-top:14px\">加载中…</div></div>";
  document.getElementById("tab-connect").innerHTML=html;
  loadTokens();
}
function kvRow(k,v){ return "<div class=\"k\">"+esc(k)+"</div><div class=\"v\">"+esc(v==null||v===""?"-":v)+" <button class=\"small\" onclick=\"copy('"+jsq(v)+"')\">复制</button></div>"; }
function copyGateway(){ var m=STATE.meta, api=m.api||{}; copy("API 地址: "+(api.private_url||api.public_url)+"\nToken: "+api.token+"\n范围: private"); }

function loadTokens(){
  if(!STATE.tab || STATE.tab!=="connect") { /* prefetch ok */ }
  api("/tokens").then(function(d){ STATE.tokens=d; renderTokens(); }).catch(function(e){ var el=document.getElementById("tokenList"); if(el) el.innerHTML="<span class=\"muted\">读取失败: "+esc(e.message)+"</span>"; });
}
function renderTokens(){
  var d=STATE.tokens; if(!d) return; var el=document.getElementById("tokenList"); if(!el) return;
  var rows="";
  if(d.legacy){ rows+="<tr><td>"+esc(d.legacy.name)+"</td><td class=\"mono\">"+esc(d.legacy.suffix)+"</td><td class=\"muted\">固定</td><td><span class=\"tag\">默认</span></td></tr>"; }
  (d.tokens||[]).forEach(function(t){
    rows+="<tr><td>"+esc(t.name)+"</td><td class=\"mono\">"+esc(String(t.token).slice(-4))+"</td><td class=\"muted\">"+new Date(t.created_at*1000).toLocaleString()+"</td><td><button class=\"small\" onclick=\"copy('"+jsq(t.token)+"')\">复制</button> <button class=\"small\" onclick=\"renameToken('"+jsq(t.id)+"')\">改名</button> <button class=\"small danger\" onclick=\"delToken('"+jsq(t.id)+"')\">删除</button></td></tr>";
  });
  el.innerHTML="<table><thead><tr><th>名称</th><th>Token 尾号</th><th>创建时间</th><th>操作</th></tr></thead><tbody>"+(rows||"<tr><td colspan=\"4\" class=\"muted\">还没有自定义 Token</td></tr>")+"</tbody></table>";
}
function addToken(){
  var name=document.getElementById("tokName").value, token=document.getElementById("tokVal").value;
  api("/tokens",{method:"POST",body:JSON.stringify({name:name,token:token})}).then(function(e){
    document.getElementById("tokName").value=""; document.getElementById("tokVal").value="";
    toast("已新增: "+e.name+"（可复制）");
    copy(e.token);
    loadTokens();
  }).catch(function(err){ toast("新增失败: "+err.message); });
}
function renameToken(id){ var name=prompt("新名称"); if(!name) return; api("/tokens/"+encodeURIComponent(id),{method:"PATCH",body:JSON.stringify({name:name})}).then(loadTokens).catch(function(e){toast("改名失败: "+e.message)}); }
function delToken(id){ if(!confirm("删除这个 Token？使用它的容器会立刻失效。")) return; api("/tokens/"+encodeURIComponent(id),{method:"DELETE"}).then(loadTokens).catch(function(e){toast("删除失败: "+e.message)}); }

function renderDeploy(){
  var m=STATE.meta; if(!m) return; var d=m.deploy||{};
  var rows=[
    ["POOL_ID",(m.pool&&m.pool.id)||"-","池标识，网关展示用"],
    ["STATE_DIR",d.state_dir,"数据目录：实例、账号、control、status.tsv 都在这里"],
    ["START_PORT",d.start_port,"第一个 wireproxy SOCKS 端口"],
    ["INSTANCE_COUNT",d.instance_count,"当前节点数（含中继预留槽位）"],
    ["INSTANCE_COUNT (env)",d.instance_count_env,"容器环境变量里的初始节点数；运行时扩容不改它"],
    ["MUX_PORT",d.mux_port,"对外 mux 端口，网关只连这个端口"],
    ["端口范围",(d.port_start||"-")+"-"+(d.port_end||"-"),"wireproxy 端口区间"],
    ["SOCKS_USER",(m.socks&&m.socks.user)||"-","上游 SOCKS 用户名前缀，节点用 user01..userNN"],
    ["REROLL_INTERVAL",d.reroll_interval,"轮换间隔秒数：每轮重新注册多少节点的账号"],
    ["REROLL_BATCH",d.reroll_batch,"每轮轮换的节点数"],
    ["HEALTH_INTERVAL",d.health_interval,"探活周期秒数；决定 15 分钟健康窗口内能覆盖多少节点"],
    ["MIN_HEALTHY",d.min_healthy,"徽标转绿所需的最小健康节点数"],
    ["DEDUP_RETRIES",d.dedup_retries,"拿到重复 /64 时的重注册次数"],
    ["REGISTER_RETRIES",d.register_retries,"wgcf 注册尝试次数"],
    ["PROBE_URL",d.probe_url,"探活地址，返回 trace 里的 ip/loc"],
    ["PROBE_TIMEOUT",d.probe_timeout,"单次探活超时秒数"],
    ["ENDPOINTS_FILE",d.endpoints_file,"WARP 入口 anycast 候选文件"],
    ["ENDPOINTS_COUNT",d.endpoints_count,"候选入口数量"],
    ["DENY_48_PREFIXES",d.deny_48_prefixes||"(空)","拒绝的 /48 前缀"],
    ["POOL_PUBLIC_HOST",d.pool_public_host,"公网主机名，写进网关的 public 范围"],
    ["POOL_PRIVATE_HOST",d.pool_private_host,"私网主机名，写进网关的 private 范围"],
    ["EXTERNAL_HOST",d.external_host||"(空)","外部主机名，部署在别的平台时填"],
    ["WGCF_KEEPALIVE",d.keepalive,"WireGuard 保活秒数"],
    ["SCALE_MIN / MAX",d.scale_min+" / "+d.scale_max,"运行时可调节点数的范围"],
    ["中继预留",d.relay_slots+" 个","末尾节点由中继容器使用，不参与池下发"],
    ["控制台账号",d.admin_user,"POOL_ADMIN_USER，登录这个网页用"]
  ];
  var tr=rows.map(function(r){ return "<tr><td class=\"mono\">"+esc(r[0])+"</td><td class=\"mono\">"+esc(r[1])+"</td><td class=\"muted\">"+esc(r[2])+"</td></tr>"; }).join("");
  var env={POOL_ID:((m.pool&&m.pool.id)||"-")+"-REGION",STATE_DIR:d.state_dir,START_PORT:d.start_port,MUX_PORT:d.mux_port,INSTANCE_COUNT:d.instance_count_env,SOCKS_USER:d.socks_user,SOCKS_PASS:"WarpPass2026",API_TOKEN:"<新池 Token>",REROLL_INTERVAL:d.reroll_interval,REROLL_BATCH:d.reroll_batch,HEALTH_INTERVAL:d.health_interval,MIN_HEALTHY:d.min_healthy,DEDUP_RETRIES:d.dedup_retries,REGISTER_RETRIES:d.register_retries,PROBE_URL:d.probe_url,PROBE_TIMEOUT:d.probe_timeout,ENDPOINTS_FILE:d.endpoints_file,POOL_PUBLIC_HOST:"<新池公网主机名>",POOL_PRIVATE_HOST:"<新池私网主机名>"};
  var html="";
  html+="<div class=\"card\"><h2>部署参数</h2><table><thead><tr><th>参数</th><th>当前值</th><th>含义</th></tr></thead><tbody>"+tr+"</tbody></table></div>";
  html+="<div class=\"card\"><h2>部署 env（JSON）</h2><pre>"+esc(JSON.stringify(env,null,2))+"</pre><div class=\"note\">这些是容器启动参数。改 INSTANCE_COUNT / START_PORT 后需要重新部署才会改变启动规模；运行中扩缩容请用「扩容与区域」页。</div></div>";
  document.getElementById("tab-deploy").innerHTML=html;
}

function renderScale(){
  var m=STATE.meta; if(!m) return; var d=m.deploy||{};
  var eps=(m.endpoints&&m.endpoints.items)||[];
  var html="";
  html+="<div class=\"two\">";
  html+="<div class=\"card\"><h2>在线扩缩容</h2><div class=\"kv\">"+kvRow("当前节点数",d.instance_count+" 个（env "+d.instance_count_env+"）")+kvRow("节点端口范围",(d.port_start||"-")+"-"+(d.port_end||"-"))+kvRow("Mux 端口",d.mux_port)+"</div>";
  html+="<div class=\"row\" style=\"margin-top:12px\"><input type=\"text\" id=\"scaleCount\" value=\""+esc(d.instance_count)+"\" style=\"max-width:160px\"><button class=\"primary\" onclick=\"applyScale()\">应用扩缩容</button></div>";
  html+="<div class=\"note\">扩容会启动新 wireproxy 并重启 mux，API 和 SOCKS 会中断几秒；已注册账号保留。缩容会停掉尾部节点（中继预留槽位在最后，缩容前先确认不要压到它们）。目标值持久化到 /data/control/scale，重启后仍生效。</div></div>";
  html+="<div class=\"card\"><h2>换地区 / 多池</h2><div class=\"note\" style=\"margin-top:0\">WARP 是 Anycast：出口地区由池容器所在地区决定（当前 "+(m.regions||[]).map(function(r){return r.loc}).join(", ")+"），改节点数不会换地区。<br>要覆盖更多地区，在目标地区再部署一个同样的池服务，设置不同的 POOL_ID、STATE_DIR（或独立卷）和端口，然后把它的 API 地址 + token 加到网关静态池。网关会合并去重所有启用池的端点。</div>";
  html+="<div class=\"row\" style=\"margin-top:10px\"><button onclick=\"copyNewPoolEnv()\">复制新池部署 env</button></div></div>";
  html+="</div>";
  html+="<div class=\"card\"><h2>WARP 入口 endpoint 候选</h2><div class=\"hint\">每个节点从下列 Cloudflare Anycast 地址里随机选一个入口；换 endpoint 只影响入口，不改变出口地区。</div><pre style=\"margin-top:10px\">"+eps.map(esc).join("  ")+"</pre></div>";
  document.getElementById("tab-scale").innerHTML=html;
}
function applyScale(){
  var n=Number(document.getElementById("scaleCount").value||0);
  api("/scale",{method:"POST",body:JSON.stringify({count:n})}).then(function(){ toast("已排队扩缩容到 "+n); setTimeout(loadAll,1500); }).catch(function(e){ toast("失败: "+e.message); });
}
function copyNewPoolEnv(){
  var m=STATE.meta,d=m.deploy||{};
  var env={POOL_ID:((m.pool&&m.pool.id)||"-")+"-REGION",STATE_DIR:d.state_dir,START_PORT:d.start_port,MUX_PORT:d.mux_port,INSTANCE_COUNT:d.instance_count_env,SOCKS_USER:d.socks_user,SOCKS_PASS:"WarpPass2026",API_TOKEN:"<新池 Token>",REROLL_INTERVAL:d.reroll_interval,REROLL_BATCH:d.reroll_batch,HEALTH_INTERVAL:d.health_interval,MIN_HEALTHY:d.min_healthy,DEDUP_RETRIES:d.dedup_retries,REGISTER_RETRIES:d.register_retries,PROBE_URL:d.probe_url,PROBE_TIMEOUT:d.probe_timeout,ENDPOINTS_FILE:d.endpoints_file,POOL_PUBLIC_HOST:"<新池公网主机名>",POOL_PRIVATE_HOST:"<新池私网主机名>"};
  copy(JSON.stringify(env,null,2));
}

function loadLogs(){
  var lines=(document.getElementById("logLines")&&document.getElementById("logLines").value)||"200";
  api("/logs?lines="+encodeURIComponent(lines)).then(function(d){
    var el=document.getElementById("logPre"); if(!el) return;
    if(typeof d==="string"){ el.textContent=d||"(空)"; return; }
    el.textContent=(d&&d.lines||[]).join("\n")||"(空)";
  }).catch(function(e){ var el=document.getElementById("logPre"); if(el) el.textContent="读取失败: "+e.message; });
}
function renderLogsStatic(){
  var html="<div class=\"card\"><h2>日志</h2><div class=\"toolbar\"><select id=\"logLines\" onchange=\"loadLogs()\"><option>200</option><option>500</option><option>1000</option></select><button onclick=\"loadLogs()\">刷新</button></div><pre id=\"logPre\">加载中…</pre></div>";
  document.getElementById("tab-logs").innerHTML=html;
}
function loadMerged(){
  var el=document.getElementById("mergedPre");
  api("/merged-status?fresh=1").then(function(d){ if(el) el.textContent=JSON.stringify(d,null,2); }).catch(function(e){ if(el) el.textContent="读取失败: "+e.message; });
}
function renderMergedStatic(){
  var html="<div class=\"card\"><h2>多池聚合</h2><div class=\"hint\">这里汇总本池与上游池（例如中继容器）的状态；网关同步端点时会把所有启用池合并去重。</div><div class=\"row\" style=\"margin-top:10px\"><button onclick=\"loadMerged()\">刷新</button></div><pre id=\"mergedPre\" style=\"margin-top:10px\">加载中…</pre></div>";
  document.getElementById("tab-merged").innerHTML=html;
}

renderNav();
renderLogsStatic();
renderMergedStatic();
document.getElementById("tab-overview").classList.add("active");
loadAll();
setInterval(function(){ if(STATE.authed && STATE.tab!=="logs"){ api("/status").then(function(s){ STATE.status=s; renderHeader(); if(STATE.tab==="overview") renderOverview(); if(STATE.tab==="nodes") renderNodes(); }).catch(function(){}); } }, 15000);
</script>
</body>
</html>
`
