// Page: 代理中心 — subscriptions, node pool, probes, logs.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, localTime, toast } from "../ui.js";

const state = {
  store: null,
  nodes: [],
  total: 0,
  page: 1,
  size: 50,
  status: "",
  provider: "",
  q: "",
  selected: new Set(),
  timer: null,
  panel: "nodes",
  cfEditing: "",
  egressPick: null,
  health: null,
  pools: [],
  poolSync: null,
  poolStatus: {},
  poolEditing: "",
  poolDetail: "",
  poolEgressLines: 0,
  poolEgressFile: "",
  poolTimer: null,
};

const statusText = () => ({
  alive: t("可用", "proxy.state.alive"),
  limited: t("可连但被限流", "proxy.state.limited"),
  dead: t("不通", "proxy.state.dead"),
  unknown: t("未测试", "proxy.state.unknown"),
});

const statusTone = { alive: "ok", limited: "warn", dead: "bad", unknown: "" };

const quotaText = () => ({
  ok: t("可用", "proxy.quota.ok"),
  limited: t("已限额", "proxy.quota.limited"),
  blocked: t("被封", "proxy.quota.blocked"),
  error: t("探测失败", "proxy.quota.error"),
  unknown: t("未探测", "proxy.quota.unknown"),
});

const quotaTone = { ok: "ok", limited: "warn", blocked: "bad", error: "warn", unknown: "" };

const egressText = () => ({
  proxy: t("代理池（全部流量）", "proxy.mode.proxy"),
  warp: t("WARP（仅免费模型）", "proxy.mode.warp"),
  "warp-only": t("WARP（免费模型，其余直连）", "proxy.mode.warpOnly"),
  direct: t("直连（不使用代理）", "proxy.mode.direct"),
});

const egressNote = () => ({
  proxy: t("免费模型和 Go/付费流量都走代理池。", "proxy.mode.note.proxy"),
  warp: t("免费模型走内置 WARP，Go/付费流量走代理池。", "proxy.mode.note.warp"),
  "warp-only": t("免费模型走内置 WARP；其余流量直连，不使用代理池。", "proxy.mode.note.warpOnly"),
  direct: t("全部流量直连，不使用 WARP 和代理池。", "proxy.mode.note.direct"),
});

const egressSpecs = () => [
  {
    mode: "proxy",
    title: t("代理池", "proxy.mode.short.proxy"),
    free: t("代理池", "proxy.flow.pool"),
    other: t("代理池", "proxy.flow.pool"),
    role: t("承载全部流量", "proxy.role.proxy"),
  },
  {
    mode: "warp",
    title: t("WARP", "proxy.mode.short.warp"),
    free: t("WARP，失败时直连兜底", "proxy.flow.warpFallback"),
    other: t("代理池", "proxy.flow.pool"),
    role: t("只承载其它流量", "proxy.role.warp"),
  },
  {
    mode: "warp-only",
    title: t("仅 WARP", "proxy.mode.short.warpOnly"),
    free: t("WARP", "proxy.flow.warp"),
    other: t("直连", "proxy.flow.direct"),
    role: t("仅用于注册 WARP", "proxy.role.warpOnly"),
  },
  {
    mode: "direct",
    title: t("直连", "proxy.mode.short.direct"),
    free: t("直连", "proxy.flow.direct"),
    other: t("直连", "proxy.flow.direct"),
    role: t("不参与", "proxy.role.direct"),
  },
];

const egressRoleText = () => ({
  proxy: t(
    "当前模式「代理池」：本页节点承载全部流量，免费模型和其它流量都从这里出网。",
    "proxy.nodeRole.proxy",
  ),
  warp: t(
    "当前模式「WARP」：本页节点只承载其它流量；免费模型走 WARP，WARP 全部不可用时回落直连。",
    "proxy.nodeRole.warp",
  ),
  "warp-only": t(
    "当前模式「仅 WARP」：本页节点仅用于给 WARP 注册新出口，不承载任何业务流量。",
    "proxy.nodeRole.warpOnly",
  ),
  direct: t("当前模式「直连」：本页节点未参与任何流量。", "proxy.nodeRole.direct"),
});

function currentEgressMode() {
  return state.egressPick || state.store?.settings?.egress_mode || "proxy";
}

function renderEgressMode() {
  const mode = currentEgressMode();
  $("#proxy-egress-mode").value = mode;
  $("#proxy-mode-current").textContent = egressText()[mode] || mode;
  $("#proxy-mode-note").textContent = egressNote()[mode] || "";
  $("#proxy-node-role").textContent = egressRoleText()[mode] || "";
  const host = $("#proxy-egress-options");
  if (!host) return;
  host.replaceChildren();
  for (const spec of egressSpecs()) {
    const card = el(
      "button",
      {
        type: "button",
        class: "egress-option",
        role: "radio",
        "aria-checked": spec.mode === mode ? "true" : "false",
        dataset: { mode: spec.mode },
      },
      [
        el("span", { class: "egress-option-title", text: spec.title }),
        el("span", { class: "egress-option-row" }, [
          el("span", { class: "egress-option-key", text: t("免费模型", "proxy.flow.free") }),
          el("span", { text: spec.free }),
        ]),
        el("span", { class: "egress-option-row" }, [
          el("span", { class: "egress-option-key", text: t("其它流量", "proxy.flow.other") }),
          el("span", { text: spec.other }),
        ]),
        el("span", {
          class: "egress-option-role",
          text: `${t("节点页", "proxy.flow.nodes")}：${spec.role}`,
        }),
      ],
    );
    card.addEventListener("click", () => {
      state.egressPick = spec.mode;
      renderEgressMode();
    });
    host.append(card);
  }
}


function quotaLabel(node) {
  const label = quotaText()[node.quota] || node.quota || "—";
  const retry = Number(node.retry_after_seconds) || 0;
  if (retry > 0 && node.quota !== "ok") return `${label} · ${Math.ceil(retry / 3600)}h`;
  return label;
}

async function loadHealth() {
  try {
    state.health = await api("/healthz");
    if (state.store) renderOverview();
  } catch {
    /* health is advisory; the node pool still renders without it */
  }
}

async function loadStore() {
  const store = await api("/api/proxy");
  if (!hasSession()) return;
  state.store = store;
  renderOverview();
  renderProviders();
  renderCFSources();
  renderSettings();
  scheduleJobPoll();
}

async function loadNodes() {
  const params = new URLSearchParams({
    page: String(state.page),
    size: String(state.size),
  });
  if (state.status) params.set("status", state.status);
  if (state.provider) params.set("provider", state.provider);
  if (state.q) params.set("q", state.q);
  const result = await api(`/api/proxy/nodes?${params}`);
  if (!hasSession()) return;
  state.nodes = result.nodes || [];
  state.total = result.total || 0;
  renderNodes();
}

function scheduleJobPoll() {
  clearTimeout(state.timer);
  if (state.store?.job?.running) {
    state.timer = setTimeout(async () => {
      try {
        await loadStore();
        await loadNodes();
      } catch {
        /* a transient poll failure leaves the next tick to retry */
      }
    }, 2500);
  }
}

function fact(label, value) {
  return el("div", { class: "fact" }, [
    el("span", { class: "fact-label", text: label }),
    el("span", { class: "fact-value", text: String(value) }),
  ]);
}

function renderOverview() {
  const store = state.store;
  if (!store) return;
  const host = $("#proxy-cards");
  host.replaceChildren();
  const rt = store.runtime || {};
  const counts = rt.statuses || {};
  const w = state.health?.warp || {};
  host.append(
    fact(t("WARP 池（免费模型）", "proxy.fact.warp"), w.size ? `${w.active ?? 0} / ${w.size}` : "—"),
    fact(t("WARP 待命 / 冷却", "proxy.fact.warpPool"), w.size ? `${w.standby ?? 0} / ${w.cooling ?? 0}` : "—"),
    fact(t("节点池节点", "proxy.fact.total"), rt.proxylines ?? 0),
    fact(t("可用", "proxy.fact.alive"), counts.alive ?? 0),
    fact(t("限流", "proxy.fact.limited"), counts.limited ?? 0),
    fact(t("不通", "proxy.fact.dead"), counts.dead ?? 0),
    fact(t("未测", "proxy.fact.unknown"), counts.unknown ?? 0),
    fact(t("已选", "proxy.fact.selected"), rt.selected ?? 0),
    fact("mihomo", rt.mihomo_alive ? `PID ${rt.mihomo_pid}` : t("未运行", "proxy.mihomoDown")),
    fact(t("上次同步", "proxy.fact.sync"), rt.last_sync ? localTime(rt.last_sync) : "—"),
  );
  const job = store.job;
  const status = $("#proxy-status");
  if (job?.running) status.textContent = t(`${job.action} 进行中…`, "proxy.jobRunning", job.action);
  else if (job?.error) status.textContent = t(`上次任务失败：${job.error}`, "proxy.jobFailed", job.error);
  else if (rt.last_sync_error) status.textContent = rt.last_sync_error;
  else status.textContent = t("订阅改动后点「同步订阅」，选中结果会落到本地 SOCKS 端口。", "proxy.hint");
}

function renderProviders() {
  const body = $("#proxy-providers");
  body.replaceChildren();
  const providers = state.store?.providers || [];
  for (const provider of providers) {
    const toggle = el("input", { type: "checkbox", checked: provider.enabled });
    toggle.addEventListener("change", async () => {
      try {
        await api(`/api/proxy/providers/${provider.id}`, {
          method: "PATCH",
          body: JSON.stringify({ enabled: toggle.checked }),
        });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    const remove = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      if (!confirm(t(`删除订阅「${provider.name}」？`, "proxy.confirmDelete", provider.name))) return;
      try {
        await api(`/api/proxy/providers/${provider.id}`, { method: "DELETE" });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { class: "mono", text: provider.id }),
        el("td", { text: provider.name }),
        el("td", { class: "mono proxy-url", text: provider.url }),
        el("td", { class: "num", text: String(provider.node_count || 0) }),
        el("td", { text: provider.last_fetch ? localTime(provider.last_fetch) : "—" }),
        el("td", { text: provider.last_error || "—" }),
        el("td", { class: "num" }, [toggle]),
        el("td", {}, [remove]),
      ]),
    );
  }
  if (!providers.length) emptyRow(body, 8, t("还没有订阅", "proxy.noProviders"));
}

function poolLive(pool) {
  const view = state.poolStatus[pool.id];
  return view?.ok ? view.status?.pool || null : null;
}

function renderPoolCards() {
  const host = $("#pool-cards");
  if (!host) return;
  host.replaceChildren();
  const pools = state.pools || [];
  const enabled = pools.filter((pool) => pool.enabled).length;
  const sync = state.poolSync || {};
  const healthy = pools.reduce((sum, pool) => sum + (poolLive(pool)?.healthy || 0), 0);
  const nodes = pools.reduce((sum, pool) => sum + (poolLive(pool)?.count || 0), 0);
  host.append(
    fact("静态池", pools.length),
    fact("启用池", enabled),
    fact("池节点（健康 / 总数）", nodes ? `${healthy} / ${nodes}` : "—"),
    fact("端点文件行数", state.poolEgressLines ?? 0),
    fact("上次同步", sync.synced_at ? localTime(sync.synced_at) : "—"),
  );
  const status = $("#pool-status");
  if (!status) return;
  if (sync.last_error) status.textContent = `上次同步失败：${sync.last_error}`;
  else if (sync.synced_at) status.textContent = `已同步 ${sync.endpoints ?? 0} 个端点 · ${state.poolEgressFile || ""}`;
  else status.textContent = "还没同步过；点「立即同步」拉取所有启用池的端点并热重载网关。";
}

function renderPoolList() {
  const body = $("#pool-list");
  if (!body) return;
  body.replaceChildren();
  const pools = state.pools || [];
  for (const pool of pools) {
    const toggle = el("input", { type: "checkbox", checked: pool.enabled });
    toggle.addEventListener("change", async () => {
      pool.enabled = toggle.checked;
      await savePools();
    });
    const status = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: "状态/调参" });
    status.addEventListener("click", () => openPoolDetail(pool.id));
    const edit = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("编辑", "common.edit") });
    edit.addEventListener("click", () => fillPoolForm(pool));
    const remove = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      if (!confirm(`删除静态池「${pool.name}」？端点列表要重新同步才会移除。`)) return;
      state.pools = pools.filter((item) => item.id !== pool.id);
      if (state.poolEditing === pool.id) resetPoolForm();
      if (state.poolDetail === pool.id) {
        state.poolDetail = "";
        $("#pool-detail").classList.add("hidden");
      }
      await savePools();
    });
    const live = poolLive(pool);
    body.append(
      el("tr", {}, [
        el("td", { text: pool.name }),
        el("td", { class: "mono proxy-url", text: pool.api_url }),
        el("td", { text: pool.scope }),
        el("td", {
          class: "num",
          text: String(live?.count ?? pool.endpoint_count ?? 0),
          title: live ? `健康 ${live.healthy ?? 0} / 启用 ${live.enabled ?? 0}` : "",
        }),
        el("td", { text: pool.last_sync ? localTime(pool.last_sync) : "—" }),
        el("td", { text: pool.last_error || "—" }),
        el("td", { class: "num" }, [toggle]),
        el("td", {}, [status, edit, remove]),
      ]),
    );
  }
  if (!pools.length) emptyRow(body, 8, "还没有静态池，点「新增池」添加。");
}

function fillPoolForm(pool) {
  state.poolEditing = pool.id || "";
  $("#pool-name").value = pool.name || "";
  $("#pool-api").value = pool.api_url || "";
  $("#pool-scope").value = pool.scope || "private";
  $("#pool-token").value = "";
  $("#pool-token").placeholder = pool.token_set ? "已保存，留空保持不变" : "Bearer token";
  $("#pool-enabled").checked = pool.enabled !== false;
  $("#pool-editor").classList.remove("hidden");
}

function resetPoolForm() {
  state.poolEditing = "";
  $("#pool-name").value = "";
  $("#pool-api").value = "";
  $("#pool-scope").value = "private";
  $("#pool-token").value = "";
  $("#pool-token").placeholder = "Bearer token";
  $("#pool-enabled").checked = true;
  $("#pool-editor").classList.add("hidden");
}

async function savePools(pools = state.pools) {
  try {
    const result = await api("/api/proxy/pools", {
      method: "PUT",
      body: JSON.stringify({ pools }),
    });
    state.pools = result.pools || [];
    renderPoolCards();
    renderPoolList();
    toast("池配置已保存", false);
    return true;
  } catch (error) {
    toast(error.message, true);
    await loadPools();
    return false;
  }
}

async function savePoolForm() {
  const name = $("#pool-name").value.trim();
  const apiURL = $("#pool-api").value.trim();
  if (!name || !apiURL) {
    toast("名称和 API 地址都要填", true);
    return;
  }
  const pool = {
    id: state.poolEditing,
    name,
    api_url: apiURL,
    scope: $("#pool-scope").value,
    enabled: $("#pool-enabled").checked,
  };
  const token = $("#pool-token").value.trim();
  if (token) pool.token = token;
  const pools = [...(state.pools || [])];
  const index = pools.findIndex((item) => item.id === state.poolEditing);
  if (index >= 0) pools[index] = { ...pools[index], ...pool };
  else pools.push(pool);
  if (await savePools(pools)) resetPoolForm();
}

async function loadPools() {
  const result = await api("/api/proxy/pools");
  if (!hasSession()) return;
  state.pools = result.pools || [];
  state.poolSync = result.pool_sync || {};
  state.poolEgressLines = result.egress_lines ?? 0;
  state.poolEgressFile = result.egress_file || "";
  renderPoolCards();
  renderPoolList();
}

async function loadPoolStatus() {
  const result = await api("/api/proxy/pools/status");
  if (!hasSession()) return;
  state.poolStatus = {};
  for (const view of result.pools || []) state.poolStatus[view.id] = view;
  state.poolEgressLines = result.egress_lines ?? state.poolEgressLines;
  renderPoolCards();
  renderPoolList();
  if (state.poolDetail) renderPoolDetail();
}

async function openPoolDetail(id) {
  state.poolDetail = id;
  $("#pool-detail").classList.remove("hidden");
  renderPoolDetail();
  try {
    await loadPoolStatus();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderPoolDetail() {
  const pool = (state.pools || []).find((item) => item.id === state.poolDetail);
  if (!pool) {
    $("#pool-detail").classList.add("hidden");
    return;
  }
  $("#pool-detail-title").textContent = `池状态 · ${pool.name}`;
  const view = state.poolStatus[pool.id];
  const data = view?.status;
  const summary = data?.pool;
  const nodes = data?.nodes || [];
  const regions = {};
  for (const node of nodes) {
    const loc = node.loc || "?";
    regions[loc] = (regions[loc] || 0) + 1;
  }
  const regionText = Object.entries(regions).map(([loc, n]) => `${loc}:${n}`).join(" / ") || "—";
  const ports = nodes.map((node) => Number(node.port)).filter(Number.isFinite);
  const portText = ports.length ? `${Math.min(...ports)}-${Math.max(...ports)}` : "—";
  const host = $("#pool-facts");
  host.replaceChildren();
  if (!view) {
    host.append(fact("状态", "加载中…"));
  } else if (!view.ok) {
    host.append(fact("状态", "连接失败"), fact("错误", view.error || "—"));
  } else if (summary) {
    host.append(
      fact("节点规模（健康 / 总数）", `${summary.healthy ?? 0} / ${summary.count ?? 0}`),
      fact("启用节点", summary.enabled ?? "—"),
      fact("地区", regionText),
      fact("节点端口", portText),
      fact("轮换间隔", `${summary.rotation_interval_seconds ?? "—"} 秒`),
      fact("每轮轮换", summary.reroll_batch ?? "—"),
      fact("健康检查", `${summary.health_interval_seconds ?? "—"} 秒`),
      fact("最小健康数", summary.min_healthy ?? "—"),
      fact("mux 端口", summary.mux_port ?? "—"),
      fact("内网地址", summary.private_host || "—"),
      fact("公网地址", summary.public_host || "—"),
      fact("SOCKS 用户", summary.socks_user || "—"),
      fact("API 地址", pool.api_url || "—"),
      fact("API Token", pool.token_set ? "已保存（不回显）" : "未设置"),
    );
    $("#pool-rotation").value = summary.rotation_interval_seconds ?? "";
    $("#pool-batch").value = summary.reroll_batch ?? "";
    $("#pool-health").value = summary.health_interval_seconds ?? "";
    $("#pool-min-healthy").value = summary.min_healthy ?? "";
    const scaleInput = $("#pool-scale-count");
    if (scaleInput && document.activeElement !== scaleInput) scaleInput.value = summary.count ?? "";
  }
  const note = $("#pool-region-note");
  if (note) {
    note.textContent = summary
      ? `当前地区 ${regionText}。WARP 出口地区由池容器所在区域决定；要换地区，请在目标区域再部署一个池服务，然后回到这里「新增池」填入它的 API 地址和 token。`
      : "";
  }
  renderPoolNodes(nodes, view);
}

function renderPoolNodes(nodes, view) {
  const body = $("#pool-nodes");
  body.replaceChildren();
  if (!view?.ok) {
    emptyRow(body, 9, view?.error || "池未连接");
    return;
  }
  for (const node of nodes) {
    const rotate = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: "轮换" });
    rotate.addEventListener("click", () => poolNodeAction(node.user, "rotate", rotate));
    const toggle = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: node.enabled ? "停用" : "启用",
    });
    toggle.addEventListener("click", () => poolNodeAction(node.user, node.enabled ? "disable" : "enable", toggle));
    body.append(
      el("tr", {}, [
        el("td", { class: "mono", text: node.user }),
        el("td", { class: "num", text: String(node.port ?? "—") }),
        el("td", { text: node.state || "unknown" }),
        el("td", { class: "num", text: node.healthy ? "是" : "否" }),
        el("td", { class: "num", text: node.enabled ? "是" : "否" }),
        el("td", { class: "mono", text: node.exit_ipv6 || node.prefix64 || "—" }),
        el("td", { text: node.loc || "—" }),
        el("td", { class: "num", text: String(node.connections ?? 0) }),
        el("td", {}, [rotate, toggle]),
      ]),
    );
  }
  if (!nodes.length) emptyRow(body, 9, "池里还没有节点");
}

async function poolNodeAction(user, action, button) {
  button.disabled = true;
  try {
    await api(`/api/proxy/pools/${encodeURIComponent(state.poolDetail)}/nodes/${encodeURIComponent(user)}/${action}`, {
      method: "POST",
      body: "{}",
    });
    toast(`已提交 ${user} ${action}`, false);
    await loadPoolStatus();
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function probePool() {
  const apiURL = $("#pool-api").value.trim();
  const token = $("#pool-token").value.trim();
  const status = $("#pool-probe-status");
  if (!apiURL) {
    toast("先填池的 API 地址", true);
    return;
  }
  status.textContent = "正在连接池…";
  try {
    const body = { api_url: apiURL };
    if (token) body.token = token;
    else if (state.poolEditing) body.id = state.poolEditing;
    const result = await api("/api/proxy/pools/probe", {
      method: "POST",
      body: JSON.stringify(body),
    });
    const meta = result.meta || {};
    const summary = result.pool || {};
    const regions = (result.regions || []).map((item) => `${item.loc}:${item.count}`).join(" / ") || "—";
    if (!$("#pool-name").value.trim() && meta.pool?.id) $("#pool-name").value = meta.pool.id;
    if (result.suggested?.scope) $("#pool-scope").value = result.suggested.scope;
    status.textContent =
      `连接成功：池 ${meta.pool?.id || summary.id || "?"} · ` +
      `节点 ${summary.count ?? 0}（健康 ${summary.healthy ?? 0}）· 地区 ${regions} · ` +
      `私网 ${meta.api?.private_url || "—"} · 公网 ${meta.api?.public_url || "—"}`;
  } catch (error) {
    status.textContent = `连接失败：${error.message}`;
    toast(error.message, true);
  }
}

async function savePoolTuning() {
  const payload = {
    rotation_interval_seconds: Number($("#pool-rotation").value),
    reroll_batch: Number($("#pool-batch").value),
    health_interval_seconds: Number($("#pool-health").value),
    min_healthy: Number($("#pool-min-healthy").value),
  };
  if (Object.values(payload).some((value) => !Number.isFinite(value))) {
    toast("四个参数都要填数字", true);
    return;
  }
  try {
    await api(`/api/proxy/pools/${encodeURIComponent(state.poolDetail)}/config`, {
      method: "PUT",
      body: JSON.stringify(payload),
    });
    toast("池参数已保存并热更新", false);
    await loadPoolStatus();
  } catch (error) {
    toast(error.message, true);
  }
}

async function rerollPool() {
  try {
    await api(`/api/proxy/pools/${encodeURIComponent(state.poolDetail)}/reroll`, {
      method: "POST",
      body: "{}",
    });
    toast("已提交全部轮换，稍后刷新状态", false);
    setTimeout(() => loadPoolStatus().catch(() => {}), 2000);
  } catch (error) {
    toast(error.message, true);
  }
}

async function scalePool() {
  const count = Number($("#pool-scale-count").value);
  if (!Number.isInteger(count) || count < 1 || count > 64) {
    toast("节点数量需为 1-64 的整数", true);
    return;
  }
  if (!window.confirm(`把池扩缩容到 ${count} 个节点？会重启池 mux，API 与 SOCKS 会短暂中断。`)) return;
  try {
    await api(`/api/proxy/pools/${encodeURIComponent(state.poolDetail)}/scale`, {
      method: "POST",
      body: JSON.stringify({ count }),
    });
    toast(`已提交扩容到 ${count} 个节点，等待池重启 mux`, false);
    setTimeout(() => loadPoolStatus().catch(() => {}), 4000);
    setTimeout(() => loadPoolStatus().catch(() => {}), 12000);
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadPoolLogs() {
  if (!state.poolDetail) return;
  try {
    const result = await api(`/api/proxy/pools/${encodeURIComponent(state.poolDetail)}/logs?lines=200`);
    $("#pool-logs").textContent = (result.lines || []).join("\n") || "(空)";
  } catch (error) {
    toast(error.message, true);
  }
}

function pollPoolSync(attempt = 0) {
  clearTimeout(state.poolTimer);
  if (attempt > 30) return;
  state.poolTimer = setTimeout(async () => {
    try {
      await loadStore();
      await loadPools();
      if (state.store?.job?.running) pollPoolSync(attempt + 1);
      else await loadPoolStatus();
    } catch {
      pollPoolSync(attempt + 1);
    }
  }, 2000);
}

async function syncPoolsNow() {
  try {
    await api("/api/proxy/pools/sync", { method: "POST", body: "{}" });
    toast("已开始同步静态池", false);
    await loadPools();
    pollPoolSync();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderSettings() {
  const settings = state.store?.settings;
  if (!settings) return;
  $("#proxy-node-count").value = settings.node_count;
  $("#proxy-port-base").value = settings.port_base;
  $("#proxy-test-url").value = settings.test_url;
  $("#proxy-timeout").value = settings.test_timeout_seconds;
  $("#proxy-auto-select").checked = Boolean(settings.auto_select_healthy);
  renderEgressMode();
}

function renderNodes() {
  const body = $("#proxy-nodes");
  body.replaceChildren();
  const tone = statusTone;
  const labels = statusText();
  for (const node of state.nodes) {
    if (node.selected) state.selected.add(node.name);
    const box = el("input", { type: "checkbox", checked: state.selected.has(node.name) });
    box.addEventListener("change", () => {
      if (box.checked) state.selected.add(node.name);
      else state.selected.delete(node.name);
    });
    const test = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("测试", "proxy.test") });
    test.addEventListener("click", async () => {
      test.disabled = true;
      try {
        await api("/api/proxy/test", { method: "POST", body: JSON.stringify({ names: [node.name] }) });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        test.disabled = false;
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { class: "num" }, [box]),
        el("td", { class: "mono", text: node.name }),
        el("td", { text: node.provider }),
        el("td", { text: node.type }),
        el("td", { class: "mono", text: `${node.server}:${node.port}` }),
        el("td", { class: "num", text: node.listen_port ? String(node.listen_port) : "—" }),
        el("td", { class: "mono", text: node.egress_ip || "—" }),
        el("td", {
          class: `proxy-state ${quotaTone[node.quota] || ""}`,
          text: quotaLabel(node),
          title: node.quota_note || "",
        }),
        el("td", { class: `proxy-state ${tone[node.status] || ""}`, text: labels[node.status] || node.status }),
        el("td", { class: "num", text: node.latency_ms ? `${node.latency_ms} ms` : "—" }),
        el("td", { text: node.last_test ? localTime(node.last_test) : "—" }),
        el("td", {}, [test]),
      ]),
    );
  }
  if (!state.nodes.length) emptyRow(body, 12, t("没有符合条件的节点", "proxy.noNodes"));
  const pages = Math.max(1, Math.ceil(state.total / state.size));
  $("#proxy-page").textContent = `${state.page} / ${pages} · ${state.total}`;
  $("#proxy-prev").disabled = state.page <= 1;
  $("#proxy-next").disabled = state.page >= pages;
}

async function refreshAll() {
  try {
    await Promise.all([loadStore(), loadNodes(), loadHealth(), loadPools()]);
  } catch (error) {
    toast(error.message, true);
  }
}

async function syncSubscriptions() {
  try {
    await api("/api/proxy/sync", { method: "POST", body: "{}" });
    toast(t("已开始同步", "toast.proxy.sync"));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function testSelection(allMatching) {
  const body = allMatching
    ? { status: state.status || undefined, provider: state.provider || undefined }
    : { names: [...state.selected] };
  if (!allMatching && !body.names.length) {
    toast(t("先在列表里勾选节点", "toast.proxy.pickNodes"), true);
    return;
  }
  try {
    const result = await api("/api/proxy/test", { method: "POST", body: JSON.stringify(body) });
    toast(t(`已开始测试 ${result.targets} 个节点`, "toast.proxy.testing", result.targets));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

function splitLines(value) {
  return value
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function renderCFSources() {
  const body = $("#cf-sources");
  if (!body) return;
  body.replaceChildren();
  const sources = state.store?.cf_sources || [];
  for (const source of sources) {
    const edit = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("编辑", "common.edit") });
    edit.addEventListener("click", () => fillCFForm(source));
    const scan = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("优选 IP", "cf.scan") });
    scan.addEventListener("click", async () => {
      scan.disabled = true;
      const label = scan.textContent;
      scan.textContent = t("扫描中…", "cf.scanning");
      try {
        const result = await api(`/api/proxy/cf/${source.id}/scan`, {
          method: "POST",
          body: JSON.stringify({ candidates: 200, timeout_ms: 1500, keep: 8 }),
        });
        const best = result.report?.hits?.[0]?.latency_ms;
        const suffix = best == null ? "" : `，最佳 ${best} ms`;
        const tail = result.sync_started ? "；已自动同步" : "；同步订阅后生效";
        toast(t(`优选完成：${result.report.reachable} 个可用${suffix}${tail}`, "toast.cf.scanned", result.report.reachable, best ?? "—"));
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        scan.disabled = false;
        scan.textContent = label;
      }
    });
    const rebuild = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("生成", "cf.rebuild") });
    rebuild.addEventListener("click", async () => {
      rebuild.disabled = true;
      try {
        await api(`/api/proxy/cf/${source.id}/build`, { method: "POST", body: "{}" });
        toast(t("节点文件已重新生成", "toast.cf.built"));
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        rebuild.disabled = false;
      }
    });
    const remove = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      if (!confirm(t(`删除出口源「${source.name}」？`, "cf.confirmDelete", source.name))) return;
      try {
        await api(`/api/proxy/cf/${source.id}`, { method: "DELETE" });
        if (state.cfEditing === source.id) resetCFForm();
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { text: source.name }),
        el("td", { class: "mono proxy-url", text: source.host }),
        el("td", { class: "num", text: String((source.egress || []).length) }),
        el("td", { class: "num", text: String(source.node_count || 0) }),
        el("td", { text: source.last_build ? localTime(source.last_build) : "—" }),
        el("td", { text: source.last_error || "—" }),
        el("td", {}, [edit, scan, rebuild, remove]),
      ]),
    );
  }
  if (!sources.length) emptyRow(body, 7, t("还没有 Cloudflare 出口源", "cf.empty"));
}

function fillCFForm(source) {
  state.cfEditing = source.id || "";
  $("#cf-name").value = source.name || "";
  $("#cf-host").value = source.host || "";
  $("#cf-uuid").value = source.uuid || "";
  $("#cf-port").value = source.port || 443;
  $("#cf-path").value = source.path_template || "/proxyip=<ip>";
  $("#cf-egress").value = (source.egress || []).join("\n");
  $("#cf-edge").value = (source.edge || []).join("\n");
  $("#cf-enabled").checked = source.enabled !== false;
}

function resetCFForm() {
  fillCFForm({ enabled: true, path_template: "/proxyip=<ip>", port: 443 });
  $("#cf-deploy").value = "";
}

function cfPayload() {
  return {
    name: $("#cf-name").value.trim(),
    host: $("#cf-host").value.trim(),
    uuid: $("#cf-uuid").value.trim(),
    port: Number($("#cf-port").value) || 443,
    path_template: $("#cf-path").value.trim() || "/proxyip=<ip>",
    egress: splitLines($("#cf-egress").value),
    edge: splitLines($("#cf-edge").value),
    enabled: $("#cf-enabled").checked,
  };
}

async function saveCFSource() {
  const payload = cfPayload();
  if (!payload.name || !payload.host || !payload.uuid) {
    toast(t("名称、Worker 域名和 UUID 都要填", "toast.cf.required"), true);
    return;
  }
  try {
    const path = state.cfEditing ? `/api/proxy/cf/${state.cfEditing}` : "/api/proxy/cf";
    const result = await api(path, {
      method: state.cfEditing ? "PATCH" : "POST",
      body: JSON.stringify(payload),
    });
    state.cfEditing = result.id || state.cfEditing;
    toast(t(`已生成 ${result.node_count || 0} 个节点，点「同步订阅」加载`, "toast.cf.saved", result.node_count || 0));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function parseCFDeploy() {
  const text = $("#cf-deploy").value.trim();
  if (!text) {
    toast(t("先粘贴部署参数", "toast.cf.noDeploy"), true);
    return;
  }
  try {
    const params = await api("/api/proxy/cf/parse", {
      method: "POST",
      body: JSON.stringify({ text }),
    });
    $("#cf-uuid").value = params.uuid || "";
    $("#cf-host").value = params.host || "";
    $("#cf-path").value = params.path_template || "/proxyip=<ip>";
    if (!$("#cf-name").value) $("#cf-name").value = (params.host || "").split(".")[0] || "cloudflare";
    const socks = params.socks5 || [];
    const proxyIPs = params.proxyip || [];
    const list = (params.path_template || "").startsWith("/socks5=") && socks.length ? socks : proxyIPs;
    if (list.length) {
      $("#cf-egress").value =
        (params.path_template || "").startsWith("/socks5=")
          ? list.map((item) => (item.startsWith("socks5://") ? item : `socks5://${item}`)).join("\n")
          : list.join("\n");
    }
    toast(t("已填入解析结果", "toast.cf.parsed"));
  } catch (error) {
    toast(error.message, true);
  }
}

async function probeEgress(allMatching) {
  const body = allMatching
    ? { status: state.status || undefined, provider: state.provider || undefined }
    : { names: [...state.selected] };
  if (!allMatching && !body.names.length) {
    toast(t("先在列表里勾选节点", "toast.proxy.pickNodes"), true);
    return;
  }
  try {
    const result = await api("/api/proxy/egress", { method: "POST", body: JSON.stringify(body) });
    toast(t(`已开始探测 ${result.targets} 个出口`, "toast.proxy.egress", result.targets));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function applySelection(auto) {
  try {
    const result = await api("/api/proxy/select", {
      method: "POST",
      body: JSON.stringify(auto ? { auto: true } : { names: [...state.selected] }),
    });
    toast(t(`已应用 ${result.selected} 个出口`, "toast.proxy.applied", result.selected));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function saveSettings(event) {
  event?.preventDefault?.();
  try {
    await api("/api/proxy/settings", {
      method: "PUT",
      body: JSON.stringify({
        node_count: Number($("#proxy-node-count").value),
        port_base: Number($("#proxy-port-base").value),
        test_url: $("#proxy-test-url").value.trim(),
        test_timeout_seconds: Number($("#proxy-timeout").value),
        auto_select_healthy: $("#proxy-auto-select").checked,
        egress_mode: $("#proxy-egress-mode").value,
      }),
    });
    toast(t("设置已保存", "toast.proxy.settings"));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function applyEgressMode() {
  const button = $("#proxy-mode-apply");
  const mode = $("#proxy-egress-mode").value;
  button.disabled = true;
  try {
    const result = await api("/api/proxy/egress-mode", {
      method: "PUT",
      body: JSON.stringify({ mode }),
    });
    if (result.changed === false) toast(t("出口方式未变化", "toast.proxy.modeSame"));
    else toast(t("正在切换出口方式…", "toast.proxy.modeSwitching"));
    await refreshAll();
  } catch (error) {
    state.egressPick = null;
    renderEgressMode();
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function addProvider(event) {
  event?.preventDefault?.();
  try {
    await api("/api/proxy/providers", {
      method: "POST",
      body: JSON.stringify({
        name: $("#proxy-provider-name").value.trim(),
        url: $("#proxy-provider-url").value.trim(),
      }),
    });
    $("#proxy-provider-name").value = "";
    $("#proxy-provider-url").value = "";
    toast(t("订阅已添加，记得点同步", "toast.proxy.providerAdded"));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadLogs() {
  try {
    const result = await api("/api/proxy/logs");
    if (!hasSession()) return;
    $("#proxy-logs").textContent = (result.lines || []).join("\n") || t("(空)", "proxy.logsEmpty");
  } catch (error) {
    toast(error.message, true);
  }
}

function showPanel(name) {
  state.panel = name;
  for (const button of document.querySelectorAll("#proxy-tabs [data-proxy-tab]")) {
    const active = button.dataset.proxyTab === name;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", active ? "true" : "false");
  }
  for (const panel of document.querySelectorAll("#page-proxy [data-proxy-panel]"))
    panel.classList.toggle("hidden", panel.dataset.proxyPanel !== name);
  if (name === "logs") loadLogs();
  if (name === "pools") {
    loadPools().catch((error) => toast(error.message, true));
    loadPoolStatus().catch((error) => toast(error.message, true));
  }
}

function bind() {
  $("#proxy-refresh").addEventListener("click", refreshAll);
  $("#proxy-sync").addEventListener("click", syncSubscriptions);
  $("#proxy-test-all").addEventListener("click", () => testSelection(true));
  $("#proxy-test-picked").addEventListener("click", () => testSelection(false));
  $("#proxy-egress-all").addEventListener("click", () => probeEgress(true));
  $("#proxy-egress-picked").addEventListener("click", () => probeEgress(false));
  $("#proxy-apply").addEventListener("click", () => applySelection(false));
  $("#proxy-apply-auto").addEventListener("click", () => applySelection(true));
  $("#cf-save").addEventListener("click", saveCFSource);
  $("#cf-parse").addEventListener("click", parseCFDeploy);
  $("#cf-reset").addEventListener("click", resetCFForm);
  for (const id of ["cf-name", "cf-host", "cf-uuid", "cf-port", "cf-path"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      saveCFSource();
    });
  $("#proxy-settings-save").addEventListener("click", saveSettings);
  $("#proxy-mode-apply").addEventListener("click", applyEgressMode);
  $("#proxy-provider-add").addEventListener("click", addProvider);
  $("#pool-sync-now").addEventListener("click", syncPoolsNow);
  $("#pool-add").addEventListener("click", () => {
    resetPoolForm();
    $("#pool-editor").classList.remove("hidden");
    $("#pool-name").focus();
  });
  $("#pool-save").addEventListener("click", savePoolForm);
  $("#pool-probe").addEventListener("click", probePool);
  $("#pool-scale-apply").addEventListener("click", scalePool);
  $("#pool-cancel").addEventListener("click", resetPoolForm);
  $("#pool-refresh-status").addEventListener("click", () =>
    loadPoolStatus().catch((error) => toast(error.message, true)),
  );
  $("#pool-tune-save").addEventListener("click", savePoolTuning);
  $("#pool-reroll").addEventListener("click", rerollPool);
  $("#pool-detail-close").addEventListener("click", () => {
    state.poolDetail = "";
    $("#pool-detail").classList.add("hidden");
  });
  $("#pool-logs-refresh").addEventListener("click", loadPoolLogs);
  for (const id of ["pool-name", "pool-api", "pool-token"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      savePoolForm();
    });
  for (const id of ["pool-rotation", "pool-batch", "pool-health", "pool-min-healthy"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      savePoolTuning();
    });
  $("#proxy-prev").addEventListener("click", () => {
    if (state.page > 1) {
      state.page -= 1;
      loadNodes();
    }
  });
  $("#proxy-next").addEventListener("click", () => {
    state.page += 1;
    loadNodes();
  });
  const reload = () => {
    state.page = 1;
    loadNodes();
  };
  $("#proxy-filter-status").addEventListener("change", (event) => {
    state.status = event.target.value;
    reload();
  });
  $("#proxy-filter-q").addEventListener("input", (event) => {
    state.q = event.target.value.trim();
    clearTimeout(state.qTimer);
    state.qTimer = setTimeout(reload, 300);
  });
  $("#proxy-filter-q").addEventListener("keydown", (event) => {
    if (event.key === "Enter") event.preventDefault();
  });
  // The proxy forms live inside the outer config form now, so Enter must not
  // bubble up into an implicit config submit.
  for (const id of ["proxy-provider-name", "proxy-provider-url"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      addProvider();
    });
  for (const id of ["proxy-node-count", "proxy-port-base", "proxy-test-url", "proxy-timeout"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      saveSettings();
    });
  $("#proxy-logs-refresh").addEventListener("click", loadLogs);
  for (const button of document.querySelectorAll("#proxy-tabs [data-proxy-tab]"))
    button.addEventListener("click", () => showPanel(button.dataset.proxyTab));
}

let bound = false;

export default {
  id: "proxy",
  title: "代理中心",
  sub: "管理订阅、节点池与本地 SOCKS 出口。",
  init() {
    if (bound) return;
    bound = true;
    bind();
    showPanel(state.panel);
    onChange(() => {
      if (state.store) renderOverview();
      if (state.store) renderEgressMode();
    });
  },
  show() {
    showPanel(state.panel);
    refreshAll();
  },
  hide() {
    clearTimeout(state.timer);
    clearTimeout(state.poolTimer);
  },
};
